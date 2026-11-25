// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
)

// Interactive sessions (docs/sessions.md): a notebook's live kernel, kept
// between tool calls by nbsandbox. Each tool call connects to the
// session's channel, does its one thing and disconnects; the session and
// its variables go on until end_session or until it is left idle.

type openSessionInput struct {
	SpaceID     string `json:"space_id" jsonschema:"the space's ID"`
	Notebook    string `json:"notebook" jsonschema:"path of the .ipynb file in the space the session belongs to"`
	ReadOnly    bool   `json:"read_only,omitempty" jsonschema:"mount the space read-only: the kernel can't change its files (and can open beside a run that changes them)"`
	Environment string `json:"environment,omitempty" jsonschema:"the space environment its connections' names are read in, e.g. dev or prod (default: the space's default)"`
}

type sessionInput struct {
	SpaceID   string `json:"space_id" jsonschema:"the space's ID"`
	SessionID string `json:"session_id" jsonschema:"the session's ID (from open_session or list_sessions)"`
}

type runCellInput struct {
	SpaceID     string `json:"space_id" jsonschema:"the space's ID"`
	SessionID   string `json:"session_id" jsonschema:"the session's ID (from open_session)"`
	Code        string `json:"code" jsonschema:"Python code to run in the session's kernel, like a notebook cell; variables stay for the next cells"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the cell to finish, 1 to 600 (default 120); a cell still running goes on, its output after that is not returned"`
}

type completeInput struct {
	SpaceID   string `json:"space_id" jsonschema:"the space's ID"`
	SessionID string `json:"session_id" jsonschema:"the session's ID"`
	Code      string `json:"code" jsonschema:"the code to complete, e.g. df[\" or pd.read_"`
	CursorPos *int   `json:"cursor_pos,omitempty" jsonschema:"where to complete, in characters (default: the end of code)"`
}

// maxImages is the most images (charts) run_cell returns for one cell.
const maxImages = 4

func (t *tools) registerSessions(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "open_session", Annotations: changes("Open a session", false),
		Description: "Start (or get) a notebook's live kernel: an interactive session. Then run_cell runs code in it one cell at a time, and the variables stay between cells, " +
			"so a slow step (loading data, training) runs once. Same sandbox as a run: no network, the same limits, only this space's files. " +
			"A read-write session writes the files its kernel changes back to the space. End it with end_session when done; one left idle with nobody connected ends by itself (30 minutes by default)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in openSessionInput) (*mcp.CallToolResult, any, error) {
			mode := "read-write"
			if in.ReadOnly {
				mode = "read-only"
			}
			body := map[string]string{"notebook": in.Notebook, "mode": mode}
			if in.Environment != "" {
				body["environment"] = in.Environment
			}
			v, _, err := t.call(ctx, req, "POST", "/v1/spaces/"+seg(in.SpaceID)+"/sessions", body, http.StatusCreated, http.StatusOK)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_sessions", Annotations: readOnly("List sessions"),
		Description: "The space's open sessions: notebook, mode and status."},
		func(ctx context.Context, req *mcp.CallToolRequest, in spaceInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "GET", "/v1/spaces/"+seg(in.SpaceID)+"/sessions", nil, http.StatusOK)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "run_cell", Annotations: changes("Run code in a session", false),
		Description: "Run Python code in a session's kernel and return what it printed, its result (the last expression), the error with its traceback if it failed, and charts as images. " +
			"The session's variables, imports and loaded data stay for the next call."},
		func(ctx context.Context, req *mcp.CallToolRequest, in runCellInput) (*mcp.CallToolResult, any, error) {
			wait := 120
			if in.WaitSeconds != nil {
				wait = min(max(*in.WaitSeconds, 1), 600)
			}
			return t.runCell(ctx, req, in.SpaceID, in.SessionID, in.Code, time.Duration(wait)*time.Second)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "complete_code", Annotations: readOnly("Complete code in a session"),
		Description: "Ask a session's kernel to complete code from its live objects: a DataFrame's columns after df[\", a module's names after np., a function's keyword arguments."},
		func(ctx context.Context, req *mcp.CallToolRequest, in completeInput) (*mcp.CallToolResult, any, error) {
			pos := len([]rune(in.Code))
			if in.CursorPos != nil {
				pos = *in.CursorPos
			}
			k, err := t.connect(ctx, req, in.SpaceID, in.SessionID)
			if err != nil {
				return nil, nil, err
			}
			defer func() { _ = k.Close() }()
			id, err := k.Send("shell", "complete_request", map[string]any{"code": in.Code, "cursor_pos": pos})
			if err != nil {
				return nil, nil, err
			}
			m, err := k.Next(ctx, 30*time.Second, func(m apiclient.KernelMsg) bool {
				return (m.Parent["msg_id"] == id && m.Type() == "complete_reply") || ended(m)
			})
			if err != nil {
				return nil, nil, err
			}
			if ended(m) {
				return nil, nil, fmt.Errorf("the session ended: %v", m.Content["reason"])
			}
			return nil, map[string]any{"matches": m.Content["matches"], "cursor_start": m.Content["cursor_start"], "cursor_end": m.Content["cursor_end"]}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "interrupt_session", Annotations: changes("Interrupt a session's cell", false),
		Description: "Stop the cell running in a session (KeyboardInterrupt). The kernel and its variables stay."},
		func(ctx context.Context, req *mcp.CallToolRequest, in sessionInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "POST", "/v1/spaces/"+seg(in.SpaceID)+"/sessions/"+seg(in.SessionID)+"/interrupt", nil,
				http.StatusOK, http.StatusAccepted, http.StatusNoContent)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "end_session", Annotations: changes("End a session", true),
		Description: "End a session: its kernel stops and its variables are gone. A read-write session's changed files are written to the space first."},
		func(ctx context.Context, req *mcp.CallToolRequest, in sessionInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "DELETE", "/v1/spaces/"+seg(in.SpaceID)+"/sessions/"+seg(in.SessionID), nil,
				http.StatusOK, http.StatusNoContent)
			return nil, v, err
		})
}

// connect opens the session's channel as the caller.
func (t *tools) connect(ctx context.Context, req *mcp.CallToolRequest, spaceID, sessionID string) (*apiclient.Kernel, error) {
	who, err := t.who(req)
	if err != nil {
		return nil, err
	}
	k, err := t.o.Client.ConnectSession(ctx, who, seg(spaceID), seg(sessionID))
	if err != nil {
		return nil, fmt.Errorf("%w (is the session still open? list_sessions shows the open ones)", err)
	}
	return k, nil
}

func ended(m apiclient.KernelMsg) bool {
	return m.Channel == "bridge" && m.Content["status"] == "ended"
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// runCell runs code and gathers its output until the kernel is idle again.
func (t *tools) runCell(ctx context.Context, req *mcp.CallToolRequest, spaceID, sessionID, code string, wait time.Duration) (*mcp.CallToolResult, any, error) {
	k, err := t.connect(ctx, req, spaceID, sessionID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = k.Close() }()
	id, err := k.Send("shell", "execute_request", map[string]any{"code": code, "silent": false})
	if err != nil {
		return nil, nil, err
	}
	var (
		out      strings.Builder
		images   []mcp.Content
		dropped  int
		failed   bool
		finished bool
		reply    string // execute_reply's status
		count    any
	)
	addData := func(data map[string]any) {
		if png, ok := data["image/png"].(string); ok {
			if len(images) >= maxImages {
				dropped++
			} else if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(png)); err == nil {
				images = append(images, &mcp.ImageContent{Data: b, MIMEType: "image/png"})
				return
			}
		}
		if text, ok := data["text/plain"].(string); ok {
			out.WriteString(text + "\n")
		}
	}
	deadline := time.Now().Add(wait)
	for !finished {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		m, err := k.Next(ctx, left, func(m apiclient.KernelMsg) bool { return m.Parent["msg_id"] == id || ended(m) })
		if err != nil {
			if ctx.Err() == nil && time.Now().After(deadline) {
				break // waited long enough; the cell goes on
			}
			return nil, nil, err
		}
		if ended(m) {
			return nil, nil, fmt.Errorf("the session ended: %v", m.Content["reason"])
		}
		data, _ := m.Content["data"].(map[string]any)
		switch m.Type() {
		case "execute_input":
			count = m.Content["execution_count"]
		case "stream":
			text, _ := m.Content["text"].(string)
			out.WriteString(text)
		case "execute_result", "display_data":
			addData(data)
		case "error":
			failed = true
			var tb []string
			if l, ok := m.Content["traceback"].([]any); ok {
				for _, x := range l {
					s, _ := x.(string)
					tb = append(tb, ansi.ReplaceAllString(s, ""))
				}
			}
			fmt.Fprintf(&out, "%v: %v\n%s\n", m.Content["ename"], m.Content["evalue"], strings.Join(tb, "\n"))
		case "execute_reply":
			reply, _ = m.Content["status"].(string)
		case "status":
			finished = m.Content["execution_state"] == "idle"
		}
	}
	if finished && reply == "" {
		// The reply comes on another channel: it can follow the idle status.
		if m, err := k.Next(ctx, 2*time.Second, func(m apiclient.KernelMsg) bool {
			return m.Parent["msg_id"] == id && m.Type() == "execute_reply"
		}); err == nil {
			reply, _ = m.Content["status"].(string)
		}
	}
	status := "ok"
	switch {
	case !finished:
		status = "running"
	case failed:
		status = "error"
	case reply == "aborted":
		// ipykernel skips requests queued when a cell fails or is interrupted.
		status = "aborted"
		failed = true
		out.WriteString("[not run: the kernel skipped it because the cell before it failed or was interrupted; run it again]\n")
	}
	head, _ := json.Marshal(map[string]any{"status": status, "execution_count": count})
	text := string(head) + "\n" + cut(out.String(), MaxText, true)
	if !finished {
		text += fmt.Sprintf("\n[still running after %s: it goes on in the session; interrupt_session stops it; its later output is not returned]", wait)
	}
	if dropped > 0 {
		text += fmt.Sprintf("\n[%d more images not shown]", dropped)
	}
	return &mcp.CallToolResult{IsError: failed, Content: append([]mcp.Content{&mcp.TextContent{Text: text}}, images...)}, nil, nil
}
