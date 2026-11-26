// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// With Kubernetes Secrets (the kind stack's default provider): a connection
// can reference a Secret someone else manages in the runs namespace (GitOps,
// Sealed Secrets, External Secrets Operator) as name#key. The fetcher Pod
// mounts it; nbsandboxd checks it exists but never keeps its value, picks up
// a rotation on the next run, and never deletes it.
func TestConnectionsKubernetesReference(t *testing.T) {
	needConnections(t)
	if !onKubernetes || os.Getenv("NBSANDBOX_TEST_VAULT") == "1" {
		t.Skip("Kubernetes Secrets provider only (make test-k8s, make report-k8s)")
	}
	sources(t)
	const ns = "nbsandbox-runs"
	secret := fmt.Sprintf("crm-team-%d", time.Now().UnixNano()%1_000_000)
	kubectl(t, "-n", ns, "create", "secret", "generic", secret, "--from-literal=password="+readerPass)
	t.Cleanup(func() {
		_, _ = runDocker("exec", kindNode, "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "-n", ns, "delete", "secret", secret, "--ignore-not-found")
	})

	newConn := func(name, ref string) response {
		return jsonCall(t, "POST", "/v1/connections", "alice", map[string]any{"name": fmt.Sprintf("%s-%d", name, connSeq.Add(1)),
			"type": "postgresql", "settings": pgSettings(), "secret": map[string]any{"ref": ref}}, nil)
	}
	// References that can't work are refused when the connection is made.
	for _, ref := range []string{"no-such-secret#password", secret + "#no-such-key", "Not_A_Name#password", secret} {
		if r := newConn("bad", ref); r.Code != http.StatusBadRequest {
			t.Errorf("reference %q: %d %s", ref, r.Code, r.Body)
		}
	}
	mk := func(name string) string {
		t.Helper()
		r := newConn(name, secret+"#password")
		if r.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", r.Code, r.Body)
		}
		if strings.Contains(string(r.Body), readerPass) {
			t.Fatalf("the connection carries the secret's value: %s", r.Body)
		}
		var c struct{ ID string }
		r.json(t, &c)
		t.Cleanup(func() { call(t, "DELETE", "/v1/connections/"+c.ID, "alice", nil, nil) })
		return c.ID
	}
	a, b := mk("crm-ref-a"), mk("crm-ref-b")
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"read.ipynb": codeNotebook(`import nbsandbox
nbsandbox.output(int(nbsandbox.sql("SELECT count(*) FROM crm.orders").iloc[0, 0]))`)})
	type outcome struct{ Status, Message, Output string }
	read := func() outcome {
		t.Helper()
		r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "alice", map[string]any{"notebook": "read.ipynb"}, nil)
		if r.Code != http.StatusCreated {
			t.Fatalf("start: %d %s", r.Code, r.Body)
		}
		var run Run
		r.json(t, &run)
		got := waitRun(t, "alice", sp, run.ID, 3*time.Minute)
		if got.Status != "succeeded" {
			return outcome{got.Status, got.Message, ""}
		}
		res := runResult(t, "alice", sp, run.ID)
		return outcome{res.Status, got.Message, string(res.Output)}
	}
	bind(t, sp, "main", map[string]map[string]string{"main": {"crm": a}})

	res := read()
	readsOK := res.Status == "succeeded" && res.Output == "1000"

	// Rotated: the next run's fetcher mounts the new value.
	kubectl(t, "-n", ns, "patch", "secret", secret, "-p", `{"stringData":{"password":"rotated-wrong"}}`)
	rot := read()
	rotatedOK := rot.Status == "platform_error" && strings.Contains(rot.Message, "password authentication failed")
	kubectl(t, "-n", ns, "patch", "secret", secret, "-p", `{"stringData":{"password":"`+readerPass+`"}}`)
	back := read()
	rotatedOK = rotatedOK && back.Status == "succeeded"

	// Deleting a connection leaves a referenced Secret alone.
	call(t, "DELETE", "/v1/connections/"+a, "alice", nil, nil)
	_, kept := runDocker("exec", kindNode, "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "-n", ns, "get", "secret", secret, "-o", "name")
	keptOK := kept == nil

	// The Secret is deleted while a connection still references it: the
	// run fails soon and says which, not at its time limit.
	bind(t, sp, "main", map[string]map[string]string{"main": {"crm": b}})
	kubectl(t, "-n", ns, "delete", "secret", secret)
	t0 := time.Now()
	gone := read()
	goneIn := time.Since(t0)
	goneOK := gone.Status == "platform_error" && strings.Contains(gone.Message, secret) && goneIn < 90*time.Second

	got := fmt.Sprintf("reads: %s %s; rotated: %s %q, then %s; the Secret after its connection was deleted: kept %v; the Secret deleted: %s in %s, %q",
		res.Status, res.Output, rot.Status, rot.Message, back.Status, keptOK, gone.Status, goneIn.Round(time.Second), gone.Message)
	ok := readsOK && rotatedOK && keptOK && goneOK
	finding(t, "connections-kubernetes_reference", "reference a Secret someone else manages (name#key), rotate it, delete its connection, then delete it",
		"refused if it or its key is missing; the run reads it; a rotation applies to the next run; nbsandbox never deletes it; once it is gone a run fails soon, naming it", got, ok)
	if !ok {
		t.Error(got)
	}
	noSecretIn(t, "the messages", rot.Message+gone.Message)
	assertNoReadLeftovers(t)
}
