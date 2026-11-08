// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDaemon answers the few Docker API endpoints the adapter uses, so the
// adapter (and API version negotiation) is tested without Docker.
func fakeDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.56")
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if i := strings.Index(path[1:], "/"); strings.HasPrefix(path, "/v1.") && i > 0 {
			path = path[i+1:] // strip /v1.xx
		}
		switch {
		case path == "/_ping":
			_, _ = w.Write([]byte("OK"))
		case path == "/info":
			_, _ = w.Write([]byte(`{"ServerVersion":"29.9.2","OperatingSystem":"Docker Desktop","KernelVersion":"7.0","CgroupVersion":"2",
				"Runtimes":{"runc":{"path":"runc"},"runsc":{"path":"/usr/local/bin/runsc"}}}`))
		case path == "/images/nbsandbox-runner:dev/json":
			_, _ = w.Write([]byte(`{"Id":"sha256:abc","RepoDigests":["nbsandbox-runner@sha256:def"],"Config":{"Labels":{"io.nbsandbox.manifest":"{\"status_schema_version\":2}"}}}`))
		case path == "/containers/create" && r.Method == "POST":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"c1","Warnings":[]}`))
		case path == "/containers/c1/start", path == "/containers/c1/stop":
			w.WriteHeader(http.StatusNoContent)
		case path == "/containers/c1/wait":
			_, _ = w.Write([]byte(`{"StatusCode":10}`))
		case path == "/containers/c1/logs":
			w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
			_, _ = w.Write(multiplex("out line\n", "err line\n"))
		case path == "/containers/c1/json":
			_, _ = w.Write([]byte(`{"Id":"c1","State":{"Running":false,"ExitCode":10,"OOMKilled":true,
				"StartedAt":"2026-11-06T01:00:00Z","FinishedAt":"2026-11-06T01:00:05Z"},"Config":{"Labels":{"nbsandbox.run":"r1"}}}`))
		case path == "/containers/c1" && r.Method == "DELETE":
			w.WriteHeader(http.StatusNoContent)
		case path == "/containers/missing/json":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container: missing"}`))
		case path == "/containers/json":
			_, _ = w.Write([]byte(`[{"Id":"c1","Labels":{"nbsandbox.run":"r1","nbsandbox.space":"s1"},"State":"running"}]`))
		case strings.HasPrefix(path, "/images/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such image"}`))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMobyClientAdapter(t *testing.T) {
	srv := fakeDaemon(t)
	c, err := NewClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	info, err := c.Info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.ServerVersion != "29.9.2" || info.CgroupVersion != "2" || strings.Join(info.Runtimes, ",") != "runc,runsc" {
		t.Fatalf("info = %+v", info)
	}
	img, err := c.ImageInspect(ctx, "nbsandbox-runner:dev")
	if err != nil {
		t.Fatalf("image inspect: %v", err)
	}
	if img.ID != "sha256:abc" || img.Labels["io.nbsandbox.manifest"] != `{"status_schema_version":2}` || len(img.RepoDigests) != 1 {
		t.Fatalf("image = %+v", img)
	}
	if _, err := c.ImageInspect(ctx, "missing:latest"); err == nil {
		t.Fatal("missing image must be an error")
	}
}

func TestMobyClientUnreachable(t *testing.T) {
	c, err := NewClient("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("ping to a missing socket must fail")
	}
	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("info to a missing socket must fail")
	}
}

func TestSupervisorAgainstFakeDaemon(t *testing.T) {
	srv := fakeDaemon(t)
	c, _ := NewClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	defer func() { _ = c.Close() }()
	s := newTestSupervisor(c, "runsc", nil)
	if err := s.CheckStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if !st.Up || st.ImageErr != nil || st.RuntimeErr != nil || st.Info.ServerVersion != "29.9.2" {
		t.Fatalf("status = %+v", st)
	}
}

// The real backend through the real client adapter, against the fake daemon.
func TestBackendThroughAdapter(t *testing.T) {
	srv := fakeDaemon(t)
	c, err := NewClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	b := NewBackend(c, quietLogger())
	var out lines
	res, err := b.Run(context.Background(), testSpec(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 10 || !res.OOMKilled || !res.Removed || res.Finished.Sub(res.Started) != 5*time.Second {
		t.Fatalf("result %+v", res)
	}
	got := out.all()
	if len(got) != 2 {
		t.Fatalf("lines %q", got)
	}
	orphans, err := b.Orphans(context.Background())
	if err != nil || len(orphans) != 1 || orphans[0].RunID != "r1" || !orphans[0].Running {
		t.Fatalf("orphans %+v %v", orphans, err)
	}
	if _, err := c.ContainerInspect(context.Background(), "missing"); !IsNotFound(err) {
		t.Fatalf("missing container: %v", err)
	}
	if IsNotFound(nil) || IsNotFound(errors.New("boom")) {
		t.Fatal("IsNotFound too broad")
	}
}
