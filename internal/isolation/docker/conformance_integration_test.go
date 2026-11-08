// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package docker

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/isolation/backendtest"
)

// The backend conformance suite against the real Docker daemon:
// DOCKER_HOST (or NBSANDBOX_DOCKER_HOST), runtime from NBSANDBOX_DOCKER_RUNTIME.
func TestDockerBackendConformance(t *testing.T) {
	host := os.Getenv("NBSANDBOX_DOCKER_HOST")
	c, err := NewClient(host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, file, _, _ := runtime.Caller(0)
	image := os.Getenv("NBSANDBOX_RUNNER_IMAGE")
	if image == "" {
		image = "nbsandbox-runner:dev"
	}
	backendtest.Run(t, backendtest.Harness{
		Backend:     NewBackend(c, slog.New(slog.NewTextHandler(io.Discard, nil))),
		Image:       image,
		Runtime:     os.Getenv("NBSANDBOX_DOCKER_RUNTIME"),
		WorkloadDir: filepath.Join(filepath.Dir(file), "..", "..", "..", "tests", "workloads"),
		TempDir:     func(t *testing.T) string { return t.TempDir() },
	})
}
