// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// The isolation suite. Bob's space holds a canary file and a running
// holder with a secret in memory and one in /dev/shm; alice's probe tries
// to reach all of it.
func TestIsolationSuite(t *testing.T) {
	canaryA, canaryB := "CANARY"+randomHex(8), randomHex(8)
	memA, memB := "MEMSECRET"+randomHex(8), randomHex(8)
	shmA, shmB := "SHMSECRET"+randomHex(8), randomHex(8)

	// Bob: canary and holder.
	spB := space(t, "bob")
	upload(t, "bob", spB, "", map[string]string{"canary.txt": canaryA + canaryB + "\n", "holder.ipynb": "@probes"})
	holder := startRun(t, "bob", spB, "holder.ipynb", map[string]any{"mem_a": memA, "mem_b": memB, "shm_a": shmA, "shm_b": shmB,
		// As long as possible under the stack's run timeout (start-up
		// counts too), to outlast the probe. It can't be released early: a
		// run works on a copy of the space, so it never sees a file
		// uploaded after it started.
		"max_seconds": int((runTimeout - holderMargin()).Seconds())})
	waitLog(t, "bob", spB, holder.ID, "holder ready", 90*time.Second)

	// Alice: the probe, while the holder is running.
	spA := space(t, "alice")
	upload(t, "alice", spA, "", map[string]string{"isolation_probe.ipynb": "@probes"})
	dataDir := envOr("NBSANDBOX_DATA_DIR", "")
	probe := startRun(t, "alice", spA, "isolation_probe.ipynb", map[string]any{
		"space_a_id": spA, "space_b_id": spB, "space_b_host_path": filepath.Join(dataDir, "spaces", spB, "files"), "data_dir": dataDir,
		"canary_a": canaryA, "canary_b": canaryB, "mem_a": memA, "mem_b": memB, "shm_a": shmA, "shm_b": shmB,
		"kubernetes": onKubernetes, "extra_targets": podTargets(t),
	})
	probe = waitRun(t, "alice", spA, probe.ID, 3*time.Minute)
	if probe.Status != "succeeded" {
		log := call(t, "GET", "/v1/spaces/"+spA+"/runs/"+probe.ID+"/log", "alice", nil, nil)
		t.Fatalf("probe run %s: %s\n%s", probe.Status, probe.Message, log.Body)
	}
	if running := getRun(t, "bob", spB, holder.ID); running.Status != "running" {
		t.Fatalf("holder was not running during the probe: %s", running.Status)
	}

	r := call(t, "GET", "/v1/spaces/"+spA+"/runs/"+probe.ID+"/artifacts/findings.json", "alice", nil, nil)
	var findings map[string]map[string]any
	if err := json.Unmarshal(r.Body, &findings); err != nil {
		t.Fatalf("findings.json: %d %s", r.Code, r.Body)
	}
	rows := []struct{ id, attempted, expected string }{
		{"space-paths", "read space B by absolute host path, by ../, and by listing / and /space/..; reach this space's other runs and published versions; list /run",
			"none of those paths exist; /run holds only this run's folder"},
		{"canary-search", "search the container filesystem for bob's canary", "not found"},
		{"proc-list", "list /proc", "only alice's own processes"},
		{"memory-secret", "search readable /proc/*/environ, cmdline and mem for the holder's in-memory secret", "not found"},
		{"shared-memory", "read /dev/shm", "holder's shared-memory secret absent"},
		{"socket-env-network", "look for the Docker socket; list environment variables; try the network", "no socket; only allowlisted variables; no network (on Kubernetes: nothing reachable, the API server, worker, kubelet and metadata service included)"},
		{"privileges", "id, CapEff, write to /usr, chmod u+s", "uid 1000; CapEff 0; NoNewPrivs; writes fail; setuid ineffective (nosuid /tmp, or no-new-privileges on Kubernetes)"},
	}
	for _, row := range rows {
		f := findings[row.id]
		pass, _ := f["pass"].(bool)
		finding(t, row.id, row.attempted, row.expected, summarize(row.id, f), pass)
		if !pass {
			b, _ := json.MarshalIndent(f, "", " ")
			t.Errorf("%s failed:\n%s", row.id, b)
		}
	}

	// Bob reaches nothing of alice's probe run through the API.
	base := "/v1/spaces/" + spA + "/runs/" + probe.ID
	var codes []string
	i8 := true
	for _, p := range []string{base, base + "/log", base + "/log/stream", base + "/output.ipynb", base + "/output.html",
		base + "/artifacts", base + "/artifacts/findings.json", "/v1/spaces/" + spA + "/files/isolation_probe.ipynb"} {
		c := call(t, "GET", p, "bob", nil, nil).Code
		codes = append(codes, fmt.Sprint(c))
		i8 = i8 && c == http.StatusNotFound
	}
	finding(t, "cross-user-api", "bob streams and downloads alice's run log, outputs and artifacts", "404 on every call",
		strings.Join(codes, " "), i8)
	if !i8 {
		t.Errorf("cross-user-api: %v", codes)
	}

	// The holder must finish normally with its secrets intact.
	h := waitRun(t, "bob", spB, holder.ID, 2*time.Minute)
	hlog := string(call(t, "GET", "/v1/spaces/"+spB+"/runs/"+holder.ID+"/log", "bob", nil, nil).Body)
	i9 := h.Status == "succeeded" && strings.Contains(hlog, "holder done intact=True")
	finding(t, "neighbour-unaffected", "the holder's run after the probes", "completed normally, secrets intact", h.Status, i9)
	if !i9 {
		t.Errorf("neighbour-unaffected: holder %s\n%s", h.Status, hlog)
	}
}

// holderMargin is how much of the run timeout the holder leaves unused: its
// kernel's start counts towards the timeout, and on kind that is slow.
func holderMargin() time.Duration {
	if onKubernetes {
		return 30 * time.Second
	}
	return 8 * time.Second
}

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

// summarize turns a probe's raw findings into one line of evidence.
func summarize(id string, f map[string]any) string {
	get := func(k string) any { return f[k] }
	switch id {
	case "space-paths":
		return fmt.Sprintf("paths exist %v; / has %d entries, none is space B", get("paths_exist"), len(asList(get("ls_root"))))
	case "canary-search":
		return fmt.Sprintf("found in %v; %v files scanned in %vs", get("found_in"), get("files_scanned"), get("seconds"))
	case "proc-list":
		return fmt.Sprintf("%v processes visible, foreign %v", get("count"), get("foreign"))
	case "memory-secret":
		return fmt.Sprintf("hits %v; readable %v; %v bytes of memory searched", get("hits"), get("readable"), get("mem_bytes_searched"))
	case "shared-memory":
		return fmt.Sprintf("/dev/shm entries %v", get("dev_shm_entries"))
	case "socket-env-network":
		return fmt.Sprintf("sockets %v; forbidden env %v; up interfaces besides lo %v; connections %v",
			get("sockets_found"), get("forbidden_env"), get("interfaces_up_besides_lo"), get("connections"))
	case "privileges":
		return fmt.Sprintf("uid %v, CapEff %v, NoNewPrivs %v, Seccomp %v, write /usr: %v, chmod u+s: %v on /tmp (%v), AppArmor: %v",
			get("uid"), get("CapEff"), get("NoNewPrivs"), get("Seccomp"), get("write_usr"), get("chmod_u+s_on_tmp"), get("tmp_mount"), get("apparmor"))
	}
	b, _ := json.Marshal(f)
	return string(b)
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
