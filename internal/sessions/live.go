// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package sessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Kernel states a client is told about (the bridge's and Jupyter's).
const (
	KernelStarting   = "starting"
	KernelIdle       = "idle"
	KernelBusy       = "busy"
	KernelRestarting = "restarting"
	KernelDead       = "dead"
)

// clientBuffer is how many messages a client may fall behind before it is
// disconnected (it reconnects and asks again).
const clientBuffer = 1024

// live is a session this process holds: its container's stdio, its
// clients, and what the idle rule needs.
type live struct {
	m    *Manager
	s    store.Session
	conn isolation.SessionConn

	mu         sync.Mutex
	clients    map[*Client]struct{}
	kernel     string    // the kernel's state, as last reported
	running    string    // the msg_id of the execute_request running, "" if none
	quietSince time.Time // when it last became quiet (no cell running, no client); zero while not
	ended      bool
	syncing    sync.Mutex // one write-back at a time

	readerDone chan struct{}
}

func newLive(m *Manager, s store.Session, conn isolation.SessionConn) *live {
	return &live{m: m, s: s, conn: conn, clients: map[*Client]struct{}{}, kernel: KernelStarting,
		quietSince: m.d.Now(), readerDone: make(chan struct{})}
}

// envelope is what nbsandboxd reads of a Jupyter message.
type envelope struct {
	Channel string `json:"channel"`
	Header  struct {
		MsgID   string `json:"msg_id"`
		MsgType string `json:"msg_type"`
	} `json:"header"`
	Parent struct {
		MsgID   string `json:"msg_id"`
		MsgType string `json:"msg_type"`
	} `json:"parent_header"`
	Content struct {
		ExecutionState string `json:"execution_state"`
		Status         string `json:"status"`
	} `json:"content"`
}

// read relays the bridge's messages to the clients until the connection
// ends, and reports why.
func (l *live) read(ready chan<- error) error {
	l.mu.Lock()
	conn, done := l.conn, l.readerDone
	l.mu.Unlock()
	defer close(done)
	r := bufio.NewReaderSize(conn, 64<<10)
	max := int(l.m.cfg.MaxMessage) + 64<<10
	for {
		line, err := readLine(r, max)
		if err != nil {
			if ready != nil {
				ready <- fmt.Errorf("the session stopped before its kernel was ready: %w", err)
			}
			return err
		}
		var e envelope
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if ready != nil && e.Channel == "bridge" && e.Content.Status == "ready" {
			ready <- nil
			ready = nil
		}
		l.observe(e)
		l.broadcast(line)
	}
}

// readLine reads one line, at most max bytes; a longer one is skipped.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		frag, err := r.ReadSlice('\n')
		if len(buf)+len(frag) <= max {
			buf = append(buf, frag...)
		} else {
			buf = buf[:0:0] // too long: drop it, keep reading to its end
			max = -1
		}
		switch {
		case err == nil:
			if max < 0 {
				return nil, errors.New("a message from the session was too large")
			}
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// observe follows the kernel's state for the idle rule and for clients
// that connect later.
func (l *live) observe(e envelope) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.m.d.Now()
	switch {
	case e.Channel == "bridge":
		switch e.Content.Status {
		case "ready":
			l.kernel, l.running = KernelIdle, ""
		case "restarting":
			l.kernel, l.running = KernelRestarting, ""
		case "dead":
			l.kernel, l.running = KernelDead, ""
		}
	case e.Channel == "iopub" && e.Header.MsgType == "status":
		// Only cells count as work; a completion's busy-idle doesn't.
		if e.Parent.MsgType == "execute_request" {
			switch e.Content.ExecutionState {
			case "busy":
				l.running = e.Parent.MsgID
			case "idle":
				if l.running == e.Parent.MsgID {
					l.running = ""
				}
			}
		}
		if e.Content.ExecutionState == KernelIdle || e.Content.ExecutionState == KernelBusy {
			l.kernel = e.Content.ExecutionState
			if l.running != "" {
				l.kernel = KernelBusy
			}
		}
	}
	l.updateQuiet(now)
}

// updateQuiet starts or stops the idle clock: it runs while no cell runs
// and no client is connected (Colab's rule). Callers hold l.mu.
func (l *live) updateQuiet(now time.Time) {
	quiet := l.running == "" && len(l.clients) == 0
	switch {
	case quiet && l.quietSince.IsZero():
		l.quietSince = now
	case !quiet:
		l.quietSince = time.Time{}
	}
}

func (l *live) broadcast(line []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.clients {
		select {
		case c.out <- line:
		default:
			// Too far behind: it reconnects and asks again.
			c.closeLocked("the connection fell too far behind")
			delete(l.clients, c)
		}
	}
	l.updateQuiet(l.m.d.Now())
}

// status is the state a client is told when it connects, and when the
// session ends.
func (l *live) statusMessage(status, reason string) []byte {
	l.mu.Lock()
	kernel := l.kernel
	l.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"channel": "bridge", "content": map[string]any{
		"status": status, "kernel": kernel, "reason": reason, "session_id": l.s.ID, "mode": l.s.Mode,
	}})
	return b
}

func (l *live) subscribe() *Client {
	c := &Client{l: l, out: make(chan []byte, clientBuffer), done: make(chan struct{})}
	hello := l.statusMessage("attached", "")
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		c.closeLocked("the session has ended")
		return c
	}
	c.out <- hello
	l.clients[c] = struct{}{}
	l.updateQuiet(l.m.d.Now())
	return c
}

func (l *live) unsubscribe(c *Client) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, c)
	l.updateQuiet(l.m.d.Now())
}

// write sends one message to the bridge.
func (l *live) write(msg []byte) error {
	if len(msg) == 0 || bytes.IndexByte(msg, '\n') >= 0 {
		return errors.New("a message is one line of JSON")
	}
	l.mu.Lock()
	ended, conn := l.ended, l.conn
	l.mu.Unlock()
	if ended {
		return ErrEnded
	}
	_, err := conn.Write(append(msg[:len(msg):len(msg)], '\n'))
	return err
}

// control sends nbsandboxd's own control message (interrupt, restart).
func (l *live) control(msgType string, content map[string]any) error {
	if content == nil {
		content = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{
		"channel": "control", "header": map[string]any{"msg_id": spaces.NewID(), "msg_type": msgType, "session": "nbsandboxd"},
		"parent_header": map[string]any{}, "metadata": map[string]any{}, "content": content,
	})
	return l.write(b)
}

// quiet reports since when the session has been quiet (zero: it isn't).
func (l *live) quiet() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.quietSince
}

// sync writes a read-write session's changes to the space.
func (l *live) sync(ctx context.Context) ([]store.Conflict, error) {
	if l.s.Mode != store.SessionReadWrite {
		return nil, nil
	}
	l.syncing.Lock()
	defer l.syncing.Unlock()
	conflicts, skipped, err := l.m.d.Spaces.Sync(ctx, l.s.SpaceID, l.s.ID)
	if err != nil {
		l.m.d.Log.Warn("write a session's changes back", "session_id", l.s.ID, "error", err)
		l.notice(fmt.Sprintf("the session's file changes could not be stored: %v", err))
		return conflicts, err
	}
	for _, c := range conflicts {
		if c.SavedAs != "" {
			l.notice(fmt.Sprintf("%s was changed meanwhile: that change was kept, and the session's version is in %s", c.Path, c.SavedAs))
		}
	}
	if len(skipped) > 0 {
		l.notice("not stored in the space (links, special files or names a space can't hold): " + strings.Join(first(skipped, 10), ", "))
	}
	return conflicts, nil
}

func first(xs []string, n int) []string {
	if len(xs) > n {
		return append(xs[:n:n], fmt.Sprintf("and %d more", len(xs)-n))
	}
	return xs
}

// notice tells the clients something about the session (not the kernel).
func (l *live) notice(text string) {
	b, _ := json.Marshal(map[string]any{"channel": "bridge", "content": map[string]any{"notice": text}})
	l.broadcast(b)
}

// end stops the session: no more messages, its container stopped, a
// read-write session's changes written back, its working copy removed, and
// its record ended (last: the space's write lock is held until its changes
// are in).
func (l *live) end(ctx context.Context, reason, by string) {
	ctx = context.WithoutCancel(ctx)
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return
	}
	l.ended = true
	l.mu.Unlock()

	bye := l.statusMessage("ended", reason)
	l.mu.Lock()
	for c := range l.clients {
		select {
		case c.out <- bye:
		default:
		}
		c.closeLocked(reason)
	}
	l.clients = map[*Client]struct{}{}
	l.mu.Unlock()

	l.mu.Lock()
	conn := l.conn
	l.mu.Unlock()
	_ = conn.Close()
	if err := l.m.d.Backend.StopSession(ctx, l.s.ID); err != nil {
		l.m.d.Log.Warn("stop a session's container", "session_id", l.s.ID, "error", err)
	}
	if l.s.Mode == store.SessionReadWrite {
		_, _ = l.sync(ctx)
	}
	l.m.d.Spaces.Discard(l.s.SpaceID, l.s.ID)
	if _, err := l.m.d.DB.EndSession(ctx, l.s.ID, reason, by, l.m.d.Now()); err != nil {
		l.m.d.Log.Error("record a session's end", "session_id", l.s.ID, "error", err)
	}
	l.m.forget(l.s.ID)
	l.m.d.Log.Info("session ended", "session_id", l.s.ID, "space_id", l.s.SpaceID, "reason", reason)
}

// detach lets go of the session without ending it (nbsandboxd shutting
// down): its container keeps running and is re-attached at the next start.
func (l *live) detach() {
	l.mu.Lock()
	for c := range l.clients {
		c.closeLocked("nbsandboxd is restarting; reconnect in a moment")
	}
	l.clients = map[*Client]struct{}{}
	l.ended = true
	conn := l.conn
	l.mu.Unlock()
	_ = conn.Close()
}

// Client is one connection to a session (a browser tab, an API client).
type Client struct {
	l      *live
	out    chan []byte
	done   chan struct{}
	once   sync.Once
	reason string
}

// Messages are the session's messages for this client, one JSON object
// each (Jupyter messages, and the "bridge" channel's own). It is closed when
// the client is disconnected; Reason says why.
func (c *Client) Messages() <-chan []byte { return c.out }

// Done is closed when the client is disconnected.
func (c *Client) Done() <-chan struct{} { return c.done }

// Reason says why the client was disconnected.
func (c *Client) Reason() string {
	select {
	case <-c.done:
		return c.reason
	default:
		return ""
	}
}

// Send passes a client's message to the kernel: one JSON object, of a type
// clients may send (CheckClientMessage).
func (c *Client) Send(msg []byte) error {
	compact, err := CheckClientMessage(msg)
	if err != nil {
		return err
	}
	select {
	case <-c.done:
		return io.ErrClosedPipe
	default:
	}
	if bytes.Contains(compact, []byte(`"execute_request"`)) {
		c.l.mu.Lock()
		c.l.quietSince = time.Time{}
		c.l.mu.Unlock()
	}
	return c.l.write(compact)
}

// Close disconnects the client; the session goes on.
func (c *Client) Close() {
	c.l.unsubscribe(c)
	c.l.mu.Lock()
	c.closeLocked("closed")
	c.l.mu.Unlock()
}

// closeLocked closes the client; the caller holds c.l.mu.
func (c *Client) closeLocked(reason string) {
	c.once.Do(func() {
		c.reason = reason
		close(c.done)
		close(c.out)
	})
}
