// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package compute is the compute view (docs/plan-compute-view.md): the
// workers, what each runs on and what it holds now. Every signed-in user
// sees it: counts and totals, no one's runs or names.
package compute

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Worker is one worker as the API shows it.
type Worker struct {
	ID string `json:"id"`
	// State: up (can run notebooks), down (alive, but it can't: Docker or
	// the runner image is missing), or gone (not seen for a while).
	State     string     `json:"state"`
	Role      string     `json:"role"`
	Version   string     `json:"version"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	SeenAt    time.Time  `json:"seen_at"`
	// Runtime is what runs notebooks there (runc, runsc, a Kubernetes
	// runtime class); Platform the Docker version and operating system;
	// Image the runner image.
	Runtime   string    `json:"runtime"`
	Platform  string    `json:"platform"`
	Image     string    `json:"image"`
	RunLimits RunLimits `json:"run_limits"`
	Runs      Slots     `json:"runs"`
	Sessions  Slots     `json:"sessions"`
	// Usage sums what its runs and sessions use now; absent when it's gone
	// or its backend can't say (docs/plan-compute-view.md).
	Usage *Totals `json:"usage,omitempty"`
}

// RunLimits are what each run there may use.
type RunLimits struct {
	MemoryBytes int64   `json:"memory_bytes"`
	CPUs        float64 `json:"cpus"`
}

// Slots are how many a worker holds, of how many it takes at once.
type Slots struct {
	Active   int `json:"active"`
	Capacity int `json:"capacity"`
}

// Source is where workers are read from (the database).
type Source interface {
	AllWorkers(ctx context.Context) ([]store.Worker, error)
	WorkerLoads(ctx context.Context) (map[string]store.WorkerLoad, error)
}

// Workers lists the workers by name. One not seen within gone is shown as
// gone; those gone a day are forgotten (store.DeleteWorker).
func Workers(ctx context.Context, src Source, gone time.Duration, now time.Time) ([]Worker, error) {
	ws, err := src.AllWorkers(ctx)
	if err != nil {
		return nil, err
	}
	loads, err := src.WorkerLoads(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Worker, 0, len(ws))
	for _, w := range ws {
		v := Worker{ID: w.ID, Role: cmp.Or(w.Role, "worker"), Version: w.Version, SeenAt: w.SeenAt.UTC(), // before 0.4 only workers published
			Runs:     Slots{Capacity: w.RunSlots},
			Sessions: Slots{Capacity: w.SessionSlots}}
		switch {
		case now.Sub(w.SeenAt) > gone:
			v.State = "gone"
		case w.Up:
			v.State = "up"
		default:
			v.State = "down"
		}
		if !w.StartedAt.IsZero() {
			t := w.StartedAt.UTC()
			v.StartedAt = &t
		}
		var rt struct {
			Runtime string `json:"runtime"`
			Image   struct {
				Ref string `json:"ref"`
			} `json:"image"`
			Limits struct {
				MemoryBytes int64   `json:"memory_bytes"`
				CPUs        float64 `json:"cpus"`
			} `json:"limits"`
			Docker struct {
				Version         string `json:"version"`
				OperatingSystem string `json:"operating_system"`
			} `json:"docker"`
		}
		if json.Unmarshal([]byte(w.Runtime), &rt) == nil {
			v.Runtime, v.Image = rt.Runtime, rt.Image.Ref
			v.RunLimits = RunLimits{MemoryBytes: rt.Limits.MemoryBytes, CPUs: rt.Limits.CPUs}
			v.Platform = platform(rt.Docker.Version, rt.Docker.OperatingSystem)
		}
		if v.State != "gone" {
			l := loads[w.ID]
			if w.Role == "all" { // a single process doesn't name itself on its sessions
				l.Runs += loads[""].Runs
				l.Sessions += loads[""].Sessions
			}
			v.Runs.Active, v.Sessions.Active = l.Runs, l.Sessions
			var t Totals
			if json.Unmarshal([]byte(w.Usage), &t) == nil && !t.SampledAt.IsZero() {
				t.SampledAt = t.SampledAt.UTC()
				v.Usage = &t
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// platform names what runs the containers: "Docker 29.1, Ubuntu 24.04", or
// on Kubernetes (whose backend reports its version and "Kubernetes
// linux/arm64" in the same fields) "Kubernetes v1.37.0, linux/arm64".
func platform(version, os string) string {
	if rest, ok := strings.CutPrefix(os, "Kubernetes"); ok {
		return strings.Join(nonEmpty("Kubernetes "+version, strings.TrimSpace(rest)), ", ")
	}
	if version != "" {
		version = "Docker " + version
	}
	return strings.Join(nonEmpty(version, os), ", ")
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
