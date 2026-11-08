// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package kubernetes is the isolation.Backend that runs each notebook as a
// Pod through the Kubernetes API, for workers in a cluster. The run's files
// are copied into the Pod and back out through the API (pods/exec), so the
// Pod needs no shared storage, host paths or credentials.
package kubernetes

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// Labels on every Pod this backend creates. LabelRun finds orphans, and the
// network policy for run Pods selects LabelRole.
const (
	LabelRun     = "nbsandbox.run"
	LabelSpace   = "nbsandbox.space"
	LabelWorker  = "nbsandbox.worker"
	LabelRole    = "nbsandbox.role" // run | probe | manifest: every one is denied the network
	LabelManaged = "app.kubernetes.io/managed-by"
)

// Container names.
const (
	runnerContainer = "runner" // the notebook: waits until the files are in
	filesContainer  = "files"  // the worker copies files in and out through it
)

// stageDone is the file the worker creates (through files) once the files
// are in; the runner starts the notebook when it appears.
const stageDone = "/run/.nbsandbox-staged"

// runnerEntrypoint is the runner image's entrypoint, started once the files
// are in (exec: the wrapper is PID 1, so SIGTERM reaches it).
var runnerEntrypoint = []string{"python", "/opt/nbsandbox/nbsandbox_runner.py"}

// Config is how run Pods are placed.
type Config struct {
	Namespace    string            // where run Pods go
	RuntimeClass string            // optional: e.g. kata or gvisor
	NodeSelector map[string]string // optional
	Tolerations  []corev1.Toleration
	// SpaceBytes bounds /space and /run on the node (ephemeral storage):
	// the space quota plus room for outputs.
	SpaceBytes int64
	WorkerID   string
}

// PodName is a run's Pod; one per run.
func PodName(runID string) string { return "nbsandbox-run-" + runID }

// buildPod turns a RunSpec into a Pod. It is a pure function, so every
// isolation control is pinned by its own unit test.
func buildPod(cfg Config, spec isolation.RunSpec) *corev1.Pod {
	no, yes := false, true
	uid := int64(1000)
	mem := resource.NewQuantity(spec.Limits.MemoryBytes, resource.BinarySI)
	cpu := resource.NewMilliQuantity(spec.Limits.NanoCPUs/1e6, resource.DecimalSI)
	limits := corev1.ResourceList{corev1.ResourceMemory: *mem, corev1.ResourceCPU: *cpu}
	small := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi"), corev1.ResourceCPU: resource.MustParse("10m")}
	containerSec := &corev1.SecurityContext{
		RunAsNonRoot:             &yes,
		RunAsUser:                &uid,
		RunAsGroup:               &uid,
		AllowPrivilegeEscalation: &no,
		ReadOnlyRootFilesystem:   &yes,
		Privileged:               &no,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	storage := func(n int64) *resource.Quantity { return resource.NewQuantity(n, resource.BinarySI) }
	volumes := []corev1.Volume{
		{Name: "space", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: storage(cfg.SpaceBytes)}}},
		{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: storage(cfg.SpaceBytes)}}},
		// /tmp and /dev/shm in memory, counted toward the run's memory.
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: storage(spec.Limits.TmpfsBytes)}}},
		{Name: "shm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: storage(spec.Limits.ShmBytes)}}},
	}
	rw := []corev1.VolumeMount{{Name: "space", MountPath: "/space"}, {Name: "run", MountPath: "/run"}, {Name: "tmp", MountPath: "/tmp"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PodName(spec.RunID),
			Namespace: cfg.Namespace,
			Labels: map[string]string{LabelRun: spec.RunID, LabelSpace: spec.SpaceID, LabelWorker: labelValue(cfg.WorkerID),
				LabelRole: "run", LabelManaged: "nbsandbox"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// No API credentials, no service environment variables, no host
			// namespaces, no DNS: the Pod has nothing to talk to (and the
			// network policy for run Pods denies all traffic).
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			HostNetwork:                  false,
			HostPID:                      false,
			HostIPC:                      false,
			ShareProcessNamespace:        &no,
			DNSPolicy:                    corev1.DNSNone,
			DNSConfig:                    &corev1.PodDNSConfig{Nameservers: []string{"127.0.0.1"}},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			TerminationGracePeriodSeconds: ptr(int64(spec.Limits.StopGrace.Seconds()) + 1),
			NodeSelector:                  cfg.NodeSelector,
			Tolerations:                   cfg.Tolerations,
			Volumes:                       volumes,
			Containers: []corev1.Container{
				{
					Name:  runnerContainer,
					Image: spec.Image,
					// Wait for the worker to copy the files in, then run.
					Command:    []string{"sh", "-c", "while [ ! -e " + stageDone + " ]; do sleep 0.1; done; rm -f " + stageDone + "; exec \"$@\"", "runner"},
					Args:       append(append([]string(nil), runnerEntrypoint...), spec.Notebook),
					WorkingDir: "/space",
					// Environment allowlist: nothing from the worker's own.
					Env: []corev1.EnvVar{
						{Name: "HOME", Value: "/tmp"},
						{Name: "PYTHONUNBUFFERED", Value: "1"},
						{Name: "NBSANDBOX_RUN_ID", Value: spec.RunID},
						{Name: "NBSANDBOX_RUN_DIR", Value: "/run"},
						{Name: "NBSANDBOX_OUTPUT_DIR", Value: "/run/artifacts"},
						{Name: "NBSANDBOX_MAX_OUTPUT", Value: strconv.FormatInt(spec.MaxOutputBytes, 10)},
						{Name: "NBSANDBOX_OUTPUT_MODE", Value: outputMode(spec.OutputMode)},
						{Name: "NBSANDBOX_LOG_CAP", Value: strconv.FormatInt(spec.Limits.LogBytes, 10)},
					},
					SecurityContext: containerSec,
					// Requests equal limits: the run gets exactly what it may use.
					Resources: corev1.ResourceRequirements{Limits: limits, Requests: limits},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "space", MountPath: "/space", ReadOnly: spec.SpaceReadOnly},
						{Name: "run", MountPath: "/run"},
						{Name: "tmp", MountPath: "/tmp"},
						{Name: "shm", MountPath: "/dev/shm"},
					},
				},
				{
					Name:            filesContainer,
					Image:           spec.Image,
					Command:         []string{"sleep", "infinity"},
					SecurityContext: containerSec,
					Resources:       corev1.ResourceRequirements{Limits: small, Requests: small},
					VolumeMounts:    rw,
				},
			},
		},
	}
	if cfg.RuntimeClass != "" {
		pod.Spec.RuntimeClassName = &cfg.RuntimeClass
		// A sandbox with its own kernel (gVisor, Kata): the node's pids
		// limit counts the sandbox's own threads too, and gVisor dies when
		// it is refused one. The run's process limit is RLIMIT_NPROC inside
		// instead, set by the runner (Pods have no ulimit setting); it
		// counts only this run's processes, as the sandbox has its own users.
		for i := range pod.Spec.Containers {
			if c := &pod.Spec.Containers[i]; c.Name == runnerContainer && spec.Limits.PIDs > 0 {
				c.Env = append(c.Env, corev1.EnvVar{Name: "NBSANDBOX_NPROC", Value: strconv.FormatInt(spec.Limits.PIDs, 10)})
			}
		}
	}
	return pod
}

func ptr[T any](v T) *T { return &v }

func outputMode(m string) string {
	if m == "" {
		return "single"
	}
	return m
}

// labelValue fits a value into a label (63 characters of [A-Za-z0-9._-]).
func labelValue(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			b[i] = '-'
		}
	}
	if len(b) > 63 {
		b = b[:63]
	}
	return string(b)
}
