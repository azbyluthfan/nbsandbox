// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"encoding/json"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// Docker's stats as `docker stats` reads them: the working set, CPU time,
// and bytes written when the runtime reports them.
func TestUsageOf(t *testing.T) {
	for name, c := range map[string]struct {
		stats string
		want  isolation.Usage
	}{
		"cgroup v2": {`{"cpu_stats":{"cpu_usage":{"total_usage":3000000000}},
			"memory_stats":{"usage":209715200,"stats":{"inactive_file":104857600}},
			"blkio_stats":{"io_service_bytes_recursive":[{"op":"read","value":10},{"op":"write","value":4096},{"op":"write","value":4096}]}}`,
			isolation.Usage{MemoryBytes: 100 << 20, CPUNanos: 3e9, WrittenBytes: 8192}},
		"cgroup v1": {`{"memory_stats":{"usage":3000,"stats":{"total_inactive_file":1000,"inactive_file":10}},
			"blkio_stats":{"io_service_bytes_recursive":[{"op":"Write","value":7}]}}`,
			isolation.Usage{MemoryBytes: 2000, WrittenBytes: 7}},
		"gVisor (no block I/O)": {`{"cpu_stats":{"cpu_usage":{"total_usage":5}},"memory_stats":{"usage":50}}`,
			isolation.Usage{MemoryBytes: 50, CPUNanos: 5, WrittenBytes: -1}},
	} {
		var st container.StatsResponse
		if err := json.Unmarshal([]byte(c.stats), &st); err != nil {
			t.Fatal(err)
		}
		if got := usageOf(st); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}
