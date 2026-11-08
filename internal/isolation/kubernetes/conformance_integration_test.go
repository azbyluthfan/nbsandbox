// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/isolation/backendtest"
)

// The shared backend conformance suite against a real cluster (make
// test-k8s-conformance starts kind and sets NBSANDBOX_TEST_KUBECONFIG).
func TestConformance(t *testing.T) {
	kubeconfig := os.Getenv("NBSANDBOX_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set NBSANDBOX_TEST_KUBECONFIG (make test-k8s-conformance)")
	}
	client, restCfg, err := Connect(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	image := os.Getenv("NBSANDBOX_RUNNER_IMAGE")
	if image == "" {
		image = "nbsandbox-runner:dev"
	}
	_, file, _, _ := runtime.Caller(0)
	b := NewBackend(Config{Namespace: "nbsandbox-runs", SpaceBytes: 1 << 30, WorkerID: "conformance"}, client, restCfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	backendtest.Run(t, backendtest.Harness{
		Backend: b, Image: image, WorkloadDir: filepath.Join(filepath.Dir(file), "..", "..", "..", "tests", "workloads"),
		TempDir: func(t *testing.T) string { return t.TempDir() },
	})
}
