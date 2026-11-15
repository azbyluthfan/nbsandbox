// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package apiclient is a small client for nbsandbox's REST API, shared by
// `nbsandbox-report check` and the MCP server.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Identity is who a request is made as: a password (basic auth) or a
// bearer token.
type Identity struct {
	Name     string
	Password string
	Token    string
}

// Client calls a deployment's API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Inspect, if set, sees every answer (the integration suite checks
	// them against api/openapi.yaml).
	Inspect func(method, path string, resp *http.Response, body []byte)
}

// Response is an API answer.
type Response struct {
	Code   int
	Body   []byte
	Header http.Header
}

// JSON decodes the body.
func (r Response) JSON(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("not JSON (%d): %.200s", r.Code, r.Body)
	}
	return nil
}

// Do sends a request as who (nil: no credentials).
func (c *Client) Do(ctx context.Context, who *Identity, method, path string, body io.Reader, headers map[string]string) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, body)
	if err != nil {
		return Response{}, err
	}
	if who != nil {
		if who.Token != "" {
			req.Header.Set("Authorization", "Bearer "+who.Token)
		} else {
			req.SetBasicAuth(who.Name, who.Password)
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if c.Inspect != nil {
		c.Inspect(method, req.URL.Path, resp, b)
	}
	return Response{Code: resp.StatusCode, Body: b, Header: resp.Header}, nil
}

// JSONCall sends body as JSON.
func (c *Client) JSONCall(ctx context.Context, who *Identity, method, path string, body any) (Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	return c.Do(ctx, who, method, path, bytes.NewReader(b), map[string]string{"Content-Type": "application/json"})
}

// Expect fails unless the answer has the given code:
// Expect(c.Do(...))(http.StatusOK, "what it was").
func Expect(r Response, err error) func(code int, what string) (Response, error) {
	return func(code int, what string) (Response, error) {
		if err != nil {
			return r, fmt.Errorf("%s: %w", what, err)
		}
		if r.Code != code {
			return r, fmt.Errorf("%s: %d %.300s", what, r.Code, r.Body)
		}
		return r, nil
	}
}

// CreateSpace creates a space and returns its ID.
func (c *Client) CreateSpace(ctx context.Context, who *Identity, name string) (string, error) {
	r, err := Expect(c.JSONCall(ctx, who, "POST", "/v1/spaces", map[string]string{"name": name}))(http.StatusCreated, "create space")
	if err != nil {
		return "", err
	}
	var sp struct{ ID string }
	return sp.ID, r.JSON(&sp)
}

// DeleteSpace cancels the space's active runs and deletes it.
func (c *Client) DeleteSpace(ctx context.Context, who *Identity, spaceID string) error {
	var list struct{ Items []Run }
	if r, err := c.Do(ctx, who, "GET", "/v1/spaces/"+spaceID+"/runs", nil, nil); err == nil && r.Code == http.StatusOK {
		_ = r.JSON(&list)
	}
	for _, run := range list.Items {
		if !run.Terminal() {
			_, _ = c.Do(ctx, who, "POST", "/v1/spaces/"+spaceID+"/runs/"+run.ID+"/cancel", nil, nil)
			_, _ = c.WaitRun(ctx, who, spaceID, run.ID, time.Minute)
		}
	}
	_, err := Expect(c.Do(ctx, who, "DELETE", "/v1/spaces/"+spaceID, nil, nil))(http.StatusNoContent, "delete space")
	return err
}

// Upload sends files into the space's top folder.
func (c *Client) Upload(ctx context.Context, who *Identity, spaceID string, files map[string][]byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range files {
		w, err := mw.CreateFormFile("file", name)
		if err != nil {
			return err
		}
		if _, err := w.Write(content); err != nil {
			return err
		}
	}
	if err := mw.Close(); err != nil {
		return err
	}
	_, err := Expect(c.Do(ctx, who, "POST", "/v1/spaces/"+spaceID+"/files", &buf,
		map[string]string{"Content-Type": mw.FormDataContentType()}))(http.StatusCreated, "upload")
	return err
}

// Run is the API's run view (the parts the checks read).
type Run struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	ErrorKind string     `json:"error_kind"`
	Fault     string     `json:"fault"`
	Message   string     `json:"message"`
	StartedAt *time.Time `json:"started_at"`
	Result    *struct {
		Status   string `json:"status"`
		Rendered bool   `json:"rendered"`
		Partial  bool   `json:"partial"`
		Error    *struct {
			CellIndex     *int   `json:"cell_index"`
			ExceptionType string `json:"exception_type"`
			Message       string `json:"exception_message"`
		} `json:"error"`
		Usage struct {
			PeakMemoryBytes *int64 `json:"peak_memory_bytes"`
		} `json:"usage"`
		LogTruncated bool `json:"log_truncated"`
	} `json:"result"`
}

// Terminal reports whether the run has ended.
func (r Run) Terminal() bool { return r.Status != "queued" && r.Status != "running" && r.Status != "" }

// StartRun starts a notebook with parameters (nil for none).
func (c *Client) StartRun(ctx context.Context, who *Identity, spaceID, notebook string, params map[string]any) (Run, error) {
	body := map[string]any{"notebook": notebook}
	if params != nil {
		body["parameters"] = params
	}
	var run Run
	r, err := Expect(c.JSONCall(ctx, who, "POST", "/v1/spaces/"+spaceID+"/runs", body))(http.StatusCreated, "start run")
	if err != nil {
		return run, err
	}
	return run, r.JSON(&run)
}

// GetRun reads a run.
func (c *Client) GetRun(ctx context.Context, who *Identity, spaceID, runID string) (Run, error) {
	var run Run
	r, err := Expect(c.Do(ctx, who, "GET", "/v1/spaces/"+spaceID+"/runs/"+runID, nil, nil))(http.StatusOK, "get run")
	if err != nil {
		return run, err
	}
	return run, r.JSON(&run)
}

// WaitRun waits until the run has ended.
func (c *Client) WaitRun(ctx context.Context, who *Identity, spaceID, runID string, timeout time.Duration) (Run, error) {
	deadline := time.Now().Add(timeout)
	for {
		run, err := c.GetRun(ctx, who, spaceID, runID)
		if err != nil || run.Terminal() {
			return run, err
		}
		if time.Now().After(deadline) {
			return run, fmt.Errorf("run %s still %s after %s", runID, run.Status, timeout)
		}
		if err := Sleep(ctx, 250*time.Millisecond); err != nil {
			return run, err
		}
	}
}

// Log returns the run's log.
func (c *Client) Log(ctx context.Context, who *Identity, spaceID, runID string) (string, error) {
	r, err := c.Do(ctx, who, "GET", "/v1/spaces/"+spaceID+"/runs/"+runID+"/log", nil, nil)
	return string(r.Body), err
}

// WaitLog waits until the run's log contains want.
func (c *Client) WaitLog(ctx context.Context, who *Identity, spaceID, runID, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if log, err := c.Log(ctx, who, spaceID, runID); err == nil && strings.Contains(log, want) {
			return nil
		}
		if err := Sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("the log of run %s never contained %q", runID, want)
}

// Cancel asks the run to stop.
func (c *Client) Cancel(ctx context.Context, who *Identity, spaceID, runID string) (Response, error) {
	return c.Do(ctx, who, "POST", "/v1/spaces/"+spaceID+"/runs/"+runID+"/cancel", nil, nil)
}

// Sleep waits d, or less if ctx ends.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
