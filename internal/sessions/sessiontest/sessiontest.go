// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package sessiontest has a fake session backend for tests of the API
// layers: each session's bridge is ready at once and answers an
// execute_request with its code echoed on stdout, then an execute_reply.
package sessiontest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// Backend is a fake isolation.SessionBackend.
type Backend struct {
	mu      sync.Mutex
	running map[string]*io.PipeWriter // session -> its output
	specs   []isolation.SessionSpec
}

type conn struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (c conn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c conn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c conn) Close() error                { _ = c.w.Close(); return c.r.Close() }

// StartSession implements isolation.SessionBackend.
func (f *Backend) StartSession(_ context.Context, spec isolation.SessionSpec) (isolation.SessionConn, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	f.mu.Lock()
	if f.running == nil {
		f.running = map[string]*io.PipeWriter{}
	}
	f.running[spec.SessionID] = outW
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	emit := func(m map[string]any) {
		b, _ := json.Marshal(m)
		_, _ = outW.Write(append(b, '\n'))
	}
	go func() {
		emit(map[string]any{"channel": "bridge", "content": map[string]any{"status": "ready"}})
		sc := bufio.NewScanner(inR)
		for sc.Scan() {
			var m struct {
				Header  map[string]any `json:"header"`
				Content map[string]any `json:"content"`
			}
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.Header["msg_type"] != "execute_request" {
				continue
			}
			emit(map[string]any{"channel": "iopub", "header": map[string]any{"msg_id": "s1", "msg_type": "stream"},
				"parent_header": m.Header, "content": map[string]any{"name": "stdout", "text": m.Content["code"]}})
			emit(map[string]any{"channel": "shell", "header": map[string]any{"msg_id": "r1", "msg_type": "execute_reply"},
				"parent_header": m.Header, "content": map[string]any{"status": "ok"}})
		}
	}()
	return conn{r: outR, w: inW}, nil
}

// AttachSession implements isolation.SessionBackend (sessions don't outlive
// a test's manager).
func (f *Backend) AttachSession(context.Context, string) (isolation.SessionConn, error) {
	return nil, errors.New("not attachable")
}

// StopSession implements isolation.SessionBackend.
func (f *Backend) StopSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w := f.running[id]; w != nil {
		_ = w.Close()
		delete(f.running, id)
	}
	return nil
}

// Sessions implements isolation.SessionBackend.
func (f *Backend) Sessions(context.Context) ([]isolation.SessionInfo, error) { return nil, nil }

// Specs are the sessions started, in order.
func (f *Backend) Specs() []isolation.SessionSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]isolation.SessionSpec(nil), f.specs...)
}
