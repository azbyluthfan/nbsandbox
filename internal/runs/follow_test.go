// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// lines is a backend that prints n lines, `gap` apart, then succeeds.
func lines(n int, gap time.Duration) func(context.Context, isolation.RunSpec, isolation.LineSink) (isolation.RunResult, error) {
	return func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		for i := 1; i <= n; i++ {
			out.Line(fmt.Sprintf("line %d", i))
			select {
			case <-time.After(gap):
			case <-ctx.Done():
				return isolation.RunResult{Stopped: true, ExitCode: 143}, nil
			}
		}
		writeOutputNotebook(spec)
		writeStatus(spec, "succeeded", nil)
		return isolation.RunResult{}, nil
	}
}

func collectLog(t *testing.T, ch <-chan LogItem) (got []string, end LogItem) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case it, ok := <-ch:
			if !ok {
				return got, end
			}
			if it.Kind == LogLine {
				got = append(got, it.Line.Text)
			} else {
				end = it
			}
		case <-timeout:
			t.Fatal("log stream didn't end")
		}
	}
}

func TestFollowLog(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.be.behave = lines(5, 20*time.Millisecond)
	sid := e.space(t, "alice")
	if _, err := e.m.FollowLog(ctx, "bob", sid, spaces.NewID(), 0); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatalf("bob follows alice's run: %v", err)
	}
	v := e.submit(t, "alice", sid, "")
	if _, err := e.m.FollowLog(ctx, "alice", sid, v.ID, -1); err == nil {
		t.Fatal("negative resume point accepted")
	}

	// Live: from the start (the run may still be queued).
	ch, err := e.m.FollowLog(ctx, "alice", sid, v.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	live, end := collectLog(t, ch)
	if len(live) < 5 || live[0] != "line 1" || end.Kind != LogDone || end.Status != StatusSucceeded || end.LastSeq != int64(len(live)) {
		t.Fatalf("live: %q, end %+v", live, end)
	}

	// Replayed from storage after the run, resuming after line 2.
	ch, err = e.m.FollowLog(ctx, "alice", sid, v.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	replay, end := collectLog(t, ch)
	if len(replay) != len(live)-2 || replay[0] != live[2] || end.Kind != LogDone || end.LastSeq != int64(len(live)) {
		t.Fatalf("replay: %q, end %+v", replay, end)
	}

	// The reader stops: the stream ends without blocking.
	e.be.behave = lines(100, 10*time.Millisecond)
	v2 := e.submit(t, "alice", sid, "")
	cctx, cancel := context.WithCancel(ctx)
	ch, err = e.m.FollowLog(cctx, "alice", sid, v2.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	cancel()
	for range ch { //nolint:revive // drain until closed
	}
	e.wait(t, "alice", sid, v2.ID, true)
}

func TestFollowEventsAndWait(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.be.behave = succeed(150 * time.Millisecond)
	sid := e.space(t, "alice")
	if _, err := e.m.FollowEvents(ctx, "bob", sid, EventsQuery{}); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatalf("bob follows alice's space: %v", err)
	}
	if _, err := e.m.FollowEvents(ctx, "alice", sid, EventsQuery{Function: "Bad!"}); err == nil {
		t.Fatal("bad function name accepted")
	}
	if _, err := e.m.FollowEvents(ctx, "alice", sid, EventsQuery{After: -1}); err == nil {
		t.Fatal("negative resume point accepted")
	}
	if _, err := e.m.FollowEvents(ctx, "alice", sid, EventsQuery{RunID: spaces.NewID()}); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	sctx, stopSpace := context.WithCancel(ctx)
	defer stopSpace()
	space, err := e.m.FollowEvents(sctx, "alice", sid, EventsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	v := e.submit(t, "alice", sid, "")

	// Waiting less than the run takes: the run as it is now.
	_, now, done, err := e.m.WaitResult(ctx, "alice", sid, v.ID, 10*time.Millisecond, nil)
	if err != nil || done || now.ID != v.ID {
		t.Fatalf("short wait: %+v %v %v", now, done, err)
	}
	stop := make(chan struct{})
	close(stop)
	if _, _, done, err := e.m.WaitResult(ctx, "alice", sid, v.ID, time.Minute, stop); err != nil || done {
		t.Fatalf("stopped wait: %v %v", done, err)
	}
	res, _, done, err := e.m.WaitResult(ctx, "alice", sid, v.ID, 10*time.Second, nil)
	if err != nil || !done || res.Status != StatusSucceeded {
		t.Fatalf("wait: %+v %v %v", res, done, err)
	}
	if _, _, _, err := e.m.WaitResult(ctx, "bob", sid, v.ID, time.Second, nil); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatalf("bob waits on alice's run: %v", err)
	}

	// The space stream saw queued, running, succeeded.
	var statuses []string
	for len(statuses) < 3 {
		select {
		case it := <-space:
			if it.Kind == EventStatus && it.Seq > 0 {
				statuses = append(statuses, it.Status)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("space events: %v", statuses)
		}
	}
	if statuses[0] != StatusQueued || statuses[2] != StatusSucceeded {
		t.Fatalf("space events: %v", statuses)
	}

	// A per-run stream of a finished run: its events, ending at the last.
	run, err := e.m.FollowEvents(ctx, "alice", sid, EventsQuery{RunID: v.ID})
	if err != nil {
		t.Fatal(err)
	}
	var last EventItem
	for it := range run {
		last = it
	}
	if last.Status != StatusSucceeded || last.Seq == 0 {
		t.Fatalf("last run event %+v", last)
	}
	// Resuming after the final event (it's gone or already seen): the
	// result is sent instead, without a sequence number.
	run, err = e.m.FollowEvents(ctx, "alice", sid, EventsQuery{RunID: v.ID, After: last.Seq})
	if err != nil {
		t.Fatal(err)
	}
	var items []EventItem
	for it := range run {
		items = append(items, it)
	}
	if len(items) != 1 || items[0].Seq != 0 || items[0].Status != StatusSucceeded || len(items[0].Data) == 0 {
		t.Fatalf("resumed after the end: %+v", items)
	}
}
