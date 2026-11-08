// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// On a cluster whose network plugin doesn't enforce NetworkPolicy (kind's
// own kindnet lets Pods reach the API server), the workers' probe Pod must
// reach something, and the workers must refuse to run notebooks: no runtime
// is offered, workers say why on /readyz, and a submitted run never starts.
// make test-k8s-cni-refuses.
func TestNetworkNotEnforcedRefusesRuns(t *testing.T) {
	if !expectOpenNetwork {
		t.Skip("only on a cluster without NetworkPolicy enforcement (make test-k8s-cni-refuses)")
	}
	workers := strings.Fields(kubectl(t, "get", "pods", "-n", "nbsandbox", "-l", "app.kubernetes.io/component=worker",
		"-o", "jsonpath={.items[*].metadata.name}"))
	if len(workers) == 0 {
		t.Fatal("no worker pods")
	}
	// Each worker logs why it takes no runs once its first probe has run,
	// and stays not ready (its /readyz answers 503 with the same reason; the
	// API server's Pod proxy hides the body of a 503, so the log is read).
	logs := func(pod string) string {
		out, _ := exec.Command("docker", "exec", kindNode, "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf",
			"-n", "nbsandbox", "logs", pod).CombinedOutput()
		return string(out)
	}
	var reports []string
	for _, pod := range workers {
		var last string
		for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(5 * time.Second) {
			if last = logs(pod); strings.Contains(last, "not isolated from the network") {
				break
			}
		}
		if !strings.Contains(last, "not isolated from the network") {
			t.Fatalf("worker %s doesn't report the open network:\n%s", pod, last)
		}
		ready := kubectl(t, "get", "pod", "-n", "nbsandbox", pod, "-o", "jsonpath={.status.containerStatuses[0].ready}")
		if ready != "false" {
			t.Fatalf("worker %s is ready (%q) though the network is open", pod, ready)
		}
		reports = append(reports, last)
	}
	reached := ""
	if i := strings.LastIndex(reports[0], "a probe pod reached "); i >= 0 {
		reached, _, _ = strings.Cut(reports[0][i+len("a probe pod reached "):], ";")
	}
	if reached == "" {
		t.Fatalf("the report doesn't say what was reached:\n%s", reports[0])
	}

	// No runtime is offered.
	rt := call(t, "GET", "/v1/runtime", "alice", nil, nil)

	// A run is refused or waits; it never starts.
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"smoke.ipynb": ""})
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "alice", map[string]any{"notebook": "smoke.ipynb"}, nil)
	outcome := fmt.Sprintf("submit %d", r.Code)
	started := false
	if r.Code == http.StatusCreated {
		var run Run
		r.json(t, &run)
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			if got := getRun(t, "alice", sp, run.ID); got.Status != "queued" {
				started = true
				outcome += ", then " + got.Status
				break
			}
		}
		if !started {
			outcome += ", still queued after 1 min"
		}
		call(t, "POST", "/v1/spaces/"+sp+"/runs/"+run.ID+"/cancel", "alice", nil, nil)
	}
	pods := strings.TrimSpace(kubectl(t, "get", "pods", "-n", "nbsandbox-runs", "-l", "nbsandbox.role=run", "-o", "name"))

	ok := rt.Code == http.StatusServiceUnavailable && !started && pods == ""
	finding(t, "cni-refuses", "deploy on kind's own network plugin (kindnet), which doesn't enforce the deny-all NetworkPolicy; submit a run",
		"the probe reaches something; every worker says so, stays not ready and takes no runs; /v1/runtime 503; no run Pod",
		fmt.Sprintf("probe reached %s; %d of %d workers report it; /v1/runtime %d; %s; run pods %q", reached, len(reports), len(workers), rt.Code, outcome, pods),
		ok)
	if !ok {
		t.Fatalf("runtime %d %s; %s; pods %q", rt.Code, rt.Body, outcome, pods)
	}
}
