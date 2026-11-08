// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/runs"
)

func readStatus(t *testing.T, c *sseClient) (sseEvent, runs.StatusEvent) {
	t.Helper()
	ev, err := c.next()
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var se runs.StatusEvent
	if ev.Name == "status" {
		if err := json.Unmarshal([]byte(ev.Data), &se); err != nil {
			t.Fatalf("event data %q: %v", ev.Data, err)
		}
	}
	return ev, se
}

func TestStatusStreams(t *testing.T) {
	e, srv, sp := streamEnv(t)
	// The space stream is open before anything happens.
	space := openStream(t, srv, "/v1/spaces/"+sp+"/events", "alice", 0)
	if space.resp.StatusCode != http.StatusOK || space.resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("space stream: %d %v", space.resp.StatusCode, space.resp.Header)
	}

	run := e.startRun(t, sp, "nb.ipynb")
	// Live, on the space stream: queued, running, succeeded with the result.
	var ids []int64
	for _, want := range []string{"queued", "running", "succeeded"} {
		ev, se := readStatus(t, space)
		if ev.Name != "status" || se.Status != want || se.RunID != run.ID || ev.ID == 0 {
			t.Fatalf("space stream: want %s, got %+v %+v", want, ev, se)
		}
		ids = append(ids, ev.ID)
		if want == "succeeded" && (se.Result == nil || se.Result.Status != "succeeded") {
			t.Fatalf("final event without its result: %s", ev.Data)
		}
	}

	// The per-run stream of a finished run replays and ends.
	one := openStream(t, srv, "/v1/spaces/"+sp+"/runs/"+run.ID+"/events", "alice", 0)
	for i, want := range []string{"queued", "running", "succeeded"} {
		ev, se := readStatus(t, one)
		if se.Status != want || ev.ID != ids[i] {
			t.Fatalf("run stream event %d: %+v %+v", i, ev, se)
		}
	}
	if _, err := one.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("run stream didn't end after the final event: %v", err)
	}

	// Resuming after the first event gives the rest, nothing twice.
	resumed := openStream(t, srv, "/v1/spaces/"+sp+"/runs/"+run.ID+"/events", "alice", ids[0])
	if ev, se := readStatus(t, resumed); ev.ID != ids[1] || se.Status != "running" {
		t.Fatalf("resume: %+v %+v", ev, se)
	}

	// Bob gets nothing, and a bad function filter is refused.
	if c := openStream(t, srv, "/v1/spaces/"+sp+"/events", "bob", 0); c.resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob: %d", c.resp.StatusCode)
	}
	if c := openStream(t, srv, "/v1/spaces/"+sp+"/events?function=BAD!", "alice", 0); c.resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad filter: %d", c.resp.StatusCode)
	}
}
