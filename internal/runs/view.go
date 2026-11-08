// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/deps"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// ResultSchemaVersion versions result.json.
const ResultSchemaVersion = 2 // 2: output, function, version, notebook_sha256, cancel details

// Result is result.json: written once, by whoever recorded
// the run's terminal status, so it always agrees with the API.
type Result struct {
	SchemaVersion      int             `json:"schema_version"`
	RunID              string          `json:"run_id"`
	SpaceID            string          `json:"space_id"`
	Notebook           string          `json:"notebook"`
	NotebookSHA256     string          `json:"notebook_sha256,omitempty"`
	Function           string          `json:"function,omitempty"`
	Version            int             `json:"version,omitempty"`
	Parameters         json.RawMessage `json:"parameters"`
	Status             string          `json:"status"`
	ErrorKind          string          `json:"error_kind,omitempty"`
	Fault              string          `json:"fault,omitempty"`
	Retryable          bool            `json:"retryable"`
	Message            string          `json:"message,omitempty"`
	CancelledBy        string          `json:"cancelled_by,omitempty"`
	CancelReason       string          `json:"cancel_reason,omitempty"`
	Error              *CellError      `json:"error,omitempty"`
	Output             json.RawMessage `json:"output,omitempty"`         // succeeded runs; and a streamed run's items so far
	OutputMode         string          `json:"output_mode,omitempty"`    // stream, or "" for one value
	Items              *int            `json:"items,omitempty"`          // streamed runs: how many items
	OutputPartial      bool            `json:"output_partial,omitempty"` // a streamed run that ended before finishing its items
	Rendered           bool            `json:"rendered"`
	Partial            bool            `json:"partial"`
	LogTruncated       bool            `json:"log_truncated"`
	DependencyWarnings []deps.Warning  `json:"dependency_warnings,omitempty"`
	Usage              ResultUsage     `json:"usage"`
	Limits             ResultLimits    `json:"limits"`
	Image              ResultImage     `json:"image"`
	Runtime            string          `json:"runtime"`
	CreatedAt          time.Time       `json:"created_at"`
	StartedAt          *time.Time      `json:"started_at,omitempty"`
	FinishedAt         time.Time       `json:"finished_at"`
}

// ResultUsage is measured resource usage.
type ResultUsage struct {
	DurationSeconds float64  `json:"duration_seconds"`
	PeakMemoryBytes *int64   `json:"peak_memory_bytes,omitempty"`
	CPUSeconds      *float64 `json:"cpu_seconds,omitempty"`
}

// ResultLimits are the limits the run had.
type ResultLimits struct {
	MemoryBytes    int64   `json:"memory_bytes"`
	CPUs           float64 `json:"cpus"`
	PIDs           int64   `json:"pids"`
	TmpBytes       int64   `json:"tmp_bytes"`
	TimeoutSeconds float64 `json:"timeout_seconds"`
	LogBytes       int64   `json:"log_bytes"`
}

// ResultImage identifies the runner image.
type ResultImage struct {
	Ref string `json:"ref"`
	ID  string `json:"id,omitempty"`
}

func (c *core) buildResult(ctx context.Context, r store.Run, out Outcome, ws *WrapperStatus, res isolation.RunResult,
	started *time.Time, finished time.Time, truncated bool) Result {
	rs := Result{
		SchemaVersion: ResultSchemaVersion, RunID: r.ID, SpaceID: r.SpaceID, Notebook: r.Notebook,
		NotebookSHA256: r.NotebookSHA256, Function: r.Function, Version: r.Version,
		Parameters: json.RawMessage(r.ParametersJSON), Status: out.Status, ErrorKind: out.ErrorKind,
		Fault: out.Fault, Retryable: Retryable(out.Status), Message: out.Message, Error: out.Cell,
		Rendered: out.Rendered, LogTruncated: truncated,
		Limits: ResultLimits{
			MemoryBytes: c.cfg.Limits.MemoryBytes, CPUs: float64(c.cfg.Limits.NanoCPUs) / 1e9, PIDs: c.cfg.Limits.PIDs,
			TmpBytes: c.cfg.Limits.TmpfsBytes, TimeoutSeconds: c.cfg.Timeout.Seconds(), LogBytes: c.cfg.LogCap,
		},
		Image:   ResultImage{Ref: c.cfg.Image, ID: c.d.ImageID()},
		Runtime: runtimeName(c.cfg.Runtime), CreatedAt: r.CreatedAt.UTC(), StartedAt: utc(started), FinishedAt: finished.UTC(),
	}
	if r.DependencyWarnings != "" {
		_ = json.Unmarshal([]byte(r.DependencyWarnings), &rs.DependencyWarnings)
	}
	if out.Status == StatusCancelled {
		rs.CancelledBy, rs.CancelReason = r.CancelledBy, r.CancelReason
	}
	if started != nil {
		rs.Usage.DurationSeconds = finished.Sub(*started).Seconds()
		if !res.Started.IsZero() && res.Finished.After(res.Started) {
			rs.Usage.DurationSeconds = res.Finished.Sub(res.Started).Seconds()
		}
	}
	if ws != nil {
		rs.Usage.PeakMemoryBytes = ws.CGroup.MemoryPeakBytes
		if ws.CGroup.CPUUsageUsec != nil {
			s := float64(*ws.CGroup.CPUUsageUsec) / 1e6
			rs.Usage.CPUSeconds = &s
		}
	}
	// An OOM kill means the run's memory reached its limit. Where nothing
	// recorded the peak (gVisor and Kubernetes kill the whole container,
	// the runner with it), the limit is the peak.
	if out.Status == StatusOOM && rs.Usage.PeakMemoryBytes == nil && c.cfg.Limits.MemoryBytes > 0 {
		peak := c.cfg.Limits.MemoryBytes
		rs.Usage.PeakMemoryBytes = &peak
	}
	// A sampled peak (gVisor) can read above the run's limit: gVisor sees
	// the whole Pod's limit, sidecar included. Never report more than the run had.
	if p := rs.Usage.PeakMemoryBytes; p != nil && c.cfg.Limits.MemoryBytes > 0 && *p > c.cfg.Limits.MemoryBytes {
		peak := c.cfg.Limits.MemoryBytes
		rs.Usage.PeakMemoryBytes = &peak
	}
	if r.OutputMode == contract.ModeStream {
		rs.OutputMode = contract.ModeStream
		if items, err := c.streamedItems(ctx, r); err == nil {
			n := len(items)
			rs.Items = &n
			if n > 0 || out.Status == StatusSucceeded {
				rs.Output, _ = json.Marshal(items)
			}
			rs.OutputPartial = out.Status != StatusSucceeded && n > 0
		} else {
			c.logFor(r).Error("read the run's items", "error", err)
		}
	}
	// A run that didn't succeed but saved some cells keeps a partial notebook.
	if out.Status != StatusSucceeded {
		if f, _, err := c.d.Spaces.OpenRunFile(ctx, r.SpaceID, r.ID, spaces.RunOutput); err == nil {
			_ = f.Close()
			rs.Partial = true
		}
	}
	return rs
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func runtimeName(r string) string {
	if r == "" {
		return "runc"
	}
	return r
}

// View is a run as the API shows it.
type View struct {
	ID                 string          `json:"id"`
	SpaceID            string          `json:"space_id"`
	Notebook           string          `json:"notebook"`
	NotebookSHA256     string          `json:"notebook_sha256,omitempty"`
	Function           string          `json:"function,omitempty"`
	Version            int             `json:"version,omitempty"`
	ReadOnlySpace      bool            `json:"read_only_space"`
	Client             string          `json:"client,omitempty"` // the application that started it on its user's behalf
	Parameters         json.RawMessage `json:"parameters"`
	Status             string          `json:"status"`
	ErrorKind          string          `json:"error_kind,omitempty"`
	Fault              string          `json:"fault,omitempty"`
	Retryable          bool            `json:"retryable"`
	Message            string          `json:"message,omitempty"`
	CancelledBy        string          `json:"cancelled_by,omitempty"`
	CancelReason       string          `json:"cancel_reason,omitempty"`
	OutputBytes        int64           `json:"output_bytes,omitempty"`
	QueuePosition      int             `json:"queue_position,omitempty"` // 1 = next to start
	DependencyWarnings []deps.Warning  `json:"dependency_warnings,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	StartedAt          *time.Time      `json:"started_at,omitempty"`
	FinishedAt         *time.Time      `json:"finished_at,omitempty"`
	Result             *Result         `json:"result,omitempty"`
}

func (m *Manager) view(ctx context.Context, r store.Run, withResult bool) View {
	v := View{
		ID: r.ID, SpaceID: r.SpaceID, Notebook: r.Notebook, NotebookSHA256: r.NotebookSHA256,
		Function: r.Function, Version: r.Version, ReadOnlySpace: !r.WritesSpace, Client: r.ClientApp,
		Parameters: json.RawMessage(r.ParametersJSON),
		Status:     r.Status, ErrorKind: r.ErrorKind, Fault: r.Fault, Retryable: Retryable(r.Status), Message: r.Message,
		CancelledBy: r.CancelledBy, CancelReason: r.CancelReason, OutputBytes: r.OutputBytes,
		CreatedAt: r.CreatedAt.UTC(), StartedAt: utc(r.StartedAt), FinishedAt: utc(r.FinishedAt),
	}
	if r.DependencyWarnings != "" {
		_ = json.Unmarshal([]byte(r.DependencyWarnings), &v.DependencyWarnings)
	}
	if r.Status == StatusQueued {
		if pos, err := m.d.DB.QueuePosition(ctx, r.ID); err == nil {
			v.QueuePosition = pos
		}
	}
	if withResult && r.Terminal() {
		if b, err := m.d.Spaces.ReadRunFile(ctx, r.SpaceID, r.ID, spaces.RunResult, m.cfg.MaxOutput+1<<20); err == nil {
			var res Result
			if json.Unmarshal(b, &res) == nil && res.Status == r.Status {
				v.Result = &res
			}
		}
	}
	return v
}

// Get returns one run with its result.
func (m *Manager) Get(ctx context.Context, owner, spaceID, runID string) (View, error) {
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
	return m.view(ctx, r, true), nil
}

// ListFilter narrows a run listing; zero fields match everything.
type ListFilter struct {
	Status        string     // one of the statuses (KnownStatus)
	Function      string     // only invocations of this function
	CreatedAfter  *time.Time // exclusive
	CreatedBefore *time.Time // exclusive
}

var knownStatuses = map[string]bool{
	StatusQueued: true, StatusRunning: true, StatusSucceeded: true, StatusFailed: true,
	StatusOOM: true, StatusKernelDied: true, StatusTimeout: true, StatusCancelled: true,
	StatusInterrupted: true, StatusPlatformError: true,
}

// KnownStatus reports whether s is a run status.
func KnownStatus(s string) bool { return knownStatuses[s] }

// List returns one page of a space's runs (without results).
func (m *Manager) List(ctx context.Context, owner, spaceID string, f ListFilter, cursor string, limit int) ([]View, string, error) {
	if f.Status != "" && !KnownStatus(f.Status) {
		return nil, "", &spaces.InvalidError{Msg: "unknown status " + f.Status}
	}
	if f.Function != "" && !spaces.ValidFunctionName(f.Function) {
		return nil, "", &spaces.InvalidError{Msg: "function is not a valid function name"}
	}
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return nil, "", err
	}
	page, err := m.d.DB.ListRuns(ctx, spaceID, store.RunFilter(f), cursor, limit)
	if errors.Is(err, store.ErrBadCursor) {
		return nil, "", &spaces.InvalidError{Msg: "invalid cursor"}
	}
	if err != nil {
		return nil, "", err
	}
	out := make([]View, 0, len(page.Items))
	for _, r := range page.Items {
		out = append(out, m.view(ctx, r, false))
	}
	return out, page.NextCursor, nil
}

// Active reports whether the space has a queued or running run.
func (m *Manager) Active(ctx context.Context, spaceID string) bool {
	for _, st := range []string{StatusQueued, StatusRunning} {
		page, err := m.d.DB.ListRuns(ctx, spaceID, store.RunFilter{Status: st}, "", 1)
		if err == nil && len(page.Items) > 0 {
			return true
		}
	}
	return false
}
