// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Checker is one named readiness check.
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// CheckFunc adapts a function to a Checker.
type CheckFunc struct {
	CheckName string
	Fn        func(ctx context.Context) error
}

// Name implements Checker.
func (c CheckFunc) Name() string { return c.CheckName }

// Check implements Checker.
func (c CheckFunc) Check(ctx context.Context) error { return c.Fn(ctx) }

// Health serves /healthz and /readyz.
type Health struct {
	checks  []Checker
	timeout time.Duration
}

// NewHealth returns a Health with the given readiness checks. Each check gets
// at most timeout.
func NewHealth(timeout time.Duration, checks ...Checker) *Health {
	return &Health{checks: checks, timeout: timeout}
}

// Liveness answers 200 while the process can serve HTTP at all.
func (h *Health) Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ReadyReport is the /readyz body.
type ReadyReport struct {
	Status string            `json:"status"` // "ready" or "not_ready"
	Checks map[string]string `json:"checks"` // name -> "ok" or the error
}

// Ready runs every check concurrently and returns the report.
func (h *Health) Ready(ctx context.Context) ReadyReport {
	rep := ReadyReport{Status: "ready", Checks: make(map[string]string, len(h.checks))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range h.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, h.timeout)
			defer cancel()
			result := "ok"
			if err := c.Check(cctx); err != nil {
				result = err.Error()
			}
			mu.Lock()
			rep.Checks[c.Name()] = result
			if result != "ok" {
				rep.Status = "not_ready"
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return rep
}

// Readiness answers 200 when every check passes, 503 otherwise.
func (h *Health) Readiness(w http.ResponseWriter, r *http.Request) {
	rep := h.Ready(r.Context())
	code := http.StatusOK
	if rep.Status != "ready" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, rep)
}

// Names lists the configured checks, sorted.
func (h *Health) Names() []string {
	out := make([]string, 0, len(h.checks))
	for _, c := range h.checks {
		out = append(out, c.Name())
	}
	sort.Strings(out)
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
