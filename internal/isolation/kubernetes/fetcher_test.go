// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

func fetcherSpec() isolation.RunSpec {
	s := testSpec()
	s.Fetcher = &isolation.Fetcher{Image: "nbsandbox-fetcher:dev", Config: []byte(`{"token":"t"}`), Token: "fetcher-token",
		Gateway: "nbsandbox-gateway.nbsandbox.svc:3128",
		Mounts:  []isolation.SecretMount{{Secret: "nbsandbox-conn-c1", Key: "secret", Path: "0"}}}
	return s
}

func TestFetcherPod(t *testing.T) {
	cfg := Config{Namespace: "runs", RuntimeClass: "gvisor", WorkerID: "w1"}
	spec := fetcherSpec()
	p := buildFetcherPod(cfg, spec, runFetcher, "10.96.0.9:3128")
	c := p.Spec.Containers[0]
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	var projected *corev1.ProjectedVolumeSource
	for _, v := range p.Spec.Volumes {
		if v.Projected != nil {
			projected = v.Projected
		}
	}
	sec := c.SecurityContext
	rows := map[string]bool{
		"not a run Pod":         p.Labels[LabelRun] == "" && p.Labels[LabelFetcher] == spec.RunID && p.Labels[LabelRole] == "fetcher",
		"no API credentials":    !*p.Spec.AutomountServiceAccountToken && !*p.Spec.EnableServiceLinks,
		"no DNS":                p.Spec.DNSPolicy == corev1.DNSNone,
		"the gateway, resolved": env["NBSANDBOX_FETCHER_GATEWAY"] == "10.96.0.9:3128",
		"hardened": *sec.RunAsNonRoot && !*sec.AllowPrivilegeEscalation && *sec.ReadOnlyRootFilesystem &&
			slices.Equal(sec.Capabilities.Drop, []corev1.Capability{"ALL"}),
		"the run's limits": c.Resources.Limits.Memory().Value() == spec.Limits.MemoryBytes &&
			c.Resources.Requests.Memory().Value() == spec.Limits.MemoryBytes,
		"the runtime class": p.Spec.RuntimeClassName != nil && *p.Spec.RuntimeClassName == "gvisor",
		"the credential mounted from its Secret": projected != nil && projected.Sources[0].Secret.Name == "nbsandbox-conn-c1" &&
			projected.Sources[0].Secret.Items[0].Key == "secret" && projected.Sources[0].Secret.Items[0].Path == "0",
		"no run files": !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.MountPath == "/space" || m.MountPath == "/run"
		}),
	}
	for name, ok := range rows {
		if !ok {
			t.Errorf("%s", name)
		}
	}
}

func TestFetcherPolicies(t *testing.T) {
	spec := fetcherSpec()
	ps := buildFetcherPolicies(Config{Namespace: "runs"}, spec, runFetcher)
	if len(ps) != 2 {
		t.Fatalf("%d policies", len(ps))
	}
	runner, fetcher := ps[0].Spec, ps[1].Spec
	if runner.PodSelector.MatchLabels[LabelRun] != spec.RunID || len(runner.Egress) != 1 ||
		runner.Egress[0].To[0].PodSelector.MatchLabels[LabelFetcher] != spec.RunID || runner.Egress[0].Ports[0].Port.IntValue() != 8815 {
		t.Errorf("runner policy: %+v", runner)
	}
	if fetcher.PodSelector.MatchLabels[LabelFetcher] != spec.RunID || len(fetcher.Ingress) != 1 ||
		fetcher.Ingress[0].From[0].PodSelector.MatchLabels[LabelRun] != spec.RunID || len(fetcher.Egress) != 0 {
		t.Errorf("fetcher policy: %+v", fetcher)
	}
}

func TestRunnerToldWhereItsFetcherIs(t *testing.T) {
	spec := fetcherSpec()
	p := buildPod(Config{Namespace: "runs"}, spec)
	withFetcher(p, "10.0.0.7", "fetcher-token")
	var env []string
	for _, e := range p.Spec.Containers[0].Env {
		env = append(env, e.Name+"="+e.Value)
	}
	if !slices.Contains(env, "NBSANDBOX_FETCHER=grpc://10.0.0.7:8815") || !slices.Contains(env, "NBSANDBOX_FETCHER_TOKEN=fetcher-token") {
		t.Fatalf("env %v", env)
	}
	if strings.Contains(strings.Join(env, " "), `"token":"t"`) {
		t.Fatal("the runner got the fetcher's configuration")
	}
}

// The fake cluster runs nothing and its logs never say "serving": the
// fetcher fails to start, the run fails at start, and everything the
// fetcher needed is removed.
func TestFetcherFailsAndIsCleanedUp(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	b := NewBackend(Config{Namespace: "runs"}, client, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.lookup = func(context.Context, string) ([]string, error) { return []string{"10.96.0.9"}, nil }
	b.poll = time.Millisecond
	// The fetcher "starts": the fake client never sets status, so set it.
	go func() {
		for i := 0; i < 2000; i++ {
			p, err := client.CoreV1().Pods("runs").Get(ctx, FetcherName(fetcherSpec().RunID), metav1.GetOptions{})
			if err == nil {
				p.Status.PodIP = "10.0.0.7"
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: fetcherContainer,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
				_, _ = client.CoreV1().Pods("runs").UpdateStatus(ctx, p, metav1.UpdateOptions{})
				return
			}
		}
	}()
	var lines []string
	_, err := b.Run(ctx, fetcherSpec(), isolation.LineFunc(func(s string) { lines = append(lines, s) }))
	if !errors.Is(err, isolation.ErrStart) || !strings.Contains(err.Error(), "stopped before it served") {
		t.Fatalf("run: %v", err)
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "[fetcher] ") {
		t.Fatalf("lines %q", lines)
	}
	pods, _ := client.CoreV1().Pods("runs").List(ctx, metav1.ListOptions{})
	secrets, _ := client.CoreV1().Secrets("runs").List(ctx, metav1.ListOptions{})
	policies, _ := client.NetworkingV1().NetworkPolicies("runs").List(ctx, metav1.ListOptions{})
	if len(pods.Items)+len(secrets.Items)+len(policies.Items) != 0 {
		t.Fatalf("left behind: %d pods, %d secrets, %d policies", len(pods.Items), len(secrets.Items), len(policies.Items))
	}
}

func TestFetcherOrphansAndKill(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: FetcherName("r2"), Namespace: "runs", Labels: map[string]string{LabelFetcher: "r2", LabelSpace: "s2"}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: FetcherName("r3"), Namespace: "runs", Labels: map[string]string{LabelFetcher: "r3"}}},
	)
	b := NewBackend(Config{Namespace: "runs"}, client, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	orphans, err := b.Orphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, o := range orphans {
		ids = append(ids, o.RunID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"r2", "r3"}) {
		t.Fatalf("orphans %+v", orphans)
	}
	for _, id := range ids {
		if err := b.Kill(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if o, _ := b.Orphans(ctx); len(o) != 0 {
		t.Fatalf("after kill: %+v", o)
	}
}

func TestResolveGateway(t *testing.T) {
	b := NewBackend(Config{}, fake.NewClientset(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.lookup = func(_ context.Context, h string) ([]string, error) {
		if h == "gw.svc" {
			return []string{"10.96.0.9"}, nil
		}
		return nil, errors.New("no such host")
	}
	for in, want := range map[string]string{"gw.svc:3128": "10.96.0.9:3128", "10.1.2.3:3128": "10.1.2.3:3128"} {
		if got, err := b.resolveGateway(context.Background(), in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"gw.svc", "nope.svc:3128", ""} {
		if _, err := b.resolveGateway(context.Background(), bad); err == nil {
			t.Errorf("%q resolved", bad)
		}
	}
}

func TestFetcherPodOwnLimits(t *testing.T) {
	spec := fetcherSpec()
	spec.Fetcher.MemoryBytes, spec.Fetcher.NanoCPUs = 384<<20, 5e8
	c := buildFetcherPod(Config{Namespace: "runs"}, spec, runFetcher, "10.96.0.9:3128").Spec.Containers[0]
	if c.Resources.Requests.Memory().Value() != 384<<20 || c.Resources.Limits.Cpu().MilliValue() != 500 {
		t.Fatalf("resources %+v", c.Resources)
	}
}

func TestFetcherPodVaultToken(t *testing.T) {
	spec := fetcherSpec()
	spec.Fetcher.VaultLogin, spec.Fetcher.ServiceAccount = true, "nbsandbox-fetcher"
	p := buildFetcherPod(Config{Namespace: "runs"}, spec, runFetcher, "10.96.0.9:3128")
	var tok *corev1.ServiceAccountTokenProjection
	for _, v := range p.Spec.Volumes {
		if v.Projected != nil && len(v.Projected.Sources) == 1 && v.Projected.Sources[0].ServiceAccountToken != nil {
			tok = v.Projected.Sources[0].ServiceAccountToken
		}
	}
	if p.Spec.ServiceAccountName != "nbsandbox-fetcher" || *p.Spec.AutomountServiceAccountToken || tok == nil ||
		tok.Audience != "vault" || *tok.ExpirationSeconds > 3600 {
		t.Fatalf("vault token: sa %q automount %v token %+v", p.Spec.ServiceAccountName, *p.Spec.AutomountServiceAccountToken, tok)
	}
	// Without Vault: no service account token of any kind.
	plain := buildFetcherPod(Config{Namespace: "runs"}, fetcherSpec(), runFetcher, "10.96.0.9:3128")
	for _, v := range plain.Spec.Volumes {
		if v.Projected != nil && v.Projected.Sources[0].ServiceAccountToken != nil {
			t.Fatal("a token without Vault")
		}
	}
}

func TestFetcherPodHostAliases(t *testing.T) {
	spec := fetcherSpec()
	spec.Fetcher.Hosts = map[string]string{"my.internal": "127.0.0.2", "a.internal": "127.0.0.3"}
	p := buildFetcherPod(Config{Namespace: "runs"}, spec, runFetcher, "10.96.0.9:3128")
	got := p.Spec.HostAliases
	if len(got) != 2 || got[0].IP != "127.0.0.3" || got[0].Hostnames[0] != "a.internal" || got[1].IP != "127.0.0.2" || got[1].Hostnames[0] != "my.internal" {
		t.Fatalf("host aliases: %+v", got)
	}
}

// A referenced Secret deleted after its connection was made: the fetcher
// Pod is scheduled but can't mount it. Its FailedMount event fails the run
// at start, naming the Secret, instead of a wait until the start timeout.
func TestFetcherSecretGone(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	b := NewBackend(Config{Namespace: "runs"}, client, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.lookup = func(context.Context, string) ([]string, error) { return []string{"10.96.0.9"}, nil }
	b.poll = time.Millisecond
	name := FetcherName(fetcherSpec().RunID)
	go func() {
		for i := 0; i < 2000; i++ {
			p, err := client.CoreV1().Pods("runs").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				time.Sleep(time.Millisecond)
				continue
			}
			p.Spec.NodeName = "node-1"
			_, _ = client.CoreV1().Pods("runs").Update(ctx, p, metav1.UpdateOptions{})
			_, _ = client.CoreV1().Events("runs").Create(ctx, &corev1.Event{
				ObjectMeta:     metav1.ObjectMeta{Name: name + ".1", Namespace: "runs"},
				InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: name, Namespace: "runs"},
				Reason:         "FailedMount",
				Message:        `MountVolume.SetUp failed for volume "secrets" : secret "crm-team" not found`,
			}, metav1.CreateOptions{})
			return
		}
	}()
	t0 := time.Now()
	_, err := b.Run(ctx, fetcherSpec(), isolation.LineFunc(func(string) {}))
	if !errors.Is(err, isolation.ErrStart) || !strings.Contains(err.Error(), `secret "crm-team" not found`) {
		t.Fatalf("run: %v", err)
	}
	if d := time.Since(t0); d > 30*time.Second {
		t.Fatalf("took %s", d)
	}
	pods, _ := client.CoreV1().Pods("runs").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Fatalf("left behind: %d pods", len(pods.Items))
	}
}

// A PodMetrics object: the runner container's working set and CPU, without
// disk writes.
func TestPodUsage(t *testing.T) {
	u, err := podUsage([]byte(`{"kind":"PodMetrics","containers":[{"name":"files","usage":{"cpu":"1m","memory":"5Mi"}},
		{"name":"runner","usage":{"cpu":"250m","memory":"128Mi"}}]}`))
	if err != nil || u.MemoryBytes != 128<<20 || u.CPUs != 0.25 || !u.Rate || u.WrittenBytes != -1 {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := podUsage([]byte(`{"containers":[]}`)); err == nil {
		t.Fatal("no runner container: want an error")
	}
	b := NewBackend(Config{Namespace: "runs"}, fake.NewClientset(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.podMetrics = func(_ context.Context, name string) ([]byte, error) {
		if name != PodName("r1") {
			t.Fatalf("pod %s", name)
		}
		return []byte(`{"containers":[{"name":"runner","usage":{"cpu":"1","memory":"1Gi"}}]}`), nil
	}
	if u, err := b.RunUsage(context.Background(), "r1"); err != nil || u.CPUs != 1 || u.MemoryBytes != 1<<30 {
		t.Fatalf("%+v %v", u, err)
	}
}
