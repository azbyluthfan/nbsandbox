// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

func testSpec() isolation.RunSpec {
	return isolation.RunSpec{RunID: "11111111-1111-4111-8111-111111111111", SpaceID: "22222222-2222-4222-8222-222222222222",
		Notebook: "nb.ipynb", Image: "nbsandbox-runner:test", MaxOutputBytes: 1 << 20, OutputMode: "stream",
		Limits: isolation.Limits{MemoryBytes: 2 << 30, NanoCPUs: 1_500_000_000, TmpfsBytes: 256 << 20, ShmBytes: 64 << 20, StopGrace: 5 * time.Second}}
}

// Every isolation control of a run Pod, one by one.
func TestBuildPod(t *testing.T) {
	cfg := Config{Namespace: "runs", RuntimeClass: "kata", SpaceBytes: 1 << 30, WorkerID: "worker 1", NodeSelector: map[string]string{"pool": "runs"}}
	spec := testSpec()
	spec.SpaceReadOnly = true
	p := buildPod(cfg, spec)
	s := p.Spec
	if p.Name != PodName(spec.RunID) || p.Namespace != "runs" || p.Labels[LabelRun] != spec.RunID || p.Labels[LabelRole] != "run" ||
		p.Labels[LabelWorker] != "worker-1" {
		t.Fatalf("metadata %+v", p.ObjectMeta)
	}
	if *s.AutomountServiceAccountToken || *s.EnableServiceLinks || s.HostNetwork || s.HostPID || s.HostIPC || *s.ShareProcessNamespace {
		t.Error("the pod gets API credentials, service variables or host namespaces")
	}
	if s.DNSPolicy != corev1.DNSNone || s.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("dns %s restart %s", s.DNSPolicy, s.RestartPolicy)
	}
	if !*s.SecurityContext.RunAsNonRoot || *s.SecurityContext.RunAsUser != 1000 || s.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security %+v", s.SecurityContext)
	}
	if *s.RuntimeClassName != "kata" || s.NodeSelector["pool"] != "runs" {
		t.Error("placement")
	}
	containers := append(append([]corev1.Container(nil), s.InitContainers...), s.Containers...)
	if len(containers) != 2 {
		t.Fatalf("%d containers", len(containers))
	}
	for _, c := range containers {
		sc := c.SecurityContext
		if !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || *sc.Privileged || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Errorf("%s: security %+v", c.Name, sc)
		}
	}
	r := s.Containers[0]
	if strings.Join(r.Args, " ") != "python /opt/nbsandbox/nbsandbox_runner.py nb.ipynb" || !strings.Contains(r.Command[2], stageDone) {
		t.Errorf("runner command %v %v", r.Command, r.Args)
	}
	if r.Name != runnerContainer || !r.Resources.Limits.Memory().Equal(*r.Resources.Requests.Memory()) ||
		r.Resources.Limits.Memory().Value() != 2<<30 || r.Resources.Limits.Cpu().MilliValue() != 1500 {
		t.Errorf("runner resources %+v", r.Resources)
	}
	env := map[string]string{}
	for _, e := range r.Env {
		env[e.Name] = e.Value
	}
	if len(env) != 8 || env["NBSANDBOX_LOG_CAP"] != strconv.FormatInt(spec.Limits.LogBytes, 10) || env["NBSANDBOX_OUTPUT_MODE"] != "stream" || env["NBSANDBOX_RUN_ID"] != spec.RunID {
		t.Errorf("runner env %v", env)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range r.VolumeMounts {
		mounts[m.MountPath] = m
	}
	if !mounts["/space"].ReadOnly || mounts["/run"].ReadOnly || mounts["/tmp"].Name != "tmp" || mounts["/dev/shm"].Name != "shm" {
		t.Errorf("runner mounts %+v", mounts)
	}
	for _, v := range s.Volumes {
		if v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil {
			t.Errorf("volume %s is not a size-limited emptyDir", v.Name)
		}
		if (v.Name == "tmp" || v.Name == "shm") && v.EmptyDir.Medium != corev1.StorageMediumMemory {
			t.Errorf("%s not in memory", v.Name)
		}
	}
	spec.SpaceReadOnly = false
	if p := buildPod(Config{}, spec); p.Spec.Containers[0].VolumeMounts[0].ReadOnly || p.Spec.RuntimeClassName != nil {
		t.Error("a writing run's /space is read-only, or a runtime class appeared")
	}
	// With a RuntimeClass (a sandbox with its own kernel) the process limit
	// is RLIMIT_NPROC, set by the runner; never without one.
	nproc := func(p *corev1.Pod) string {
		for _, c := range p.Spec.Containers {
			for _, e := range c.Env {
				if c.Name == runnerContainer && e.Name == "NBSANDBOX_NPROC" {
					return e.Value
				}
			}
		}
		return ""
	}
	spec.Limits.PIDs = 256
	if v := nproc(buildPod(cfg, spec)); v != "256" {
		t.Errorf("NBSANDBOX_NPROC with a runtime class: %q", v)
	}
	if v := nproc(buildPod(Config{}, spec)); v != "" {
		t.Errorf("NBSANDBOX_NPROC without a runtime class: %q", v)
	}
	if labelValue(string(bytes.Repeat([]byte("x"), 80))) != string(bytes.Repeat([]byte("x"), 63)) {
		t.Error("long label values aren't cut")
	}
}

func TestTarRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "data", "empty"), 0o700)
	_ = os.WriteFile(filepath.Join(src, "nb.ipynb"), []byte("{}"), 0o600)
	_ = os.WriteFile(filepath.Join(src, "data", "a.csv"), []byte("a,b"), 0o600)
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("SECRET"), 0o600)
	_ = os.Symlink(outside, filepath.Join(src, "link"))
	var buf bytes.Buffer
	_ = os.WriteFile(filepath.Join(src, "log.txt"), []byte("the worker's"), 0o600)
	if err := writeTar(&buf, src, workerOwned); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("the worker's")) {
		t.Fatal("the worker's log was copied")
	}
	if duKiB("12\t/run\n30\t/space\n") != 42 {
		t.Fatal("du sizes")
	}
	if bytes.Contains(buf.Bytes(), []byte("SECRET")) {
		t.Fatal("a link was followed into the tar")
	}
	if _, err := readTar(bytes.NewReader(buf.Bytes()), dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "data", "a.csv")); string(b) != "a,b" {
		t.Fatalf("a.csv %q", b)
	}
	if info, err := os.Stat(filepath.Join(dst, "data", "empty")); err != nil || !info.IsDir() {
		t.Fatal("empty folder lost")
	}
	if _, err := readTar(bytes.NewReader(buf.Bytes()), t.TempDir(), 3); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("size cap: %v", err)
	}
	if err := clearDir(dst); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Fatalf("clearDir left %v", entries)
	}
}

// What a Pod sends back is never trusted: links are skipped, paths can't
// leave the folder.
func TestReadTarRefusesEscapes(t *testing.T) {
	build := func(hs ...*tar.Header) io.Reader {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range hs {
			_ = tw.WriteHeader(h)
			if h.Typeflag == tar.TypeReg {
				_, _ = tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
			}
		}
		_ = tw.Close()
		return &buf
	}
	dir := t.TempDir()
	skipped, err := readTar(build(
		&tar.Header{Typeflag: tar.TypeSymlink, Name: "./link", Linkname: "/etc/passwd"},
		&tar.Header{Typeflag: tar.TypeReg, Name: "./ok.txt", Size: 2, Mode: 0o644},
	), dir, 1<<20)
	if err != nil || !slices.Equal(skipped, []string{"link"}) {
		t.Fatalf("skipped %v %v", skipped, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Fatal("a link was created")
	}
	for _, name := range []string{"../escape", "/abs/path", "a/../../escape"} {
		if _, err := readTar(build(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: 1, Mode: 0o644}), t.TempDir(), 1<<20); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestOrphansAndKill(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: PodName("r1"), Namespace: "runs", Labels: map[string]string{LabelRun: "r1", LabelSpace: "s1"}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "runs"}},
	)
	b := NewBackend(Config{Namespace: "runs"}, client, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if b.Name() != "kubernetes" {
		t.Fatal(b.Name())
	}
	orphans, err := b.Orphans(ctx)
	if err != nil || len(orphans) != 1 || orphans[0].RunID != "r1" || orphans[0].SpaceID != "s1" || !orphans[0].Running {
		t.Fatalf("orphans %+v %v", orphans, err)
	}
	if err := b.Kill(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := b.Kill(ctx, "r1"); err != nil {
		t.Fatalf("killing a gone pod: %v", err)
	}
	if orphans, _ := b.Orphans(ctx); len(orphans) != 0 {
		t.Fatalf("after kill %+v", orphans)
	}
}

func TestPodStates(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: runnerContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}},
			{Name: filesContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		},
	}}
	if !running(p, filesContainer, false) || running(p, filesContainer, true) || !started(p, runnerContainer) ||
		terminated(p, runnerContainer).Reason != "OOMKilled" || terminated(p, filesContainer) != nil || podFailed(p) != nil {
		t.Fatal("state helpers")
	}
	p.Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "not found"}}
	if podFailed(p) == nil {
		t.Fatal("image pull failure not seen")
	}
	for _, c := range []struct {
		st      corev1.ContainerStateTerminated
		stopped bool
		want    bool
	}{
		{corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}, false, true},
		{corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 137}, false, true},
		{corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 137}, true, false},
		{corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}, false, false},
		{corev1.ContainerStateTerminated{Reason: "Completed"}, false, false},
	} {
		if got := oomKilled(&c.st, c.stopped); got != c.want {
			t.Errorf("oomKilled(%s %d, stopped %t) = %t", c.st.Reason, c.st.ExitCode, c.stopped, got)
		}
	}
	if !evicted(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}}) {
		t.Fatal("eviction not seen")
	}
}

// appendTo appends inside the folder only.
func TestAppendTo(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []string{"a\n", "b\n"} {
		if n, err := appendTo(dir, isolation.RunEventsFile, []byte(s)); err != nil || n != 2 {
			t.Fatalf("append %q: %d %v", s, n, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, isolation.RunEventsFile)); string(b) != "a\nb\n" {
		t.Fatalf("got %q", b)
	}
	if _, err := appendTo(dir, "../escape", []byte("x")); err == nil {
		t.Fatal("wrote outside the folder")
	}
}

// The appender counts what it wrote, so following starts again after it.
func TestAppenderCounts(t *testing.T) {
	a := &appender{dir: t.TempDir(), file: isolation.RunEventsFile}
	for _, s := range []string{"{\"kind\":\"item\"}\n", "{\"kind\":"} {
		if _, err := a.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if a.n != int64(len("{\"kind\":\"item\"}\n{\"kind\":")) {
		t.Fatalf("counted %d", a.n)
	}
}
