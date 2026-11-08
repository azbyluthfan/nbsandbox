// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/api"
	"github.com/azbyluthfan/nbsandbox/internal/completions"
	"github.com/azbyluthfan/nbsandbox/internal/config"
	"github.com/azbyluthfan/nbsandbox/internal/deps"
	"github.com/azbyluthfan/nbsandbox/internal/grpcapi"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// workerID names this process in run leases: the setting, or the host name
// (unique per container and per Kubernetes pod).
func workerID(cfg config.Settings) string {
	if cfg.WorkerID != "" {
		return cfg.WorkerID
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "local"
}

// publishWorker tells API nodes, through the database, what this worker
// runs and that it is alive, until ctx ends; then it forgets the worker.
//
// changed wakes it early (Docker came or went).
func publishWorker(ctx context.Context, db *store.DB, id string, cfg config.Settings, sup backendSupervisor, comps *completions.Publisher, changed <-chan struct{}, log *slog.Logger) {
	every := cfg.Runs.Lease / 3
	publish := func(ctx context.Context) {
		st := sup.Status()
		w := store.Worker{ID: id, Up: st.Up && st.Manifest != nil, Runtime: "{}"}
		if info, err := runtimeInfo(cfg, st, comps); err == nil { //nolint:contextcheck // the index is stored under the publisher's own (process) context
			b, _ := json.Marshal(info)
			w.Runtime = string(b)
		}
		if st.Manifest != nil {
			b, _ := json.Marshal(st.Manifest)
			w.Manifest = string(b)
		}
		if err := db.PutWorker(ctx, w); err != nil {
			log.Warn("publish worker state", "error", err)
		}
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		publish(ctx)
		select {
		case <-ctx.Done():
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			_ = db.DeleteWorker(dctx, id, 24*time.Hour)
			return
		case <-t.C:
		case <-changed:
		}
	}
}

// workerView is what an API node knows about the workers: read from the
// database, at most every couple of seconds.
type workerView struct {
	db     *store.DB
	within time.Duration // a worker not seen for this long is gone

	mu       sync.Mutex
	read     time.Time
	up       bool
	runtime  api.RuntimeInfo
	manifest *deps.Manifest
	err      error
}

func (v *workerView) refresh() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Since(v.read) < 2*time.Second {
		return
	}
	v.read = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, err := v.db.LiveWorkers(ctx, v.within)
	v.err = err
	v.up = err == nil && len(ws) > 0 && ws[0].Up
	if !v.up {
		return
	}
	var info api.RuntimeInfo
	if json.Unmarshal([]byte(ws[0].Runtime), &info) == nil {
		v.runtime = info
	}
	if m, err := deps.ParseManifest(ws[0].Manifest); err == nil {
		v.manifest = m
	}
}

// Available reports whether some worker can run notebooks.
func (v *workerView) Available() bool {
	v.refresh()
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.up
}

// Manifest is the runner image's, as a live worker reported it (nil: none).
func (v *workerView) Manifest() *deps.Manifest {
	v.refresh()
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.up {
		return nil
	}
	return v.manifest
}

// Runtime is GET /v1/runtime, as a live worker reported it.
func (v *workerView) Runtime() (api.RuntimeInfo, error) {
	v.refresh()
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.up || v.runtime.Image.Ref == "" {
		return api.RuntimeInfo{}, runs.ErrDockerUnavailable
	}
	return v.runtime, nil
}

// withGRPC serves gRPC, gRPC-Web and Connect calls next to the REST API, on
// the same port.
func withGRPC(rest, rpc http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, grpcapi.Prefix) {
			rpc.ServeHTTP(w, r)
			return
		}
		rest.ServeHTTP(w, r)
	})
}

// workerHandler is all a worker serves: liveness, readiness and metrics.
func workerHandler(health *obs.Health, metrics *obs.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.Liveness)
	mux.HandleFunc("GET /readyz", health.Readiness)
	mux.Handle("GET /metrics", metrics.Handler())
	return mux
}
