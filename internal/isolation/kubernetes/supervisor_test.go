// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const manifest = `{"schema_version":1,"status_schema_version":2,"python_version":"3.13","distributions":{"pandas":"3"},"import_names":{"pandas":["pandas"]},"stdlib":["os"]}`

func newSupervisor(t *testing.T, objs ...any) (*Supervisor, *[]bool) {
	t.Helper()
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "runs"}})
	for _, o := range objs {
		if rc, ok := o.(*nodev1.RuntimeClass); ok {
			_, _ = client.NodeV1().RuntimeClasses().Create(context.Background(), rc, metav1.CreateOptions{})
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := NewBackend(Config{Namespace: "runs", WorkerID: "w1"}, client, nil, log)
	var changes []bool
	s := NewSupervisor(b, SupervisorConfig{Image: "runner:1", Logger: log, OnChange: func(up bool) { changes = append(changes, up) }})
	return s, &changes
}

func TestSupervisor(t *testing.T) {
	ctx := context.Background()
	s, changes := newSupervisor(t)
	probeOut := "PROBE DONE\n"
	s.runPod = func(_ context.Context, role string, cmd []string) (string, string, error) {
		switch role {
		case "manifest":
			return manifest + "\n", "docker.io/library/runner@sha256:abc", nil
		case "probe":
			if j := strings.Join(cmd, " "); !strings.Contains(j, "1.1.1.1:443") || !strings.Contains(j, "169.254.169.254:80") {
				t.Errorf("probe targets %v", cmd)
			}
			return probeOut, "", nil
		}
		return "", "", errors.New("unexpected pod")
	}
	if err := s.CheckStartup(ctx); err != nil {
		t.Fatal(err)
	}
	s.apply(s.check(ctx), true)
	st := s.Status()
	if !st.Up || st.Manifest == nil || st.Image.ID == "" || st.Image.RepoDigests[0] != "sha256:abc" || s.DockerCheck(ctx) != nil ||
		s.ImageCheck(ctx) != nil || s.RuntimeCheck(ctx) != nil {
		t.Fatalf("status %+v", st)
	}
	// The network turns out open: runs refused until a probe passes again.
	probeOut = "REACHED 1.1.1.1:443\nPROBE DONE\n"
	s.probedAt = s.probedAt.AddDate(0, 0, -1)
	s.apply(s.check(ctx), false)
	if err := s.RuntimeCheck(ctx); !errors.Is(err, ErrNetworkOpen) || s.Status().Up {
		t.Fatalf("open network: %v", err)
	}
	if len(*changes) != 2 || !(*changes)[0] || (*changes)[1] {
		t.Fatalf("changes %v", *changes)
	}
	// A probe that gives no answer is a failure too.
	probeOut = ""
	if err := s.probe(ctx); err == nil {
		t.Fatal("silent probe passed")
	}
}

func TestSupervisorConfigErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := newSupervisor(t)
	s.b.cfg.Namespace = "missing"
	if err := s.CheckStartup(ctx); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing namespace: %v", err)
	}
	s.b.cfg.Namespace, s.b.cfg.RuntimeClass = "runs", "kata"
	if err := s.CheckStartup(ctx); err == nil || !strings.Contains(err.Error(), "kata") {
		t.Fatalf("missing runtime class: %v", err)
	}
	s2, _ := newSupervisor(t, &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata"}, Handler: "kata"})
	s2.b.cfg.RuntimeClass = "kata"
	if err := s2.CheckStartup(ctx); err != nil {
		t.Fatal(err)
	}
	// A bad manifest: not ready.
	s.b.cfg.RuntimeClass = ""
	s.runPod = func(context.Context, string, []string) (string, string, error) { return "not json", "", nil }
	if st := s.check(ctx); st.Up || st.ImageErr == nil {
		t.Fatalf("bad manifest %+v", st)
	}
	s.runPod = func(context.Context, string, []string) (string, string, error) {
		return "", "", errors.New("ErrImagePull")
	}
	if st := s.check(ctx); st.Up || s.ImageCheck(ctx) == nil && st.ImageErr == nil {
		t.Fatalf("image missing %+v", st)
	}
}
