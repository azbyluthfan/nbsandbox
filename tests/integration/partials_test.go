// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A notebook whose output is a stream of items, one a second.
const streamingNotebook = `{"nbformat": 4, "nbformat_minor": 5, "metadata": {"kernelspec": {"name": "python3", "display_name": "Python 3", "language": "python"}},
 "cells": [
  {"cell_type": "code", "metadata": {}, "execution_count": null, "outputs": [], "source": "import time, nbsandbox"},
  {"cell_type": "code", "metadata": {}, "execution_count": null, "outputs": [],
   "source": "for i in range(3):\n    nbsandbox.emit({'step': i}, total=3)\n    nbsandbox.progress((i + 1) / 3, f'step {i}')\n    time.sleep(1)"}
 ]}`

type sseLine struct {
	name, id, data string
	at             time.Time
}

func readSSE(t *testing.T, path string, header map[string]string) []sseLine {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+path, nil)
	req.SetBasicAuth("alice", passwords["alice"])
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out []sseLine
	var cur sseLine
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			cur.data, cur.at = strings.TrimPrefix(line, "data: "), time.Now()
		case line == "" && cur.data != "":
			out = append(out, cur)
			cur = sseLine{}
		}
	}
	return out
}

// A streamed output arrives item by item on the result stream while the
// notebook works, then the result with the same items as a list; the JSON
// result has the same list; the status stream stays status-only.
func TestStreamedOutput(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"stream.ipynb": streamingNotebook})
	run := startRun(t, "alice", sp, "stream.ipynb", nil)
	base := "/v1/spaces/" + sp + "/runs/" + run.ID

	evs := readSSE(t, base+"/result?include=progress", map[string]string{"Accept": "text/event-stream"})
	var names, ids []string
	var items []sseLine
	for _, e := range evs {
		names = append(names, e.name)
		if e.name == "item" {
			items = append(items, e)
			ids = append(ids, e.id)
		}
	}
	last := evs[len(evs)-1]
	var res struct {
		Status string          `json:"status"`
		Output json.RawMessage `json:"output"`
		Items  int             `json:"items"`
	}
	_ = json.Unmarshal([]byte(last.data), &res)
	var j struct {
		Output json.RawMessage `json:"output"`
	}
	call(t, "GET", base+"/result", "alice", nil, nil).json(t, &j)
	status := readSSE(t, base+"/events", nil)
	var spread time.Duration // first item to the end
	if len(items) > 0 {
		spread = last.at.Sub(items[0].at)
	}
	// Live: the items arrive as they are emitted, a second apart, so the
	// last comes 2 s after the first (at least 1 s on a busy cluster, where
	// copying them out of a Pod batches a little); not all at once (0 s) when
	// the run ends.
	live := len(items) == 3 && spread > 1500*time.Millisecond && items[2].at.Sub(items[0].at) > time.Second
	ok := live && last.name == "result" && res.Status == "succeeded" && res.Items == 3 &&
		strings.Join(ids, ",") == "1,2,3" && strings.Count(strings.Join(names, ","), "progress") == 3 &&
		string(res.Output) == `[{"step":0},{"step":1},{"step":2}]` && string(j.Output) == string(res.Output) && len(status) == 3
	finding(t, "streamed-output", "a notebook emits three items a second apart; follow the result stream, then read the result as JSON",
		"items arrive live (ids 1, 2, 3), then the result with the same list; JSON gives the same list; the status stream has only status",
		fmt.Sprintf("events %v; first item %.1fs before the end, last %.1fs after the first; JSON output %s; %d status events", names, spread.Seconds(), itemSpan(items).Seconds(), j.Output, len(status)), ok)
	if !ok {
		t.Fatalf("events %+v, result %+v, JSON %s", evs, res, j.Output)
	}
}

// itemSpan is the time from the first item's arrival to the last's.
func itemSpan(items []sseLine) time.Duration {
	if len(items) < 2 {
		return 0
	}
	return items[len(items)-1].at.Sub(items[0].at)
}
