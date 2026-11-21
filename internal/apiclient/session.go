// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package apiclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Session is an interactive session, as the API shows it.
type Session struct {
	ID       string `json:"id"`
	SpaceID  string `json:"space_id"`
	Notebook string `json:"notebook"`
	Mode     string `json:"mode"`
	Status   string `json:"status"`
}

// ErrSessionsOff means the deployment has interactive sessions turned off.
var ErrSessionsOff = errors.New("interactive sessions are turned off on this deployment")

// OpenSession opens (or gets) a notebook's session; mode "" is read-write.
func (c *Client) OpenSession(ctx context.Context, who *Identity, spaceID, notebook, mode string) (Session, error) {
	r, err := c.JSONCall(ctx, who, "POST", "/v1/spaces/"+spaceID+"/sessions", map[string]string{"notebook": notebook, "mode": mode})
	if err != nil {
		return Session{}, err
	}
	if r.Code == http.StatusNotFound && strings.Contains(string(r.Body), "turned off") {
		return Session{}, ErrSessionsOff
	}
	if r.Code != http.StatusCreated && r.Code != http.StatusOK {
		return Session{}, fmt.Errorf("open a session: %d %.300s", r.Code, r.Body)
	}
	var s Session
	return s, r.JSON(&s)
}

// EndSession ends a session.
func (c *Client) EndSession(ctx context.Context, who *Identity, spaceID, id string) error {
	r, err := c.Do(ctx, who, "DELETE", "/v1/spaces/"+spaceID+"/sessions/"+id, nil, nil)
	if err != nil {
		return err
	}
	if r.Code != http.StatusOK && r.Code != http.StatusNotFound {
		return fmt.Errorf("end a session: %d %.300s", r.Code, r.Body)
	}
	return nil
}

// KernelMsg is a message on a session's channel.
type KernelMsg struct {
	Channel string         `json:"channel"`
	Header  map[string]any `json:"header"`
	Parent  map[string]any `json:"parent_header"`
	Content map[string]any `json:"content"`
}

// Type is the message's msg_type.
func (m KernelMsg) Type() string { s, _ := m.Header["msg_type"].(string); return s }

// Kernel is a connection to a session's channel (its WebSocket).
type Kernel struct {
	c *websocket.Conn
}

// ConnectSession opens a session's channel as who, waiting for it to start.
func (c *Client) ConnectSession(ctx context.Context, who *Identity, spaceID, id string) (*Kernel, error) {
	u := strings.TrimSuffix(c.BaseURL, "/") + "/v1/spaces/" + spaceID + "/sessions/" + id + "/channel"
	u = "ws" + strings.TrimPrefix(u, "http") // http -> ws, https -> wss
	h := http.Header{}
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil) // only to encode basic credentials
	switch {
	case who == nil:
	case who.Header != "":
		h.Set("Authorization", who.Header)
	case who.Token != "":
		h.Set("Authorization", "Bearer "+who.Token)
	default:
		req.SetBasicAuth(who.Name, who.Password)
		h.Set("Authorization", req.Header.Get("Authorization"))
	}
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Minute} // the first connect waits for the kernel
	if t, ok := c.HTTP.Transport.(*http.Transport); ok && t.TLSClientConfig != nil {
		d.TLSClientConfig = t.TLSClientConfig
	}
	ws, resp, err := d.DialContext(ctx, u, h)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		return nil, fmt.Errorf("connect to the session: %w (%d)", err, code)
	}
	k := &Kernel{c: ws}
	if _, err := k.Next(ctx, 2*time.Minute, func(m KernelMsg) bool { return m.Channel == "bridge" }); err != nil {
		_ = ws.Close()
		return nil, err
	}
	return k, nil
}

// Close disconnects; the session goes on.
func (k *Kernel) Close() error { return k.c.Close() }

// Send sends a Jupyter message and returns its msg_id.
func (k *Kernel) Send(channel, msgType string, content map[string]any) (string, error) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	msg, _ := json.Marshal(map[string]any{"channel": channel, "header": map[string]any{"msg_id": id, "msg_type": msgType, "session": "check"},
		"parent_header": map[string]any{}, "metadata": map[string]any{}, "content": content})
	return id, k.c.WriteMessage(websocket.TextMessage, msg)
}

// Next reads messages until one matches, within timeout.
func (k *Kernel) Next(ctx context.Context, timeout time.Duration, match func(KernelMsg) bool) (KernelMsg, error) {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		_ = k.c.SetReadDeadline(deadline)
		_, b, err := k.c.ReadMessage()
		if err != nil {
			return KernelMsg{}, fmt.Errorf("session channel: %w", err)
		}
		var m KernelMsg
		if json.Unmarshal(b, &m) == nil && match(m) {
			return m, nil
		}
	}
}

// Run executes code and returns what it printed (errors as "ERROR <name>").
func (k *Kernel) Run(ctx context.Context, code string, timeout time.Duration) (string, error) {
	id, err := k.Send("shell", "execute_request", map[string]any{"code": code, "silent": false})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for {
		m, err := k.Next(ctx, timeout, func(m KernelMsg) bool { return m.Parent["msg_id"] == id || m.Channel == "bridge" })
		if err != nil {
			return out.String(), err
		}
		switch {
		case m.Channel == "bridge" && m.Content["status"] == "ended":
			return out.String(), fmt.Errorf("the session ended: %v", m.Content["reason"])
		case m.Channel == "iopub" && m.Type() == "stream":
			text, _ := m.Content["text"].(string)
			out.WriteString(text)
		case m.Channel == "iopub" && m.Type() == "error":
			name, _ := m.Content["ename"].(string)
			out.WriteString("ERROR " + name)
		case m.Channel == "iopub" && m.Type() == "status" && m.Content["execution_state"] == "idle":
			return out.String(), nil
		}
	}
}
