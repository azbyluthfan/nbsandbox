// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package compute

import (
	"context"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

type fakeSource struct {
	ws    []store.Worker
	loads map[string]store.WorkerLoad
}

func (f fakeSource) AllWorkers(context.Context) ([]store.Worker, error) { return f.ws, nil }
func (f fakeSource) WorkerLoads(context.Context) (map[string]store.WorkerLoad, error) {
	return f.loads, nil
}

func TestWorkers(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	rt := `{"runtime":"runsc","image":{"ref":"nbsandbox-runner:dev"},"limits":{"memory_bytes":2147483648,"cpus":1},
		"docker":{"version":"29.1","operating_system":"Ubuntu 24.04"}}`
	src := fakeSource{
		ws: []store.Worker{
			{ID: "a", Up: true, Runtime: rt, Role: "worker", Version: "0.4.0", StartedAt: now.Add(-time.Hour), SeenAt: now.Add(-5 * time.Second), RunSlots: 2, SessionSlots: 4},
			{ID: "b", Up: false, Runtime: "{}", SeenAt: now.Add(-10 * time.Second), RunSlots: 1},
			{ID: "c", Up: true, Runtime: "{}", Role: "worker", SeenAt: now.Add(-5 * time.Minute), RunSlots: 1},
			{ID: "one", Up: true, Runtime: "{}", Role: "all", SeenAt: now, RunSlots: 2, SessionSlots: 2},
		},
		loads: map[string]store.WorkerLoad{"a": {Runs: 2, Sessions: 1}, "c": {Runs: 1}, "one": {Runs: 1}, "": {Sessions: 2}},
	}
	ws, err := Workers(context.Background(), src, 30*time.Second, now)
	if err != nil || len(ws) != 4 {
		t.Fatalf("%+v %v", ws, err)
	}
	a, b, c, one := ws[0], ws[1], ws[2], ws[3]
	if a.State != "up" || a.Runtime != "runsc" || a.Image != "nbsandbox-runner:dev" || a.Platform != "Docker 29.1, Ubuntu 24.04" ||
		a.RunLimits != (RunLimits{MemoryBytes: 2 << 30, CPUs: 1}) || a.Runs != (Slots{2, 2}) || a.Sessions != (Slots{1, 4}) ||
		a.StartedAt == nil || a.Version != "0.4.0" {
		t.Errorf("a %+v", a)
	}
	if got := platform("v1.37.0", "Kubernetes linux/arm64"); got != "Kubernetes v1.37.0, linux/arm64" {
		t.Errorf("kubernetes platform %q", got)
	}
	if b.State != "down" || b.StartedAt != nil || b.Role != "worker" { // published before 0.4: no role
		t.Errorf("b %+v", b)
	}
	if c.State != "gone" || c.Runs.Active != 0 { // gone: what it held is not counted
		t.Errorf("c %+v", c)
	}
	if one.Runs != (Slots{1, 2}) || one.Sessions != (Slots{2, 2}) { // a single process: its unnamed sessions are its own
		t.Errorf("one %+v", one)
	}
}
