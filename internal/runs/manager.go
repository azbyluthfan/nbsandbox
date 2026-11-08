// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/deps"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/logs"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Errors the API maps to status codes.
var (
	ErrDockerUnavailable   = errors.New("docker is unavailable; try again later")                // 503
	ErrSpaceBusy           = errors.New("the space already has a queued or running run")         // 409
	ErrNotFinished         = errors.New("the run has not finished")                              // 409
	ErrAlreadyFinished     = errors.New("the run has already finished")                          // 409
	ErrIdempotencyMismatch = errors.New("Idempotency-Key was already used with another request") // 422
)

// DependencyError rejects a submission when dependency_check is "reject".
type DependencyError struct{ Warnings []deps.Warning }

func (e *DependencyError) Error() string {
	return fmt.Sprintf("the runner image does not provide %d of the notebook's dependencies", len(e.Warnings))
}

// Config configures the manager. Limits come from settings, never requests.
type Config struct {
	Image             string
	Runtime           string
	Limits            isolation.Limits
	Timeout           time.Duration
	Concurrency       int
	LogCap            int64
	Retention         time.Duration
	RetentionEvery    time.Duration // how often retention runs (default 1h)
	DependencyCheck   string        // warn | reject
	MaxParameters     int64         // bytes of parameters JSON (default 1 MiB)
	MaxOutput         int64         // bytes of the notebook's returned value (default 1 MiB)
	EventRetention    time.Duration // status events kept for replay (default 24h)
	IdempotencyWindow time.Duration // how long an Idempotency-Key returns the original run (default 24h)
	DiskCheckEvery    time.Duration // how often a running run's disk use is checked (default 5s)

	// Several processes (see seams.go).
	WorkerID      string        // names this worker in run leases (default "local")
	NoExecute     bool          // role "api": record and serve runs, never execute them
	SharedWorkers bool          // other workers may exist: share live logs, wait for leases to expire
	Lease         time.Duration // how long a worker's hold on a run lasts unless renewed (default 30s)
	QueuePoll     time.Duration // how often to look for work and cancels without a notification (default 2s)

	// RequireSchemas refuses to publish a function without input and output
	// schemas (functions.require_schemas).
	RequireSchemas bool
}

// Deps are the manager's collaborators.
type Deps struct {
	DB       *store.DB
	Spaces   *spaces.Service
	Backend  isolation.Backend
	Manifest func() *deps.Manifest // nil result: check skipped
	ImageID  func() string
	Metrics  *obs.Metrics
	Log      *slog.Logger
	Now      func() time.Time
	// Available reports whether some worker can run notebooks now; nil: this
	// process's own executor decides (Docker up, not shutting down).
	Available func() bool
}

// core is what both sides of the manager share: settings, collaborators,
// status wake-ups, and recording a run's terminal status.
type core struct {
	cfg     Config
	d       Deps
	wake    *memWakeups // per space: status events
	logWake *memWakeups // per run: live log lines (other nodes' runs)
}

func (c *core) logFor(r store.Run) *slog.Logger {
	return c.d.Log.With("run_id", r.ID, "space_id", r.SpaceID)
}

// Manager owns the run lifecycle. It is the submit side (see seams.go):
// validating and recording runs, queueing them, cancels, views, results,
// status streams, functions and retention. Execution is the executor's.
type Manager struct {
	*core
	cancels Canceller
	ex      *executor

	publishMu sync.Mutex // one publish or function delete at a time

	root       context.Context
	rootCancel context.CancelFunc
	background sync.WaitGroup // retention loop
}

// New creates a manager. Call Start to begin executing runs.
func New(cfg Config, d Deps) *Manager {
	if cfg.RetentionEvery == 0 {
		cfg.RetentionEvery = time.Hour
	}
	if cfg.MaxParameters == 0 {
		cfg.MaxParameters = DefaultMaxParamsBytes
	}
	if cfg.MaxOutput == 0 {
		cfg.MaxOutput = 1 << 20
	}
	if cfg.EventRetention == 0 {
		cfg.EventRetention = 24 * time.Hour
	}
	if cfg.IdempotencyWindow == 0 {
		cfg.IdempotencyWindow = 24 * time.Hour
	}
	if cfg.DiskCheckEvery == 0 {
		cfg.DiskCheckEvery = 5 * time.Second
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Manifest == nil {
		d.Manifest = func() *deps.Manifest { return nil }
	}
	if d.ImageID == nil {
		d.ImageID = func() string { return "" }
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "local"
	}
	if cfg.Lease == 0 {
		cfg.Lease = 30 * time.Second
	}
	if cfg.QueuePoll == 0 {
		cfg.QueuePoll = 2 * time.Second
	}
	c := &core{cfg: cfg, d: d, wake: newMemWakeups(), logWake: newMemWakeups()}
	ex := newExecutor(c)
	m := &Manager{core: c, cancels: ex, ex: ex}
	m.root, m.rootCancel = context.WithCancel(context.Background())
	return m
}

// Start starts the workers (unless NoExecute), the notification listener,
// the lease and metrics upkeep, and the retention loop. Runs execute once
// SetDockerUp(true) has been called.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.d.DB.Ping(ctx); err != nil {
		return err
	}
	m.ex.updateMetrics()
	if !m.cfg.NoExecute {
		m.ex.start() //nolint:contextcheck // workers live as long as the manager (its root context), not Start's caller
	}
	for _, loop := range []func(context.Context){m.listen, m.maintenance} {
		m.background.Add(1)
		go func() {
			defer m.background.Done()
			loop(m.root) //nolint:contextcheck // as long as the manager
		}()
	}
	m.background.Add(1)
	go func() {
		defer m.background.Done()
		m.retentionLoop()
	}()
	return nil
}

// SetDockerUp is called by the Docker supervisor. While Docker
// is down nothing starts; when it comes back, leftover containers and
// half-finished runs are reconciled and the queue resumes.
func (m *Manager) SetDockerUp(up bool) { m.ex.setDockerUp(up) }

// SubmitRequest is a new run.
type SubmitRequest struct {
	Notebook       string
	Parameters     json.RawMessage
	IdempotencyKey string
	ReadOnlySpace  bool // mount /space read-only (the run can't change the space's files)
}

// ContractError rejects parameters that don't match the notebook's input
// contract, or a notebook whose contract is invalid (400).
type ContractError = contract.Error

// Submit validates and queues a run. Requests that
// are rejected never consume a run slot.
func (m *Manager) Submit(ctx context.Context, owner, spaceID string, req SubmitRequest) (View, bool, error) {
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return View{}, false, err
	}
	nb, err := m.d.Spaces.ReadNotebook(ctx, spaceID, req.Notebook)
	if err != nil {
		return View{}, false, err
	}
	params, err := NormalizeParameters(req.Parameters, m.cfg.MaxParameters)
	if err != nil {
		return View{}, false, err
	}
	c, err := resolveContract(nb, nil)
	if err != nil {
		return View{}, false, err
	}
	if err := c.ValidateInput(json.RawMessage(params)); err != nil {
		return View{}, false, err
	}
	return m.submit(ctx, owner, spaceID, req, submission{notebook: nb, params: params, outputSchema: c})
}

// clientApp is the application calling on its user's behalf, from the
// caller's token ("" for basic auth, or when the token doesn't say).
func clientApp(ctx context.Context) string {
	if p, ok := auth.From(ctx); ok {
		return p.Client
	}
	return ""
}

// resolveContract reads a notebook's contract (or uses the one given), and
// settles its output mode against the calls the notebook makes: refused if
// it calls both nbsandbox.output and nbsandbox.emit, or contradicts the
// declared mode.
func resolveContract(nb []byte, given *contract.Spec) (*contract.Contract, error) {
	spec := given
	if spec == nil {
		var err error
		if spec, err = contract.SpecFromNotebook(nb); err != nil {
			return nil, err
		}
	}
	var c *contract.Contract
	if spec != nil {
		var err error
		if c, err = contract.FromSpec(*spec); err != nil {
			return nil, err
		}
	}
	calls, err := contract.Scan(nb)
	if err != nil {
		return nil, err
	}
	return contract.Resolve(c, calls)
}

// submission is a validated run, ready to queue.
type submission struct {
	notebook     []byte
	params       string
	outputSchema *contract.Contract
	function     string
	version      int
}

func (m *Manager) submit(ctx context.Context, owner, spaceID string, req SubmitRequest, sub submission) (View, bool, error) {
	nb, params := sub.notebook, sub.params
	if err := CheckLanguage(nb); err != nil {
		return View{}, false, err
	}
	var warnings []deps.Warning
	if man := m.d.Manifest(); man != nil {
		warnings = deps.Check(man, m.d.Spaces, spaceID, req.Notebook, nb)
		if len(warnings) > 0 && m.cfg.DependencyCheck == "reject" {
			return View{}, false, &DependencyError{Warnings: warnings}
		}
	}
	if available := m.d.Available; (available != nil && !available()) || (available == nil && !m.ex.accepting()) {
		return View{}, false, ErrDockerUnavailable
	}

	var idem *store.Idempotency
	if req.IdempotencyKey != "" {
		if len(req.IdempotencyKey) > 255 {
			return View{}, false, &spaces.InvalidError{Msg: "Idempotency-Key is longer than 255 characters"}
		}
		target := req.Notebook
		if sub.function != "" {
			target = fmt.Sprintf("function:%s:%d", sub.function, sub.version)
		}
		idem = &store.Idempotency{Owner: owner, Key: req.IdempotencyKey, BodyHash: bodyHash(target, params),
			Since: m.d.Now().Add(-m.cfg.IdempotencyWindow)}
	}
	var warnJSON string
	if len(warnings) > 0 {
		b, _ := json.Marshal(warnings)
		warnJSON = string(b)
	}
	// The notebook snapshot is stored before the row and recorded with it:
	// a queued run executes the notebook as it was now, whatever happens to
	// the file afterwards, on whichever worker starts it.
	newID := spaces.NewID()
	runFiles, err := m.d.Spaces.CreateRun(ctx, newID, nb, []byte(params))
	if err != nil {
		return View{}, false, err
	}
	var outputSchema string
	if sub.outputSchema.HasOutput() {
		outputSchema = string(sub.outputSchema.OutputJSON)
	}
	sum := sha256.Sum256(nb)
	nr := store.NewRun{
		ID: newID, SpaceID: spaceID, Owner: owner, Notebook: req.Notebook,
		ParametersJSON: params, DependencyWarnings: warnJSON, CreatedAt: m.d.Now(),
		Function: sub.function, Version: sub.version, NotebookSHA256: hex.EncodeToString(sum[:]), Files: runFiles,
		OutputSchema: outputSchema, OutputMode: sub.outputSchema.GetMode(), ClientApp: clientApp(ctx),
		ReadOnlySpace: req.ReadOnlySpace || sub.function != "",
	}
	pos := 1
	if n, err := m.d.DB.QueueLength(ctx); err == nil {
		pos = n + 1
	}
	id, replay, seq, err := m.d.DB.SubmitRunWithEvent(ctx, nr, idem,
		m.event(store.Run{ID: nr.ID, SpaceID: spaceID, Function: nr.Function, Version: nr.Version}, StatusQueued, pos, nil))
	// On a refusal or a replay the stored snapshot is unreferenced: the
	// cleanup removes it.
	switch {
	case errors.Is(err, store.ErrActiveRun):
		return View{}, false, ErrSpaceBusy
	case errors.Is(err, store.ErrIdempotencyMismatch):
		return View{}, false, ErrIdempotencyMismatch
	case err != nil:
		return View{}, false, err
	}
	if seq > 0 {
		m.wake.Notify(spaceID)
	}
	if !replay {
		m.ex.signal() // a local worker needn't wait for the notification
	}
	v, err := m.Get(ctx, owner, spaceID, id)
	return v, replay, err
}

// record makes a terminal status visible, consistently, even when a
// cancel races completion. result.json is written first: the API only shows it once the
// database says the run is terminal, and only if the statuses match, so a
// reader never sees a finished run without its result. If another writer won
// the terminal transition, result.json is rewritten to match the winner.
//
// f fences the transition: a worker passes its lease token, a cancel of a
// queued run passes QueuedOnly.
func (c *core) record(ctx context.Context, r store.Run, out Outcome, result Result, f store.Fence) bool {
	c.writeResult(ctx, r, result)
	// The run's files go to object storage before the final status is
	// visible, so whoever sees "finished" finds them.
	if err := c.d.Spaces.StoreRunOutputs(ctx, r.SpaceID, r.ID, f.Token); err != nil {
		c.logFor(r).Error("store the run's outputs", "error", err)
	}
	cr := callerResult(result)
	var won bool
	var seq int64
	err := retryDB(ctx, func() error {
		var err error
		won, seq, err = c.d.DB.FinishRunWithEvent(ctx, r.ID, f, store.Outcome{
			Status: out.Status, ErrorKind: out.ErrorKind, Fault: out.Fault, Message: out.Message, FinishedAt: result.FinishedAt,
			OutputBytes: int64(len(result.Output)),
		}, c.event(r, out.Status, 0, &cr))
		return err
	})
	if err != nil {
		c.logFor(r).Error("record run outcome", "error", err)
		return false
	}
	if seq > 0 {
		c.wake.Notify(r.SpaceID)
	}
	if !won {
		if cur, err := c.d.DB.RunByID(ctx, r.ID); err == nil && cur.Terminal() {
			result.Status, result.ErrorKind, result.Fault, result.Message, result.Error = cur.Status, cur.ErrorKind, cur.Fault, cur.Message, nil
			result.Retryable, result.Output = Retryable(cur.Status), nil
			result.CancelledBy, result.CancelReason = cur.CancelledBy, cur.CancelReason
			if cur.FinishedAt != nil {
				result.FinishedAt = cur.FinishedAt.UTC()
			}
			c.writeResult(ctx, r, result)
		}
		c.logFor(r).Warn("run already had a terminal status; keeping it", "would_be", out.Status)
		return false
	}
	return true
}

func (c *core) readStatus(ctx context.Context, r store.Run) *WrapperStatus {
	b, err := c.d.Spaces.ReadRunFile(ctx, r.SpaceID, r.ID, spaces.RunStatus, 1<<20)
	if err != nil {
		return nil
	}
	var ws WrapperStatus
	if json.Unmarshal(b, &ws) != nil || ws.SchemaVersion != deps.SupportedStatusSchema || ws.State == "" {
		return nil
	}
	return &ws
}

func (c *core) writeResult(ctx context.Context, r store.Run, res Result) {
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := c.d.Spaces.WriteRunFile(ctx, r.SpaceID, r.ID, spaces.RunResult, append(b, '\n')); err != nil {
		c.logFor(r).Error("write result.json", "error", err)
	}
}

// MaxCancelReason bounds the reason a caller may give for a cancel.
const MaxCancelReason = 500

// Cancel cancels a queued or running run. A queued run is
// finished at once; a running one is stopped asynchronously. by (the
// caller) and reason are recorded and reported with the result.
func (m *Manager) Cancel(ctx context.Context, owner, spaceID, runID, by, reason string) (View, error) {
	if len(reason) > MaxCancelReason {
		return View{}, &spaces.InvalidError{Msg: fmt.Sprintf("reason is longer than %d bytes", MaxCancelReason)}
	}
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return View{}, err
	}
	r, err := m.d.DB.GetRun(ctx, spaceID, runID)
	if errors.Is(err, store.ErrNotFound) {
		return View{}, spaces.ErrNotFound
	}
	if err != nil {
		return View{}, err
	}
	if r.Terminal() {
		return View{}, ErrAlreadyFinished
	}
	if _, err := m.d.DB.RequestCancel(ctx, runID, by, reason); err != nil {
		return View{}, err
	}
	r.CancelledBy, r.CancelReason = by, reason

	// The request is in the database first, so whichever side gets there
	// first wins. A run executing here is stopped now; one executing on
	// another worker is stopped by it (notified now, or at its next check).
	// A run still queued is finished here, unless a worker starts it at
	// the same moment: then that worker stops it.
	if m.cancels.Cancel(runID, ErrCancelled) {
		return m.Get(ctx, owner, spaceID, runID)
	}
	if err := m.d.DB.Notify(ctx, store.ChanCancel, runID); err != nil {
		m.logFor(r).Warn("notify the run's worker of the cancel", "error", err) // it polls too
	}
	if r.Status == StatusQueued {
		out := Outcome{Status: StatusCancelled, ErrorKind: KindCancelled, Fault: FaultCaller, Message: "cancelled before it started"}
		if m.record(ctx, r, out, m.buildResult(ctx, r, out, nil, isolation.RunResult{}, nil, m.d.Now(), false), store.Fence{QueuedOnly: true}) &&
			m.d.Metrics != nil {
			m.d.Metrics.RunsTotal.WithLabelValues(StatusCancelled).Inc()
		}
	}
	return m.Get(ctx, owner, spaceID, runID)
}

// Reconcile cleans up after a restart of nbsandboxd or Docker:
// containers of runs nbsandboxd isn't executing are stopped and removed, and
// runs left in "running" are finished as interrupted.
func (m *Manager) Reconcile(ctx context.Context) { m.ex.reconcile(ctx) }

// Shutdown stops taking runs, waits up to grace for active runs, then stops
// the rest; they end as interrupted. Queued runs stay
// queued and resume at the next start.
func (m *Manager) Shutdown(grace time.Duration) {
	m.ex.shutdown(grace)
	m.rootCancel()
	// Nothing the manager started may outlive it (it would touch the
	// database after the caller closes it).
	m.background.Wait()
}

// RunRetention deletes finished runs older than the retention period and
// expired idempotency keys.
func (m *Manager) RunRetention(ctx context.Context) {
	now := m.d.Now()
	old, err := m.d.DB.FinishedBefore(ctx, now.Add(-m.cfg.Retention))
	if err != nil {
		m.d.Log.Warn("retention: list runs", "error", err)
		return
	}
	for _, r := range old {
		if err := m.d.Spaces.RemoveRun(r.SpaceID, r.ID); err != nil {
			m.logFor(r).Warn("retention: remove run files", "error", err)
			continue
		}
		if err := m.d.DB.DeleteRun(ctx, r.SpaceID, r.ID); err != nil {
			m.logFor(r).Warn("retention: delete run", "error", err)
		}
	}
	if len(old) > 0 {
		m.d.Log.Info("retention: deleted old runs", "count", len(old))
	}
	if _, err := m.d.DB.DeleteIdempotencyBefore(ctx, now.Add(-m.cfg.IdempotencyWindow)); err != nil {
		m.d.Log.Warn("retention: idempotency keys", "error", err)
	}
	if _, err := m.d.DB.DeleteEventsBefore(ctx, now.Add(-m.cfg.EventRetention)); err != nil {
		m.d.Log.Warn("retention: status events", "error", err)
	}
	if n, err := m.d.Spaces.CollectGarbage(ctx, spaces.CleanupGrace); err != nil {
		m.d.Log.Warn("retention: unreferenced file contents", "error", err)
	} else if n > 0 {
		m.d.Log.Info("retention: removed unreferenced file contents", "objects", n)
	}
}

func (m *Manager) retentionLoop() {
	t := time.NewTicker(m.cfg.RetentionEvery)
	defer t.Stop()
	run := func() {
		// One node at a time is enough; the others skip this round.
		if _, err := m.d.DB.TryExclusive(m.root, retentionLock, m.RunRetention); err != nil && m.root.Err() == nil {
			m.d.Log.Warn("retention", "error", err)
		}
	}
	run()
	for {
		select {
		case <-m.root.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// retentionLock is the advisory lock key of the retention pass.
const retentionLock = 7_310_003

// Delete removes a finished run's outputs and record.
func (m *Manager) Delete(ctx context.Context, owner, spaceID, runID string) error {
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return err
	}
	r, err := m.d.DB.GetRun(ctx, spaceID, runID)
	if errors.Is(err, store.ErrNotFound) {
		return spaces.ErrNotFound
	}
	if err != nil {
		return err
	}
	if !r.Terminal() {
		return ErrNotFinished
	}
	if err := m.d.Spaces.RemoveRun(spaceID, runID); err != nil {
		return err
	}
	if err := m.d.DB.DeleteRun(ctx, spaceID, runID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// OpenOutput opens output.ipynb, output.html, log.txt, or an artifact.
func (m *Manager) OpenOutput(ctx context.Context, owner, spaceID, runID, rel string) (*os.File, os.FileInfo, error) {
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return nil, nil, err
	}
	r, err := m.d.DB.GetRun(ctx, spaceID, runID)
	if err != nil {
		return nil, nil, spaces.ErrNotFound
	}
	f, info, err := m.d.Spaces.OpenRunFile(ctx, spaceID, runID, rel)
	if errors.Is(err, spaces.ErrNotFound) && rel == spaces.RunLog && (!r.Terminal() || r.Worker != "") {
		// Running on another worker (or it went away before storing its
		// log): the lines it shipped so far.
		return m.chunkedLog(ctx, runID)
	}
	return f, info, err
}

// chunkedLog is a run's log so far from its live log chunks, as a file.
func (m *Manager) chunkedLog(ctx context.Context, runID string) (*os.File, os.FileInfo, error) {
	chunks, err := m.d.DB.LogChunks(ctx, runID, 0)
	if err != nil {
		return nil, nil, err
	}
	if len(chunks) == 0 {
		return nil, nil, spaces.ErrNotFound
	}
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Text)
	}
	return m.d.Spaces.TempFile(spaces.RunLog, strings.NewReader(b.String()))
}

// PromoteArtifact copies one of a run's artifacts into the space's files.
func (m *Manager) PromoteArtifact(ctx context.Context, owner, spaceID, runID, artifact, to string) (spaces.File, error) {
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return spaces.File{}, err
	}
	if _, err := m.d.DB.GetRun(ctx, spaceID, runID); err != nil {
		return spaces.File{}, spaces.ErrNotFound
	}
	return m.d.Spaces.PromoteArtifact(ctx, owner, spaceID, runID, artifact, to)
}

// Artifacts lists a run's artifacts.
func (m *Manager) Artifacts(ctx context.Context, owner, spaceID, runID string) ([]spaces.File, error) {
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return nil, err
	}
	if _, err := m.d.DB.GetRun(ctx, spaceID, runID); err != nil {
		return nil, spaces.ErrNotFound
	}
	return m.d.Spaces.ListRunArtifacts(ctx, spaceID, runID)
}

// Logs returns the live log hubs of executing runs.
func (m *Manager) Logs() *logs.Registry { return m.ex.logs }

// RunStatus returns a run's current status (for log streaming), after
// checking the caller owns the space.
func (m *Manager) RunStatus(ctx context.Context, owner, spaceID, runID string) (string, error) {
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return "", err
	}
	r, err := m.d.DB.GetRun(ctx, spaceID, runID)
	if errors.Is(err, store.ErrNotFound) {
		return "", spaces.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return r.Status, nil
}

// Draining reports whether the manager is shutting down.
func (m *Manager) Draining() bool { return m.ex.isDraining() }

// dbRetryFor is how long a write that must not be lost (a run's final
// status, an item of its output) keeps trying through a database outage.
const dbRetryFor = 20 * time.Second

// retryDB runs write until it succeeds, ctx ends, or dbRetryFor passes,
// backing off between tries: a database restart or failover shouldn't
// fail a run.
func retryDB(ctx context.Context, write func() error) error {
	deadline := time.Now().Add(dbRetryFor)
	wait := 250 * time.Millisecond
	for {
		err := write()
		if err == nil || ctx.Err() != nil || time.Now().Add(wait).After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(2*wait, 2*time.Second)
	}
}
