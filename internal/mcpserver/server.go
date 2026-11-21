// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package mcpserver exposes nbsandbox to AI agents as Model Context Protocol
// tools: spaces, files, runs, functions, interactive sessions and the
// runtime, each a thin call to the REST API as the agent's user. The
// server holds no data and grants nothing: every call is authorised by
// nbsandbox, with the caller's own credentials.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
)

// Options configure the server.
type Options struct {
	// Client calls nbsandbox's API.
	Client *apiclient.Client
	// Identity is used for every call (stdio: one local user). Nil: each
	// call must carry its own Authorization header (HTTP mode), which is
	// passed through to nbsandbox unchanged.
	Identity *apiclient.Identity
	// Version is reported to MCP clients.
	Version string
}

// MaxText is the most text a tool returns in one answer (a log tail, a
// file); more is cut, with a note saying so.
const MaxText = 64 << 10

// New returns the MCP server with every tool.
func New(o Options) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "nbsandbox", Title: "nbsandbox", Version: o.Version}, &mcp.ServerOptions{
		Instructions: instructions,
	})
	t := &tools{o: o}
	t.register(s)
	return s
}

const instructions = `nbsandbox runs Jupyter notebooks (Python) in isolated sandboxes: each run gets a fresh container with no network, limited memory, CPU and time, and a copy of its space's files.

Typical use: create or pick a space, write a notebook (nbformat 4 JSON) and any data files into it, run the notebook with parameters, and read the result: its status, the error with the failing cell, the value it returned with nbsandbox.output(...), the log, and the files it wrote to its artifacts. Published functions are notebooks with an input and output contract; invoke them with parameters.

To explore step by step, open a session on a notebook (open_session): a live kernel where run_cell runs code and the variables stay between calls, so data is loaded once. End it with end_session when done.

Runs can't reach the internet: everything a notebook needs must be in the space or in the runner image (nbsandbox_runtime lists its packages).`

// tools implements the handlers.
type tools struct{ o Options }

func (t *tools) who(req *mcp.CallToolRequest) (*apiclient.Identity, error) {
	if t.o.Identity != nil {
		return t.o.Identity, nil
	}
	if req != nil && req.Extra != nil {
		if h := req.Extra.Header.Get("Authorization"); h != "" {
			return &apiclient.Identity{Header: h}, nil
		}
	}
	return nil, errors.New("no credentials: send an Authorization header (Bearer token or Basic) with each request")
}

// call does one API request and decodes a JSON answer into a generic value.
func (t *tools) call(ctx context.Context, req *mcp.CallToolRequest, method, p string, body any, okCodes ...int) (any, int, error) {
	who, err := t.who(req)
	if err != nil {
		return nil, 0, err
	}
	var r apiclient.Response
	if body != nil {
		r, err = t.o.Client.JSONCall(ctx, who, method, p, body)
	} else {
		r, err = t.o.Client.Do(ctx, who, method, p, nil, nil)
	}
	if err != nil {
		return nil, 0, err
	}
	if !contains(okCodes, r.Code) {
		return nil, r.Code, apiError(r)
	}
	if len(r.Body) == 0 {
		return map[string]any{"ok": true}, r.Code, nil
	}
	var v any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		return nil, r.Code, fmt.Errorf("%s %s: not JSON (%d)", method, p, r.Code)
	}
	return v, r.Code, nil
}

// text does one API request whose answer is text (a log, a file).
func (t *tools) text(ctx context.Context, req *mcp.CallToolRequest, p string) (string, error) {
	who, err := t.who(req)
	if err != nil {
		return "", err
	}
	r, err := t.o.Client.Do(ctx, who, "GET", p, nil, nil)
	if err != nil {
		return "", err
	}
	if r.Code != http.StatusOK {
		return "", apiError(r)
	}
	return string(r.Body), nil
}

// apiError turns an error answer into the API's own message.
func apiError(r apiclient.Response) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(r.Body, &e) == nil && e.Error.Message != "" {
		return fmt.Errorf("nbsandbox: %d %s: %s", r.Code, e.Error.Code, e.Error.Message)
	}
	return fmt.Errorf("nbsandbox: %d %.300s", r.Code, strings.TrimSpace(string(r.Body)))
}

func contains(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// seg escapes one path segment (an ID or a name).
func seg(s string) string { return url.PathEscape(s) }

// filePath escapes a file path, keeping its folders.
func filePath(p string) string {
	parts := strings.Split(strings.Trim(path.Clean("/"+p), "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// cut keeps the last n bytes of s, with a note when it cut.
func cut(s string, n int, fromEnd bool) string {
	if len(s) <= n {
		return s
	}
	if fromEnd {
		return fmt.Sprintf("[%d earlier bytes not shown]\n", len(s)-n) + s[len(s)-n:]
	}
	return s[:n] + fmt.Sprintf("\n[%d more bytes not shown]", len(s)-n)
}
