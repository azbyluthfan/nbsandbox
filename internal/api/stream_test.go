// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/runs"
)

type sseEvent struct {
	ID       int64
	Name     string
	Data     string
	Received time.Time
}

// sseClient reads events from a live stream.
type sseClient struct {
	resp *http.Response
	r    *bufio.Reader
	// Comments counts ": ..." lines (heartbeats).
	Comments int
}

func openStream(t *testing.T, srv *httptest.Server, path, user string, lastID int64) *sseClient {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.SetBasicAuth(user, user+"-password")
	if lastID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastID, 10))
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return &sseClient{resp: resp, r: newReader(resp.Body)}
}

func newReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

// next returns the next event, or io.EOF when the stream ends.
func (c *sseClient) next() (sseEvent, error) {
	var ev sseEvent
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if ev.Name != "" {
				ev.Received = time.Now()
				return ev, nil
			}
		case strings.HasPrefix(line, ":"):
			c.Comments++
		case strings.HasPrefix(line, "id: "):
			ev.ID, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			ev.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.Data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// readAll reads until the stream ends; it returns the line events and the
// final non-line event.
func (c *sseClient) readAll(t *testing.T) ([]sseEvent, sseEvent) {
	t.Helper()
	var lines []sseEvent
	for {
		ev, err := c.next()
		if errors.Is(err, io.EOF) {
			return lines, sseEvent{}
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Name != "line" {
			return lines, ev
		}
		lines = append(lines, ev)
	}
}

func checkSequence(t *testing.T, lines []sseEvent, from int64) {
	t.Helper()
	for i, ev := range lines {
		if ev.ID != from+int64(i) {
			t.Fatalf("event %d has id %d, want %d (gap or duplicate)", i, ev.ID, from+int64(i))
		}
	}
}

func streamEnv(t *testing.T) (testEnv, *httptest.Server, string) {
	t.Helper()
	e := newEnv(t)
	srv := httptest.NewServer(e.handler)
	t.Cleanup(srv.Close)
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "lines.ipynb", nbJSON, "nb.ipynb", nbJSON, "slow.ipynb", nbJSON)
	return e, srv, sp.ID
}

func (e testEnv) startRun(t *testing.T, spaceID, notebook string) runs.View {
	t.Helper()
	rec := e.req("POST", "/v1/spaces/"+spaceID+"/runs", "alice", strings.NewReader(`{"notebook":"`+notebook+`"}`), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var v runs.View
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return v
}

// Lines arrive while the run is still going, in order.
func TestStreamLive(t *testing.T) {
	e, srv, sp := streamEnv(t)
	v := e.startRun(t, sp, "lines.ipynb")
	c := openStream(t, srv, "/v1/spaces/"+sp+"/runs/"+v.ID+"/log/stream", "alice", 0)
	if c.resp.Header.Get("Content-Type") != "text/event-stream" || c.resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers %v", c.resp.Header)
	}
	lines, end := c.readAll(t)
	checkSequence(t, lines, 1)
	if len(lines) != 32 || lines[1].Data != "tick 1" || lines[30].Data != "tick 30" ||
		lines[31].Data != "[nbsandbox] run finished: succeeded" {
		t.Fatalf("%d lines: first %q last %q", len(lines), lines[0].Data, lines[len(lines)-1].Data)
	}
	if spread := lines[30].Received.Sub(lines[1].Received); spread < 400*time.Millisecond {
		t.Fatalf("lines were not delivered live (all within %v)", spread)
	}
	var done struct {
		Status      string `json:"status"`
		LastEventID int64  `json:"last_event_id"`
	}
	_ = json.Unmarshal([]byte(end.Data), &done)
	if end.Name != "done" || done.Status != "succeeded" || done.LastEventID != 32 {
		t.Fatalf("end %+v", end)
	}
	// The status is recorded by the time "done" arrives.
	if got := e.waitRun(t, "alice", sp, v.ID); got.Status != "succeeded" {
		t.Fatalf("status after done: %s", got.Status)
	}
}

// Disconnect and resume with Last-Event-ID; join mid-run.
func TestStreamResumeAndJoinMidRun(t *testing.T) {
	e, srv, sp := streamEnv(t)
	v := e.startRun(t, sp, "lines.ipynb")
	path := "/v1/spaces/" + sp + "/runs/" + v.ID + "/log/stream"

	first := openStream(t, srv, path, "alice", 0)
	var got []sseEvent
	for len(got) < 5 {
		ev, err := first.next()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev)
	}
	_ = first.resp.Body.Close() // disconnect

	time.Sleep(150 * time.Millisecond) // the run moves on meanwhile
	resumed := openStream(t, srv, path, "alice", got[len(got)-1].ID)
	rest, end := resumed.readAll(t)
	all := append(got, rest...)
	checkSequence(t, all, 1)
	if len(all) != 32 || end.Name != "done" {
		t.Fatalf("resumed: %d lines, end %+v", len(all), end)
	}

	// A third viewer joining after the run finished gets the full log
	// from disk, then done.
	late := openStream(t, srv, path, "alice", 0)
	lines, end := late.readAll(t)
	checkSequence(t, lines, 1)
	if len(lines) != 32 || end.Name != "done" || !strings.Contains(end.Data, `"status":"succeeded"`) {
		t.Fatalf("late: %d lines, end %+v", len(lines), end)
	}
	// Resuming a finished run past the end gives just done.
	tail := openStream(t, srv, path, "alice", 32)
	if lines, end := tail.readAll(t); len(lines) != 0 || end.Name != "done" {
		t.Fatalf("tail: %d lines, %+v", len(lines), end)
	}
}

// A stream opened while the run is queued waits for it (with heartbeats).
func TestStreamWaitsForQueuedRun(t *testing.T) {
	e, srv, sp := streamEnv(t)
	// Fill both slots so the next run queues.
	other1, other2 := e.createSpace(t, "alice", ""), e.createSpace(t, "alice", "")
	e.upload(t, "alice", other1.ID, "", "slow.ipynb", nbJSON)
	e.upload(t, "alice", other2.ID, "", "slow.ipynb", nbJSON)
	s1, s2 := e.startRun(t, other1.ID, "slow.ipynb"), e.startRun(t, other2.ID, "slow.ipynb")
	time.Sleep(100 * time.Millisecond)
	v := e.startRun(t, sp, "lines.ipynb")
	if got, _ := e.runs.Get(t.Context(), "alice", sp, v.ID); got.Status != "queued" {
		t.Fatalf("status %s, want queued", got.Status)
	}
	c := openStream(t, srv, "/v1/spaces/"+sp+"/runs/"+v.ID+"/log/stream", "alice", 0)
	go func() {
		time.Sleep(300 * time.Millisecond)
		e.req("POST", "/v1/spaces/"+other1.ID+"/runs/"+s1.ID+"/cancel", "alice", nil, "")
		e.req("POST", "/v1/spaces/"+other2.ID+"/runs/"+s2.ID+"/cancel", "alice", nil, "")
	}()
	lines, end := c.readAll(t)
	checkSequence(t, lines, 1)
	if len(lines) != 32 || end.Name != "done" || c.Comments < 3 {
		t.Fatalf("%d lines, end %+v, %d heartbeats", len(lines), end, c.Comments)
	}
}

// A cancelled queued run never had a log: the stream just ends with done.
func TestStreamOfRunThatNeverStarted(t *testing.T) {
	e, srv, sp := streamEnv(t)
	busy := e.createSpace(t, "alice", "")
	e.upload(t, "alice", busy.ID, "", "slow.ipynb", nbJSON)
	other := e.createSpace(t, "alice", "")
	e.upload(t, "alice", other.ID, "", "slow.ipynb", nbJSON)
	b1, b2 := e.startRun(t, busy.ID, "slow.ipynb"), e.startRun(t, other.ID, "slow.ipynb")
	time.Sleep(100 * time.Millisecond)
	v := e.startRun(t, sp, "nb.ipynb")
	e.req("POST", "/v1/spaces/"+sp+"/runs/"+v.ID+"/cancel", "alice", nil, "")
	c := openStream(t, srv, "/v1/spaces/"+sp+"/runs/"+v.ID+"/log/stream", "alice", 0)
	if lines, end := c.readAll(t); len(lines) != 0 || !strings.Contains(end.Data, `"status":"cancelled"`) {
		t.Fatalf("%d lines, end %+v", len(lines), end)
	}
	e.req("POST", "/v1/spaces/"+busy.ID+"/runs/"+b1.ID+"/cancel", "alice", nil, "")
	e.req("POST", "/v1/spaces/"+other.ID+"/runs/"+b2.ID+"/cancel", "alice", nil, "")
}

// On shutdown, open streams get a final event and close.
func TestStreamEndsOnShutdown(t *testing.T) {
	e, srv, sp := streamEnv(t)
	v := e.startRun(t, sp, "slow.ipynb")
	c := openStream(t, srv, "/v1/spaces/"+sp+"/runs/"+v.ID+"/log/stream", "alice", 0)
	if ev, err := c.next(); err != nil || ev.Name != "line" {
		t.Fatalf("first event %+v %v", ev, err)
	}
	close(e.shutdown)
	_, end := c.readAll(t)
	if end.Name != "shutdown" || !strings.Contains(end.Data, `"last_event_id":1`) {
		t.Fatalf("end %+v", end)
	}
	if _, err := c.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("stream still open: %v", err)
	}
	e.req("POST", "/v1/spaces/"+sp+"/runs/"+v.ID+"/cancel", "alice", nil, "")
}

func TestStreamErrors(t *testing.T) {
	e, srv, sp := streamEnv(t)
	v := e.startRun(t, sp, "nb.ipynb")
	path := "/v1/spaces/" + sp + "/runs/" + v.ID + "/log/stream"
	for name, c := range map[string]struct {
		user, query string
		code        int
	}{
		"other user":     {"bob", "", http.StatusNotFound},
		"bad after":      {"alice", "?after=-1", http.StatusBadRequest},
		"not a number":   {"alice", "?after=x", http.StatusBadRequest},
		"no credentials": {"", "", http.StatusUnauthorized},
	} {
		req, _ := http.NewRequest("GET", srv.URL+path+c.query, nil)
		if c.user != "" {
			req.SetBasicAuth(c.user, c.user+"-password")
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("%s: %d, want %d", name, resp.StatusCode, c.code)
		}
	}
	e.waitRun(t, "alice", sp, v.ID)
}
