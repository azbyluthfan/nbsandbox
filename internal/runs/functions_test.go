// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// recordingRun remembers the spec of each run and reads model.txt from the
// folder mounted at /space, the way a notebook would.
type recordingRun struct {
	mu    sync.Mutex
	specs map[string]isolation.RunSpec
	seen  map[string]string
}

func (rr *recordingRun) run(_ context.Context, spec isolation.RunSpec, _ isolation.LineSink) (isolation.RunResult, error) {
	b, _ := os.ReadFile(filepath.Join(spec.SpaceDir, "models", "model.txt"))
	rr.mu.Lock()
	rr.specs[spec.RunID], rr.seen[spec.RunID] = spec, string(b)
	rr.mu.Unlock()
	_ = os.WriteFile(filepath.Join(spec.RunDir, "output.json"), []byte(`{"forecast": [1], "model": "`+strings.TrimSpace(string(b))+`"}`), 0o600)
	writeStatus(spec, "succeeded", nil)
	return isolation.RunResult{}, nil
}

func TestPublishAndInvoke(t *testing.T) {
	e := newEnv(t)
	rr := &recordingRun{specs: map[string]isolation.RunSpec{}, seen: map[string]string{}}
	e.be.behave = rr.run
	ctx := context.Background()
	sid := e.space(t, "alice")
	_, _ = e.sp.WriteFile(ctx, "alice", sid, spaces.WriteRequest{Path: "models/model.txt", Body: strings.NewReader("v1\n")})
	e.upload(t, "alice", sid, "forecast.ipynb", nbContract)

	v1, err := e.m.Publish(ctx, "alice", sid, "forecast", PublishRequest{Notebook: "forecast.ipynb", Files: []string{"models"}})
	if err != nil || v1.Version != 1 || v1.Contract == nil || string(v1.Contract.Input) == "null" || v1.BundleBytes == 0 {
		t.Fatalf("publish v1: %+v %v", v1, err)
	}
	// The space changes; v1 must not.
	f, _ := e.sp.Stat(ctx, "alice", sid, "models/model.txt")
	_, _ = e.sp.WriteFile(ctx, "alice", sid, spaces.WriteRequest{Path: "models/model.txt", Body: strings.NewReader("v2\n"), IfMatch: f.Version})
	v2, err := e.m.Publish(ctx, "alice", sid, "forecast", PublishRequest{Notebook: "forecast.ipynb", Files: []string{"models/model.txt"}})
	if err != nil || v2.Version != 2 {
		t.Fatalf("publish v2: %+v %v", v2, err)
	}

	call := func(version int, params string) View {
		t.Helper()
		v, _, err := e.m.Invoke(ctx, "alice", sid, "forecast", InvokeRequest{Version: version, Parameters: json.RawMessage(params)})
		if err != nil {
			t.Fatal(err)
		}
		return e.wait(t, "alice", sid, v.ID, true) //nolint:contextcheck // the helper polls with its own context
	}
	old, latest := call(1, `{"region": "jakarta"}`), call(0, `{"region": "bandung"}`)
	if rr.seen[old.ID] != "v1\n" || rr.seen[latest.ID] != "v2\n" {
		t.Fatalf("v1 saw %q, latest saw %q", rr.seen[old.ID], rr.seen[latest.ID])
	}
	if old.Function != "forecast" || old.Version != 1 || latest.Version != 2 || !old.ReadOnlySpace {
		t.Fatalf("run views %+v %+v", old, latest)
	}
	spec := rr.specs[old.ID]
	if !spec.SpaceReadOnly || spec.SpaceDir != e.sp.WorkPath(sid, old.ID) {
		t.Fatalf("invocation spec %+v", spec)
	}
	res, _ := e.m.GetResult(ctx, "alice", sid, old.ID)
	if string(res.Output) != `{"forecast":[1],"model":"v1"}` || res.Function != "forecast" || res.Version != 1 {
		t.Fatalf("result %+v %s", res, res.Output)
	}

	// The input contract applies to invocations; bad input never queues.
	var ce *contract.Error
	if _, _, err := e.m.Invoke(ctx, "alice", sid, "forecast", InvokeRequest{Parameters: json.RawMessage(`{"region": 5}`)}); !errors.As(err, &ce) {
		t.Fatalf("contract not applied: %v", err)
	}
	if _, _, err := e.m.Invoke(ctx, "alice", sid, "forecast", InvokeRequest{Version: 9}); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatalf("missing version: %v", err)
	}
	if _, _, err := e.m.Invoke(ctx, "bob", sid, "forecast", InvokeRequest{}); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatalf("bob invokes: %v", err)
	}

	fn, err := e.m.Function(ctx, "alice", sid, "forecast")
	if err != nil || fn.LatestVersion != 2 || len(fn.Versions) != 2 || fn.Versions[0].Version != 2 {
		t.Fatalf("function %+v %v", fn, err)
	}
	if list, _ := e.m.Functions(ctx, "alice", sid); len(list) != 1 {
		t.Fatalf("functions %+v", list)
	}
	runs, _, _ := e.m.List(ctx, "alice", sid, storeFilter("forecast"), "", 50)
	if len(runs) != 2 {
		t.Fatalf("invocations listed: %d", len(runs))
	}
}

func TestPublishValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sid := e.space(t, "alice")
	var ie *spaces.InvalidError
	for name, req := range map[string]struct {
		fn  string
		req PublishRequest
	}{
		"bad name":         {"Forecast!", PublishRequest{Notebook: "nb.ipynb"}},
		"missing notebook": {"f", PublishRequest{Notebook: "nope.ipynb"}},
		"missing file":     {"f", PublishRequest{Notebook: "nb.ipynb", Files: []string{"nope.csv"}}},
		"traversal":        {"f", PublishRequest{Notebook: "nb.ipynb", Files: []string{"../x"}}},
	} {
		var pe *spaces.PathError
		if _, err := e.m.Publish(ctx, "alice", sid, req.fn, req.req); !errors.As(err, &ie) && !errors.As(err, &pe) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if fns, _ := e.m.Functions(ctx, "alice", sid); len(fns) != 0 {
		t.Fatalf("a failed publish created a function: %+v", fns)
	}
}

// Invocations see the space read-only, so they may run while another
// invocation, or a normal run that writes the space, is active.
func TestInvocationsRunInParallel(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Concurrency = 3 })
	release := make(chan struct{})
	var running sync.WaitGroup
	running.Add(3)
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, _ isolation.LineSink) (isolation.RunResult, error) {
		running.Done()
		<-release
		writeStatus(spec, "succeeded", nil)
		return isolation.RunResult{}, nil
	}
	ctx := context.Background()
	sid := e.space(t, "alice")
	if _, err := e.m.Publish(ctx, "alice", sid, "fn", PublishRequest{Notebook: "nb.ipynb"}); err != nil {
		t.Fatal(err)
	}
	writer := e.submit(t, "alice", sid, "")
	a, _, errA := e.m.Invoke(ctx, "alice", sid, "fn", InvokeRequest{})
	b, _, errB := e.m.Invoke(ctx, "alice", sid, "fn", InvokeRequest{})
	if errA != nil || errB != nil {
		t.Fatalf("invocations refused while another run was active: %v %v", errA, errB)
	}
	// A second writing run is still refused.
	if _, _, err := e.m.Submit(ctx, "alice", sid, SubmitRequest{Notebook: "nb.ipynb"}); !errors.Is(err, ErrSpaceBusy) {
		t.Fatalf("second writer: %v", err)
	}
	done := make(chan struct{})
	go func() { running.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the three runs didn't run at the same time")
	}
	// A function with active invocations can't be deleted.
	if err := e.m.DeleteFunction(ctx, "alice", sid, "fn"); !errors.Is(err, ErrFunctionBusy) {
		t.Fatalf("delete while active: %v", err)
	}
	close(release)
	for _, id := range []string{writer.ID, a.ID, b.ID} {
		e.wait(t, "alice", sid, id, true)
	}
	if err := e.m.DeleteFunction(ctx, "alice", sid, "fn"); err != nil {
		t.Fatal(err)
	}
	if fns, _ := e.m.Functions(ctx, "alice", sid); len(fns) != 0 {
		t.Fatalf("function left after delete: %+v", fns)
	}
	if v, err := e.m.Get(ctx, "alice", sid, a.ID); err != nil || v.Function != "fn" {
		t.Fatalf("past invocations must stay: %+v %v", v, err)
	}
}

func storeFilter(function string) ListFilter { return ListFilter{Function: function} }
