// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"testing"
	"time"
)

func TestPhases(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	epoch := func(ms int) float64 { return float64(at(ms).UnixNano()) / 1e9 }
	tl := Timeline{Created: at(0), Claimed: at(40), BackendStart: at(140), BackendDone: at(3900)}
	ws := &WrapperStatus{Timings: map[string]float64{
		"wrapper_started": epoch(500), "imports_done": epoch(1200), "kernel_ready": epoch(2400),
		"executed": epoch(3100), "rendered": epoch(3600)}}
	got := Phases(tl, at(450), at(3700), ws)
	want := map[string]float64{"queued": 0.04, "prepare": 0.1, "container_start": 0.31, "runner_start": 0.75,
		"kernel_start": 1.2, "execute": 0.7, "render": 0.5, "exit": 0.1, "teardown": 0.2}
	for k, v := range want {
		if d := got[k] - v; d > 0.002 || d < -0.002 {
			t.Errorf("%s: %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("phases %v", got)
	}

	// No runner marks (it died, or an old image): those phases are left out,
	// and the gap they span isn't charged to another phase.
	got = Phases(tl, at(450), at(3700), nil)
	if _, ok := got["execute"]; ok || got["container_start"] != 0.31 || got["teardown"] != 0.2 {
		t.Errorf("without marks: %v", got)
	}
	if _, ok := got["exit"]; ok {
		t.Errorf("exit needs the render mark: %v", got)
	}

	// A container clock behind nbsandboxd's by a second drops the phases it
	// makes negative instead of reporting nonsense.
	got = Phases(tl, at(450), at(3700), &WrapperStatus{Timings: map[string]float64{"imports_done": epoch(-500)}})
	if _, ok := got["runner_start"]; ok {
		t.Errorf("negative phase kept: %v", got)
	}
	if Phases(Timeline{}, time.Time{}, time.Time{}, nil) != nil {
		t.Error("nothing known: nil")
	}
}
