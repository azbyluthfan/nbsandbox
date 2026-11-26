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

	"github.com/moby/moby/api/types/container"

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
	cfg, host := buildContainerConfig(spec)
	if spec.Fetcher != nil {
		cleanup, ip, err := b.startFetcher(ctx, spec, runFetcher, out)
		defer cleanup()
		if err != nil {
			return isolation.RunResult{}, err
		}
		host.ExtraHosts = append(host.ExtraHosts, hostsEntry(spec.RunID, ip))
	}
	return b.runContainer(ctx, ContainerName(spec.RunID), spec.RunID, cfg, host, spec.Limits.StopGrace, out)
}

// runContainer creates, starts and follows a container until it ends (or
// ctx does: then it is stopped with grace), and removes it.
func (b *Backend) runContainer(ctx context.Context, name, logID string, cfg *container.Config, host *container.HostConfig,
	stopGrace time.Duration, out isolation.LineSink) (isolation.RunResult, error) {
	var res isolation.RunResult
	cctx, cancel := freshContext(ctx) // creating must not be cut short by an early cancel
	id, err := b.client.ContainerCreate(cctx, name, cfg, host)
	cancel()
	if err != nil {
		return res, fmt.Errorf("%w: create container: %w", isolation.ErrStart, err)
	}
	res.ContainerID = id
	remove := func() {
		cctx, cancel := freshContext(ctx)
		defer cancel()
		if err := b.client.ContainerRemove(cctx, id); err != nil && !IsNotFound(err) {
			b.log.Warn("remove run container", "run_id", logID, "error", err)
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
		b.follow(logsCtx, id, out, logID)
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
		if err := b.client.ContainerStop(cctx, id, stopGrace); err != nil && !IsNotFound(err) {
			b.log.Warn("stop run container", "run_id", logID, "error", err)
		}
		cancel()
		select {
		case <-waitRes:
		case err := <-waitErr:
			lost = err
		case <-time.After(stopGrace + 15*time.Second):
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

// Orphans implements isolation.Backend: run containers, and the fetchers
// and networks runs left without one (one entry per run).
func (b *Backend) Orphans(ctx context.Context) ([]isolation.Orphan, error) {
	list, err := b.client.ContainerList(ctx, LabelRun)
	if err != nil {
		return nil, err
	}
	out := make([]isolation.Orphan, 0, len(list))
	seen := map[string]bool{}
	for _, c := range list {
		out = append(out, isolation.Orphan{RunID: c.Labels[LabelRun], SpaceID: c.Labels[LabelSpace], ContainerID: c.ID, Running: c.Running})
		seen[c.Labels[LabelRun]] = true
	}
	fetchers, err := b.client.ContainerList(ctx, LabelFetcher)
	if err != nil {
		return nil, err
	}
	nets, err := b.client.NetworkList(ctx, LabelNetwork)
	if err != nil {
		return nil, err
	}
	add := func(run, space string) {
		if run != "" && !seen[run] {
			seen[run] = true
			out = append(out, isolation.Orphan{RunID: run, SpaceID: space})
		}
	}
	for _, c := range fetchers {
		add(c.Labels[LabelFetcher], c.Labels[LabelSpace])
	}
	for _, n := range nets {
		add(n.Labels[LabelNetwork], n.Labels[LabelSpace])
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
	return b.killFetcher(cctx, runID, runFetcher)
}

var _ isolation.Backend = (*Backend)(nil)

// BuildPackages implements isolation.PackageBuilder: it runs the builder
// image until it ends, ctx ends (stopped, 1 s grace), or the build times out
// with ctx.
func (b *Backend) BuildPackages(ctx context.Context, spec isolation.BuildSpec, out isolation.LineSink) (isolation.RunResult, error) {
	cfg, host := buildBuilderConfig(spec)
	return b.runContainer(ctx, BuildContainerName(spec.ID), "build-"+spec.ID, cfg, host, time.Second, out)
}

// RemoveBuildOrphans implements isolation.PackageBuilder: build containers
// an earlier life of worker left on this Docker host are stopped and removed
// (their builds' leases run out, and another build takes over). Other
// workers' builds are left alone.
func (b *Backend) RemoveBuildOrphans(ctx context.Context, worker string) error {
	list, err := b.client.ContainerList(ctx, LabelBuild)
	if err != nil {
		return err
	}
	for _, c := range list {
		if c.Labels[LabelBuildWorker] != worker {
			continue
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		if err := b.client.ContainerStop(cctx, c.ID, time.Second); err != nil && !IsNotFound(err) {
			b.log.Warn("stop leftover build container", "container", c.ID, "error", err)
		}
		if err := b.client.ContainerRemove(cctx, c.ID); err != nil && !IsNotFound(err) {
			b.log.Warn("remove leftover build container", "container", c.ID, "error", err)
		}
		cancel()
	}
	return nil
}

var _ isolation.PackageBuilder = (*Backend)(nil)
