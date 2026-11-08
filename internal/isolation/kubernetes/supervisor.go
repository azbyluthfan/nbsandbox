// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/azbyluthfan/nbsandbox/internal/deps"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// ErrNetworkOpen: a probe Pod, labelled like a run, reached something. The
// network policy for run Pods is missing or not enforced (the cluster's
// network plugin must support NetworkPolicy), so runs are refused.
var ErrNetworkOpen = errors.New("run pods are not isolated from the network")

// SupervisorConfig configures a Supervisor.
type SupervisorConfig struct {
	Image        string        // runner image
	Interval     time.Duration // between API checks (default 15s)
	ProbeEvery   time.Duration // between network probes (default 10m)
	ProbeTargets []string      // host:port pairs a run Pod must not reach; the API server and the internet are always tried
	OnChange     func(up bool)
	SetUpGauge   func(float64)
	Logger       *slog.Logger
}

// Supervisor watches the Kubernetes side, as the Docker supervisor watches
// Docker: the API answers, the run namespace exists, the runner image's
// manifest is known (read by a one-off Pod), and a probe Pod labelled like a
// run reaches nothing. Runs are taken only while all hold.
type Supervisor struct {
	b   *Backend
	cfg SupervisorConfig

	mu         sync.RWMutex
	status     isolation.Status
	probedAt   time.Time
	probeErr   error
	manifestOf string // the image whose manifest is known
	compOf     string // the image ID whose completion index was read
	comp       []byte
	compBusy   bool

	// runPod runs a one-off Pod and returns its output and image ID
	// (replaced in tests).
	runPod func(ctx context.Context, role string, cmd []string) (string, string, error)
}

// NewSupervisor creates a supervisor for b.
func NewSupervisor(b *Backend, cfg SupervisorConfig) *Supervisor {
	if cfg.Interval == 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.ProbeEvery == 0 {
		cfg.ProbeEvery = 10 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Supervisor{b: b, cfg: cfg, status: isolation.Status{DockerErr: errors.New("not checked yet")}}
	s.runPod = s.oneOffPod
	return s
}

// Status returns the latest status.
func (s *Supervisor) Status() isolation.Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// CheckStartup fails only for a configuration error found now: the run
// namespace doesn't exist, or the runtime class doesn't. An unreachable API
// is not an error here; the loop keeps checking.
func (s *Supervisor) CheckStartup(ctx context.Context) error {
	ns := s.b.cfg.Namespace
	if _, err := s.b.client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return fmt.Errorf("kubernetes: the run namespace %q does not exist", ns)
	}
	if rc := s.b.cfg.RuntimeClass; rc != "" {
		if _, err := s.b.client.NodeV1().RuntimeClasses().Get(ctx, rc, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return fmt.Errorf("kubernetes: the runtime class %q does not exist (never replaced by the default)", rc)
		}
	}
	return nil
}

// Run checks until ctx ends.
func (s *Supervisor) Run(ctx context.Context) {
	first := true
	for {
		s.apply(s.check(ctx), first)
		first = false
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.Interval):
		}
	}
}

func (s *Supervisor) apply(next isolation.Status, first bool) {
	s.mu.Lock()
	prev := s.status
	if next.Up != prev.Up || first {
		next.Since = time.Now()
	} else {
		next.Since = prev.Since
	}
	s.status = next
	s.mu.Unlock()
	if s.cfg.SetUpGauge != nil {
		s.cfg.SetUpGauge(map[bool]float64{true: 1, false: 0}[next.Up])
	}
	if next.Up != prev.Up || first {
		if next.Up {
			s.cfg.Logger.Info("kubernetes ready for runs", "namespace", s.b.cfg.Namespace, "version", next.Info.ServerVersion)
		} else {
			s.cfg.Logger.Warn("kubernetes not ready for runs", "error", errors.Join(next.DockerErr, next.ImageErr, next.RuntimeErr))
		}
		if s.cfg.OnChange != nil && (!first || next.Up) {
			s.cfg.OnChange(next.Up)
		}
	}
}

func (s *Supervisor) check(ctx context.Context) isolation.Status {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	prev := s.Status()
	st := isolation.Status{Image: prev.Image, Manifest: prev.Manifest, Info: prev.Info}
	v, err := s.b.client.Discovery().ServerVersion()
	if err == nil {
		_, err = s.b.client.CoreV1().Namespaces().Get(cctx, s.b.cfg.Namespace, metav1.GetOptions{})
	}
	if err != nil {
		st.DockerErr = fmt.Errorf("kubernetes API: %w", err)
		return st
	}
	st.Info = isolation.Info{ServerVersion: v.GitVersion, OperatingSystem: "Kubernetes " + v.Platform}
	if rc := s.b.cfg.RuntimeClass; rc != "" {
		st.Info.Runtimes = []string{rc}
	}

	// The runner image's manifest, once per image.
	s.mu.RLock()
	known := s.manifestOf == s.cfg.Image && st.Manifest != nil
	s.mu.RUnlock()
	if !known {
		out, imageID, err := s.runPod(ctx, "manifest", []string{"python", "/opt/nbsandbox/export_manifest.py"})
		if err != nil {
			st.ImageErr = fmt.Errorf("runner image %s: %w", s.cfg.Image, err)
			return st
		}
		m, err := deps.ParseManifest(strings.TrimSpace(out))
		if err != nil {
			st.ImageErr = fmt.Errorf("runner image %s: %w", s.cfg.Image, err)
			return st
		}
		st.Manifest, st.Image = m, isolation.Image{ID: imageID, RepoDigests: digests(imageID)}
		s.mu.Lock()
		s.manifestOf = s.cfg.Image
		s.mu.Unlock()
	}

	// The completion index for editors, once per image ID, in the
	// background (it is large, and only editors need it).
	s.mu.Lock()
	if st.Image.ID != "" {
		if s.compOf == st.Image.ID {
			st.Completions = s.comp
		} else if !s.compBusy {
			s.compBusy = true
			go s.readCompletions(context.WithoutCancel(ctx), st.Image.ID)
		}
	}
	s.mu.Unlock()

	// The network probe, now and then.
	s.mu.RLock()
	due := time.Since(s.probedAt) >= s.cfg.ProbeEvery
	probeErr := s.probeErr
	s.mu.RUnlock()
	if due {
		probeErr = s.probe(ctx)
		s.mu.Lock()
		s.probedAt, s.probeErr = time.Now(), probeErr
		s.mu.Unlock()
	}
	if probeErr != nil {
		st.RuntimeErr = probeErr
		return st
	}
	st.Up = true
	return st
}

func digests(imageID string) []string {
	if i := strings.Index(imageID, "@sha256:"); i >= 0 {
		return []string{imageID[i+1:]}
	}
	return nil
}

// probeScript tries each host:port it is given and prints REACHED for each
// that answers.
const probeScript = `
import socket, sys
for t in sys.argv[1:]:
    host, port = t.rsplit(":", 1)
    try:
        socket.create_connection((host, int(port)), timeout=3).close()
        print("REACHED " + t, flush=True)
    except Exception:
        pass
print("PROBE DONE", flush=True)
`

// ProbeTargets is what a run Pod must not reach: the API server, the
// internet, and what the caller adds (the worker's own address, DNS).
func (s *Supervisor) probeTargets() []string {
	// The internet, and the cloud metadata service: on every cloud it hands
	// out the node's or a managed identity's credentials.
	targets := append([]string{"1.1.1.1:443", "8.8.8.8:53", "169.254.169.254:80"}, s.cfg.ProbeTargets...)
	if h, p := apiServerAddr(); h != "" {
		targets = append(targets, net.JoinHostPort(h, p))
	}
	return targets
}

// probe starts a Pod labelled like a run and checks it reaches nothing.
func (s *Supervisor) probe(ctx context.Context) error {
	out, _, err := s.runPod(ctx, "probe", append([]string{"python", "-c", probeScript}, s.probeTargets()...))
	if err != nil {
		return fmt.Errorf("network probe: %w", err)
	}
	if !strings.Contains(out, "PROBE DONE") {
		return fmt.Errorf("network probe: no answer (%q)", out)
	}
	var reached []string
	for _, line := range strings.Split(out, "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(line), "REACHED "); ok {
			reached = append(reached, t)
		}
	}
	if len(reached) > 0 {
		return fmt.Errorf("%w: a probe pod reached %s; make sure the run namespace's deny-all NetworkPolicy exists and the "+
			"network plugin enforces NetworkPolicy", ErrNetworkOpen, strings.Join(reached, ", "))
	}
	return nil
}

// oneOffPod runs cmd in the runner image as a Pod with the run's
// restrictions, waits for it, and returns its output and image ID.
func (s *Supervisor) oneOffPod(ctx context.Context, role string, cmd []string) (string, string, error) {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	spec := isolation.RunSpec{RunID: role + "-" + hex.EncodeToString(b), Image: s.cfg.Image, Notebook: "-",
		Limits: isolation.Limits{MemoryBytes: 256 << 20, NanoCPUs: 500_000_000, TmpfsBytes: 16 << 20, ShmBytes: 16 << 20, StopGrace: time.Second}}
	pod := buildPod(s.b.cfg, spec)
	pod.Name = "nbsandbox-" + spec.RunID
	pod.Labels = map[string]string{LabelRole: role, LabelManaged: "nbsandbox", LabelWorker: labelValue(s.b.cfg.WorkerID)}
	pod.Spec.InitContainers = nil
	pod.Spec.Containers = pod.Spec.Containers[:1]
	pod.Spec.Containers[0].Command, pod.Spec.Containers[0].Args = cmd, nil
	pods := s.b.client.CoreV1().Pods(s.b.cfg.Namespace)
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if _, err := pods.Create(cctx, pod, metav1.CreateOptions{}); err != nil {
		return "", "", err
	}
	defer func() {
		dctx, cancel := freshContext(ctx)
		defer cancel()
		zero := int64(0)
		_ = pods.Delete(dctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	}()
	var imageID string
	err := s.b.waitFor(cctx, pod.Name, func(p *corev1.Pod) (bool, error) {
		if st := status(p, runnerContainer, false); st != nil {
			imageID = st.ImageID
		}
		return terminated(p, runnerContainer) != nil, podFailed(p)
	})
	if err != nil {
		return "", "", err
	}
	stream, err := pods.GetLogs(pod.Name, &corev1.PodLogOptions{Container: runnerContainer}).Stream(cctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = stream.Close() }()
	out, err := io.ReadAll(io.LimitReader(stream, 6<<20))
	return string(out), imageID, err
}

// DockerCheck reports whether the Kubernetes API answers (readiness check
// "docker", kept for the same dashboards).
func (s *Supervisor) DockerCheck(context.Context) error { return s.Status().DockerErr }

// ImageCheck reports whether the runner image's manifest is known.
func (s *Supervisor) ImageCheck(context.Context) error {
	st := s.Status()
	if st.DockerErr != nil {
		return errors.New("kubernetes unreachable")
	}
	return st.ImageErr
}

// RuntimeCheck reports whether run Pods are isolated from the network.
func (s *Supervisor) RuntimeCheck(context.Context) error {
	st := s.Status()
	if st.DockerErr != nil {
		return errors.New("kubernetes unreachable")
	}
	return st.RuntimeErr
}

// readCompletions reads the runner image's completion index through a
// one-off Pod (like the manifest), remembering an image without one too.
func (s *Supervisor) readCompletions(ctx context.Context, imageID string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := []string{"python", "-c", "import base64, sys; sys.stdout.write(base64.b64encode(open(sys.argv[1], 'rb').read()).decode())",
		isolation.CompletionsPath}
	out, _, err := s.runPod(ctx, "manifest", cmd)
	var b []byte
	if err == nil {
		b, err = base64.StdEncoding.DecodeString(strings.TrimSpace(out))
		if err == nil && len(b) > isolation.MaxCompletions {
			b, err = nil, fmt.Errorf("larger than %d bytes", isolation.MaxCompletions)
		}
	}
	if err != nil {
		s.cfg.Logger.Info("the runner image has no completion index for editors", "image", s.cfg.Image, "error", err)
	} else {
		s.cfg.Logger.Info("read the runner image's completion index", "image", s.cfg.Image, "bytes", len(b))
	}
	s.mu.Lock()
	s.compBusy = false
	if err == nil || ctx.Err() == nil {
		s.compOf, s.comp = imageID, b
		if s.status.Image.ID == imageID {
			s.status.Completions = b
		}
	}
	s.mu.Unlock()
}
