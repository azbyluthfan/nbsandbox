// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package sessions

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// With API and worker processes, a session runs on a worker, and the API
// node a client reached proxies to it: the worker serves its sessions on an
// internal listener (InternalHandler), authenticated with the secret every
// process shares; the API node finds the worker's address in the workers
// table. The owner was checked by the API node; the worker trusts it.

// Conn is a client's connection to a session: one held here (a *Client),
// or one held by another worker, through it.
type Conn interface {
	// Messages are the session's messages for this client, one JSON object
	// each; closed when the client is disconnected (Reason says why).
	Messages() <-chan []byte
	// Send passes a client's message to the kernel.
	Send(msg []byte) error
	// Done is closed when the client is disconnected.
	Done() <-chan struct{}
	Reason() string
	Close()
}

// InternalPrefix is where a worker serves its sessions to API nodes.
const InternalPrefix = "/internal/v1/sessions/"

// lostAfter is how long a session's worker may be missing from the
// workers table before the session ends as interrupted (a restarting
// worker re-attaches well within it).
const lostAfter = 2 * time.Minute

// InternalHandler serves this worker's sessions to API nodes: the channel
// (a WebSocket) and interrupt, restart and sync, for the API node's
// already-authorized clients. Every request carries the shared secret.
func (m *Manager) InternalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+InternalPrefix+"{id}/channel", m.internalChannel)
	mux.HandleFunc("POST "+InternalPrefix+"{id}/{action}", m.internalAction)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.secretOK(r.Header.Get("Authorization")) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (m *Manager) secretOK(h string) bool {
	if m.cfg.Secret == "" {
		return false
	}
	got, ok := strings.CutPrefix(h, "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(m.cfg.Secret)) == 1
}

// held returns the session this worker holds (or is starting).
func (m *Manager) held(ctx context.Context, id string) (*live, error) {
	if !spaces.ValidID(id) {
		return nil, store.ErrNotFound
	}
	if l := m.get(id); l != nil {
		return l, nil
	}
	s, err := m.d.DB.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if !s.Open() {
		return nil, ErrEnded
	}
	if s.Worker != m.cfg.WorkerID {
		return nil, ErrNotHere
	}
	return m.waitLive(ctx, id)
}

func (m *Manager) internalChannel(w http.ResponseWriter, r *http.Request) {
	l, err := m.held(r.Context(), r.PathValue("id"))
	if err != nil {
		internalError(w, err)
		return
	}
	c := l.subscribe()
	defer c.Close()
	ws, err := internalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.Close() }()
	ServeWebSocket(r.Context(), ws, c, m.stop)
}

var internalUpgrader = websocket.Upgrader{ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10}

func (m *Manager) internalAction(w http.ResponseWriter, r *http.Request) {
	l, err := m.held(r.Context(), r.PathValue("id"))
	if err != nil {
		internalError(w, err)
		return
	}
	var out any = map[string]bool{"ok": true}
	switch r.PathValue("action") {
	case "interrupt":
		err = l.control("interrupt_request", nil)
	case "restart":
		err = l.control("shutdown_request", map[string]any{"restart": true})
	case "sync":
		var conflicts []store.Conflict
		conflicts, err = l.sync(r.Context())
		out = conflicts
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// internalError tells the API node what went wrong, by status.
func internalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrEnded):
		http.Error(w, err.Error(), http.StatusGone)
	case errors.Is(err, ErrNotHere):
		http.Error(w, err.Error(), http.StatusMisdirectedRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// fromStatus turns a worker's answer back into the error it stood for.
func fromStatus(code int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	switch code {
	case http.StatusNotFound:
		return store.ErrNotFound
	case http.StatusGone:
		return ErrEnded
	case http.StatusMisdirectedRequest, http.StatusUnauthorized:
		return fmt.Errorf("%w (%s)", ErrNotHere, msg)
	default:
		return fmt.Errorf("the session's worker answered %d: %s", code, msg)
	}
}

// workerAddr is where the worker holding s serves its sessions.
func (m *Manager) workerAddr(ctx context.Context, s store.Session) (string, error) {
	if m.cfg.Secret == "" || m.d.Workers == nil || s.Worker == "" {
		return "", ErrNotHere
	}
	ws, err := m.d.Workers(ctx)
	if err != nil {
		return "", err
	}
	for _, w := range ws {
		if w.ID == s.Worker && w.SessionAddr != "" {
			return w.SessionAddr, nil
		}
	}
	return "", fmt.Errorf("%w: its worker %s can't be reached now", ErrUnavailable, s.Worker)
}

func (m *Manager) internalURL(addr, scheme, id, rest string) string {
	u := url.URL{Scheme: scheme, Host: addr, Path: InternalPrefix + id + "/" + rest}
	return u.String()
}

// remoteAction asks the worker holding s to interrupt, restart or sync it.
func (m *Manager) remoteAction(ctx context.Context, s store.Session, action string) ([]byte, error) {
	addr, err := m.workerAddr(ctx, s)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.internalURL(addr, "http", s.ID, action), nil)
	req.Header.Set("Authorization", "Bearer "+m.cfg.Secret)
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fromStatus(resp.StatusCode, body)
	}
	return body, nil
}

func (m *Manager) httpClient() *http.Client {
	if m.d.HTTP != nil {
		return m.d.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute} // a sync writes files back
}

// remoteAttach connects to s on the worker holding it.
func (m *Manager) remoteAttach(ctx context.Context, s store.Session) (Conn, error) {
	addr, err := m.workerAddr(ctx, s)
	if err != nil {
		return nil, err
	}
	d := websocket.Dialer{HandshakeTimeout: m.cfg.StartTimeout}
	h := http.Header{"Authorization": {"Bearer " + m.cfg.Secret}}
	ws, resp, err := d.DialContext(ctx, m.internalURL(addr, "ws", s.ID, "channel"), h)
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, fromStatus(resp.StatusCode, body)
		}
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	rc := &remoteConn{ws: ws, out: make(chan []byte, clientBuffer), done: make(chan struct{})}
	go rc.read()
	return rc, nil
}

// remoteConn is a client's connection to a session through its worker.
type remoteConn struct {
	ws     *websocket.Conn
	out    chan []byte
	done   chan struct{}
	wmu    sync.Mutex
	once   sync.Once
	reason string
}

func (c *remoteConn) read() {
	defer c.finish("the session's worker closed the connection")
	for {
		typ, msg, err := c.ws.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) && ce.Text != "" {
				c.finish(ce.Text)
			}
			return
		}
		if typ != websocket.TextMessage {
			continue
		}
		select {
		case c.out <- msg:
		case <-c.done:
			return
		}
	}
}

func (c *remoteConn) finish(reason string) {
	c.once.Do(func() {
		c.reason = reason
		close(c.done)
		close(c.out)
	})
}

func (c *remoteConn) Messages() <-chan []byte { return c.out }

func (c *remoteConn) Done() <-chan struct{} { return c.done }

func (c *remoteConn) Send(msg []byte) error {
	compact, err := CheckClientMessage(msg) // checked here too: the worker trusts the API node
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteMessage(websocket.TextMessage, compact)
}

func (c *remoteConn) Reason() string {
	select {
	case <-c.done:
		return c.reason
	default:
		return ""
	}
}

func (c *remoteConn) Close() {
	c.wmu.Lock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	c.wmu.Unlock()
	_ = c.ws.Close()
	c.finish("closed")
}

// reapLoop ends sessions nobody can hold any more, on every node of a
// cluster: those whose worker has been gone for lostAfter (its process and
// containers with it), and those no worker took in time.
func (m *Manager) reapLoop(ctx context.Context) {
	missing := map[string]time.Time{} // worker -> first seen missing
	t := time.NewTicker(m.cfg.Tick * 3)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.reap(ctx, missing)
		}
	}
}

func (m *Manager) reap(ctx context.Context, missing map[string]time.Time) {
	open, err := m.d.DB.OpenSessions(ctx, store.SessionFilter{})
	if err != nil || len(open) == 0 {
		return
	}
	live := map[string]bool{}
	if m.d.Workers != nil {
		ws, err := m.d.Workers(ctx)
		if err != nil {
			return
		}
		for _, w := range ws {
			live[w.ID] = true
		}
	}
	now := m.d.Now()
	for w := range missing {
		if live[w] {
			delete(missing, w)
		}
	}
	for _, s := range open {
		switch {
		case s.Worker == "" && now.Sub(s.CreatedAt) > m.cfg.StartTimeout+time.Minute:
			_, _ = m.d.DB.EndSession(ctx, s.ID, "no worker could start it", "", now)
		case s.Worker != "" && s.Worker != m.cfg.WorkerID && !live[s.Worker]:
			first, ok := missing[s.Worker]
			if !ok {
				missing[s.Worker] = now
			} else if now.Sub(first) >= lostAfter {
				_, _ = m.d.DB.EndSession(ctx, s.ID, "interrupted: its worker is gone", "", now)
			}
		}
	}
}

// ServeWebSocket relays between a WebSocket and a session connection until
// either ends (or shutdown is closed): the session's messages out, the
// client's in; a message the session refuses is answered with a "bridge"
// refusal.
func ServeWebSocket(ctx context.Context, ws *websocket.Conn, c Conn, shutdown <-chan struct{}) {
	ws.SetReadLimit(MaxClientMessage)
	_ = ws.SetReadDeadline(time.Now().Add(channelWait))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(channelWait)) })

	refusals := make(chan []byte, 16)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			typ, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if typ != websocket.TextMessage {
				continue
			}
			if err := c.Send(msg); err != nil {
				select {
				case refusals <- Refusal(err):
				default:
				}
			}
		}
	}()

	ping := time.NewTicker(channelPing)
	defer ping.Stop()
	write := func(typ int, b []byte) error {
		_ = ws.SetWriteDeadline(time.Now().Add(channelWriteWait))
		return ws.WriteMessage(typ, b)
	}
	closing := func(code int, text string) {
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, clipClose(text)), time.Now().Add(time.Second))
	}
	for {
		select {
		case msg, ok := <-c.Messages():
			if !ok {
				closing(websocket.CloseNormalClosure, c.Reason())
				return
			}
			if write(websocket.TextMessage, msg) != nil {
				return
			}
		case msg := <-refusals:
			if write(websocket.TextMessage, msg) != nil {
				return
			}
		case <-ping.C:
			if write(websocket.PingMessage, nil) != nil {
				return
			}
		case <-readDone:
			return
		case <-shutdown:
			closing(websocket.CloseServiceRestart, "nbsandboxd is restarting; reconnect in a moment")
			return
		case <-ctx.Done():
			return
		}
	}
}

// WebSocket timing.
var (
	channelPing      = 30 * time.Second // keeps proxies from closing a quiet connection
	channelWait      = 75 * time.Second // without a pong for this long, the client is gone
	channelWriteWait = 10 * time.Second
)

// Refusal tells a client its message wasn't passed on.
func Refusal(err error) []byte {
	b, _ := json.Marshal(map[string]any{"channel": "bridge", "content": map[string]any{"refused": true, "error": err.Error()}})
	return b
}

// clipClose keeps a close reason within a control frame (125 bytes).
func clipClose(s string) string {
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}

// conflictsFrom reads a worker's sync answer.
func conflictsFrom(body []byte) ([]store.Conflict, error) {
	var out []store.Conflict
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return nil, nil
	}
	err := json.Unmarshal(body, &out)
	return out, err
}
