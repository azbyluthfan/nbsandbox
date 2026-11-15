// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
)

// fakeAPI answers the calls Load and the space helpers make.
type fakeAPI struct {
	mu      sync.Mutex
	os      string
	auth    []string
	deleted []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/runtime":
		_ = json.NewEncoder(w).Encode(map[string]any{"runtime": "runsc", "packages": map[string]string{"pandas": "2"},
			"limits": map[string]any{"timeout_seconds": 90, "log_bytes": 1024}, "docker": map[string]any{"operating_system": f.os}})
	case r.Method == "POST" && r.URL.Path == "/v1/spaces":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"s1"}`))
	case r.Method == "POST" && r.URL.Path == "/v1/spaces/s1/files":
		w.WriteHeader(http.StatusCreated)
	case r.Method == "GET" && r.URL.Path == "/v1/spaces/s1/runs":
		_, _ = w.Write([]byte(`{"items":[]}`))
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/spaces/"):
		f.mu.Lock()
		f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/v1/spaces/"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func newEnv(t *testing.T, f *fakeAPI) *Env {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Env{Client: &Client{BaseURL: srv.URL + "/", HTTP: srv.Client()},
		A: &Identity{Name: "alice", Password: "pw"}, B: &Identity{Name: "bob", Token: "tok"}}
}

func TestLoadReadsLimitsAndPlatform(t *testing.T) {
	for _, tc := range []struct {
		os   string
		kube bool
	}{{"Kubernetes linux/amd64", true}, {"Docker Desktop", false}} {
		f := &fakeAPI{os: tc.os}
		e := newEnv(t, f)
		if err := e.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		if e.Kubernetes != tc.kube || e.Runtime != "runsc" || e.RunTimeout != 90*time.Second || e.LogBytes != 1024 {
			t.Errorf("%s: %+v", tc.os, e)
		}
		if _, ok := e.Info["packages"]; ok {
			t.Error("the package list belongs in the report's facts")
		}
		// alice by password, bob by token: both asked once.
		if len(f.auth) != 2 || !strings.HasPrefix(f.auth[0], "Basic ") || f.auth[1] != "Bearer tok" {
			t.Errorf("auth headers %q", f.auth)
		}
	}
}

func TestCleanupDeletesSpacesEvenAfterCancel(t *testing.T) {
	f := &fakeAPI{}
	e := newEnv(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	id, err := e.space(ctx, e.A, map[string][]byte{"a.ipynb": []byte("{}")})
	if err != nil || id != "s1" {
		t.Fatal(id, err)
	}
	cancel() // Ctrl-C
	e.Cleanup(ctx)
	if len(f.deleted) != 1 || f.deleted[0] != "s1" {
		t.Errorf("deleted %v", f.deleted)
	}
	e.Cleanup(context.Background()) // nothing left to do
	if len(f.deleted) != 1 {
		t.Errorf("deleted twice: %v", f.deleted)
	}
}

func TestExpect(t *testing.T) {
	if _, err := apiclient.Expect(Response{Code: 201}, nil)(201, "x"); err != nil {
		t.Error(err)
	}
	if _, err := apiclient.Expect(Response{Code: 409, Body: []byte("busy")}, nil)(201, "start run"); err == nil || !strings.Contains(err.Error(), "start run: 409 busy") {
		t.Errorf("got %v", err)
	}
}

func TestEveryCheckIsNamedAndNotebooksExist(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range All() {
		if c.Name == "" || len(c.Findings) == 0 || c.run == nil || seen[c.Name] || Get(c.Name) != c {
			t.Errorf("check %+v", c)
		}
		seen[c.Name] = true
		for _, id := range c.Findings {
			if _, dup := NotChecked[id]; dup {
				t.Errorf("%s is both checked and not checked", id)
			}
		}
	}
	for _, nb := range []string{"smoke.ipynb", "oom.ipynb", "cpu_fixed.ipynb", "cpu_hog.ipynb", "pids.ipynb", "tmpfill.ipynb",
		"endless_print.ipynb", "infinite.ipynb", "long.ipynb", "crash.ipynb", "fail_cell3.ipynb", "render_fail.ipynb"} {
		if len(workload(nb)) == 0 {
			t.Errorf("workload %s empty", nb)
		}
	}
	for _, nb := range []string{"holder.ipynb", "isolation_probe.ipynb"} {
		if len(probe(nb)) == 0 {
			t.Errorf("probe %s empty", nb)
		}
	}
}
