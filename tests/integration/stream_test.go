// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type event struct {
	ID       int64
	Name     string
	Data     string
	Received time.Time
}

type stream struct {
	resp *http.Response
	r    *bufio.Reader
}

// openStream connects to a run's live log, resuming after lastID.
func openStream(t *testing.T, user, spaceID, runID string, lastID int64) *stream {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/v1/spaces/"+spaceID+"/runs/"+runID+"/log/stream", nil)
	req.SetBasicAuth(user, passwords[user])
	if lastID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastID, 10))
	}
	resp, err := (&http.Client{}).Do(req) // no client timeout: streams are long
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream: %d %s", resp.StatusCode, b)
	}
	return &stream{resp: resp, r: bufio.NewReaderSize(resp.Body, 64<<10)}
}

func (s *stream) close() { _ = s.resp.Body.Close() }

func (s *stream) next() (event, error) {
	var ev event
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "" && ev.Name != "":
			ev.Received = time.Now()
			return ev, nil
		case strings.HasPrefix(line, "id: "):
			ev.ID, _ = strconv.ParseInt(line[4:], 10, 64)
		case strings.HasPrefix(line, "event: "):
			ev.Name = line[7:]
		case strings.HasPrefix(line, "data: "):
			ev.Data = line[6:]
		}
	}
}

// readToEnd collects line events until the first other event (done,
// dropped, shutdown) or the end of the stream.
func (s *stream) readToEnd() ([]event, event, error) {
	var lines []event
	for {
		ev, err := s.next()
		if err != nil {
			return lines, event{}, err
		}
		if ev.Name != "line" {
			return lines, ev, nil
		}
		lines = append(lines, ev)
	}
}

// sequential reports the first gap or duplicate, if any.
func sequential(lines []event, from int64) error {
	for i, ev := range lines {
		if ev.ID != from+int64(i) {
			return fmt.Errorf("event %d has id %d, want %d", i, ev.ID, from+int64(i))
		}
	}
	return nil
}

func logLines(t *testing.T, user, spaceID, runID string) []string {
	t.Helper()
	body := string(call(t, "GET", "/v1/spaces/"+spaceID+"/runs/"+runID+"/log", user, nil, nil).Body)
	return strings.Split(strings.TrimSuffix(body, "\n"), "\n")
}

func sameAsLog(t *testing.T, lines []event, log []string) error {
	t.Helper()
	if len(lines) != len(log) {
		return fmt.Errorf("streamed %d lines, log.txt has %d", len(lines), len(log))
	}
	for i := range log {
		if lines[i].Data != log[i] {
			return fmt.Errorf("line %d: streamed %q, log.txt %q", i+1, lines[i].Data, log[i])
		}
	}
	return nil
}

// One line a second for 10 s arrives live, in order.
func TestLiveLines(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"stream.ipynb": ""})
	run := startRun(t, "alice", sp, "stream.ipynb", nil)
	s := openStream(t, "alice", sp, run.ID, 0)
	defer s.close()
	lines, end, err := s.readToEnd()
	if err != nil {
		t.Fatal(err)
	}
	var first, last time.Time
	var order []int
	for _, ev := range lines {
		var n int
		if _, err := fmt.Sscanf(ev.Data, "line %d", &n); err == nil {
			if first.IsZero() {
				first = ev.Received
			}
			last = ev.Received
			order = append(order, n)
		}
	}
	inOrder := len(order) == 10
	for i, n := range order {
		inOrder = inOrder && n == i
	}
	lead := end.Received.Sub(first)
	ok := end.Name == "done" && sequential(lines, 1) == nil && inOrder && lead > 8*time.Second && last.Sub(first) > 8*time.Second
	finding(t, "live-lines", "notebook prints one line a second for 10 s", "first line long before the run ends; order preserved",
		fmt.Sprintf("first line %.1fs before done; lines %v; %s", lead.Seconds(), order, end.Data), ok)
	if !ok {
		t.Fatalf("lead %v order %v end %+v", lead, order, end)
	}
}

// Disconnect after 5 lines, resume with Last-Event-ID.
func TestResume(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"stream.ipynb": ""})
	run := startRun(t, "alice", sp, "stream.ipynb", nil)
	s := openStream(t, "alice", sp, run.ID, 0)
	var got []event
	for len(got) < 5 {
		ev, err := s.next()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev)
	}
	s.close()
	time.Sleep(2 * time.Second)
	s2 := openStream(t, "alice", sp, run.ID, got[len(got)-1].ID)
	defer s2.close()
	rest, end, err := s2.readToEnd()
	if err != nil {
		t.Fatal(err)
	}
	all := append(got, rest...)
	errSeq := sequential(all, 1)
	errLog := sameAsLog(t, all, logLines(t, "alice", sp, run.ID))
	ok := end.Name == "done" && errSeq == nil && errLog == nil
	finding(t, "resume", "disconnect after 5 lines, reconnect with Last-Event-ID", "remaining lines, no gaps or duplicates",
		fmt.Sprintf("%d lines total; sequence %v; matches log.txt %v", len(all), errSeq == nil, errLog == nil), ok)
	if !ok {
		t.Fatalf("seq %v log %v end %+v", errSeq, errLog, end)
	}
}

// Connect mid-run.
func TestJoinMidRun(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"stream.ipynb": ""})
	run := startRun(t, "alice", sp, "stream.ipynb", nil)
	waitLog(t, "alice", sp, run.ID, "line 4", time.Minute)
	s := openStream(t, "alice", sp, run.ID, 0)
	defer s.close()
	lines, end, err := s.readToEnd()
	if err != nil {
		t.Fatal(err)
	}
	errSeq := sequential(lines, 1)
	errLog := sameAsLog(t, lines, logLines(t, "alice", sp, run.ID))
	ok := end.Name == "done" && errSeq == nil && errLog == nil && strings.Contains(lines[0].Data, "Input Notebook")
	finding(t, "join-mid-run", "connect mid-run (after line 4)", "earlier lines replayed, then live lines, no gaps",
		fmt.Sprintf("%d lines; sequence %v; matches log.txt %v", len(lines), errSeq == nil, errLog == nil), ok)
	if !ok {
		t.Fatalf("seq %v log %v end %+v", errSeq, errLog, end)
	}
}

// Connect after the run finished.
func TestAfterFinish(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"fail_cell3.ipynb": ""})
	run := waitRun(t, "alice", sp, startRun(t, "alice", sp, "fail_cell3.ipynb", nil).ID, 2*time.Minute)
	start := time.Now()
	s := openStream(t, "alice", sp, run.ID, 0)
	defer s.close()
	lines, end, err := s.readToEnd()
	if err != nil {
		t.Fatal(err)
	}
	var done struct{ Status string }
	_ = json.Unmarshal([]byte(end.Data), &done)
	errLog := sameAsLog(t, lines, logLines(t, "alice", sp, run.ID))
	ok := end.Name == "done" && done.Status == "failed" && errLog == nil && time.Since(start) < 5*time.Second
	finding(t, "replay-after-finish", "connect after the run finished", "full log replayed, then done with the final status",
		fmt.Sprintf("%d lines in %.2fs; done %s; matches log.txt %v", len(lines), time.Since(start).Seconds(), done.Status, errLog == nil), ok)
	if !ok {
		t.Fatalf("log %v end %+v", errLog, end)
	}
}

// Five viewers, one deliberately slow.
func TestSlowViewer(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"burst.ipynb": ""})

	// Baseline: the same run with nobody watching.
	base := waitRun(t, "alice", sp, startRun(t, "alice", sp, "burst.ipynb", nil).ID, 3*time.Minute)
	baseDur := runDuration(t, sp, base.ID)

	run := startRun(t, "alice", sp, "burst.ipynb", nil)
	var wg sync.WaitGroup
	fastErrs := make([]error, 4)
	fastCounts := make([]int, 4)
	for i := 0; i < 4; i++ {
		s := openStream(t, "alice", sp, run.ID, 0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.close()
			lines, end, err := s.readToEnd()
			fastCounts[i] = len(lines)
			switch {
			case err != nil:
				fastErrs[i] = err
			case end.Name != "done":
				fastErrs[i] = fmt.Errorf("ended with %s", end.Name)
			default:
				fastErrs[i] = sequential(lines, 1)
			}
		}()
	}
	slow := openStream(t, "alice", sp, run.ID, 0)
	first, _ := slow.next()
	// Stop reading until the run is over.
	finished := waitRun(t, "alice", sp, run.ID, 3*time.Minute)
	runDur := runDuration(t, sp, run.ID)

	got := []event{first}
	dropped := false
	for {
		lines, end, err := slow.readToEnd()
		got = append(got, lines...)
		if err != nil {
			t.Fatalf("slow viewer: %v", err)
		}
		if end.Name == "done" {
			break
		}
		if end.Name != "dropped" {
			t.Fatalf("slow viewer ended with %+v", end)
		}
		dropped = true
		slow.close()
		slow = openStream(t, "alice", sp, run.ID, got[len(got)-1].ID) // resume
	}
	slow.close()
	wg.Wait()

	log := logLines(t, "alice", sp, run.ID)
	slowErr := sameAsLog(t, got, log)
	fastOK := true
	for i := range fastErrs {
		fastOK = fastOK && fastErrs[i] == nil && fastCounts[i] == len(log)
	}
	unaffected := runDur <= baseDur*3/2+2*time.Second
	// Viewers read the run's lines at their own pace: the slow one is never
	// dropped, and gets every line once it reads again.
	ok := finished.Status == "succeeded" && fastOK && slowErr == nil && !dropped && unaffected
	finding(t, "slow-viewer", "five viewers on a 20,000-line run, one stops reading",
		"run duration unaffected; fast viewers complete; the slow viewer isn't dropped and gets every line once it reads again",
		fmt.Sprintf("run %.1fs (alone %.1fs); fast viewers complete %v (%v); slow dropped %v, complete %v",
			runDur.Seconds(), baseDur.Seconds(), fastOK, fastCounts, dropped, slowErr == nil), ok)
	if !ok {
		t.Fatalf("fast %v %v slow %v dropped %v run %v base %v", fastErrs, fastCounts, slowErr, dropped, runDur, baseDur)
	}
}

func runDuration(t *testing.T, sp, runID string) time.Duration {
	t.Helper()
	var v struct {
		Result struct {
			Usage struct {
				DurationSeconds float64 `json:"duration_seconds"`
			} `json:"usage"`
		} `json:"result"`
	}
	call(t, "GET", "/v1/spaces/"+sp+"/runs/"+runID, "alice", nil, nil).json(t, &v)
	return time.Duration(v.Result.Usage.DurationSeconds * float64(time.Second))
}
