// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"net"
	"os"

	corev1 "k8s.io/api/core/v1"

	"github.com/azbyluthfan/nbsandbox/internal/config"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/isolation/docker"
	"github.com/azbyluthfan/nbsandbox/internal/isolation/kubernetes"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
)

// backendSupervisor watches what runs notebooks (Docker, or the Kubernetes
// API) for readiness and the run manager.
type backendSupervisor interface {
	Status() isolation.Status
	CheckStartup(ctx context.Context) error
	Run(ctx context.Context)
	DockerCheck(context.Context) error
	ImageCheck(context.Context) error
	RuntimeCheck(context.Context) error
}

// newBackend creates the configured backend and its supervisor. onChange
// hears when runs can start or not.
func newBackend(cfg config.Settings, workerID string, log *slog.Logger, metrics *obs.Metrics, onChange func(bool)) (
	isolation.Backend, backendSupervisor, func(), error) {
	if cfg.Backend == "kubernetes" {
		client, restCfg, err := kubernetes.Connect(cfg.Kubernetes.Kubeconfig)
		if err != nil {
			return nil, nil, nil, err
		}
		var tolerations []corev1.Toleration
		for _, t := range cfg.Kubernetes.Tolerations {
			tolerations = append(tolerations, corev1.Toleration{Key: t.Key, Operator: corev1.TolerationOperator(t.Operator),
				Value: t.Value, Effect: corev1.TaintEffect(t.Effect)})
		}
		b := kubernetes.NewBackend(kubernetes.Config{
			Namespace: cfg.Kubernetes.Namespace, RuntimeClass: cfg.Kubernetes.RuntimeClass, NodeSelector: cfg.Kubernetes.NodeSelector,
			Tolerations: tolerations,
			// /space and /run on the node: the space quota, twice (the
			// space's files and what the run writes).
			SpaceBytes: 2 * int64(cfg.Uploads.MaxSpace), WorkerID: workerID,
		}, client, restCfg, log)
		sup := kubernetes.NewSupervisor(b, kubernetes.SupervisorConfig{
			Image: cfg.Docker.Image, ProbeTargets: probeTargets(cfg), SetUpGauge: metrics.DockerUp.Set, Logger: log, OnChange: onChange,
		})
		return b, sup, func() {}, nil
	}
	c, err := docker.NewClient(cfg.Docker.Host)
	if err != nil {
		return nil, nil, nil, err
	}
	sup := docker.NewSupervisor(c, docker.SupervisorConfig{
		Host: cfg.Docker.Host, Image: cfg.Docker.Image, Runtime: cfg.Docker.Runtime,
		SetUpGauge: metrics.DockerUp.Set, Logger: log, OnChange: onChange,
	})
	return docker.NewBackend(c, log), sup, func() { _ = c.Close() }, nil
}

// probeTargets adds the worker's own address (NBSANDBOX_POD_IP) and its
// node's kubelet (NBSANDBOX_NODE_IP), both from the Helm chart, to what a
// run Pod must not reach.
func probeTargets(cfg config.Settings) []string {
	out := append([]string(nil), cfg.Kubernetes.ProbeTargets...)
	if ip := os.Getenv("NBSANDBOX_NODE_IP"); ip != "" {
		out = append(out, net.JoinHostPort(ip, "10250"))
	}
	if ip := getenvPodIP(); ip != "" {
		_, port, err := net.SplitHostPort(cfg.Listen)
		if err != nil || port == "" {
			port = "8080"
		}
		out = append(out, net.JoinHostPort(ip, port))
	}
	return out
}

func getenvPodIP() string { return os.Getenv("NBSANDBOX_POD_IP") }
