// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"strings"
	"testing"
)

// podTargets are what a run Pod must not reach on Kubernetes besides the
// internet: the API server, a worker Pod, and the node's kubelet.
func podTargets(t *testing.T) []string {
	if !onKubernetes {
		return []string{}
	}
	api := strings.TrimSpace(kubectl(t, "get", "svc", "kubernetes", "-n", "default", "-o", "jsonpath={.spec.clusterIP}"))
	worker := strings.TrimSpace(kubectl(t, "get", "pods", "-n", "nbsandbox", "-l", "app.kubernetes.io/component=worker", "-o", "jsonpath={.items[0].status.podIP}"))
	node := strings.TrimSpace(kubectl(t, "get", "nodes", "-o", "jsonpath={.items[0].status.addresses[0].address}"))
	return []string{api + ":443", worker + ":8080", node + ":10250"}
}
