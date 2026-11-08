// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// MaxLineBytes splits longer output lines so one line can't grow without bound.
const MaxLineBytes = 64 << 10

// cleanupTimeout bounds every Docker call made after a run's context ended.
const cleanupTimeout = 30 * time.Second

// Backend runs notebooks in Docker containers (runc, or gVisor via runsc).
type Backend struct {
	client Client
	log    *slog.Logger
}

// NewBackend returns the Docker backend.
func NewBackend(c Client, log *slog.Logger) *Backend { return &Backend{client: c, log: log} }

// Name implements isolation.Backend.
func (b *Backend) Name() string { return "docker" }

// Run implements isolation.Backend.
//
// Every Docker call after ctx ends uses a fresh context:
// calls made with the cancelled context would fail at once and leave the
// container running.
func (b *Backend) Run(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
	var res isolation.RunResult
	cfg, host := buildContainerConfig(spec)

	cctx, cancel := freshContext(ctx) // creating must not be cut short by an early cancel
	id, err := b.client.ContainerCreate(cctx, ContainerName(spec.RunID), cfg, host)
	cancel()
	if err != nil {
		return res, fmt.Errorf("%w: create container: %w", isolation.ErrStart, err)
	}
	res.ContainerID = id
	remove := func() {
		cctx, cancel := freshContext(ctx)
		defer cancel()
		if err := b.client.ContainerRemove(cctx, id); err != nil && !IsNotFound(err) {
			b.log.Warn("remove run container", "run_id", spec.RunID, "error", err)
			return
		}
		res.Removed = true
	}
	if ctx.Err() != nil { // cancelled before start: nothing ran
		remove()
		res.Stopped = true
		return res, nil
	}

	cctx, cancel = freshContext(ctx)
	err = b.client.ContainerStart(cctx, id)
	cancel()
	if err != nil {
		remove()
		return res, fmt.Errorf("%w: start container: %w", isolation.ErrStart, err)
	}

	// Follow output. Without "since", logs include everything from the start
	// (checked against Docker), so following after start loses nothing.
	logsCtx, stopLogs := context.WithCancel(context.WithoutCancel(ctx))
	defer stopLogs()
	followDone := make(chan struct{})
	go func() {
		defer close(followDone)
		b.follow(logsCtx, id, out, spec.RunID)
	}()

	// Wait with a context that only ends when we give up, never with ctx.
	waitCtx, stopWait := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWait()
	waitRes, waitErr := b.client.ContainerWait(waitCtx, id)

	var lost error
	select {
	case <-waitRes:
	case err := <-waitErr:
		lost = err
	case <-ctx.Done():
		res.Stopped = true
		cctx, cancel := freshContext(ctx)
		if err := b.client.ContainerStop(cctx, id, spec.Limits.StopGrace); err != nil && !IsNotFound(err) {
			b.log.Warn("stop run container", "run_id", spec.RunID, "error", err)
		}
		cancel()
		select {
		case <-waitRes:
		case err := <-waitErr:
			lost = err
		case <-time.After(spec.Limits.StopGrace + 15*time.Second):
			lost = errors.New("container did not stop")
		}
	}

	// Let the follower drain what is left, but not forever.
	select {
	case <-followDone:
	case <-time.After(10 * time.Second):
		stopLogs()
		<-followDone
	}

	cctx, cancel = freshContext(ctx)
	st, inspectErr := b.client.ContainerInspect(cctx, id)
	cancel()
	if inspectErr != nil {
		// Docker is gone or the container vanished: leave cleanup to
		// reconciliation and report it as a platform problem.
		return res, fmt.Errorf("%w: %w", isolation.ErrDockerLost, errors.Join(lost, inspectErr))
	}
	res.ExitCode, res.OOMKilled = st.ExitCode, st.OOMKilled
	res.Started, res.Finished = st.StartedAt, st.FinishedAt
	if lost != nil && st.Running {
		return res, fmt.Errorf("%w: %w", isolation.ErrDockerLost, lost)
	}
	remove()
	return res, nil
}

// freshContext keeps ctx's values but not its cancellation, with a bounded
// lifetime: every Docker call after a run's context ends uses one.
func freshContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// follow streams the container's output into out, one line at a time.
func (b *Backend) follow(ctx context.Context, id string, out isolation.LineSink, runID string) {
	rc, err := b.client.ContainerLogs(ctx, id)
	if err != nil {
		b.log.Warn("follow run output", "run_id", runID, "error", err)
		return
	}
	defer func() { _ = rc.Close() }()

	// stdout and stderr arrive multiplexed; both become log lines. A mutex
	// keeps lines whole when both streams write at once.
	var mu sync.Mutex
	emit := func(s string) { mu.Lock(); out.Line(s); mu.Unlock() }
	outW, errW := newLineWriter(emit), newLineWriter(emit)
	if _, err := stdcopy.StdCopy(outW, errW, rc); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		b.log.Debug("run output stream ended", "run_id", runID, "error", err)
	}
	outW.Flush()
	errW.Flush()
}

// lineWriter turns a byte stream into lines: split on \n, strip \r, and cut
// lines longer than MaxLineBytes into pieces.
type lineWriter struct {
	buf  bytes.Buffer
	emit func(string)
}

func newLineWriter(emit func(string)) *lineWriter { return &lineWriter{emit: emit} }

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		i := bytes.IndexByte(data, '\n')
		switch {
		case i >= 0 && i <= MaxLineBytes:
			w.emit(string(bytes.TrimSuffix(data[:i], []byte("\r"))))
			w.buf.Next(i + 1)
		case len(data) >= MaxLineBytes:
			w.emit(string(data[:MaxLineBytes]))
			w.buf.Next(MaxLineBytes)
		default:
			return len(p), nil
		}
	}
}

// Flush emits a final line without a trailing newline.
func (w *lineWriter) Flush() {
	if w.buf.Len() > 0 {
		w.emit(string(bytes.TrimSuffix(w.buf.Bytes(), []byte("\r"))))
		w.buf.Reset()
	}
}

// Orphans implements isolation.Backend.
func (b *Backend) Orphans(ctx context.Context) ([]isolation.Orphan, error) {
	list, err := b.client.ContainerList(ctx, LabelRun)
	if err != nil {
		return nil, err
	}
	out := make([]isolation.Orphan, 0, len(list))
	for _, c := range list {
		out = append(out, isolation.Orphan{RunID: c.Labels[LabelRun], SpaceID: c.Labels[LabelSpace], ContainerID: c.ID, Running: c.Running})
	}
	return out, nil
}

// Kill implements isolation.Backend: stop (1 s grace) and remove.
func (b *Backend) Kill(ctx context.Context, runID string) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	name := ContainerName(runID)
	if err := b.client.ContainerStop(cctx, name, time.Second); err != nil && !IsNotFound(err) {
		return err
	}
	if err := b.client.ContainerRemove(cctx, name); err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

var _ isolation.Backend = (*Backend)(nil)
