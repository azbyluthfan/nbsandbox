// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// fakeData binds "crm" in dev (the default) and prod, in every space.
type fakeData struct {
	mu   sync.Mutex
	gone bool // the connections were deleted after submission
	got  map[string]string
}

func (f *fakeData) Resolve(_ context.Context, _, _, env string, _ []string) (string, map[string]string, error) {
	switch env {
	case "", "dev":
		return "dev", map[string]string{"crm": "conn-dev"}, nil
	case "prod":
		return "prod", map[string]string{"crm": "conn-prod"}, nil
	}
	return "", nil, &spaces.InvalidError{Msg: "no such environment"}
}

// Declared: the space declares crm, a postgresql name.
func (f *fakeData) Declared(context.Context, string) (map[string]string, error) {
	return map[string]string{"crm": "postgresql"}, nil
}

// Types: conn-prod is a postgresql connection; anything else mysql.
func (f *fakeData) Types(_ context.Context, _ string, names map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for n, id := range names {
		out[n] = "mysql"
		if id == "conn-prod" || id == "conn-dev" {
			out[n] = "postgresql"
		}
	}
	return out, nil
}

func (f *fakeData) Fetcher(_ context.Context, runID, _ string, names map[string]string, _ time.Time) (*isolation.Fetcher, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone {
		return nil, errors.New(`"crm"'s connection no longer exists`)
	}
	f.got = names
	return &isolation.Fetcher{Image: "fetcher:test", Token: "tok-" + runID}, nil
}

func TestRunsReadConnections(t *testing.T) {
	ctx := context.Background()
	data := &fakeData{}
	e := newEnvWith(t, func(d *Deps, _ *store.DB, _ string) { d.Data = data })
	var mu sync.Mutex
	var fetchers []*isolation.Fetcher
	ok := succeed(time.Millisecond)
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		mu.Lock()
		fetchers = append(fetchers, spec.Fetcher)
		mu.Unlock()
		return ok(ctx, spec, out)
	}
	sp := e.space(t, "alice")

	v, _, err := e.m.Submit(ctx, "alice", sp, SubmitRequest{Notebook: "nb.ipynb", Environment: "prod"})
	if err != nil || v.Environment != "prod" {
		t.Fatalf("submit: %+v %v", v, err)
	}
	if got := e.wait(t, "alice", sp, v.ID, true); got.Status != StatusSucceeded {
		t.Fatalf("run: %+v", got)
	}
	if names, _ := e.db.RunConnections(ctx, v.ID); names["crm"] != "conn-prod" {
		t.Fatalf("recorded: %v", names)
	}
	data.mu.Lock()
	gotNames := data.got
	data.mu.Unlock()
	if gotNames["crm"] != "conn-prod" || len(fetchers) != 1 || fetchers[0] == nil || fetchers[0].Token != "tok-"+v.ID {
		t.Fatalf("fetcher: %v %+v", gotNames, fetchers)
	}

	// The default environment.
	v = e.submit(t, "alice", sp, "")
	if v.Environment != "dev" {
		t.Fatalf("default: %+v", v)
	}
	e.wait(t, "alice", sp, v.ID, true)

	// An unknown environment is the caller's mistake; nothing is queued.
	var inv *spaces.InvalidError
	if _, _, err := e.m.Submit(ctx, "alice", sp, SubmitRequest{Notebook: "nb.ipynb", Environment: "staging"}); !errors.As(err, &inv) {
		t.Fatalf("unknown environment: %v", err)
	}

	// A connection deleted while the run waited: it fails at start, and says why.
	data.mu.Lock()
	data.gone = true
	data.mu.Unlock()
	v = e.submit(t, "alice", sp, "")
	got := e.wait(t, "alice", sp, v.ID, true)
	if got.Status != StatusPlatformError || !strings.Contains(got.Message, "no longer exists") {
		t.Fatalf("connection gone: %+v", got)
	}
}

func TestEnvironmentWithoutConnections(t *testing.T) {
	e := newEnv(t)
	sp := e.space(t, "alice")
	var inv *spaces.InvalidError
	if _, _, err := e.m.Submit(context.Background(), "alice", sp, SubmitRequest{Notebook: "nb.ipynb", Environment: "prod"}); !errors.As(err, &inv) {
		t.Fatalf("environment with connections off: %v", err)
	}
	v := e.submit(t, "alice", sp, "")
	if v.Environment != "" {
		t.Fatalf("environment: %+v", v)
	}
}

// A published version declares the names its space binds, and their
// types; an invocation reads only those, and only if its environment binds
// them alike.
func TestFunctionsReadDeclaredConnections(t *testing.T) {
	ctx := context.Background()
	data := &fakeData{}
	e := newEnvWith(t, func(d *Deps, _ *store.DB, _ string) { d.Data = data })
	sid := e.space(t, "alice")
	v, err := e.m.Publish(ctx, "alice", sid, "fn", PublishRequest{Notebook: "nb.ipynb"})
	if err != nil || v.Connections["crm"] != "postgresql" {
		t.Fatalf("publish: %+v %v", v, err)
	}
	run, _, err := e.m.Invoke(ctx, "alice", sid, "fn", InvokeRequest{Environment: "prod"})
	if err != nil || run.Environment != "prod" {
		t.Fatalf("invoke: %+v %v", run, err)
	}
	e.wait(t, "alice", sid, run.ID, true)
	if names, _ := e.db.RunConnections(ctx, run.ID); names["crm"] != "conn-prod" {
		t.Fatalf("recorded: %v", names)
	}
	var inv *spaces.InvalidError
	if _, _, err := e.m.Invoke(ctx, "alice", sid, "fn", InvokeRequest{Environment: "staging"}); !errors.As(err, &inv) {
		t.Fatalf("unknown environment: %v", err)
	}
}

func TestFunctionNeedsMatchingConnections(t *testing.T) {
	ctx := context.Background()
	e := newEnvWith(t, func(d *Deps, _ *store.DB, _ string) { d.Data = &fakeData{} })
	sid := e.space(t, "alice")
	cases := map[string]map[string]string{
		"not bound":    {"billing": "postgresql"},
		"another type": {"crm": "sqlserver"},
	}
	for name, declared := range cases {
		_, _, err := e.m.resolveData(ctx, "alice", sid, SubmitRequest{}, submission{function: "fn", connections: declared})
		var inv *spaces.InvalidError
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), "fn needs connections") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A function that reads nothing takes no environment.
	if _, _, err := e.m.resolveData(ctx, "alice", sid, SubmitRequest{Environment: "dev"}, submission{function: "fn"}); err == nil {
		t.Error("an environment for a function that reads nothing")
	}
	// Connections off: a function that reads some can't run.
	off := newEnv(t)
	if _, _, err := off.m.resolveData(ctx, "alice", sid, SubmitRequest{}, submission{function: "fn", connections: map[string]string{"crm": "postgresql"}}); err == nil {
		t.Error("ran with connections off")
	}
}
