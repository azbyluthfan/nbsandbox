// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Error kinds for a run's returned value.
const (
	KindOutputInvalid  = "output_invalid"   // not JSON, or doesn't match the output contract
	KindOutputTooLarge = "output_too_large" // over runs.max_output
)

// Retryable reports whether the same request may succeed if sent again:
// only for platform problems. A notebook that failed, ran out of memory or
// timed out will do the same with the same input.
func Retryable(status string) bool {
	return status == StatusInterrupted || status == StatusPlatformError
}

// checkOutput runs after a run succeeded: it reads the value the notebook
// returned (runs/<id>/output.json), checks size, JSON and the output
// contract, and turns a bad value into a failed run. It returns the outcome
// and the value (nil when the notebook returned none).
func (c *core) checkOutput(ctx context.Context, r store.Run, out Outcome) (Outcome, json.RawMessage) {
	if out.Status != StatusSucceeded {
		return out, nil
	}
	fail := func(kind, msg string) (Outcome, json.RawMessage) {
		out.Status, out.ErrorKind, out.Fault, out.Message = StatusFailed, kind, FaultNotebook, msg
		return out, nil
	}
	if r.OutputMode == contract.ModeStream {
		// The items were checked as they came; the output is their list.
		if _, ok := c.d.Spaces.RunFileSize(ctx, r.SpaceID, r.ID, spaces.RunValue); ok {
			return fail(KindOutputInvalid, "the notebook returns a stream of items (nbsandbox.emit), but it called nbsandbox.output")
		}
		return out, nil
	}
	var value json.RawMessage
	if n, ok := c.d.Spaces.RunFileSize(ctx, r.SpaceID, r.ID, spaces.RunValue); ok {
		if n > c.cfg.MaxOutput {
			return fail(KindOutputTooLarge, fmt.Sprintf("the notebook returned %d bytes with nbsandbox.output; the limit is %d. "+
				"Write large results to an artifact instead", n, c.cfg.MaxOutput))
		}
		b, err := c.d.Spaces.ReadRunFile(ctx, r.SpaceID, r.ID, spaces.RunValue, c.cfg.MaxOutput)
		if err != nil || !json.Valid(b) {
			return fail(KindOutputInvalid, "the value returned with nbsandbox.output is not valid JSON")
		}
		value = b
	}
	if r.OutputSchema != "" {
		schema, err := contract.Compile(nil, json.RawMessage(r.OutputSchema))
		if err != nil { // compiled fine at submit; only a changed library could get here
			return fail(KindOutputInvalid, "the output contract can't be compiled: "+err.Error())
		}
		if err := schema.ValidateOutput(value); err != nil {
			return fail(KindOutputInvalid, err.Error())
		}
	}
	return out, value
}

// CallerResult is what a caller gets for a finished run: the output for a
// succeeded run, or an error object with the reasons for any other final
// status. GET .../runs/{id}/result, the final status event, and ?wait=
// answers all use it.
type CallerResult struct {
	RunID    string          `json:"run_id"`
	Status   string          `json:"status"`
	Function string          `json:"function,omitempty"`
	Version  int             `json:"version,omitempty"`
	Output   json.RawMessage `json:"output,omitempty"`
	// A streamed run: how many items it sent, and whether it ended before
	// finishing (output then holds the items so far).
	Items   *int         `json:"items,omitempty"`
	Partial bool         `json:"partial,omitempty"`
	Error   *CallerError `json:"error,omitempty"`
}

// CallerError explains a run that didn't succeed.
type CallerError struct {
	Kind             string `json:"kind"`
	Fault            string `json:"fault"`
	Retryable        bool   `json:"retryable"`
	Message          string `json:"message"`
	CellIndex        *int   `json:"cell_index,omitempty"`
	ExecutionCount   *int   `json:"execution_count,omitempty"`
	ExceptionType    string `json:"exception_type,omitempty"`
	ExceptionMessage string `json:"exception_message,omitempty"`
	CancelledBy      string `json:"cancelled_by,omitempty"`
	CancelReason     string `json:"cancel_reason,omitempty"`
	PeakMemoryBytes  *int64 `json:"peak_memory_bytes,omitempty"`
	MemoryLimitBytes int64  `json:"memory_limit_bytes,omitempty"`
}

// callerResult builds the caller's view from result.json.
func callerResult(res Result) CallerResult {
	cr := CallerResult{RunID: res.RunID, Status: res.Status, Function: res.Function, Version: res.Version,
		Items: res.Items, Partial: res.OutputPartial}
	if res.Status == StatusSucceeded || res.OutputPartial {
		if len(res.Output) > 0 { // result.json is indented; callers get it compact
			var b bytes.Buffer
			if json.Compact(&b, res.Output) == nil {
				cr.Output = b.Bytes()
			}
		}
		if res.Status == StatusSucceeded {
			return cr
		}
	}
	e := &CallerError{Kind: res.ErrorKind, Fault: res.Fault, Retryable: Retryable(res.Status), Message: res.Message,
		CancelledBy: res.CancelledBy, CancelReason: res.CancelReason}
	if e.Kind == "" {
		e.Kind = res.Status
	}
	if res.Error != nil {
		e.CellIndex, e.ExecutionCount = res.Error.CellIndex, res.Error.ExecutionCount
		e.ExceptionType, e.ExceptionMessage = res.Error.ExceptionType, res.Error.Message
	}
	if res.Status == StatusOOM {
		e.PeakMemoryBytes, e.MemoryLimitBytes = res.Usage.PeakMemoryBytes, res.Limits.MemoryBytes
	}
	cr.Error = e
	return cr
}

// ErrStillActive: the run hasn't finished, so it has no result yet (409).
type ErrStillActive struct{ Status string }

func (e *ErrStillActive) Error() string {
	return fmt.Sprintf("the run is %s; it has no result yet", e.Status)
}

// GetResult returns a finished run's result.
func (m *Manager) GetResult(ctx context.Context, owner, spaceID, runID string) (CallerResult, error) {
	v, err := m.Get(ctx, owner, spaceID, runID)
	if err != nil {
		return CallerResult{}, err
	}
	if v.Status == StatusQueued || v.Status == StatusRunning {
		return CallerResult{}, &ErrStillActive{Status: v.Status}
	}
	if v.Result == nil {
		return CallerResult{}, errors.New("the run's result.json is missing or doesn't match its status")
	}
	return callerResult(*v.Result), nil
}
