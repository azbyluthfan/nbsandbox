// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// A notebook with an input and an output contract.
const nbContract = `{"nbformat": 4, "nbformat_minor": 5, "cells": [], "metadata": {"nbsandbox": {"contract": {
  "input":  {"type": "object", "required": ["region"], "properties": {"region": {"type": "string"},
             "cfg": {"type": "object", "properties": {"months": {"type": "integer", "minimum": 1}}}}},
  "output": {"type": "object", "required": ["forecast"]}}}}}`

func (e *env) upload(t *testing.T, owner, spaceID, name, body string) {
	t.Helper()
	done := false
	if _, err := e.sp.Upload(context.Background(), owner, spaceID, "", func() (*spaces.UploadPart, error) {
		if done {
			return nil, io.EOF
		}
		done = true
		return &spaces.UploadPart{Name: name, Body: strings.NewReader(body)}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// returning is a run that writes value as output.json (raw bytes; "" writes
// nothing) and succeeds.
func returning(value string) func(context.Context, isolation.RunSpec, isolation.LineSink) (isolation.RunResult, error) {
	return func(_ context.Context, spec isolation.RunSpec, _ isolation.LineSink) (isolation.RunResult, error) {
		if value != "" {
			_ = os.WriteFile(filepath.Join(spec.RunDir, "output.json"), []byte(value), 0o600)
		}
		writeOutputNotebook(spec)
		writeStatus(spec, "succeeded", nil)
		return isolation.RunResult{ExitCode: 0}, nil
	}
}

func TestOutputReturned(t *testing.T) {
	e := newEnv(t)
	e.be.behave = returning(`{"forecast": [10.5, 11], "model": {"mape": 0.07}}`)
	sid := e.space(t, "alice")
	v := e.wait(t, "alice", sid, e.submit(t, "alice", sid, `{"cfg": {"a": [1, {"b": null}]}}`).ID, true)
	if v.Status != StatusSucceeded || v.OutputBytes == 0 || v.Retryable {
		t.Fatalf("view %+v", v)
	}
	res, err := e.m.GetResult(context.Background(), "alice", sid, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSucceeded || res.Error != nil || string(res.Output) != `{"forecast":[10.5,11],"model":{"mape":0.07}}` {
		t.Fatalf("result %+v %s", res, res.Output)
	}
	if _, err := e.m.GetResult(context.Background(), "bob", sid, v.ID); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatalf("bob reads alice's result: %v", err)
	}
}

func TestOutputProblemsFailTheRun(t *testing.T) {
	cases := map[string]struct {
		nb, value, kind string
	}{
		"not JSON":                {nb, `{"forecast": `, KindOutputInvalid},
		"too large":               {nb, `"` + strings.Repeat("x", 2048) + `"`, KindOutputTooLarge},
		"contract mismatch":       {nbContract, `{"other": 1}`, KindOutputInvalid},
		"contract, nothing given": {nbContract, ``, KindOutputInvalid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, func(cfg *Config) { cfg.MaxOutput = 1024 })
			e.be.behave = returning(c.value)
			sid := e.space(t, "alice")
			e.upload(t, "alice", sid, "nb.ipynb", c.nb)
			v, _, err := e.m.Submit(context.Background(), "alice", sid, SubmitRequest{Notebook: "nb.ipynb",
				Parameters: json.RawMessage(`{"region": "jakarta"}`)})
			if err != nil {
				t.Fatal(err)
			}
			v = e.wait(t, "alice", sid, v.ID, true)
			if v.Status != StatusFailed || v.ErrorKind != c.kind || v.Fault != FaultNotebook {
				t.Fatalf("view %+v", v)
			}
			res, _ := e.m.GetResult(context.Background(), "alice", sid, v.ID)
			if res.Output != nil || res.Error == nil || res.Error.Kind != c.kind || res.Error.Retryable {
				t.Fatalf("result %+v", res)
			}
		})
	}
}

func TestInputContract(t *testing.T) {
	e := newEnv(t)
	sid := e.space(t, "alice")
	e.upload(t, "alice", sid, "nb.ipynb", nbContract)
	_, _, err := e.m.Submit(context.Background(), "alice", sid, SubmitRequest{Notebook: "nb.ipynb",
		Parameters: json.RawMessage(`{"cfg": {"months": 0}}`)})
	var ce *contract.Error
	if !errors.As(err, &ce) || len(ce.Problems) != 2 {
		t.Fatalf("want a contract error with two problems, got %v", err)
	}
	if got := e.be.startedRuns(); len(got) != 0 {
		t.Fatal("a rejected submission used a run slot")
	}
	entries, _ := os.ReadDir(filepath.Join(e.sp.Dir(sid), spaces.RunsDir))
	if len(entries) != 0 {
		t.Fatalf("a rejected submission left a run folder: %v", entries)
	}
	e.upload(t, "alice", sid, "bad.ipynb", `{"nbformat": 4, "cells": [], "metadata": {"nbsandbox": {"contract": {"input": {"type": 7}}}}}`)
	if _, _, err := e.m.Submit(context.Background(), "alice", sid, SubmitRequest{Notebook: "bad.ipynb"}); !errors.As(err, &ce) {
		t.Fatalf("invalid contract accepted: %v", err)
	}
}

func TestSnapshotAtSubmit(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		<-release
		b, _ := os.ReadFile(filepath.Join(spec.RunDir, spaces.RunInput))
		out.Line("ran: " + string(b))
		writeStatus(spec, "succeeded", nil)
		return isolation.RunResult{}, nil
	}
	sid := e.space(t, "alice")
	v := e.submit(t, "alice", sid, "")
	// The file changes after submission; the run must still execute the
	// notebook as it was.
	e.upload(t, "alice", sid, "nb.ipynb", nbContract)
	close(release)
	v = e.wait(t, "alice", sid, v.ID, true)
	log, _ := e.sp.ReadRunFile(context.Background(), sid, v.ID, spaces.RunLog, 1<<20)
	if !strings.Contains(string(log), "import pandas") || strings.Contains(string(log), "contract") {
		t.Fatalf("the run didn't execute the snapshot:\n%s", log)
	}
	if v.NotebookSHA256 == "" || v.Result == nil || v.Result.NotebookSHA256 != v.NotebookSHA256 {
		t.Fatalf("snapshot hash not recorded: %+v", v)
	}
}

func TestCancelRecordsWhoAndWhy(t *testing.T) {
	e := newEnv(t)
	e.be.behave = succeed(time.Hour)
	sid := e.space(t, "alice")
	v := e.submit(t, "alice", sid, "")
	e.wait(t, "alice", sid, v.ID, false)
	if _, err := e.m.Cancel(context.Background(), "alice", sid, v.ID, "alice", "superseded by a newer request"); err != nil {
		t.Fatal(err)
	}
	v = e.wait(t, "alice", sid, v.ID, true)
	res, _ := e.m.GetResult(context.Background(), "alice", sid, v.ID)
	if v.CancelledBy != "alice" || res.Error == nil || res.Error.CancelledBy != "alice" ||
		res.Error.CancelReason != "superseded by a newer request" || res.Error.Retryable || res.Error.Fault != FaultCaller {
		t.Fatalf("view %+v result %+v", v, res.Error)
	}
	if _, err := e.m.Cancel(context.Background(), "alice", sid, v.ID, "alice", strings.Repeat("x", MaxCancelReason+1)); err == nil {
		t.Fatal("over-long reason accepted")
	}
}

func TestResultWhileActiveAndRetryable(t *testing.T) {
	e := newEnv(t)
	e.be.behave = succeed(time.Hour)
	sid := e.space(t, "alice")
	v := e.submit(t, "alice", sid, "")
	e.wait(t, "alice", sid, v.ID, false)
	var sa *ErrStillActive
	if _, err := e.m.GetResult(context.Background(), "alice", sid, v.ID); !errors.As(err, &sa) || sa.Status != StatusRunning {
		t.Fatalf("result of a running run: %v", err)
	}
	for status, want := range map[string]bool{StatusInterrupted: true, StatusPlatformError: true, StatusFailed: false,
		StatusOOM: false, StatusTimeout: false, StatusCancelled: false, StatusSucceeded: false, StatusKernelDied: false} {
		if Retryable(status) != want {
			t.Errorf("Retryable(%s) = %v", status, !want)
		}
	}
}

func TestStatusEvents(t *testing.T) {
	e := newEnv(t)
	sid := e.space(t, "alice")
	ctx := context.Background()
	_, wake, err := e.m.Events(ctx, "alice", sid, EventsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	v := e.submit(t, "alice", sid, "")
	select {
	case <-wake:
	case <-time.After(5 * time.Second):
		t.Fatal("no wake-up on submit")
	}
	e.wait(t, "alice", sid, v.ID, true)
	evs, _, err := e.m.Events(ctx, "alice", sid, EventsQuery{RunID: v.ID})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range evs {
		got = append(got, ev.Status)
	}
	if strings.Join(got, ",") != "queued,running,succeeded" {
		t.Fatalf("events %v", got)
	}
	var last StatusEvent
	_ = json.Unmarshal([]byte(evs[2].Data), &last)
	if last.Result == nil || last.Result.Status != StatusSucceeded {
		t.Fatalf("final event without the result: %s", evs[2].Data)
	}
	// Resuming after the second event returns only the third.
	rest, _, _ := e.m.Events(ctx, "alice", sid, EventsQuery{After: evs[1].Seq})
	if len(rest) != 1 || rest[0].Seq != evs[2].Seq {
		t.Fatalf("resume: %+v", rest)
	}
	if _, _, err := e.m.Events(ctx, "bob", sid, EventsQuery{}); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatal("bob reads alice's events")
	}
	// Pruned events are reported, so a client knows to re-read.
	e.now.Add((25 * time.Hour).Milliseconds())
	if _, err := e.db.AddEvent(ctx, store.Event{SpaceID: sid, RunID: v.ID, Status: "x", Data: "{}", CreatedAt: time.UnixMilli(e.now.Load())}); err != nil {
		t.Fatal(err)
	}
	e.m.RunRetention(ctx)
	if _, _, err := e.m.Events(ctx, "alice", sid, EventsQuery{After: evs[0].Seq}); !errors.Is(err, ErrEventsPruned) {
		t.Fatalf("pruned events not reported: %v", err)
	}
}

// A run that writes past the space's quota is stopped while it runs.
func TestDiskQuotaStopsRun(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.DiskCheckEvery = 20 * time.Millisecond })
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, _ isolation.LineSink) (isolation.RunResult, error) {
		// The space's quota is 10 MiB; write 12 MiB, then keep running.
		_ = os.WriteFile(filepath.Join(spec.RunDir, "artifacts", "big.bin"), make([]byte, 12<<20), 0o600)
		<-ctx.Done()
		writeStatus(spec, "terminated", nil)
		return isolation.RunResult{Stopped: true, ExitCode: 143}, nil
	}
	sid := e.space(t, "alice")
	v := e.wait(t, "alice", sid, e.submit(t, "alice", sid, "").ID, true)
	if v.Status != StatusFailed || v.ErrorKind != KindResourceDisk || v.Fault != FaultNotebook {
		t.Fatalf("view %+v", v)
	}
}
