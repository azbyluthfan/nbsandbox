// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nbkube "github.com/azbyluthfan/nbsandbox/internal/isolation/kubernetes"
)

// Kube is what a kubeconfig adds to a check of a Kubernetes deployment
// (read-only: services, pods, nodes and network policies).
type Kube struct {
	// Targets a run must not reach: the API server, a worker Pod, the
	// node's kubelet.
	Targets []string
	// Version is the cluster's Kubernetes version.
	Version string
	// Policies are the NetworkPolicies in the runs' namespace.
	Policies []string
	// RunPod lists the run's Pod if it is still present.
	RunPod func(ctx context.Context, runID string) ([]string, error)
}

// ConnectKube reads the extras from the cluster in kubeconfig. ns is
// nbsandbox's namespace (the workers), runsNS the runs'.
func ConnectKube(ctx context.Context, kubeconfig, ns, runsNS string) (*Kube, error) {
	client, _, err := nbkube.Connect(kubeconfig)
	if err != nil {
		return nil, err
	}
	k := &Kube{}
	if v, err := client.Discovery().ServerVersion(); err == nil {
		k.Version = v.GitVersion
	}
	svc, err := client.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("the API server's service: %w", err)
	}
	k.Targets = append(k.Targets, svc.Spec.ClusterIP+":443")
	workers, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=worker"})
	if err != nil {
		return nil, fmt.Errorf("worker pods in %s: %w", ns, err)
	}
	for _, p := range workers.Items {
		if p.Status.PodIP != "" {
			k.Targets = append(k.Targets, p.Status.PodIP+":8080")
			break
		}
	}
	if nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1}); err == nil && len(nodes.Items) > 0 {
		for _, a := range nodes.Items[0].Status.Addresses {
			if a.Type == "InternalIP" {
				k.Targets = append(k.Targets, a.Address+":10250")
				break
			}
		}
	}
	if pols, err := client.NetworkingV1().NetworkPolicies(runsNS).List(ctx, metav1.ListOptions{}); err == nil {
		for _, p := range pols.Items {
			k.Policies = append(k.Policies, p.Name)
		}
	}
	k.RunPod = func(ctx context.Context, runID string) ([]string, error) {
		pods, err := client.CoreV1().Pods(runsNS).List(ctx, metav1.ListOptions{LabelSelector: nbkube.LabelRun + "=" + runID})
		if err != nil {
			return nil, err
		}
		var names []string
		for _, p := range pods.Items {
			if p.DeletionTimestamp == nil {
				names = append(names, p.Name)
			}
		}
		return names, nil
	}
	return k, nil
}
