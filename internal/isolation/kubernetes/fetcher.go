// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// LabelFetcher marks a run's fetcher Pod, its configuration Secret and its
// network policies (the value: the run). Not LabelRun: the run Pod alone
// is the run.
const LabelFetcher = "nbsandbox.fetcher"

// LabelSessionFetcher marks an interactive session's fetcher and what
// goes with it: apart from runs', so the run reconciler never takes a live
// session's fetcher for an orphan.
const LabelSessionFetcher = "nbsandbox.session.fetcher"

// fetcherKind says whose fetcher it is: the label on the fetcher and its
// objects, and the label of the Pod that may reach it (its runner).
type fetcherKind struct{ label, peer string }

var (
	runFetcher     = fetcherKind{LabelFetcher, LabelRun}
	sessionFetcher = fetcherKind{LabelSessionFetcher, LabelSession}
)

// fetcherContainer is the fetcher Pod's only container.
const fetcherContainer = "fetcher"

// FetcherName names a run's fetcher Pod, its configuration Secret and the
// network policy that lets its runner in.
func FetcherName(runID string) string { return "nbsandbox-fetch-" + runID }

// runnerPolicyName names the policy that lets a run's runner reach its fetcher.
func runnerPolicyName(runID string) string { return "nbsandbox-run-" + runID }

// fetcherReady is the line the fetcher prints once it serves.
const fetcherReady = "fetcher: serving"

// fetcherStartFailed starts the one line the fetcher prints when it can't
// start: which source, where, and why (fetcher.py's START_FAILED).
const fetcherStartFailed = "fetcher: can't start: "

// fetcherStartTimeout bounds the fetcher's start: pulling its image,
// attaching every source.
const fetcherStartTimeout = 3 * time.Minute

// fetcherConfigPath is where the fetcher reads its configuration (the
// per-run Secret).
const fetcherConfigPath = "/etc/nbsandbox-fetcher/config.json"

// buildFetcherConfigSecret is the run's fetcher configuration: the run's
// tokens and its sources (credentials only for providers nbsandboxd reads;
// the others are mounted from their own Secrets).
func buildFetcherConfigSecret(cfg Config, spec isolation.RunSpec, kind fetcherKind) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: FetcherName(spec.RunID), Namespace: cfg.Namespace,
			Labels: map[string]string{kind.label: spec.RunID, LabelSpace: spec.SpaceID, LabelManaged: "nbsandbox"}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"config.json": spec.Fetcher.Config},
	}
}

// buildFetcherPolicies are a run's own network rules, on top of the
// namespace's deny-all: its runner may reach its fetcher's port, and its
// fetcher may be reached by its runner. Nothing else: another run's runner
// can't reach this fetcher. (Fetchers reaching the gateway is a policy of
// the chart's, the same for every fetcher.)
func buildFetcherPolicies(cfg Config, spec isolation.RunSpec, kind fetcherKind) []*networkingv1.NetworkPolicy {
	port := intstr.FromInt32(isolation.FetcherPort)
	tcp := corev1.ProtocolTCP
	ports := []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}
	runner := &metav1.LabelSelector{MatchLabels: map[string]string{kind.peer: spec.RunID}}
	fetcher := &metav1.LabelSelector{MatchLabels: map[string]string{kind.label: spec.RunID}}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: cfg.Namespace,
			Labels: map[string]string{kind.label: spec.RunID, LabelManaged: "nbsandbox"}}
	}
	return []*networkingv1.NetworkPolicy{
		{ObjectMeta: meta(runnerPolicyName(spec.RunID)), Spec: networkingv1.NetworkPolicySpec{
			PodSelector: *runner, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: fetcher}}, Ports: ports}},
		}},
		{ObjectMeta: meta(FetcherName(spec.RunID)), Spec: networkingv1.NetworkPolicySpec{
			PodSelector: *fetcher, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: runner}}, Ports: ports}},
		}},
	}
}

// buildFetcherPod is the run's fetcher: a run Pod's hardening (no API
// credentials, no DNS, non-root, read-only root, no capabilities, the run's
// limits and runtime class), its configuration and mounted credentials,
// and no files of the run's.
func buildFetcherPod(cfg Config, spec isolation.RunSpec, kind fetcherKind, gateway string) *corev1.Pod {
	no, yes := false, true
	uid := int64(1000)
	memBytes, nanoCPUs := spec.Limits.MemoryBytes, spec.Limits.NanoCPUs
	if spec.Fetcher.MemoryBytes > 0 {
		memBytes = spec.Fetcher.MemoryBytes
	}
	if spec.Fetcher.NanoCPUs > 0 {
		nanoCPUs = spec.Fetcher.NanoCPUs
	}
	mem := resource.NewQuantity(memBytes, resource.BinarySI)
	cpu := resource.NewMilliQuantity(nanoCPUs/1e6, resource.DecimalSI)
	limits := corev1.ResourceList{corev1.ResourceMemory: *mem, corev1.ResourceCPU: *cpu}
	mode := int32(0o440)
	volumes := []corev1.Volume{
		{Name: "config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: FetcherName(spec.RunID), DefaultMode: &mode}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(spec.Limits.TmpfsBytes, resource.BinarySI)}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/etc/nbsandbox-fetcher/config.json", SubPath: "config.json", ReadOnly: true},
		{Name: "tmp", MountPath: "/tmp"},
	}
	if len(spec.Fetcher.Mounts) > 0 {
		var sources []corev1.VolumeProjection
		for _, m := range spec.Fetcher.Mounts {
			sources = append(sources, corev1.VolumeProjection{Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: m.Secret},
				Items:                []corev1.KeyToPath{{Key: m.Key, Path: m.Path}},
			}})
		}
		volumes = append(volumes, corev1.Volume{Name: "secrets", VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{Sources: sources, DefaultMode: &mode}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "secrets", MountPath: isolation.FetcherSecretsDir, ReadOnly: true})
	}
	if spec.Fetcher.VaultLogin {
		// A token of its service account for Vault's audience only: the
		// API server refuses it; Vault's Kubernetes auth accepts it.
		exp := int64(600)
		volumes = append(volumes, corev1.Volume{Name: "vault-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: isolation.FetcherVaultAudience, ExpirationSeconds: &exp, Path: "token"}}},
			DefaultMode: &mode}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "vault-token", MountPath: path.Dir(isolation.FetcherVaultTokenPath), ReadOnly: true})
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: FetcherName(spec.RunID), Namespace: cfg.Namespace,
			Labels: map[string]string{kind.label: spec.RunID, LabelSpace: spec.SpaceID, LabelWorker: labelValue(cfg.WorkerID),
				LabelRole: "fetcher", LabelManaged: "nbsandbox"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			ShareProcessNamespace:        &no,
			// No DNS: the worker resolved the gateway's address.
			DNSPolicy: corev1.DNSNone,
			DNSConfig: &corev1.PodDNSConfig{Nameservers: []string{"127.0.0.1"}},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			TerminationGracePeriodSeconds: ptr(int64(1)),
			NodeSelector:                  cfg.NodeSelector,
			Tolerations:                   cfg.Tolerations,
			Volumes:                       volumes,
			Containers: []corev1.Container{{
				Name:  fetcherContainer,
				Image: spec.Fetcher.Image,
				Env: []corev1.EnvVar{
					{Name: "HOME", Value: "/tmp"},
					{Name: "PYTHONUNBUFFERED", Value: "1"},
					{Name: "NBSANDBOX_FETCHER_CONFIG", Value: fetcherConfigPath},
					{Name: "NBSANDBOX_FETCHER_LISTEN", Value: "grpc://0.0.0.0:" + strconv.Itoa(isolation.FetcherPort)},
					{Name: "NBSANDBOX_FETCHER_GATEWAY", Value: gateway},
					{Name: "NBSANDBOX_FETCHER_MEMORY", Value: strconv.FormatInt(memBytes, 10)},
				},
				Ports: []corev1.ContainerPort{{Name: "flight", ContainerPort: isolation.FetcherPort, Protocol: corev1.ProtocolTCP}},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid,
					AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Privileged: &no,
					Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Resources:    corev1.ResourceRequirements{Limits: limits, Requests: limits},
				VolumeMounts: mounts,
			}},
		},
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Fetcher.Hosts)) {
		pod.Spec.HostAliases = append(pod.Spec.HostAliases, corev1.HostAlias{IP: spec.Fetcher.Hosts[name], Hostnames: []string{name}})
	}
	if cfg.RuntimeClass != "" {
		pod.Spec.RuntimeClassName = &cfg.RuntimeClass
	}
	if spec.Fetcher.VaultLogin {
		pod.Spec.ServiceAccountName = spec.Fetcher.ServiceAccount
	}
	return pod
}

// withFetcher tells the run Pod's runner where its fetcher is.
func withFetcher(pod *corev1.Pod, addr, token string) {
	for i := range pod.Spec.Containers {
		if c := &pod.Spec.Containers[i]; c.Name == runnerContainer {
			c.Env = append(c.Env,
				corev1.EnvVar{Name: isolation.FetcherEnv, Value: "grpc://" + net.JoinHostPort(addr, strconv.Itoa(isolation.FetcherPort))},
				corev1.EnvVar{Name: isolation.FetcherTokenEnv, Value: token})
		}
	}
}

// resolveGateway turns the gateway's host:port into an address the fetcher
// can use without DNS.
func (b *Backend) resolveGateway(ctx context.Context, hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", fmt.Errorf("the gateway's address %q: %w", hostport, err)
	}
	if net.ParseIP(host) != nil {
		return hostport, nil
	}
	lookup := b.lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := lookup(cctx, host)
	if err != nil || len(addrs) == 0 {
		return "", fmt.Errorf("resolve the gateway %q: %w", host, err)
	}
	return net.JoinHostPort(addrs[0], port), nil
}

// startFetcher creates the run's fetcher (its configuration Secret, its
// network policies, its Pod) and returns its address once it serves. The
// cleanup it returns removes all of it; call it whatever the error.
func (b *Backend) startFetcher(ctx context.Context, spec isolation.RunSpec, kind fetcherKind, out isolation.LineSink) (string, func(), error) {
	cleanup := func() {
		cctx, cancel := freshContext(ctx)
		defer cancel()
		if err := b.killFetcher(cctx, spec.RunID); err != nil { // the names don't depend on the kind
			b.log.Warn("remove a run's fetcher", "run_id", spec.RunID, "error", err)
		}
	}
	gw, err := b.resolveGateway(ctx, spec.Fetcher.Gateway)
	if err != nil {
		return "", cleanup, fmt.Errorf("%w: %w", isolation.ErrStart, err)
	}
	cctx, cancel := freshContext(ctx)
	defer cancel()
	if _, err := b.client.CoreV1().Secrets(b.cfg.Namespace).Create(cctx, buildFetcherConfigSecret(b.cfg, spec, kind), metav1.CreateOptions{}); err != nil {
		return "", cleanup, fmt.Errorf("%w: the fetcher's configuration: %w", isolation.ErrStart, err)
	}
	for _, p := range buildFetcherPolicies(b.cfg, spec, kind) {
		if _, err := b.client.NetworkingV1().NetworkPolicies(b.cfg.Namespace).Create(cctx, p, metav1.CreateOptions{}); err != nil {
			return "", cleanup, fmt.Errorf("%w: the run's network policy: %w", isolation.ErrStart, err)
		}
	}
	if _, err := b.client.CoreV1().Pods(b.cfg.Namespace).Create(cctx, buildFetcherPod(b.cfg, spec, kind, gw), metav1.CreateOptions{}); err != nil {
		return "", cleanup, fmt.Errorf("%w: create the fetcher: %w", isolation.ErrStart, err)
	}
	addr, err := b.fetcherReady(ctx, FetcherName(spec.RunID), out)
	if err != nil {
		return "", cleanup, fmt.Errorf("%w: %w", isolation.ErrStart, err)
	}
	return addr, cleanup, nil
}

// fetcherReady waits until the fetcher runs, follows its output into the
// run's log until it says it serves, and returns its Pod's address. If it
// ends first, its last lines say why.
func (b *Backend) fetcherReady(ctx context.Context, name string, out isolation.LineSink) (string, error) {
	wctx, cancel := context.WithTimeout(ctx, fetcherStartTimeout)
	defer cancel()
	var checked time.Time
	if err := b.waitFor(wctx, name, func(p *corev1.Pod) (bool, error) {
		if started(p, fetcherContainer) && p.Status.PodIP != "" {
			return true, nil
		}
		if err := podFailed(p); err != nil {
			return false, err
		}
		// Scheduled but not started: a Secret it mounts may be gone.
		if p.Spec.NodeName != "" && time.Since(checked) >= mountCheckEvery {
			checked = time.Now()
			return false, b.mountFailed(wctx, name)
		}
		return false, nil
	}); err != nil {
		if wctx.Err() != nil && ctx.Err() == nil {
			return "", errors.New("the fetcher didn't start in time")
		}
		return "", fmt.Errorf("the fetcher didn't start: %w", err)
	}
	stream, err := b.client.CoreV1().Pods(b.cfg.Namespace).GetLogs(name, &corev1.PodLogOptions{Container: fetcherContainer, Follow: true}).Stream(wctx)
	if err != nil {
		return "", fmt.Errorf("follow the fetcher: %w", err)
	}
	defer func() { _ = stream.Close() }()
	var last []string
	var failed string
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		out.Line("[fetcher] " + line)
		if strings.HasPrefix(line, fetcherReady) {
			p, err := b.client.CoreV1().Pods(b.cfg.Namespace).Get(wctx, name, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			return p.Status.PodIP, nil
		}
		if r, ok := strings.CutPrefix(line, fetcherStartFailed); ok {
			failed = r
		}
		if last = append(last, line); len(last) > 3 {
			last = last[1:]
		}
	}
	if wctx.Err() != nil && ctx.Err() == nil {
		return "", errors.New("the fetcher didn't start in time")
	}
	if failed != "" {
		return "", fmt.Errorf("the fetcher couldn't start: %s", failed)
	}
	if len(last) == 0 {
		return "", errors.New("the fetcher stopped before it served")
	}
	return "", fmt.Errorf("the fetcher stopped before it served: %s", strings.Join(last, " | "))
}

// mountCheckEvery is how often a fetcher that doesn't start is checked
// for a Secret it can't mount.
const mountCheckEvery = 2 * time.Second

// mountFailed reports a fetcher Pod whose Secrets can't be mounted: a
// referenced Secret deleted or without its key after its connection was
// made. The kubelet would retry until the start timeout; the Pod's events
// say so within seconds, without reading the Secret.
func (b *Backend) mountFailed(ctx context.Context, name string) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	evs, err := b.client.CoreV1().Events(b.cfg.Namespace).List(cctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Pod,involvedObject.name=" + name + ",reason=FailedMount"})
	if err != nil {
		return nil // only a faster failure; the start timeout still applies
	}
	for _, e := range evs.Items {
		if e.InvolvedObject.Name == name && e.Reason == "FailedMount" &&
			(strings.Contains(e.Message, "not found") || strings.Contains(e.Message, "couldn't find key")) {
			return fmt.Errorf("a connection's secret can't be mounted (deleted, or without its key, since the connection was made): %s", e.Message)
		}
	}
	return nil
}

// killFetcher removes a run's fetcher Pod, its configuration Secret and
// its network policies, whichever exist.
func (b *Backend) killFetcher(ctx context.Context, runID string) error {
	zero := int64(0)
	var errs []error
	ignore := func(err error) {
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	ignore(b.client.CoreV1().Pods(b.cfg.Namespace).Delete(ctx, FetcherName(runID), metav1.DeleteOptions{GracePeriodSeconds: &zero}))
	ignore(b.client.CoreV1().Secrets(b.cfg.Namespace).Delete(ctx, FetcherName(runID), metav1.DeleteOptions{}))
	np := b.client.NetworkingV1().NetworkPolicies(b.cfg.Namespace)
	ignore(np.Delete(ctx, FetcherName(runID), metav1.DeleteOptions{}))
	ignore(np.Delete(ctx, runnerPolicyName(runID), metav1.DeleteOptions{}))
	return errors.Join(errs...)
}

// fetcherOrphans lists the runs whose fetcher Pod or configuration Secret
// exists.
func (b *Backend) fetcherOrphans(ctx context.Context, kind fetcherKind) (map[string]string, error) {
	runs := map[string]string{} // run (or session) -> space
	pods, err := b.client.CoreV1().Pods(b.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: kind.label})
	if err != nil {
		return nil, err
	}
	for _, p := range pods.Items {
		runs[p.Labels[kind.label]] = p.Labels[LabelSpace]
	}
	secrets, err := b.client.CoreV1().Secrets(b.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: kind.label})
	if err != nil {
		return nil, err
	}
	for _, s := range secrets.Items {
		if _, ok := runs[s.Labels[kind.label]]; !ok {
			runs[s.Labels[kind.label]] = s.Labels[LabelSpace]
		}
	}
	return runs, nil
}
