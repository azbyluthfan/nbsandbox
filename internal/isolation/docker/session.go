// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// Labels on every session container. nbsandbox.session finds them.
const LabelSession = "nbsandbox.session"

// SessionContainerName is a session's container name; one per session.
func SessionContainerName(sessionID string) string { return "nbsandbox-session-" + sessionID }

// buildSessionConfig turns a SessionSpec into Docker settings: a run's
// container (the same isolation controls, pinned by the run's tests), with
// the session bridge as its main process, stdin kept open for attaches, no
// /run folder, and no log storage (stdout is the protocol, not a log).
func buildSessionConfig(spec isolation.SessionSpec) (*container.Config, *container.HostConfig) {
	spaceMode := "rw"
	if spec.SpaceReadOnly {
		spaceMode = "ro"
	}
	cfg, host := buildContainerConfig(isolation.RunSpec{
		RunID: spec.SessionID, SpaceID: spec.SpaceID, Image: spec.Image, Runtime: spec.Runtime, Limits: spec.Limits,
		SpaceReadOnly: spec.SpaceReadOnly,
	})
	cfg.Entrypoint = []string{"python", isolation.SessionBridge}
	cfg.Cmd = nil
	cfg.Env = []string{
		"HOME=/tmp",
		"PYTHONUNBUFFERED=1",
		"NBSANDBOX_SESSION_ID=" + spec.SessionID,
		"NBSANDBOX_SESSION_MAX_MESSAGE=" + strconv.FormatInt(spec.MaxMessage, 10),
	}
	cfg.Labels = map[string]string{LabelSession: spec.SessionID, LabelSpace: spec.SpaceID}
	cfg.OpenStdin, cfg.StdinOnce = true, false
	cfg.AttachStdin, cfg.AttachStdout, cfg.AttachStderr = true, true, true
	host.Binds = []string{spec.SpaceDir + ":/space:" + spaceMode}
	if spec.PackagesDir != "" {
		host.Binds = append(host.Binds, spec.PackagesDir+":"+isolation.PackagesPath+":ro")
	}
	host.LogConfig = container.LogConfig{Type: "none"}
	return cfg, host
}

// StartSession implements isolation.SessionBackend: create, attach, start
// (attached first, so nothing the bridge says is missed).
func (b *Backend) StartSession(ctx context.Context, spec isolation.SessionSpec) (isolation.SessionConn, error) {
	cfg, host := buildSessionConfig(spec)
	name := SessionContainerName(spec.SessionID)
	id, err := b.client.ContainerCreate(ctx, name, cfg, host)
	if err != nil {
		return nil, fmt.Errorf("%w: create the session's container: %w", isolation.ErrStart, err)
	}
	fail := func(err error) (isolation.SessionConn, error) {
		cctx, cancel := freshContext(ctx)
		defer cancel()
		_ = b.client.ContainerRemove(cctx, id)
		return nil, err
	}
	conn, err := b.attach(ctx, id, spec.SessionID)
	if err != nil {
		return fail(fmt.Errorf("%w: attach to the session: %w", isolation.ErrStart, err))
	}
	if err := b.client.ContainerStart(ctx, id); err != nil {
		_ = conn.Close()
		return fail(fmt.Errorf("%w: start the session's container: %w", isolation.ErrStart, err))
	}
	return conn, nil
}

// AttachSession implements isolation.SessionBackend.
func (b *Backend) AttachSession(ctx context.Context, sessionID string) (isolation.SessionConn, error) {
	return b.attach(ctx, SessionContainerName(sessionID), sessionID)
}

// StopSession implements isolation.SessionBackend.
func (b *Backend) StopSession(ctx context.Context, sessionID string) error {
	name := SessionContainerName(sessionID)
	cctx, cancel := freshContext(ctx)
	defer cancel()
	// The bridge has nothing to save: the kernel's state goes with it.
	if err := b.client.ContainerRemove(cctx, name); err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// Sessions implements isolation.SessionBackend.
func (b *Backend) Sessions(ctx context.Context) ([]isolation.SessionInfo, error) {
	list, err := b.client.ContainerList(ctx, LabelSession)
	if err != nil {
		return nil, err
	}
	out := make([]isolation.SessionInfo, 0, len(list))
	for _, c := range list {
		out = append(out, isolation.SessionInfo{SessionID: c.Labels[LabelSession], SpaceID: c.Labels[LabelSpace], Running: c.Running})
	}
	return out, nil
}

func (b *Backend) attach(ctx context.Context, id, sessionID string) (isolation.SessionConn, error) {
	a, err := b.client.ContainerAttach(ctx, id)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	stderr := &lineLogger{log: func(line string) { b.log.Warn("session bridge", "session_id", sessionID, "stderr", line) }}
	go func() {
		_, err := stdcopy.StdCopy(pw, stderr, a.Reader)
		if err == nil {
			err = io.EOF
		}
		pw.CloseWithError(err)
	}()
	return &attachedConn{r: pr, w: a.Conn}, nil
}

// attachedConn is a SessionConn over an attach: the demultiplexed stdout,
// and stdin.
type attachedConn struct {
	r    *io.PipeReader
	w    io.WriteCloser
	once sync.Once
	mu   sync.Mutex // one message at a time on stdin
}

func (c *attachedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *attachedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.Write(p)
}

func (c *attachedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.w.Close()
		_ = c.r.CloseWithError(errors.New("detached"))
	})
	return err
}

// lineLogger logs what a session's bridge writes to stderr, a line at a
// time, at most maxStderrLines lines per attach (a broken bridge can't flood
// nbsandboxd's log).
type lineLogger struct {
	log  func(string)
	buf  []byte
	n    int
	quit bool
}

const maxStderrLines = 50

func (l *lineLogger) Write(p []byte) (int, error) {
	if l.quit {
		return len(p), nil
	}
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		line := string(l.buf[:i])
		l.buf = l.buf[i+1:]
		if l.n++; l.n > maxStderrLines {
			l.log("(more stderr not logged)")
			l.quit, l.buf = true, nil
			return len(p), nil
		}
		l.log(line)
	}
	if len(l.buf) > MaxLineBytes {
		l.log(string(l.buf))
		l.buf = nil
	}
	return len(p), nil
}
