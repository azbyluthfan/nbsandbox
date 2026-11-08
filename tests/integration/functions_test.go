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

// streamClient has no timeout: streams are long.
var streamClient = &http.Client{}

// callerResult is the API's result for a finished run (the parts tests read).
type callerResult struct {
	RunID    string          `json:"run_id"`
	Status   string          `json:"status"`
	Function string          `json:"function"`
	Version  int             `json:"version"`
	Output   json.RawMessage `json:"output"`
	Error    *struct {
		Kind          string `json:"kind"`
		Fault         string `json:"fault"`
		Retryable     bool   `json:"retryable"`
		Message       string `json:"message"`
		CellIndex     *int   `json:"cell_index"`
		ExceptionType string `json:"exception_type"`
	} `json:"error"`
}

func publish(t *testing.T, sp, name string, files ...string) int {
	t.Helper()
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/"+name+"/versions", "alice",
		map[string]any{"notebook": "forecast_fn.ipynb", "files": files}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", r.Code, r.Body)
	}
	var v struct{ Version int }
	r.json(t, &v)
	return v.Version
}

// invoke calls a function and waits for its result.
func invoke(t *testing.T, sp, path string, params any) (int, callerResult, []byte) {
	t.Helper()
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/"+path+"?wait=50s", "alice", map[string]any{"parameters": params}, nil)
	var res callerResult
	_ = json.Unmarshal(r.Body, &res)
	return r.Code, res, r.Body
}

// A published function: nested JSON in, JSON out, the bundle's own model
// file, and immutability while the space changes.
func TestFunctionInvoke(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"forecast_fn.ipynb": ""})
	upload(t, "alice", sp, "models", map[string]string{"model.txt": "m1\n"})
	if v := publish(t, sp, "forecast", "models"); v != 1 {
		t.Fatalf("first version %d", v)
	}
	// The space changes after publishing: version 1 must not.
	cur := call(t, "GET", "/v1/spaces/"+sp+"/files/models/model.txt", "alice", nil, nil).Header.Get("ETag")
	if r := call(t, "PUT", "/v1/spaces/"+sp+"/files/models/model.txt", "alice", strings.NewReader("m2\n"),
		map[string]string{"If-Match": cur}); r.Code != http.StatusOK {
		t.Fatalf("edit model: %d %s", r.Code, r.Body)
	}

	params := map[string]any{"region": "bandung", "cfg": map[string]any{"months": 4, "include": []any{"sales", nil}},
		"big": json.Number("123456789012345678901234567890")}
	code, res, body := invoke(t, sp, "forecast/invoke", params)
	var out struct {
		Region   string
		Model    string
		Forecast []float64
		Table    []map[string]any
		Echo     json.RawMessage
	}
	_ = json.Unmarshal(res.Output, &out)
	ok := code == http.StatusOK && res.Status == "succeeded" && res.Version == 1 && out.Model == "m1" &&
		out.Region == "bandung" && len(out.Forecast) == 4 && out.Forecast[3] == 6 && len(out.Table) == 4 &&
		strings.Contains(string(out.Echo), `"big":123456789012345678901234567890`) && strings.Contains(string(out.Echo), `"include":["sales",null]`)
	finding(t, "function-invoke", "invoke a published notebook with nested JSON and wait", "succeeded; output computed from the input and the bundle",
		fmt.Sprintf("%d %s; model %q, forecast %v, echo %s", code, res.Status, out.Model, out.Forecast, out.Echo), ok)
	if !ok {
		t.Fatalf("invoke: %s", body)
	}

	v2 := publish(t, sp, "forecast", "models")
	_, latest, _ := invoke(t, sp, "forecast/invoke", map[string]any{"region": "x"})
	_, pinned, _ := invoke(t, sp, "forecast/versions/1/invoke", map[string]any{"region": "x"})
	var lo, po struct{ Model string }
	_ = json.Unmarshal(latest.Output, &lo)
	_ = json.Unmarshal(pinned.Output, &po)
	ok = v2 == 2 && latest.Version == 2 && lo.Model == "m2" && pinned.Version == 1 && po.Model == "m1"
	finding(t, "function-immutable", "edit the model file, publish v2, call latest and v1", "latest sees m2; v1 still sees m1",
		fmt.Sprintf("latest v%d %q; v1 %q", latest.Version, lo.Model, po.Model), ok)
	if !ok {
		t.Fatalf("immutability: %+v %+v", latest, pinned)
	}
}

// Contracts and errors: bad input never runs; failures come back as an error
// object with the reasons.
func TestFunctionErrors(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"forecast_fn.ipynb": ""})
	upload(t, "alice", sp, "models", map[string]string{"model.txt": "m1\n"})
	publish(t, sp, "forecast", "models")

	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/forecast/invoke", "alice",
		map[string]any{"parameters": map[string]any{"cfg": map[string]any{"months": 99}}}, nil)
	var e struct {
		Error struct {
			Code     string
			Problems []struct{ Pointer string }
		}
	}
	r.json(t, &e)
	var runsList struct{ Items []any }
	call(t, "GET", "/v1/spaces/"+sp+"/runs", "alice", nil, nil).json(t, &runsList)
	okInput := r.Code == http.StatusBadRequest && e.Error.Code == "invalid_parameters" && len(e.Error.Problems) == 2 && len(runsList.Items) == 0

	_, failed, _ := invoke(t, sp, "forecast/invoke", map[string]any{"region": "x", "fail": true})
	okFail := failed.Status == "failed" && failed.Output == nil && failed.Error != nil && failed.Error.Kind == "execution_error" &&
		failed.Error.ExceptionType == "ValueError" && failed.Error.CellIndex != nil && *failed.Error.CellIndex == 1 && !failed.Error.Retryable
	_, bad, _ := invoke(t, sp, "forecast/invoke", map[string]any{"region": "x", "bad_output": true})
	okOutput := bad.Status == "failed" && bad.Error != nil && bad.Error.Kind == "output_invalid"
	finding(t, "function-errors", "invalid input; a notebook that raises; an output that breaks the contract",
		"400 invalid_parameters, no run; failed with cell and exception; failed output_invalid",
		fmt.Sprintf("input %d %s (%d problems, %d runs); raise %s/%v; output %s/%v", r.Code, e.Error.Code, len(e.Error.Problems),
			len(runsList.Items), failed.Status, failed.Error, bad.Status, bad.Error), okInput && okFail && okOutput)
	if !okInput || !okFail || !okOutput {
		t.Fatalf("input %v %s; fail %+v; output %+v", okInput, r.Body, failed.Error, bad.Error)
	}
}

// writer is a notebook that writes a file into its working directory.
const writer = `{"nbformat": 4, "nbformat_minor": 5, "metadata": {}, "cells": [
 {"cell_type": "code", "metadata": {}, "outputs": [], "source": "import time\nopen('written.txt', 'w').write('x')\ntime.sleep(3)\nimport nbsandbox\nnbsandbox.output('ok')"}]}`

// Invocations see their bundle read-only, so several run at once.
func TestInvocationsReadOnlyAndParallel(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"writer.ipynb": writer, "forecast_fn.ipynb": ""})
	upload(t, "alice", sp, "models", map[string]string{"model.txt": "m1\n"})
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/writer/versions", "alice", map[string]any{"notebook": "writer.ipynb"}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("publish writer: %d %s", r.Code, r.Body)
	}
	_, inv, _ := invoke(t, sp, "writer/invoke", map[string]any{})
	run := startRun(t, "alice", sp, "writer.ipynb", nil)
	run = waitRun(t, "alice", sp, run.ID, 2*time.Minute)
	files := string(call(t, "GET", "/v1/spaces/"+sp+"/files", "alice", nil, nil).Body)
	okRO := inv.Status == "failed" && inv.Error != nil && strings.Contains(inv.Error.Message, "Read-only") &&
		run.Status == "succeeded" && strings.Contains(files, "written.txt")
	finding(t, "read-only-invocation", "a function writes into /space; the same notebook as a normal run",
		"invocation fails on the read-only file system; the normal run writes the file",
		fmt.Sprintf("invocation %s (%v); run %s; file listed %v", inv.Status, inv.Error, run.Status, strings.Contains(files, "written.txt")), okRO)

	// Two invocations at once, beside nothing else: both run together.
	publish(t, sp, "forecast", "models")
	start := time.Now()
	results := make(chan callerResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/forecast/invoke?wait=50s", "alice",
				map[string]any{"parameters": map[string]any{"region": "p"}}, nil)
			var res callerResult
			_ = json.Unmarshal(r.Body, &res)
			if r.Code == http.StatusAccepted { // still going when the wait ended: keep waiting
				var run struct{ ID string }
				_ = json.Unmarshal(r.Body, &run)
				res = waitResult(t, sp, run.ID)
			}
			results <- res
		}()
	}
	a, b := <-results, <-results
	var list struct {
		Items []struct {
			StartedAt  *time.Time `json:"started_at"`
			FinishedAt *time.Time `json:"finished_at"`
		}
	}
	call(t, "GET", "/v1/spaces/"+sp+"/runs?limit=2", "alice", nil, nil).json(t, &list)
	overlap := len(list.Items) == 2 && list.Items[0].StartedAt != nil && list.Items[1].FinishedAt != nil &&
		list.Items[0].StartedAt.Before(*list.Items[1].FinishedAt)
	okPar := a.Status == "succeeded" && b.Status == "succeeded" && overlap
	finding(t, "parallel-invocations", "two invocations of one space at once", "both succeed, and their runs overlap in time",
		fmt.Sprintf("%s, %s in %.1fs; overlap %v", a.Status, b.Status, time.Since(start).Seconds(), overlap), okPar)
	if !okRO || !okPar {
		t.Fatalf("read-only %v (%+v / %s), parallel %v", okRO, inv, run.Status, okPar)
	}
}

// The space's status stream reports an invocation from queued to its result,
// and a reconnect resumes after the last event seen.
func TestSpaceStatusStream(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"forecast_fn.ipynb": ""})
	upload(t, "alice", sp, "models", map[string]string{"model.txt": "m1\n"})
	publish(t, sp, "forecast", "models")

	read := func(after string) ([]string, string) {
		req, _ := http.NewRequest("GET", baseURL+"/v1/spaces/"+sp+"/events?function=forecast", nil)
		req.SetBasicAuth("alice", passwords["alice"])
		if after != "" {
			req.Header.Set("Last-Event-ID", after)
		}
		resp, err := streamClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var statuses []string
		var lastID string
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				lastID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				var ev struct{ Status string }
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev)
				statuses = append(statuses, ev.Status)
				if ev.Status != "queued" && ev.Status != "running" {
					return statuses, lastID
				}
				if after == "" && len(statuses) == 1 {
					return statuses, lastID // drop the connection after the first event
				}
			}
		}
		return statuses, lastID
	}
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/forecast/invoke", "alice",
		map[string]any{"parameters": map[string]any{"region": "s"}}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("invoke: %d %s", r.Code, r.Body)
	}
	first, id := read("")
	rest, _ := read(id)
	all := append(first, rest...)
	ok := strings.Join(all, ",") == "queued,running,succeeded"
	finding(t, "status-stream", "follow the space's status stream, drop the connection after one event, resume",
		"queued, running, succeeded: each once, none lost", strings.Join(all, ","), ok)
	if !ok {
		t.Fatalf("events %v", all)
	}
}

// Start-up latency baseline: submit to first cell, for the roadmap's fast
// starts. Report-only.
func TestStartLatency(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"forecast_fn.ipynb": ""})
	upload(t, "alice", sp, "models", map[string]string{"model.txt": "m1\n"})
	publish(t, sp, "forecast", "models")
	var samples []time.Duration
	for i := 0; i < 3; i++ {
		start := time.Now()
		code, res, _ := invoke(t, sp, "forecast/invoke", map[string]any{"region": "l"})
		if code != http.StatusOK || res.Status != "succeeded" {
			t.Fatalf("invoke %d: %s", code, res.Status)
		}
		samples = append(samples, time.Since(start))
	}
	finding(t, "start-latency", "three small invocations, request to result", "recorded (target: under 1 s of start-up; see the roadmap)",
		fmt.Sprintf("%v", samples), true)
}

// waitResult long-polls a run's result until it has finished.
func waitResult(t *testing.T, sp, runID string) callerResult {
	t.Helper()
	deadline := time.Now().Add(runTimeout + 2*time.Minute)
	for time.Now().Before(deadline) {
		r := call(t, "GET", "/v1/spaces/"+sp+"/runs/"+runID+"/result?wait=50s", "alice", nil, nil)
		if r.Code == http.StatusOK {
			var res callerResult
			_ = json.Unmarshal(r.Body, &res)
			return res
		}
	}
	t.Errorf("run %s didn't finish", runID)
	return callerResult{}
}
