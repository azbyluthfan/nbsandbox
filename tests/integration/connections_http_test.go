// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// HTTP APIs through the run's fetcher, end to end (make test-connections):
// a test API on the sources' network (testdata/api_source.py), reached by
// the notebook's own code with each kind of credential, which never enters
// the run.

const (
	srcAPI          = "nbsandbox-src-api"
	apiToken        = "api-SECRET-tok-42"
	apiBasicPass    = "basic-SECRET-pw"
	apiClientSecret = "oauth-SECRET-cs"
)

var apiOnce sync.Once

// apiSource starts the test API once per test binary, in the fetcher's own
// base image.
func apiSource(t *testing.T) {
	t.Helper()
	apiOnce.Do(func() {
		_, _ = runDocker("rm", "-fv", srcAPI)
		script, err := filepath.Abs("testdata/api_source.py")
		if err != nil {
			t.Fatal(err)
		}
		docker(t, "run", "-d", "--name", srcAPI, "--network", sourcesNet(), "-v", script+":/srv/api.py:ro",
			"python:3.13-slim@sha256:09a17cad02668ae61ece136b4fd36ac3849e14f670ebedb637ea3e15580525bc", "python", "/srv/api.py")
		retry(t, "test API", 60*time.Second, func() (string, error) {
			return runDocker("exec", srcAPI, "python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/admin-hits')")
		})
	})
}

func apiURL(path string) string { return "http://" + srcAPI + ":8080/" + path }

// apiSpace binds one name per kind of credential.
func apiSpace(t *testing.T, notebook string) string {
	t.Helper()
	apiSource(t)
	tok := connection(t, "events", "http", map[string]any{"url": apiURL("token-api/")}, apiToken)
	basic := connection(t, "basic", "http", map[string]any{"url": apiURL("basic-api/"), "auth": "basic", "user": "ann"}, apiBasicPass)
	oauth := connection(t, "oauth", "http", map[string]any{"url": apiURL("oauth-api/"), "auth": "oauth2",
		"client_id": "nb-client", "token_url": apiURL("oauth/token")}, apiClientSecret)
	open := connection(t, "open", "http", map[string]any{"url": apiURL("open-api/"), "auth": "none"}, "")
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"api.ipynb": notebook})
	bind(t, sp, "dev", map[string]map[string]string{"dev": {"events": tok, "basic": basic, "oauth": oauth, "open": open}})
	return sp
}

func TestConnectionsHTTP(t *testing.T) {
	needConnections(t)
	sp := apiSpace(t, codeNotebook(`import nbsandbox, os, json, socket, requests
out = {}
api = nbsandbox.http("events")
out["orders"] = len(api.get("orders").json()["orders"])
out["upload"] = api.post("/upload", data=b"x" * (2 << 20)).json()["bytes"]
out["query"] = api.get("echo", params={"q": "a b"}).json()["path"]
out["events"] = [l for l in api.get("events", stream=True).iter_lines() if l.startswith(b"data:")].__len__()
for name in ["basic", "oauth", "open"]:
    r = nbsandbox.http(name).get("echo")
    out[name] = r.status_code
# Another client, and an SDK given the run's token as its API key.
c = nbsandbox.connection("events")
out["plain"] = requests.get(c.url + "orders", headers=c.headers).status_code
out["sdk_style"] = requests.get(c.url + "orders", headers={"Authorization": "Bearer " + c.token}).status_code
out["no_token"] = requests.get(c.url + "orders").status_code
# requests resolves "../" itself: /admin is no connection's name. Encoded,
# it reaches the fetcher, which refuses it.
out["outside"] = requests.get(c.url + "../admin", headers=c.headers).status_code
out["encoded"] = requests.get(c.url + "%2e%2e/admin", headers=c.headers).status_code
try:
    nbsandbox.http("missing")
    out["missing"] = "allowed"
except nbsandbox.APIError as e:
    out["missing"] = "refused"
# Not reachable without the fetcher; no credential anywhere in the run.
try:
    socket.create_connection(("`+srcAPI+`", 8080), timeout=3).close()
    out["direct"] = "allowed"
except OSError:
    out["direct"] = "refused"
blob = json.dumps(dict(os.environ))
out["secret_seen"] = any(s[::-1] in blob for s in ["`+reverse(apiToken)+`", "`+reverse(apiBasicPass)+`", "`+reverse(apiClientSecret)+`"])
out["connections"] = sorted((x["name"], x["type"]) for x in nbsandbox.connections())
nbsandbox.output(out)`))
	run := startRun(t, "alice", sp, "api.ipynb", nil)
	res := runResult(t, "alice", sp, run.ID)
	log := string(call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/log", "alice", nil, nil).Body)
	if res.Status != "succeeded" {
		t.Fatalf("run: %+v\n%s", res, log)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"orders": 10.0, "upload": float64(2 << 20), "query": "/token-api/echo?q=a+b", "events": 5.0,
		"basic": 200.0, "oauth": 200.0, "open": 200.0, "plain": 200.0, "sdk_style": 200.0, "no_token": 401.0,
		"outside": 404.0, "encoded": 403.0, "missing": "refused", "direct": "refused", "secret_seen": false}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s: got %v, want %v", k, out[k], v)
		}
	}
	if b, _ := json.Marshal(out["connections"]); string(b) != `[["basic","http"],["events","http"],["oauth","http"],["open","http"]]` {
		t.Errorf("connections: %s", b)
	}
	// The path outside every connection's URL was never reached, and no
	// credential is in the run's log.
	if hits := docker(t, "exec", srcAPI, "python", "-c",
		"import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8080/admin-hits').read().decode())"); !strings.Contains(hits, `"hits": []`) {
		t.Errorf("the API's admin path was reached: %s", hits)
	}
	for _, s := range []string{apiToken, apiBasicPass, apiClientSecret} {
		if strings.Contains(log, s) {
			t.Errorf("the run's log holds a credential")
		}
	}
	assertNoReadLeftovers(t)
}
