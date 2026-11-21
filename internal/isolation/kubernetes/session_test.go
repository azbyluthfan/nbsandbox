// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

var (
	_ isolation.SessionBackend    = (*Backend)(nil)
	_ isolation.SessionFilePuller = (*Backend)(nil)
)

func sessionSpec() isolation.SessionSpec {
	return isolation.SessionSpec{SessionID: "33333333-3333-4333-8333-333333333333", SpaceID: "22222222-2222-4222-8222-222222222222",
		Image: "nbsandbox-runner:test", MaxMessage: 2 << 20, SpaceReadOnly: true,
		Limits: isolation.Limits{MemoryBytes: 2 << 30, NanoCPUs: 1_500_000_000, PIDs: 256, TmpfsBytes: 256 << 20, ShmBytes: 64 << 20, StopGrace: 5 * time.Second}}
}

// A session's Pod is a run's Pod, with the bridge as its process: every
// isolation control of a run's stays.
func TestBuildSessionPodKeepsTheRunsIsolation(t *testing.T) {
	cfg := Config{Namespace: "runs", RuntimeClass: "gvisor", SpaceBytes: 1 << 30, WorkerID: "w1"}
	spec := sessionSpec()
	s := buildSessionPod(cfg, spec)
	r := buildPod(cfg, isolation.RunSpec{RunID: spec.SessionID, SpaceID: spec.SpaceID, Image: spec.Image, Limits: spec.Limits, SpaceReadOnly: true})
	if !reflect.DeepEqual(s.Spec.SecurityContext, r.Spec.SecurityContext) || !reflect.DeepEqual(s.Spec.Volumes, r.Spec.Volumes) ||
		*s.Spec.AutomountServiceAccountToken || s.Spec.DNSPolicy != corev1.DNSNone || *s.Spec.RuntimeClassName != "gvisor" {
		t.Fatalf("pod-level controls differ from a run's: %+v", s.Spec)
	}
	for i, c := range s.Spec.Containers {
		rc := r.Spec.Containers[i]
		if !reflect.DeepEqual(c.SecurityContext, rc.SecurityContext) || !reflect.DeepEqual(c.Resources, rc.Resources) ||
			!reflect.DeepEqual(c.VolumeMounts, rc.VolumeMounts) {
			t.Fatalf("container %s differs from a run's", c.Name)
		}
	}
	if s.Name != SessionPodName(spec.SessionID) || s.Labels[LabelSession] != spec.SessionID || s.Labels[LabelRun] != "" ||
		s.Labels[LabelRole] != "session" {
		t.Fatalf("metadata %+v", s.ObjectMeta)
	}
}

func TestBuildSessionPodRunsTheBridge(t *testing.T) {
	s := buildSessionPod(Config{Namespace: "runs", RuntimeClass: "gvisor"}, sessionSpec())
	c := s.Spec.Containers[0]
	if c.Name != runnerContainer || !slices.Equal(c.Args, []string{"python", isolation.SessionBridge}) || !c.Stdin || c.StdinOnce || c.TTY {
		t.Fatalf("runner: %+v", c)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["NBSANDBOX_SESSION_ID"] != sessionSpec().SessionID || env["NBSANDBOX_SESSION_MAX_MESSAGE"] != "2097152" ||
		env["NBSANDBOX_NPROC"] != "256" || env["NBSANDBOX_RUN_ID"] != "" {
		t.Fatalf("env %v", env)
	}
	if !c.VolumeMounts[0].ReadOnly || c.VolumeMounts[0].MountPath != "/space" {
		t.Fatalf("a read-only session's /space: %+v", c.VolumeMounts[0])
	}
}

func TestSessionsAndStopSession(t *testing.T) {
	id := sessionSpec().SessionID
	client := fake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: SessionPodName(id), Namespace: "runs", Labels: map[string]string{LabelSession: id, LabelSpace: "s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nbsandbox-run-x", Namespace: "runs", Labels: map[string]string{LabelRun: "x"}},
	})
	b := NewBackend(Config{Namespace: "runs"}, client, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := b.Sessions(context.Background())
	if err != nil || len(got) != 1 || got[0] != (isolation.SessionInfo{SessionID: id, SpaceID: "s", Running: true}) {
		t.Fatalf("%+v %v", got, err)
	}
	if err := b.StopSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := b.StopSession(context.Background(), id); err != nil {
		t.Fatalf("stopping a session that's gone: %v", err)
	}
	if got, _ := b.Sessions(context.Background()); len(got) != 0 {
		t.Fatalf("after stop: %+v", got)
	}
}

func TestLineSinkCapsStderr(t *testing.T) {
	var lines []string
	l := &lineSink{log: func(s string) { lines = append(lines, s) }}
	_, _ = l.Write([]byte("a\nb"))
	_, _ = l.Write([]byte("c\n"))
	for range 60 {
		_, _ = l.Write([]byte("x\n"))
	}
	if lines[0] != "a" || lines[1] != "bc" || len(lines) != 51 || lines[50] != "(more stderr not logged)" {
		t.Fatalf("%d %q", len(lines), lines[:3])
	}
}
