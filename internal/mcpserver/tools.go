// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Inputs. Fields without omitempty are required; the jsonschema tag is the
// description an agent sees.

type noInput struct{}

type spaceInput struct {
	SpaceID string `json:"space_id" jsonschema:"the space's ID (from list_spaces or create_space)"`
}

type createSpaceInput struct {
	Name string `json:"name" jsonschema:"a name for the space"`
}

type fileInput struct {
	SpaceID string `json:"space_id" jsonschema:"the space's ID"`
	Path    string `json:"path" jsonschema:"the file's path in the space, e.g. analysis.ipynb or data/input.csv"`
}

type writeFileInput struct {
	SpaceID string `json:"space_id" jsonschema:"the space's ID"`
	Path    string `json:"path" jsonschema:"the file's path in the space; folders are created as needed; an existing file is replaced"`
	Content string `json:"content" jsonschema:"the file's text; for a notebook, its nbformat 4 JSON"`
}

type runNotebookInput struct {
	SpaceID     string         `json:"space_id" jsonschema:"the space's ID"`
	Notebook    string         `json:"notebook" jsonschema:"path of the .ipynb file in the space"`
	Parameters  map[string]any `json:"parameters,omitempty" jsonschema:"values for the notebook's parameters cell (any JSON)"`
	ReadOnly    bool           `json:"read_only,omitempty" jsonschema:"mount the space read-only, so the run can't change its files"`
	WaitSeconds *int           `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the result, 0 to 600 (default 120); if the run is still going, call get_run_result later"`
}

type runInput struct {
	SpaceID string `json:"space_id" jsonschema:"the space's ID"`
	RunID   string `json:"run_id" jsonschema:"the run's ID"`
}

type runResultInput struct {
	SpaceID     string `json:"space_id" jsonschema:"the space's ID"`
	RunID       string `json:"run_id" jsonschema:"the run's ID"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the run to finish, 0 to 600 (default 120)"`
}

type runLogInput struct {
	SpaceID   string `json:"space_id" jsonschema:"the space's ID"`
	RunID     string `json:"run_id" jsonschema:"the run's ID"`
	TailBytes int    `json:"tail_bytes,omitempty" jsonschema:"how much of the end of the log to return (default and most: 65536)"`
}

type artifactInput struct {
	SpaceID string `json:"space_id" jsonschema:"the space's ID"`
	RunID   string `json:"run_id" jsonschema:"the run's ID"`
	Path    string `json:"path" jsonschema:"the artifact's path, from list_artifacts"`
}

type functionInput struct {
	SpaceID string `json:"space_id" jsonschema:"the space's ID"`
	Name    string `json:"name" jsonschema:"the function's name"`
}

type invokeInput struct {
	SpaceID     string         `json:"space_id" jsonschema:"the space's ID"`
	Name        string         `json:"name" jsonschema:"the function's name"`
	Version     int            `json:"version,omitempty" jsonschema:"a published version (default: the latest)"`
	Parameters  map[string]any `json:"parameters,omitempty" jsonschema:"the function's input, matching its contract (get_function)"`
	WaitSeconds *int           `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the result, 0 to 600 (default 120)"`
}

func readOnly(title string) *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &no}
}

func changes(title string, destructive bool) *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: &destructive, OpenWorldHint: &no}
}

func (t *tools) register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "nbsandbox_runtime", Annotations: readOnly("Runtime and packages"),
		Description: "The Python version, installed packages and per-run limits (memory, CPUs, time, disk) of the runner every notebook runs in. Runs have no network: a package not listed here can't be installed."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "GET", "/v1/runtime", nil, http.StatusOK)
			if m, ok := v.(map[string]any); ok {
				delete(m, "modules") // the package list says the same, shorter
				delete(m, "completions")
			}
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_spaces", Annotations: readOnly("List spaces"),
		Description: "The caller's spaces: each holds files (notebooks, data) and the runs made from them."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "GET", "/v1/spaces", nil, http.StatusOK)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "create_space", Annotations: changes("Create a space", false),
		Description: "Create a new, empty space and return it (its id is what the other tools take as space_id)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in createSpaceInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "POST", "/v1/spaces", map[string]string{"name": in.Name}, http.StatusCreated)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_files", Annotations: readOnly("List files"),
		Description: "The space's files and folders, with sizes and versions."},
		func(ctx context.Context, req *mcp.CallToolRequest, in spaceInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "GET", "/v1/spaces/"+seg(in.SpaceID)+"/files", nil, http.StatusOK)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "read_file", Annotations: readOnly("Read a file"),
		Description: fmt.Sprintf("A text file of the space (a notebook's JSON, a CSV, code). The first %d KiB at most.", MaxText>>10)},
		func(ctx context.Context, req *mcp.CallToolRequest, in fileInput) (*mcp.CallToolResult, any, error) {
			body, err := t.text(ctx, req, "/v1/spaces/"+seg(in.SpaceID)+"/files/"+filePath(in.Path))
			if err != nil {
				return nil, nil, err
			}
			return textResult(body, false), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "write_file", Annotations: changes("Write a file", true),
		Description: "Write a text file into the space (a notebook as nbformat 4 JSON, a CSV, a Python module), replacing a file with the same path. Notebooks must be Python notebooks."},
		func(ctx context.Context, req *mcp.CallToolRequest, in writeFileInput) (*mcp.CallToolResult, any, error) {
			v, err := t.upload(ctx, req, in.SpaceID, in.Path, []byte(in.Content))
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "run_notebook", Annotations: changes("Run a notebook", false),
		Description: "Run a notebook of the space in a fresh sandbox, with parameters, and wait for its result: the status, the error with the failing cell if it failed, and the value it returned with nbsandbox.output(...). " +
			"A run works on a copy of the space; its changes to the files are written back when it ends (unless read_only). Files it writes under its artifacts folder are listed by list_artifacts."},
		func(ctx context.Context, req *mcp.CallToolRequest, in runNotebookInput) (*mcp.CallToolResult, any, error) {
			body := map[string]any{"notebook": in.Notebook}
			if in.Parameters != nil {
				body["parameters"] = in.Parameters
			}
			if in.ReadOnly {
				body["space"] = "read-only"
			}
			v, _, err := t.call(ctx, req, "POST", "/v1/spaces/"+seg(in.SpaceID)+"/runs", body, http.StatusCreated, http.StatusOK)
			if err != nil {
				return nil, nil, err
			}
			id, _ := v.(map[string]any)["id"].(string)
			if id == "" {
				return nil, v, nil
			}
			out, err := t.waitResult(ctx, req, in.SpaceID, id, in.WaitSeconds)
			return nil, out, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_run_result", Annotations: readOnly("Get a run's result"),
		Description: "Wait for a run to finish (up to wait_seconds) and return its result: status, error and the notebook's output. If it is still going, returns the run as it is."},
		func(ctx context.Context, req *mcp.CallToolRequest, in runResultInput) (*mcp.CallToolResult, any, error) {
			out, err := t.waitResult(ctx, req, in.SpaceID, in.RunID, in.WaitSeconds)
			return nil, out, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_run", Annotations: readOnly("Get a run"),
		Description: "A run's details now: status, timing, resource usage, error, without waiting."},
		func(ctx context.Context, req *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "GET", "/v1/spaces/"+seg(in.SpaceID)+"/runs/"+seg(in.RunID), nil, http.StatusOK)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_runs", Annotations: readOnly("List runs"),
		Description: "The space's runs, newest first."},
		func(ctx context.Context, req *mcp.CallToolRequest, in spaceInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "GET", "/v1/spaces/"+seg(in.SpaceID)+"/runs", nil, http.StatusOK)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_run_log", Annotations: readOnly("Get a run's log"),
		Description: "The end of a run's log: what its cells printed, and nbsandbox's own lines about the run (start, cells, finish)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in runLogInput) (*mcp.CallToolResult, any, error) {
			body, err := t.text(ctx, req, "/v1/spaces/"+seg(in.SpaceID)+"/runs/"+seg(in.RunID)+"/log")
			if err != nil {
				return nil, nil, err
			}
			n := in.TailBytes
			if n <= 0 || n > MaxText {
				n = MaxText
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: cut(body, n, true)}}}, nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "cancel_run", Annotations: changes("Cancel a run", true),
		Description: "Stop a queued or running run. Its partial output is kept."},
		func(ctx context.Context, req *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "POST", "/v1/spaces/"+seg(in.SpaceID)+"/runs/"+seg(in.RunID)+"/cancel", nil, http.StatusAccepted, http.StatusOK)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_artifacts", Annotations: readOnly("List a run's artifacts"),
		Description: "The files a run wrote to its artifacts folder (charts, CSVs, models)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "GET", "/v1/spaces/"+seg(in.SpaceID)+"/runs/"+seg(in.RunID)+"/artifacts", nil, http.StatusOK)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "read_artifact", Annotations: readOnly("Read an artifact"),
		Description: fmt.Sprintf("A text artifact of a run (CSV, JSON, Markdown). The first %d KiB at most; binary files are described, not returned.", MaxText>>10)},
		func(ctx context.Context, req *mcp.CallToolRequest, in artifactInput) (*mcp.CallToolResult, any, error) {
			body, err := t.text(ctx, req, "/v1/spaces/"+seg(in.SpaceID)+"/runs/"+seg(in.RunID)+"/artifacts/"+filePath(in.Path))
			if err != nil {
				return nil, nil, err
			}
			return textResult(body, false), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_functions", Annotations: readOnly("List functions"),
		Description: "The space's published functions: notebooks callable with an input and an output contract."},
		func(ctx context.Context, req *mcp.CallToolRequest, in spaceInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "GET", "/v1/spaces/"+seg(in.SpaceID)+"/functions", nil, http.StatusOK)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_function", Annotations: readOnly("Get a function's contract"),
		Description: "A function's input and output schemas (JSON Schema) for its latest version: what invoke_function's parameters must look like."},
		func(ctx context.Context, req *mcp.CallToolRequest, in functionInput) (*mcp.CallToolResult, any, error) {
			v, _, err := t.call(ctx, req, "GET", "/v1/spaces/"+seg(in.SpaceID)+"/functions/"+seg(in.Name)+"/contract", nil, http.StatusOK)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "invoke_function", Annotations: changes("Invoke a function", false),
		Description: "Call a published function with parameters and wait for its result. Function calls never change the space's files."},
		func(ctx context.Context, req *mcp.CallToolRequest, in invokeInput) (*mcp.CallToolResult, any, error) {
			p := "/v1/spaces/" + seg(in.SpaceID) + "/functions/" + seg(in.Name)
			if in.Version > 0 {
				p += fmt.Sprintf("/versions/%d", in.Version)
			}
			body := map[string]any{}
			if in.Parameters != nil {
				body["parameters"] = in.Parameters
			}
			v, _, err := t.call(ctx, req, "POST", p+"/invoke", body, http.StatusCreated, http.StatusOK)
			if err != nil {
				return nil, nil, err
			}
			id, _ := v.(map[string]any)["id"].(string)
			if id == "" {
				return nil, v, nil
			}
			out, err := t.waitResult(ctx, req, in.SpaceID, id, in.WaitSeconds)
			return nil, out, err
		})

	t.registerSessions(s)
}

// waitResult waits for the run's result, in steps of the API's longest
// wait (60 s), up to the asked time.
func (t *tools) waitResult(ctx context.Context, req *mcp.CallToolRequest, spaceID, runID string, waitSeconds *int) (any, error) {
	total := 120
	if waitSeconds != nil {
		total = min(max(*waitSeconds, 0), 600)
	}
	deadline := time.Now().Add(time.Duration(total) * time.Second)
	base := "/v1/spaces/" + seg(spaceID) + "/runs/" + seg(runID)
	for {
		left := max(int(time.Until(deadline).Seconds()), 0)
		v, code, err := t.call(ctx, req, "GET", fmt.Sprintf("%s/result?wait=%ds", base, min(left, 60)),
			nil, http.StatusOK, http.StatusAccepted, http.StatusConflict)
		if err != nil {
			return nil, err
		}
		if code == http.StatusOK {
			return map[string]any{"run_id": runID, "finished": true, "result": v}, nil
		}
		if left == 0 || ctx.Err() != nil {
			run, _, err := t.call(ctx, req, "GET", base, nil, http.StatusOK)
			if err != nil {
				return nil, err
			}
			return map[string]any{"run_id": runID, "finished": false, "run": run,
				"note": "still running: call get_run_result again to keep waiting, get_run_log to follow it, or cancel_run"}, nil
		}
	}
}

// upload writes one file, creating its folder.
func (t *tools) upload(ctx context.Context, req *mcp.CallToolRequest, spaceID, p string, content []byte) (any, error) {
	who, err := t.who(req)
	if err != nil {
		return nil, err
	}
	clean := strings.Trim(path.Clean("/"+p), "/")
	dir, name := path.Split(clean)
	if name == "" {
		return nil, fmt.Errorf("path %q names no file", p)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, err := mw.CreateFormFile("file", name)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(content); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	q := ""
	if dir = strings.Trim(dir, "/"); dir != "" {
		q = "?folder=" + filePath(dir)
	}
	r, err := t.o.Client.Do(ctx, who, "POST", "/v1/spaces/"+seg(spaceID)+"/files"+q, &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
	if err != nil {
		return nil, err
	}
	if r.Code != http.StatusCreated {
		return nil, apiError(r)
	}
	var v any
	if err := r.JSON(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// textResult returns a file's text, or a note when it isn't text.
func textResult(body string, _ bool) *mcp.CallToolResult {
	if !utf8.ValidString(body) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("[a binary file of %d bytes: not shown]", len(body))}}}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: cut(body, MaxText, false)}}}
}
