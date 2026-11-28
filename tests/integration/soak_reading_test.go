// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The soak's reading workload (make soak-connections): with connections on,
// a space that reads runs beside the others, in turn:
//   - a run reading a database and object storage, and streaming, and
//     calling an HTTP API through its fetcher (JSON and server-sent events);
//   - a run whose environment has a wrong password (refused at start);
//   - a call of a published function that reads;
//   - a session that reads, then ends.
// Fetchers, their networks and the gateway's memory are sampled with the rest.

// soakReads reports whether the soak reads (connections on, sources up).
func soakReads() bool { return os.Getenv("NBSANDBOX_TEST_CONNECTIONS") == "1" }

const soakReadNotebook = `import nbsandbox
by_region = nbsandbox.sql("SELECT region, count(*) AS n FROM crm.orders GROUP BY 1 ORDER BY 1")
lake = nbsandbox.sql("SELECT sum(amount)::BIGINT AS total FROM lake.csv('sales/2026.csv')")
rows = sum(b.num_rows for b in nbsandbox.stream("SELECT * FROM crm.orders"))
api = nbsandbox.http("events")
orders = len(api.get("orders").json()["orders"])
events = sum(1 for l in api.get("events", stream=True).iter_lines() if l.startswith(b"data:"))
nbsandbox.output({"n": int(by_region.n.sum()), "lake": int(lake.total.iloc[0]), "rows": rows, "api": orders, "events": events})`

// soakReadSpace makes the reading space and publishes its function.
func soakReadSpace(t *testing.T) string {
	t.Helper()
	apiSource(t)
	events := connection(t, "events", "http", map[string]any{"url": apiURL("token-api/")}, apiToken)
	sp := readerSpaceWith(t, codeNotebook(soakReadNotebook), map[string]string{"events": events})
	if r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/totals/versions", "alice", map[string]any{"notebook": "read.ipynb"}, nil); r.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", r.Code, r.Body)
	}
	return sp
}

// soakRead is one reading cycle, chosen by n.
func soakRead(t *testing.T, sp string, n int) {
	t.Helper()
	const want = `{"n":1000,"lake":22,"rows":1000,"api":10,"events":5}`
	compact := func(b []byte) string {
		var v any
		if json.Unmarshal(b, &v) != nil {
			return string(b)
		}
		out, _ := json.Marshal(v)
		return string(out)
	}
	switch n % 4 {
	case 0: // a reading run
		run := startRun(t, "alice", sp, "read.ipynb", nil)
		if res := runResult(t, "alice", sp, run.ID); res.Status != "succeeded" || compact(res.Output) != compact([]byte(want)) {
			t.Errorf("reading run %d: %s %s", n, res.Status, res.Output)
		}
	case 1: // refused at start: the fetcher can't attach with a wrong password
		r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "alice", map[string]any{"notebook": "read.ipynb", "environment": "prod"}, nil)
		if r.Code != http.StatusCreated {
			t.Errorf("prod run %d: %d %s", n, r.Code, r.Body)
			return
		}
		var run Run
		r.json(t, &run)
		if got := waitRun(t, "alice", sp, run.ID, 3*time.Minute); got.Status != "platform_error" || !strings.Contains(got.Message, "password authentication failed") {
			t.Errorf("prod run %d: %+v", n, got)
		}
	case 2: // a function call
		r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/totals/invoke?wait=50s", "alice", map[string]any{"environment": "dev"}, nil)
		var res callerResult
		_ = json.Unmarshal(r.Body, &res)
		if res.Status != "succeeded" || compact(res.Output) != compact([]byte(want)) {
			t.Errorf("function call %d: %d %s", n, r.Code, r.Body)
		}
	case 3: // a reading session
		r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/sessions", "alice", map[string]string{"notebook": "read.ipynb", "environment": "dev", "mode": "read-only"}, nil)
		if r.Code != http.StatusCreated {
			t.Errorf("session %d: open: %d %s", n, r.Code, r.Body)
			return
		}
		var s struct{ ID string }
		r.json(t, &s)
		k := dialSession(t, "alice", sp, s.ID)
		out, _ := k.run(`import nbsandbox; print(int(nbsandbox.sql("SELECT count(*) FROM crm.orders").iloc[0, 0]))`)
		_ = k.c.Close()
		call(t, "DELETE", "/v1/spaces/"+sp+"/sessions/"+s.ID, "alice", nil, nil)
		if out != "1000\n" {
			t.Errorf("session %d: %q", n, out)
		}
	}
}

// readingLeftoversQuiet counts fetchers and their networks (Docker) or
// fetcher Pods, Secrets and policies (Kubernetes), runs' and sessions'.
func readingLeftoversQuiet() float64 {
	var n int
	count := func(args ...string) {
		out, err := exec.Command("docker", args...).Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			n += len(strings.Split(strings.TrimSpace(string(out)), "\n"))
		}
	}
	for _, l := range []string{"nbsandbox.fetcher", "nbsandbox.session.fetcher"} {
		if onKubernetes {
			count("exec", kindNode, "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "-n", "nbsandbox-runs", "get",
				"pods,secrets,networkpolicies", "-l", l, "-o", "name")
		} else {
			count("ps", "-aq", "--filter", "label="+l)
		}
	}
	if !onKubernetes {
		for _, l := range []string{"nbsandbox.network", "nbsandbox.session.network"} {
			count("network", "ls", "-q", "--filter", "label="+l)
		}
	}
	return float64(n)
}

// gatewayMiB is the gateway container's memory (Docker; -1 elsewhere).
func gatewayMiB() float64 {
	if onKubernetes {
		return -1
	}
	out, err := exec.Command("docker", "stats", "--no-stream", "--format", "{{.MemUsage}}", "nbsandbox-gateway").Output()
	if err != nil {
		return -1
	}
	used, _, _ := strings.Cut(strings.TrimSpace(string(out)), " / ")
	for _, u := range []struct {
		unit string
		mul  float64
	}{{"GiB", 1024}, {"MiB", 1}, {"KiB", 1.0 / 1024}, {"kB", 1.0 / 1024}, {"B", 1.0 / (1 << 20)}} { // longest first: "MiB" ends in "B"
		unit, mul := u.unit, u.mul
		if v, ok := strings.CutSuffix(used, unit); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f * mul
			}
		}
	}
	return -1
}
