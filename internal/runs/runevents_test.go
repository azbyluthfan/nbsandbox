// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// streamNB returns items with nbsandbox.emit; meta is its metadata.
func streamNB(meta string) string {
	if meta == "" {
		meta = "{}"
	}
	return `{"nbformat": 4, "nbformat_minor": 5, "metadata": ` + meta + `, "cells": [
 {"cell_type": "code", "source": "import nbsandbox\nfor r in regions:\n    nbsandbox.emit(r)", "metadata": {}, "outputs": []}]}`
}

const singleNB = `{"nbformat": 4, "nbformat_minor": 5, "metadata": {}, "cells": [
 {"cell_type": "code", "source": "import nbsandbox\nnbsandbox.output(1)", "metadata": {}, "outputs": []}]}`

// reporting is a run that appends lines to its events file, a little apart,
// then ends with state (succeeded, or a failure the runner reports).
func reporting(state string, lines ...string) func(context.Context, isolation.RunSpec, isolation.LineSink) (isolation.RunResult, error) {
	return func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		f, err := os.OpenFile(filepath.Join(spec.RunDir, RunEventsFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return isolation.RunResult{}, err
		}
		for _, l := range lines {
			_, _ = f.WriteString(l)
			select {
			case <-time.After(60 * time.Millisecond):
			case <-ctx.Done():
				_ = f.Close()
				return isolation.RunResult{Stopped: true, ExitCode: 143}, nil
			}
		}
		_ = f.Close()
		writeOutputNotebook(spec)
		if state == "succeeded" {
			writeStatus(spec, "succeeded", nil)
			return isolation.RunResult{}, nil
		}
		writeStatus(spec, state, map[string]any{"error": map[string]any{"cell_index": 0, "execution_count": 1, "ename": "ValueError", "evalue": "boom"}})
		return isolation.RunResult{ExitCode: 10}, nil
	}
}

func item(v string) string { return `{"kind":"item","item":` + v + `}` + "\n" }

func (e *env) submitNB(t *testing.T, sid, name, body string) View {
	t.Helper()
	e.upload(t, "alice", sid, name, body)
	v, _, err := e.m.Submit(context.Background(), "alice", sid, SubmitRequest{Notebook: name})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func collectResult(t *testing.T, ch <-chan ResultEvent) (items []int, kinds []string, res *CallerResult) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return items, kinds, res
			}
			kinds = append(kinds, ev.Kind)
			switch ev.Kind {
			case "item":
				items = append(items, ev.Index)
			case "result":
				res = ev.Result
			}
		case <-timeout:
			t.Fatal("result stream didn't end")
		}
	}
}

func TestStreamedOutput(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.be.behave = reporting("succeeded",
		`{"kind":"cell","cell_index":0,"state":"started"}`+"\n",
		item(`{"region":"jakarta","total":1200}`),
		`{"kind":"progress","fraction":0.5,"message":"half"}`+"\n",
		`{"kind":"item","it`, `em":[1,2],"total":3}`+"\n", // a line written in two pieces, with a total
		item(`"third"`),
		`{"kind":"cell","cell_index":0,"state":"finished","outcome":"ok","duration_seconds":0.1}`+"\n",
	)
	sid := e.space(t, "alice")
	v := e.submitNB(t, sid, "report.ipynb", streamNB(""))

	// Live, with progress.
	ch, err := e.m.FollowResult(ctx, "alice", sid, v.ID, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	items, kinds, res := collectResult(t, ch)
	if fmt.Sprint(items) != "[1 2 3]" || strings.Join(kinds, ",") != "item,progress,item,item,result" {
		t.Fatalf("items %v kinds %v", items, kinds)
	}
	want := `[{"region":"jakarta","total":1200},[1,2],"third"]`
	if res.Status != StatusSucceeded || string(res.Output) != want || res.Items == nil || *res.Items != 3 || res.Partial {
		t.Fatalf("result %+v %s", res, res.Output)
	}
	// The same as JSON.
	if got, _ := e.m.GetResult(ctx, "alice", sid, v.ID); string(got.Output) != want {
		t.Fatalf("JSON output %s", got.Output)
	}
	// Replayed after the run, resuming after item 2, without progress.
	ch, _ = e.m.FollowResult(ctx, "alice", sid, v.ID, 2, false)
	if items, kinds, res := collectResult(t, ch); fmt.Sprint(items) != "[3]" || strings.Join(kinds, ",") != "item,result" || res == nil {
		t.Fatalf("replay %v %v", items, kinds)
	}
	// The status stream has only status changes, unless asked.
	evs, _ := e.m.FollowEvents(ctx, "alice", sid, EventsQuery{RunID: v.ID})
	var types []string
	for it := range evs {
		types = append(types, it.Type)
	}
	if strings.Join(types, ",") != "status,status,status" {
		t.Fatalf("default events %v", types)
	}
	evs, _ = e.m.FollowEvents(ctx, "alice", sid, EventsQuery{RunID: v.ID, Include: map[string]bool{"progress": true, "cell": true}})
	types = nil
	for it := range evs {
		types = append(types, it.Type)
	}
	if strings.Join(types, ",") != "status,status,cell,progress,cell,status" {
		t.Fatalf("events with progress and cells %v", types)
	}
	if _, err := e.m.FollowEvents(ctx, "alice", sid, EventsQuery{Include: map[string]bool{"item": true}}); err == nil {
		t.Fatal("items offered on the events stream")
	}
	if _, err := e.m.FollowResult(ctx, "bob", sid, v.ID, 0, false); err == nil {
		t.Fatal("bob followed alice's result")
	}
	// The events file isn't kept as an output.
	if _, _, err := e.m.OpenOutput(ctx, "alice", sid, v.ID, RunEventsFile); err == nil {
		t.Fatal("events file stored")
	}
	// A single-value run's stream: just the result.
	e.be.behave = succeed(10 * time.Millisecond)
	s := e.submitNB(t, sid, "single.ipynb", singleNB)
	ch, _ = e.m.FollowResult(ctx, "alice", sid, s.ID, 0, false)
	if items, kinds, res := collectResult(t, ch); len(items) != 0 || strings.Join(kinds, ",") != "result" || res.Items != nil {
		t.Fatalf("single run's stream %v %+v", kinds, res)
	}
}

// A run that fails after streaming some items returns them, marked partial.
func TestStreamFailureKeepsItems(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.be.behave = reporting("execution_error", item(`1`), item(`2`))
	sid := e.space(t, "alice")
	v := e.submitNB(t, sid, "report.ipynb", streamNB(""))
	e.wait(t, "alice", sid, v.ID, true)
	res, err := e.m.GetResult(ctx, "alice", sid, v.ID)
	if err != nil || res.Status != StatusFailed || !res.Partial || string(res.Output) != `[1,2]` || *res.Items != 2 || res.Error == nil {
		t.Fatalf("failed stream %+v %s %v", res, res.Output, err)
	}
	ch, _ := e.m.FollowResult(ctx, "alice", sid, v.ID, 0, false)
	if items, _, r := collectResult(t, ch); fmt.Sprint(items) != "[1 2]" || !r.Partial {
		t.Fatalf("replayed %v %+v", items, r)
	}
	// A failure before any item: no output.
	e.be.behave = reporting("execution_error")
	v = e.submitNB(t, sid, "report.ipynb", streamNB(""))
	e.wait(t, "alice", sid, v.ID, true)
	if res, _ := e.m.GetResult(ctx, "alice", sid, v.ID); res.Partial || res.Output != nil || *res.Items != 0 {
		t.Fatalf("failed before any item %+v", res)
	}
}

// Items that break the contract or a limit fail the run (they are never
// dropped); the items before stay, marked partial.
func TestStreamContractBreaks(t *testing.T) {
	big := `"` + strings.Repeat("x", MaxRunEventBytes) + `"`
	var burst []string
	for i := 0; i < 3*ItemsPerSec; i++ {
		burst = append(burst, item(fmt.Sprint(i)))
	}
	schema := `{"nbsandbox": {"contract": {"output": {"mode": "stream", "item": {"type": "integer"}}}}}`
	for name, c := range map[string]struct {
		nb    string
		lines []string
		kind  string
		msg   string
	}{
		"emit in a single-value run": {singleNB, []string{item(`1`)}, KindOutputInvalid, "returns one value"},
		"item against its schema":    {streamNB(schema), []string{item(`1`), item(`"two"`)}, KindOutputInvalid, "item 2"},
		"item too large":             {streamNB(""), []string{item(`1`), `{"kind":"item","item":` + big + "}\n"}, KindOutputTooLarge, "64 KiB"},
		"unreadable line":            {streamNB(""), []string{item(`1`), "not json\n"}, KindOutputInvalid, "can't read"},
		"too fast":                   {streamNB(""), []string{strings.Join(burst, "")}, KindOutputInvalid, "faster than"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			e.be.behave = reporting("succeeded", append(c.lines, item(`99`))...)
			sid := e.space(t, "alice")
			v := e.submitNB(t, sid, "nb2.ipynb", c.nb)
			e.wait(t, "alice", sid, v.ID, true)
			res, _ := e.m.GetResult(ctx, "alice", sid, v.ID)
			if res.Status != StatusFailed || res.Error == nil || res.Error.Kind != c.kind || !strings.Contains(res.Error.Message, c.msg) {
				t.Fatalf("%+v %+v", res, res.Error)
			}
			if strings.Contains(string(res.Output), "99") {
				t.Fatalf("items after the break were kept: %s", res.Output)
			}
		})
	}
}

// A stream-mode notebook that calls nbsandbox.output fails at the end.
func TestStreamCallingOutputFails(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		_ = os.WriteFile(filepath.Join(spec.RunDir, "output.json"), []byte(`1`), 0o600)
		writeOutputNotebook(spec)
		writeStatus(spec, "succeeded", nil)
		return isolation.RunResult{}, nil
	}
	sid := e.space(t, "alice")
	v := e.submitNB(t, sid, "report.ipynb", streamNB(""))
	e.wait(t, "alice", sid, v.ID, true)
	if res, _ := e.m.GetResult(ctx, "alice", sid, v.ID); res.Status != StatusFailed || res.Error.Kind != KindOutputInvalid {
		t.Fatalf("%+v", res)
	}
}

// When another node ends a streamed run (its worker went away), the items
// recorded so far are its partial output.
func TestTakeoverKeepsItems(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(c *Config) { c.NoExecute = true })
	sid := e.space(t, "alice")
	v := e.submitNB(t, sid, "report.ipynb", streamNB(""))
	if _, err := e.db.MarkRunning(ctx, v.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		b, _ := json.Marshal(RunEvent{RunID: v.ID, Kind: store.EventItem, Index: i, Item: json.RawMessage(fmt.Sprint(i * 10))})
		if _, err := e.db.AddEvent(ctx, store.Event{SpaceID: sid, RunID: v.ID, Status: StatusRunning, Kind: store.EventItem,
			Data: string(b), CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	e.m.interruptIfRunning(ctx, v.ID, "the worker went away", store.Fence{})
	res, err := e.m.GetResult(ctx, "alice", sid, v.ID)
	if err != nil || res.Status != StatusInterrupted || !res.Partial || string(res.Output) != `[10,20]` || !res.Error.Retryable {
		t.Fatalf("%+v %s %v", res, res.Output, err)
	}
}

// A link planted as the events file is never followed.
func TestRunEventsIgnoreLinks(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	secret := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(secret, []byte(item(`"SECRET"`)), 0o600)
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		_ = os.Symlink(secret, filepath.Join(spec.RunDir, RunEventsFile))
		time.Sleep(2 * runEventsPoll)
		writeOutputNotebook(spec)
		writeStatus(spec, "succeeded", nil)
		return isolation.RunResult{}, nil
	}
	sid := e.space(t, "alice")
	v := e.submitNB(t, sid, "report.ipynb", streamNB(""))
	e.wait(t, "alice", sid, v.ID, true)
	if res, _ := e.m.GetResult(ctx, "alice", sid, v.ID); strings.Contains(string(res.Output), "SECRET") || *res.Items != 0 {
		t.Fatalf("read through a link: %+v", res)
	}
}

// The mode is settled before the run: a notebook calling both is refused;
// a contract given at publish replaces the notebook's.
func TestOutputModeAtSubmitAndPublish(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sid := e.space(t, "alice")
	both := `{"nbformat": 4, "nbformat_minor": 5, "metadata": {}, "cells": [
 {"cell_type": "code", "source": "import nbsandbox\nnbsandbox.emit(1)", "metadata": {}, "outputs": []},
 {"cell_type": "code", "source": "nbsandbox.output(2)", "metadata": {}, "outputs": []}]}`
	e.upload(t, "alice", sid, "both.ipynb", both)
	var ce *ContractError
	if _, _, err := e.m.Submit(ctx, "alice", sid, SubmitRequest{Notebook: "both.ipynb"}); !errors.As(err, &ce) || !strings.Contains(err.Error(), "cell 0") {
		t.Fatalf("both calls: %v", err)
	}
	e.upload(t, "alice", sid, "report.ipynb", streamNB(""))
	v, err := e.m.Publish(ctx, "alice", sid, "regions", PublishRequest{Notebook: "report.ipynb"})
	if err != nil || v.Contract.Mode != contract.ModeStream || !v.Contract.ModeInferred {
		t.Fatalf("inferred stream: %+v %v", v.Contract, err)
	}
	spec := contract.Spec{Input: json.RawMessage(`{"type":"object"}`), Output: json.RawMessage(`{"mode":"stream","item":{"type":"string"}}`)}
	v, err = e.m.Publish(ctx, "alice", sid, "regions", PublishRequest{Notebook: "report.ipynb", Contract: &spec})
	if err != nil || v.Contract.ModeInferred || string(v.Contract.Item) != `{"type":"string"}` || string(v.Contract.Output) != "null" {
		t.Fatalf("declared stream: %+v %v", v.Contract, err)
	}
	bad := contract.Spec{Output: json.RawMessage(`{"mode":"single","schema":{}}`)}
	if _, err := e.m.Publish(ctx, "alice", sid, "regions", PublishRequest{Notebook: "report.ipynb", Contract: &bad}); !errors.As(err, &ce) {
		t.Fatalf("declared single, notebook emits: %v", err)
	}
	// An invocation of a stream function streams.
	e.be.behave = reporting("succeeded", item(`"jakarta"`))
	run, _, err := e.m.Invoke(ctx, "alice", sid, "regions", InvokeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	e.wait(t, "alice", sid, run.ID, true)
	if res, _ := e.m.GetResult(ctx, "alice", sid, run.ID); string(res.Output) != `["jakarta"]` {
		t.Fatalf("invocation %+v", res)
	}

	strict := newEnv(t, func(c *Config) { c.RequireSchemas = true })
	sid2 := strict.space(t, "alice")
	strict.upload(t, "alice", sid2, "report.ipynb", streamNB(""))
	if _, err := strict.m.Publish(ctx, "alice", sid2, "f", PublishRequest{Notebook: "report.ipynb"}); !errors.As(err, &ce) {
		t.Fatalf("require_schemas: %v", err)
	}
	if _, err := strict.m.Publish(ctx, "alice", sid2, "f", PublishRequest{Notebook: "report.ipynb", Contract: &spec}); err != nil {
		t.Fatalf("with schemas: %v", err)
	}
}

func TestParseRunEvent(t *testing.T) {
	for line, ok := range map[string]bool{
		`{"kind":"item","item":null}`:                                      true,
		`{"kind":"item","item":{"a":1},"total":3}`:                         true,
		`{"kind":"item","item":1,"total":0}`:                               false,
		`{"kind":"item"}`:                                                  false,
		`{"kind":"progress","fraction":0}`:                                 true,
		`{"kind":"progress","fraction":1,"message":"done"}`:                true,
		`{"kind":"progress","fraction":-0.1}`:                              false,
		`{"kind":"progress"}`:                                              false,
		`{"kind":"cell","cell_index":2,"state":"started"}`:                 true,
		`{"kind":"cell","cell_index":2,"state":"finished","outcome":"ok"}`: true,
		`{"kind":"cell","cell_index":2,"state":"finished"}`:                false,
		`{"kind":"cell","cell_index":-1,"state":"started"}`:                false,
		`{"kind":"cell","state":"started"}`:                                false,
		`{"kind":"status","status":"succeeded"}`:                           false,
		`[1,2]`:                                                            false,
	} {
		if _, err := parseRunEvent([]byte(line)); (err == nil) != ok {
			t.Errorf("%s: %v", line, err)
		}
	}
	ev, _ := parseRunEvent([]byte(`{"kind":"progress","fraction":0.5,"message":"` + strings.Repeat("m", 2000) + `"}`))
	if len(ev.Message) != 1000 {
		t.Fatalf("message not capped: %d", len(ev.Message))
	}
}
