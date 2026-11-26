// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// SamplingInterval is how often runs are sampled on Kubernetes: about
// metrics-server's own resolution.
const SamplingInterval = 15 * time.Second

// RunUsage implements isolation.UsageReader from the metrics API
// (metrics-server): the runner container's memory working set and CPU, both
// measured outside the Pod. Disk writes aren't in it. Without metrics-server
// it fails, and the run keeps only its end-of-run usage.
func (b *Backend) RunUsage(ctx context.Context, runID string) (isolation.Usage, error) {
	return b.usage(ctx, PodName(runID))
}

// SessionUsage implements isolation.UsageReader.
func (b *Backend) SessionUsage(ctx context.Context, sessionID string) (isolation.Usage, error) {
	return b.usage(ctx, SessionPodName(sessionID))
}

func (b *Backend) usage(ctx context.Context, pod string) (isolation.Usage, error) {
	raw, err := b.podMetrics(ctx, pod)
	if err != nil {
		return isolation.Usage{}, err
	}
	return podUsage(raw)
}

// podUsage reads a PodMetrics object's runner container.
func podUsage(raw []byte) (isolation.Usage, error) {
	var pm struct {
		Containers []struct {
			Name  string            `json:"name"`
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(raw, &pm); err != nil {
		return isolation.Usage{}, err
	}
	for _, c := range pm.Containers {
		if c.Name != runnerContainer {
			continue
		}
		mem, err := resource.ParseQuantity(c.Usage["memory"])
		if err != nil {
			return isolation.Usage{}, fmt.Errorf("memory %q: %w", c.Usage["memory"], err)
		}
		cpu, err := resource.ParseQuantity(c.Usage["cpu"])
		if err != nil {
			return isolation.Usage{}, fmt.Errorf("cpu %q: %w", c.Usage["cpu"], err)
		}
		return isolation.Usage{MemoryBytes: mem.Value(), CPUs: float64(cpu.MilliValue()) / 1000, Rate: true, WrittenBytes: -1}, nil
	}
	return isolation.Usage{}, fmt.Errorf("no %s container in its metrics yet", runnerContainer)
}
