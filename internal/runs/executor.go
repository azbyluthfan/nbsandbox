// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/logs"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// executor is the execute side of the run manager (see seams.go): workers
// claim queued runs, run them through the Backend, follow their logs,
// enforce the timeout and disk quota, and record the outcome, all fenced by
// the run's lease. It also stops leftover containers. It reaches the submit
// side only through the database and Wakeups, and implements Canceller.
type executor struct {
	*core

	mu       sync.Mutex
	cond     *sync.Cond // signalled when a run may be claimable or the state below changes
	kicks    uint64     // bumped by signal; a worker that found nothing waits for the next
	active   map[string]*activeRun
	dockerUp bool
	draining bool
	stopped  bool

	logs *logs.Registry

	root       context.Context
	rootCancel context.CancelFunc
	workers    sync.WaitGroup
	background sync.WaitGroup // reconcile passes, heartbeats
	reconcileM sync.Mutex
}

type activeRun struct {
	cancel  context.CancelCauseFunc
	spaceID string
	token   int64
}

func newExecutor(c *core) *executor {
	e := &executor{core: c, active: map[string]*activeRun{}, logs: logs.NewRegistry()}
	e.cond = sync.NewCond(&e.mu)
	e.root, e.rootCancel = context.WithCancel(context.Background())
	return e
}

// start launches the workers. Runs execute once Docker is up.
func (e *executor) start() {
	for i := 0; i < e.cfg.Concurrency; i++ {
		e.workers.Add(1)
		go e.worker()
	}
	e.background.Add(1)
	go func() {
		defer e.background.Done()
		e.heartbeats()
	}()
}

// signal tells idle workers a run may be claimable (one was queued or
// released, or it's time to look again).
func (e *executor) signal() {
	e.mu.Lock()
	e.kicks++
	e.cond.Broadcast()
	e.mu.Unlock()
}

// accepting reports whether new runs can be taken: Docker is up and the
// service isn't shutting down.
func (e *executor) accepting() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dockerUp && !e.draining
}

func (e *executor) isDraining() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.draining
}

func (e *executor) setDockerUp(up bool) {
	e.mu.Lock()
	e.dockerUp = up
	if !up || e.stopped {
		e.mu.Unlock()
		return
	}
	e.background.Add(1)
	e.mu.Unlock()
	go func() {
		defer e.background.Done()
		e.reconcile(e.root)
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	}()
}

func (e *executor) updateMetrics() {
	if e.d.Metrics == nil {
		return
	}
	e.mu.Lock()
	n := len(e.active)
	e.mu.Unlock()
	e.d.Metrics.ActiveRuns.Set(float64(n))
}

// Cancel implements Canceller.
func (e *executor) Cancel(runID string, cause error) bool {
	e.mu.Lock()
	a := e.active[runID]
	e.mu.Unlock()
	if a == nil {
		return false
	}
	a.cancel(cause)
	return true
}

// worker claims and executes queued runs one at a time until shutdown.
func (e *executor) worker() {
	defer e.workers.Done()
	var seen uint64 // the kick after which this worker last found nothing
	for {
		e.mu.Lock()
		for !e.stopped && !e.draining && (!e.dockerUp || e.kicks == seen) {
			e.cond.Wait()
		}
		if e.stopped || e.draining {
			e.mu.Unlock()
			return
		}
		kick := e.kicks
		e.mu.Unlock()

		r, err := e.d.DB.ClaimRun(e.root, e.cfg.WorkerID, e.cfg.Lease)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) && e.root.Err() == nil {
				e.d.Log.Warn("claim a queued run", "error", err)
			}
			seen = kick // nothing to take until the next kick
			continue
		}
		runCtx, cancel := context.WithCancelCause(e.root)
		// Registered before the run is marked running, so a cancel always
		// finds it.
		e.mu.Lock()
		e.active[r.ID] = &activeRun{cancel: cancel, spaceID: r.SpaceID, token: r.Token}
		e.mu.Unlock()
		e.updateMetrics()

		e.execute(runCtx, r)

		cancel(nil)
		e.mu.Lock()
		delete(e.active, r.ID)
		e.mu.Unlock()
		e.updateMetrics()
	}
}

// heartbeats renews the leases of this worker's runs and stops those it lost;
// it also stops runs whose cancel was requested (in case the notification
// was missed) and looks for claimable runs (in case a "queued" one was).
func (e *executor) heartbeats() {
	beat := time.NewTicker(e.cfg.Lease / 3)
	defer beat.Stop()
	poll := time.NewTicker(e.cfg.QueuePoll)
	defer poll.Stop()
	// With other workers, containers of runs taken over from a worker that
	// went away (and came back) are found by a periodic pass.
	var sweep <-chan time.Time
	if e.cfg.SharedWorkers {
		t := time.NewTicker(e.cfg.Lease)
		defer t.Stop()
		sweep = t.C
	}
	for {
		select {
		case <-e.root.Done():
			return
		case <-sweep:
			if e.accepting() {
				e.reconcile(e.root)
			}
		case <-poll.C:
			e.signal()
			e.checkCancels(e.root)
		case <-beat.C:
			e.renewLeases(e.root)
		}
	}
}

func (e *executor) leases() []store.Lease {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]store.Lease, 0, len(e.active))
	for id, a := range e.active {
		out = append(out, store.Lease{RunID: id, Token: a.token})
	}
	return out
}

func (e *executor) renewLeases(ctx context.Context) {
	lost, err := e.d.DB.RenewLeases(ctx, e.leases(), e.cfg.Lease)
	if err != nil {
		e.d.Log.Warn("renew run leases", "error", err)
		return
	}
	for _, id := range lost {
		// Another node took over or ended the run (e.g. this worker
		// couldn't reach the database for too long): stop it; nothing it
		// does from now on would be recorded.
		if e.Cancel(id, ErrLeaseLost) {
			e.d.Log.Warn("lost the lease of a running run; stopping it", "run_id", id)
		}
	}
}

func (e *executor) checkCancels(ctx context.Context) {
	ls := e.leases()
	if len(ls) == 0 {
		return
	}
	ids := make([]string, len(ls))
	for i, l := range ls {
		ids[i] = l.RunID
	}
	cancelled, err := e.d.DB.CancelRequested(ctx, ids)
	if err != nil {
		return
	}
	for _, id := range cancelled {
		e.Cancel(id, ErrCancelled)
	}
}

// execute runs one claimed run to a terminal status.
func (e *executor) execute(runCtx context.Context, r store.Run) {
	ctx := context.WithoutCancel(runCtx) // bookkeeping must finish even after a cancel
	id := r.ID
	log := e.logFor(r)
	started := e.d.Now()
	ok, seq, err := e.d.DB.MarkRunningWithEvent(ctx, id, r.Token, started, e.event(r, StatusRunning, 0, nil))
	if err != nil || !ok {
		if err != nil {
			log.Error("mark running", "error", err)
			if _, err := e.d.DB.ReleaseRun(ctx, id, r.Token); err != nil {
				log.Warn("release the claim", "error", err) // the lease expires instead
			}
		}
		return // cancelled while queued, or no longer this worker's
	}
	if seq > 0 {
		e.wake.Notify(r.SpaceID)
	}
	// A cancel requested between the claim and now found nothing to stop.
	if c, err := e.d.DB.CancelRequested(ctx, []string{id}); err == nil && len(c) == 1 {
		e.Cancel(id, ErrCancelled)
	}
	log.Info("run started", "notebook", r.Notebook)

	var (
		res    isolation.RunResult
		runErr error
		cause  error // why nbsandboxd ended it: ErrCancelled, ErrShutdown, ErrTimeout
		hub    *logs.Hub
		// closed when the live log shipper is done (after the hub closes)
		shipped <-chan struct{}
		// set when the run's output broke its contract or a limit
		outputErr *OutputError
	)
	defer func() {
		if p := recover(); p != nil {
			log.Error("run panicked", "panic", p)
			runErr = fmt.Errorf("internal error: %v", p)
		}
		e.finish(ctx, r, started, res, runErr, cause, outputErr, hub)
		if shipped != nil {
			<-shipped
		}
	}()

	if err := e.d.Spaces.PrepareRun(ctx, r.SpaceID, r.ID); err != nil {
		runErr = fmt.Errorf("%w: prepare the run's folder: %w", isolation.ErrStart, err)
		return
	}
	w, err := e.d.Spaces.OpenRunLog(r.SpaceID, r.ID)
	if err != nil {
		runErr = fmt.Errorf("%w: open the run log: %w", isolation.ErrStart, err)
		return
	}
	hub = e.logs.Open(r.ID, w, e.cfg.LogCap)
	if e.cfg.SharedWorkers {
		shipped = e.shipLog(ctx, r, hub)
	}

	tctx, cancel := context.WithTimeoutCause(runCtx, e.cfg.Timeout, ErrTimeout)
	defer cancel()
	// Disk: the run can write to its own folder (and to the space, unless it
	// sees it read-only). Quotas are checked by nbsandboxd, not the
	// filesystem, so check while it runs and stop it once over.
	dctx, stopDisk := context.WithCancelCause(tctx)
	defer stopDisk(nil)
	go e.watchDisk(dctx, r.SpaceID, stopDisk)
	tctx = dctx
	spaceDir, err := e.spaceDirFor(ctx, r)
	if err != nil {
		runErr = fmt.Errorf("%w: prepare the space's files: %w", isolation.ErrStart, err)
		return
	}
	stopEvents := e.tailRunEvents(ctx, r, hub)
	res, runErr = e.d.Backend.Run(tctx, isolation.RunSpec{
		RunID: r.ID, SpaceID: r.SpaceID, Notebook: r.Notebook,
		SpaceDir: spaceDir, SpaceReadOnly: !r.WritesSpace, RunDir: e.d.Spaces.RunPath(r.SpaceID, r.ID),
		Image: e.cfg.Image, Runtime: e.cfg.Runtime, Limits: e.cfg.Limits, MaxOutputBytes: e.cfg.MaxOutput,
		OutputMode: r.OutputMode, DiskBytes: e.diskAllowance(ctx, r.SpaceID),
	}, hub)
	outputErr = stopEvents() // everything it reported is recorded before its final status
	// The timeout's own cause, or the parent's (cancel, shutdown); nil when
	// the run ended by itself.
	cause = context.Cause(tctx)
}

// finish classifies the run, records the terminal status (first wins),
// writes result.json, and then ends the live log. The log ends last, so a
// viewer that receives "done" always finds the final status recorded.
func (e *executor) finish(ctx context.Context, r store.Run, started time.Time, res isolation.RunResult,
	runErr, cause error, outputErr *OutputError, hub *logs.Hub) {
	log := e.logFor(r)
	if errors.Is(runErr, isolation.ErrDockerLost) {
		// Best effort; if Docker is still gone, reconciliation removes it.
		_ = e.d.Backend.Kill(ctx, r.ID)
	}
	if r.Function == "" {
		e.writeBack(ctx, r, hub)
	} else {
		e.d.Spaces.Discard(r.SpaceID, r.ID)
	}
	ws := e.readStatus(ctx, r)
	out := Classify(Evidence{Cause: cause, BackendErr: runErr, Result: res, Status: ws, MemoryLimit: e.cfg.Limits.MemoryBytes})
	if out.Status == StatusCancelled {
		if cur, err := e.d.DB.RunByID(ctx, r.ID); err == nil {
			r.CancelledBy, r.CancelReason = cur.CancelledBy, cur.CancelReason
		}
	}
	out, value := e.checkOutput(ctx, r, out)
	if outputErr != nil {
		// The run was stopped because its output broke the contract or a
		// limit: that is why it ended, whatever else happened.
		out, value = Outcome{Status: StatusFailed, ErrorKind: outputErr.Kind, Fault: FaultNotebook, Message: outputErr.Msg,
			Rendered: out.Rendered}, nil
	}

	var truncated bool
	if hub != nil {
		hub.Line(fmt.Sprintf("[nbsandbox] run finished: %s", out.Status))
		_, _, truncated, _ = hub.Stats()
	}
	finished := e.d.Now()
	result := e.buildResult(ctx, r, out, ws, res, &started, finished, truncated)
	if r.OutputMode != contract.ModeStream {
		result.Output = value // a stream's output (its items) is set by buildResult
	}
	won := e.record(ctx, r, out, result, store.Fence{Token: r.Token})
	if e.cfg.SharedWorkers {
		if err := e.d.DB.DeleteLogChunks(ctx, r.ID); err != nil {
			log.Warn("delete live log chunks", "error", err)
		}
	}
	if errors.Is(runErr, isolation.ErrDockerLost) {
		e.removeContainerLater(r)
	}
	if hub != nil {
		final := out.Status
		if !won {
			if cur, err := e.d.DB.RunByID(ctx, r.ID); err == nil {
				final = cur.Status
			}
		}
		_ = hub.Close(final)
		e.logs.Remove(r.ID)
	}
	if !won {
		return
	}
	e.d.Spaces.InvalidateUsage(r.SpaceID)
	if e.d.Metrics != nil {
		e.d.Metrics.RunsTotal.WithLabelValues(out.Status).Inc()
		e.d.Metrics.RunDuration.Observe(finished.Sub(started).Seconds())
	}
	log.Info("run finished", "status", out.Status, "error_kind", out.ErrorKind, "fault", out.Fault,
		"duration_ms", finished.Sub(started).Milliseconds())
}

// reconcile cleans up after a restart of nbsandboxd or Docker: containers of
// runs no worker is running are stopped and removed, and runs left active by
// this worker's previous life are finished as interrupted (claimed ones go
// back to the queue). With other workers around (SharedWorkers), a run is
// only taken as abandoned once its lease has expired; their containers on a
// shared Docker host are left alone.
func (e *executor) reconcile(ctx context.Context) {
	e.reconcileM.Lock()
	defer e.reconcileM.Unlock()
	isActive := func(id string) bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.active[id] != nil
	}
	// A run is abandoned if no worker can be running it: with a sole
	// worker, any this one isn't running; otherwise one leased to this
	// worker's previous life (same ID: worker IDs must be unique) or whose
	// lease ran out.
	abandoned := func(r store.Run) bool {
		return !e.cfg.SharedWorkers || r.Worker == "" || r.Worker == e.cfg.WorkerID ||
			(!r.LeaseUntil.IsZero() && r.LeaseUntil.Before(e.d.Now()))
	}
	orphans, err := e.d.Backend.Orphans(ctx)
	if err != nil {
		e.d.Log.Warn("list orphan containers", "error", err)
		return
	}
	for _, o := range orphans {
		if isActive(o.RunID) {
			continue
		}
		r, err := e.d.DB.RunByID(ctx, o.RunID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			continue
		case !r.Terminal() && !abandoned(r):
			continue // another worker's
		case !r.Terminal():
			e.interruptIfRunning(ctx, o.RunID, "nbsandboxd or Docker restarted while the run was active; its container was stopped", store.Fence{Token: r.Token})
		}
		if err := e.d.Backend.Kill(ctx, o.RunID); err != nil {
			e.d.Log.Warn("remove orphan container", "run_id", o.RunID, "error", err)
		} else {
			e.d.Log.Info("removed orphan container", "run_id", o.RunID, "was_running", o.Running)
		}
	}
	// Runs left active without a container: with other workers, only this
	// worker's own (the rest is expireLeases', on any node).
	for _, status := range []string{StatusRunning, StatusQueued} {
		rs, err := e.d.DB.RunsWithStatus(ctx, status)
		if err != nil {
			e.d.Log.Warn("list active runs", "error", err)
			return
		}
		for _, r := range rs {
			switch {
			case isActive(r.ID), e.cfg.SharedWorkers && r.Worker != e.cfg.WorkerID:
			case r.Status == StatusRunning:
				e.interruptIfRunning(ctx, r.ID, "nbsandboxd restarted while the run was active", store.Fence{Token: r.Token})
			case r.Worker != "":
				if _, err := e.d.DB.ReleaseRun(ctx, r.ID, r.Token); err != nil {
					e.d.Log.Warn("release a claimed run", "run_id", r.ID, "error", err)
				}
			}
		}
	}
}

func (c *core) interruptIfRunning(ctx context.Context, runID, msg string, f store.Fence) {
	r, err := c.d.DB.RunByID(ctx, runID)
	if err != nil || r.Terminal() {
		return
	}
	out := Outcome{Status: StatusInterrupted, ErrorKind: KindOrphan, Fault: FaultPlatform, Message: msg + "; the notebook was not at fault"}
	if !c.record(ctx, r, out, c.buildResult(ctx, r, out, c.readStatus(ctx, r), isolation.RunResult{}, r.StartedAt, c.d.Now(), false), f) {
		return
	}
	c.d.Spaces.InvalidateUsage(r.SpaceID)
	if c.d.Metrics != nil {
		c.d.Metrics.RunsTotal.WithLabelValues(out.Status).Inc()
	}
	c.logFor(r).Warn("run interrupted", "reason", msg)
}

// removeContainerLater keeps trying to remove the container of a run that
// ended because Docker became unreachable, until Docker is back. The
// reconcile pass after Docker returns can't do it while the run is still
// finishing (it skips active runs), so the run takes care of its own.
func (e *executor) removeContainerLater(r store.Run) {
	e.background.Add(1)
	go func() {
		defer e.background.Done()
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for tries := 0; tries < 300; tries++ {
			if err := e.d.Backend.Kill(e.root, r.ID); err == nil {
				return
			}
			select {
			case <-e.root.Done():
				return // shutting down; the next start reconciles
			case <-t.C:
			}
		}
		e.logFor(r).Warn("container of a run that lost Docker not removed; the next reconcile removes it")
	}()
}

// shutdown stops taking runs, waits up to grace for active runs, then stops
// the rest; they end as interrupted. Queued runs stay queued.
func (e *executor) shutdown(grace time.Duration) {
	e.mu.Lock()
	e.draining = true
	e.cond.Broadcast()
	e.mu.Unlock()

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) && e.activeCount() > 0 {
		time.Sleep(100 * time.Millisecond)
	}
	e.mu.Lock()
	for id, a := range e.active {
		e.d.Log.Info("stopping run for shutdown", "run_id", id)
		a.cancel(ErrShutdown)
	}
	e.mu.Unlock()

	done := make(chan struct{})
	go func() { e.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(e.cfg.Limits.StopGrace + 45*time.Second):
		e.d.Log.Error("runs did not stop in time")
	}
	e.mu.Lock()
	e.stopped = true
	e.cond.Broadcast()
	e.mu.Unlock()
	e.rootCancel()
	e.background.Wait()
}

func (e *executor) activeCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.active)
}

// spaceDirFor is the folder mounted at /space: a working copy of the
// space's files (checked out from the file index and object storage), or the
// published bundle for a function invocation.
func (e *executor) spaceDirFor(ctx context.Context, r store.Run) (string, error) {
	if r.Function != "" {
		return e.d.Spaces.CheckoutBundle(ctx, r.SpaceID, r.Function, r.Version, r.ID)
	}
	return e.d.Spaces.Checkout(ctx, r.SpaceID, r.ID)
}

// writeBack applies a writing run's changes to the space (whatever the
// outcome, as files written to the space always were) and removes the
// working copy. What couldn't be applied as the run left it is said in the
// run's log.
func (e *executor) writeBack(ctx context.Context, r store.Run, hub *logs.Hub) {
	defer e.d.Spaces.Discard(r.SpaceID, r.ID)
	if !r.WritesSpace || !dirExists(e.d.Spaces.WorkPath(r.SpaceID, r.ID)) {
		return
	}
	note := func(f string, a ...any) {
		if hub != nil {
			hub.Line("[nbsandbox] " + fmt.Sprintf(f, a...))
		}
	}
	conflicts, skipped, err := e.d.Spaces.Checkin(ctx, r.SpaceID, r.ID, r.Token)
	switch {
	case errors.Is(err, store.ErrFenced):
		e.logFor(r).Warn("the run's changes to the space were not stored: this worker no longer holds its lease")
	case errors.Is(err, store.ErrQuota):
		note("the run's changes to the space were not stored: they would exceed the space's quota")
	case err != nil:
		e.logFor(r).Error("write the run's changes back", "error", err)
		note("the run's changes to the space could not be stored (%v)", err)
	}
	for _, c := range conflicts {
		if c.SavedAs != "" {
			note("%s was changed while the run worked: the change was kept, and the run's version is in %s", c.Path, c.SavedAs)
		} else {
			note("%s was changed while the run worked, so the run's deletion of it was not applied", c.Path)
		}
	}
	if len(skipped) > 0 {
		shown := skipped
		if len(shown) > 10 {
			shown = append(shown[:10:10], fmt.Sprintf("and %d more", len(skipped)-10))
		}
		note("not stored in the space (links, special files or names a space can't hold): %s", strings.Join(shown, ", "))
	}
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// diskAllowance is the space's quota, for backends that watch the run's
// files where they are (0: no quota known).
func (e *executor) diskAllowance(ctx context.Context, spaceID string) int64 {
	u, err := e.d.Spaces.Usage(ctx, spaceID)
	if err != nil {
		return 0
	}
	return u.QuotaBytes
}

// watchDisk stops a run (cause ErrDiskQuota) once its space is over quota.
func (e *executor) watchDisk(ctx context.Context, spaceID string, stop context.CancelCauseFunc) {
	t := time.NewTicker(e.cfg.DiskCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.d.Spaces.InvalidateUsage(spaceID)
		u, err := e.d.Spaces.Usage(ctx, spaceID)
		if err == nil && u.QuotaBytes > 0 && u.BytesUsed > u.QuotaBytes {
			e.d.Log.Warn("stopping run: space over quota", "space_id", spaceID, "bytes", u.BytesUsed, "quota", u.QuotaBytes)
			stop(ErrDiskQuota)
			return
		}
	}
}
