// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// When a piece of a reading run fails: its fetcher, a source, the gateway,
// nbsandboxd. Each failure must reach the notebook as a clear error soon
// (not a hang until the run's time limit), never with a credential in it,
// what can recover must, and nothing may be left behind.

// killFetcher removes a run's or session's fetcher, as a crash or an
// eviction would: its process ends and its connections close. (A fetcher
// whose network vanishes first, a force-deleted Pod or a lost node, leaves
// the runner's stream open until the client's keepalive gives up, about six
// and a half minutes: longer than these tests' runs may last.)
func killFetcher(t *testing.T, id string) {
	t.Helper()
	if onKubernetes {
		kubectl(t, "-n", "nbsandbox-runs", "delete", "pod", "nbsandbox-fetch-"+id, "--timeout=60s")
		return
	}
	docker(t, "rm", "-f", "nbsandbox-fetch-"+id)
}

// restartGateway restarts the gateway, as a crash or a rollout would, and
// waits until it serves again.
func restartGateway(t *testing.T) {
	t.Helper()
	if onKubernetes {
		kubectl(t, "-n", "nbsandbox", "rollout", "restart", "deploy/nbsandbox-gateway")
		kubectl(t, "-n", "nbsandbox", "rollout", "status", "deploy/nbsandbox-gateway", "--timeout=180s")
		return
	}
	docker(t, "restart", "-t", "1", "nbsandbox-gateway")
}

// noSecretIn fails if a message carries a source's credentials.
func noSecretIn(t *testing.T, what, msg string) {
	t.Helper()
	for _, s := range []string{readerPass, s3Secret} {
		if strings.Contains(msg, s) {
			t.Errorf("%s carries a credential: %q", what, msg)
		}
	}
}

// A run's fetcher dies while a query streams: the query fails at once with a
// clear error, later queries fail the same way, the run ends normally, and
// its fetcher's network and policies go with it.
func TestConnectionsFetcherDies(t *testing.T) {
	needConnections(t)
	sp := readerSpace(t, codeNotebook(`import nbsandbox, time
res = {"first": int(nbsandbox.sql("SELECT count(*) FROM crm.orders").iloc[0, 0])}
t0 = time.time()
try:
    n = 0
    for b in nbsandbox.stream("SELECT range AS i, md5(range::VARCHAR) AS h FROM range(1000000000)"):
        n += b.num_rows
        if not res.get("streaming"):
            res["streaming"] = True
            print("STREAMING", flush=True)
    res["stream"] = "completed"
except nbsandbox.QueryError as e:
    res["stream_error"] = str(e)
res["stream_seconds"] = time.time() - t0
t0 = time.time()
try:
    nbsandbox.sql("SELECT count(*) FROM crm.orders")
    res["after"] = "read"
except nbsandbox.QueryError as e:
    res["after_error"] = str(e)
res["after_seconds"] = time.time() - t0
nbsandbox.output(res)`))
	run := startRun(t, "alice", sp, "read.ipynb", nil)
	waitLog(t, "alice", sp, run.ID, "STREAMING", 3*time.Minute)
	killed := time.Now()
	killFetcher(t, run.ID)
	res := runResult(t, "alice", sp, run.ID)
	ended := time.Since(killed)
	var out struct {
		First        int     `json:"first"`
		Stream       string  `json:"stream"`
		StreamError  string  `json:"stream_error"`
		After        string  `json:"after"`
		AfterError   string  `json:"after_error"`
		AfterSeconds float64 `json:"after_seconds"`
	}
	_ = json.Unmarshal(res.Output, &out)
	got := fmt.Sprintf("run %s %s after the kill; the stream: %q; the next query (%.1fs): %q",
		res.Status, ended.Round(time.Second), out.StreamError, out.AfterSeconds, out.AfterError)
	gone := "the fetcher can't be reached"
	ok := res.Status == "succeeded" && out.First == 1000 && out.Stream == "" && strings.Contains(out.StreamError, gone) &&
		out.After == "" && strings.Contains(out.AfterError, gone) && out.AfterSeconds < 30 && ended < 2*time.Minute
	finding(t, "connections-fetcher_dies", "kill a run's fetcher while a query streams",
		"the stream and later queries fail at once with an error the notebook can catch; the run ends; nothing is left", got, ok)
	if !ok {
		t.Errorf("%s\n%s", got, res.Output)
	}
	noSecretIn(t, "the errors", out.StreamError+out.AfterError)
	assertNoReadLeftovers(t)
}

// A session reads while its source stops and starts, the gateway restarts,
// nbsandboxd restarts (Docker) and finally its fetcher dies: each failure is
// a clear error in the cell, and reading comes back when the piece does.
func TestConnectionsSessionFailures(t *testing.T) {
	needConnections(t)
	sp := readerSpace(t, codeNotebook("1"))
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/sessions", "alice", map[string]string{"notebook": "read.ipynb", "environment": "dev"}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("open: %d %s", r.Code, r.Body)
	}
	var s struct{ ID string }
	r.json(t, &s)
	ended := false
	end := func() {
		if !ended {
			ended = true
			call(t, "DELETE", "/v1/spaces/"+sp+"/sessions/"+s.ID, "alice", nil, nil)
		}
	}
	t.Cleanup(end)
	k := connectSession(t, "alice", sp, s.ID)
	// read: one query; "rows N" or "error <message>", and how long it took.
	k.run(`import nbsandbox, time
def read(q="SELECT count(*) FROM crm.orders"):
    t0 = time.time()
    try:
        r = "rows " + str(int(nbsandbox.sql(q).iloc[0, 0]))
    except nbsandbox.QueryError as e:
        r = "error " + str(e)
    print(f"{time.time() - t0:.1f} {r}")
# until: read again until it works, for up to a minute.
def until(q="SELECT count(*) FROM crm.orders"):
    t0 = time.time()
    while True:
        try:
            r = "rows " + str(int(nbsandbox.sql(q).iloc[0, 0]))
            break
        except nbsandbox.QueryError as e:
            r = "error " + str(e)
            if time.time() - t0 > 60:
                break
            time.sleep(1)
    print(f"{time.time() - t0:.1f} {r}")`)
	read := func(fn string) (float64, string) {
		t.Helper()
		out, _ := k.run(fn + "()")
		secs, rest, _ := strings.Cut(strings.TrimSpace(out), " ")
		f, err := strconv.ParseFloat(secs, 64)
		if err != nil {
			t.Fatalf("%s: %q", fn, out)
		}
		noSecretIn(t, fn, rest)
		return f, rest
	}
	if _, got := read("read"); got != "rows 1000" {
		t.Fatalf("first read: %q", got)
	}

	step := func(id, attempted, expected string, ok bool, got string) {
		t.Helper()
		finding(t, id, attempted, expected, got, ok)
		if !ok {
			t.Errorf("%s: %s", id, got)
		}
	}

	// The source stops, then starts again.
	docker(t, "stop", "-t", "1", srcPG)
	down, downGot := read("read")
	docker(t, "start", srcPG)
	retry(t, "the source back", 60*time.Second, func() (string, error) {
		return runDocker("exec", srcPG, "pg_isready", "-U", "postgres")
	})
	back, backGot := read("until")
	step("connections-source_restarts", "stop a session's source between queries, then start it",
		"the query fails at once with the source's error; reading works again once the source is back",
		strings.HasPrefix(downGot, "error ") && down < 30 && backGot == "rows 1000",
		fmt.Sprintf("while down (%.1fs): %q; once back (%.1fs): %q", down, downGot, back, backGot))

	// The gateway restarts.
	restartGateway(t)
	gw, gwGot := read("until")
	step("connections-gateway_restarts", "restart the gateway while a session reads",
		"reading works again once the gateway is back (its tokens are signed, not stored)",
		gwGot == "rows 1000", fmt.Sprintf("after the restart (%.1fs): %q", gw, gwGot))

	// nbsandboxd restarts: the session and its fetcher outlive it.
	if !onKubernetes {
		compose(t, "restart", "nbsandboxd")
		if err := waitReady(time.Minute); err != nil {
			t.Fatal(err)
		}
		k = connectSession(t, "alice", sp, s.ID)
		re, reGot := read("until")
		step("connections-nbsandboxd_restarts_session", "restart nbsandboxd while a session reads",
			"the session reconnects and reads through the same fetcher", reGot == "rows 1000",
			fmt.Sprintf("after the restart (%.1fs): %q", re, reGot))
	}

	// The fetcher dies: queries fail at once; ending the session cleans up.
	killFetcher(t, s.ID)
	dead, deadGot := read("read")
	step("connections-session_fetcher_dies", "kill a session's fetcher, then query",
		"the query fails at once with an error the cell can catch; ending the session leaves nothing",
		strings.HasPrefix(deadGot, "error the fetcher can't be reached") && dead < 30, fmt.Sprintf("(%.1fs): %q", dead, deadGot))
	end()
	assertNoSessionContainer(t, s.ID)
	assertNoReadLeftovers(t)
}

// nbsandboxd restarts while a run streams (Docker; on Kubernetes the worker
// holding a run is a Pod, covered by the run tests): the run ends as
// interrupted, and its fetcher and network are removed.
func TestConnectionsRunInterrupted(t *testing.T) {
	needConnections(t)
	needCompose(t)
	sp := readerSpace(t, codeNotebook(`import nbsandbox
for b in nbsandbox.stream("SELECT range AS i, md5(range::VARCHAR) AS h FROM range(1000000000)"):
    print("STREAMING", flush=True)
    break
for b in nbsandbox.stream("SELECT range AS i, md5(range::VARCHAR) AS h FROM range(1000000000)"):
    pass`))
	run := startRun(t, "alice", sp, "read.ipynb", nil)
	waitLog(t, "alice", sp, run.ID, "STREAMING", 3*time.Minute)
	compose(t, "restart", "nbsandboxd")
	if err := waitReady(time.Minute); err != nil {
		t.Fatal(err)
	}
	got := waitRun(t, "alice", sp, run.ID, 3*time.Minute)
	ok := got.Status == "interrupted"
	finding(t, "connections-run_interrupted", "restart nbsandboxd while a run streams from its fetcher",
		"the run ends as interrupted; its fetcher and network are removed", got.Status+": "+got.Message, ok)
	if !ok {
		t.Errorf("run: %+v", got)
	}
	assertNoReadLeftovers(t)
}
