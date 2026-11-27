// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
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

var (
	apiOnce sync.Once
	// apiCA signed the test API's HTTPS certificate.
	apiCA string
)

// apiSource starts the test API once per test binary, in the fetcher's own
// base image: HTTP on 8080, HTTPS on 8443. The script and certificate go in
// by environment, as for the TLS sources.
func apiSource(t *testing.T) {
	t.Helper()
	apiOnce.Do(func() {
		_, _ = runDocker("rm", "-fv", srcAPI)
		script, err := os.ReadFile("testdata/api_source.py")
		if err != nil {
			t.Fatal(err)
		}
		ca := newAuthority(t, "nbsandbox test API CA")
		apiCA = ca.pem
		cert, key := ca.leaf(t, srcAPI)
		docker(t, "run", "-d", "--name", srcAPI, "--network", sourcesNet(), "-e", "API_SCRIPT="+string(script),
			"-e", "TLS_CERT="+cert, "-e", "TLS_KEY="+key, "--entrypoint", "sh",
			"python:3.13-slim@sha256:09a17cad02668ae61ece136b4fd36ac3849e14f670ebedb637ea3e15580525bc",
			"-c", `printf '%s' "$API_SCRIPT" > /tmp/api.py && exec python /tmp/api.py`)
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
		"basic": 200.0, "oauth": 200.0, "open": 200.0, "plain": 200.0, "sdk_style": 200.0, "outside": 404.0}
	// What the isolation report shows.
	expectRefused(t, "connections-http_credentials", "a notebook looks for its HTTP connections' credentials in its environment",
		fmt.Sprintf("found: %v", out["secret_seen"]), out["secret_seen"] == false)
	expectRefused(t, "connections-http_direct", "a notebook calls an API directly, not through its fetcher",
		fmt.Sprint(out["direct"]), out["direct"] == "refused")
	expectRefused(t, "connections-http_no_token", "a request to the fetcher's HTTP port without the run's token",
		fmt.Sprintf("HTTP %v", out["no_token"]), out["no_token"] == 401.0)
	expectRefused(t, "connections-http_outside_path", "a request for a path outside the connection's URL (an encoded ..)",
		fmt.Sprintf("HTTP %v", out["encoded"]), out["encoded"] == 403.0)
	expectRefused(t, "connections-http_unbound", "an HTTP name the run's space doesn't bind",
		fmt.Sprint(out["missing"]), out["missing"] == "refused")
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

// HTTPS APIs: the API's certificate checked against the connection's ca,
// or the system's authorities without one.
func TestConnectionsHTTPTLS(t *testing.T) {
	needConnections(t)
	apiSource(t)
	other := newAuthority(t, "another CA").pem
	secure := func(ca string) string {
		s := map[string]any{"url": "https://" + srcAPI + ":8443/token-api/"}
		if ca != "" {
			s["ca"] = ca
		}
		return connection(t, "secure", "http", s, apiToken)
	}
	// Connections before the space: cleanups run last first, and a space's
	// connections can be deleted only once it is gone.
	ours, system, others := secure(apiCA), secure(""), secure(other)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"tls.ipynb": codeNotebook(`import nbsandbox
out = {}
for name in ["ours", "system", "other"]:
    try:
        out[name] = nbsandbox.http(name).get("orders").status_code
    except nbsandbox.APIError as e:
        out[name] = str(e)
nbsandbox.output(out)`)})
	bind(t, sp, "dev", map[string]map[string]string{"dev": {"ours": ours, "system": system, "other": others}})
	run := startRun(t, "alice", sp, "tls.ipynb", nil)
	res := runResult(t, "alice", sp, run.ID)
	if res.Status != "succeeded" {
		t.Fatalf("run: %+v", res)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out["ours"] != 200.0 {
		t.Errorf("with the connection's ca: %v", out["ours"])
	}
	for k, what := range map[string]string{
		"system": "an HTTPS API whose certificate is from an authority the system doesn't trust",
		"other":  "an HTTPS API whose certificate is from another authority than the connection's ca",
	} {
		s, _ := out[k].(string)
		expectRefused(t, "connections-http_tls_"+k, what, s, strings.Contains(s, "CERTIFICATE_VERIFY_FAILED"))
	}
}

const srcKeycloak = "nbsandbox-src-keycloak"

// OAuth 2.0 client credentials against a real Keycloak (the realm of make
// test-keycloak, started here as a source): the fetcher gets a token for
// reader-app and the API sees it.
func TestConnectionsHTTPKeycloak(t *testing.T) {
	needConnections(t)
	if os.Getenv("NBSANDBOX_TEST_KEYCLOAK_SOURCE") == "0" {
		t.Skip("NBSANDBOX_TEST_KEYCLOAK_SOURCE=0: no Keycloak source")
	}
	apiSource(t)
	realm, err := os.ReadFile("../../deploy/keycloak/realm.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = runDocker("rm", "-fv", srcKeycloak)
	docker(t, "run", "-d", "--name", srcKeycloak, "--network", sourcesNet(), "--memory", "1g",
		"-e", "REALM="+string(realm), "-e", "JAVA_OPTS_KC_HEAP=-XX:MaxRAMPercentage=50", "--entrypoint", "sh",
		"quay.io/keycloak/keycloak:26.8.1@sha256:3f3c5f8e03c614774b4bd9173b88391fefa4153bfc010d8012d59ad825377332", "-c",
		`mkdir -p /opt/keycloak/data/import && printf '%s' "$REALM" > /opt/keycloak/data/import/realm.json &&
		exec /opt/keycloak/bin/kc.sh start-dev --import-realm --http-port=8180`)
	t.Cleanup(func() { _, _ = runDocker("rm", "-fv", srcKeycloak) })
	retry(t, "Keycloak", 180*time.Second, func() (string, error) {
		return runDocker("exec", srcAPI, "python", "-c",
			"import urllib.request; urllib.request.urlopen('http://"+srcKeycloak+":8180/realms/nbsandbox/.well-known/openid-configuration')")
	})
	kc := connection(t, "kc", "http", map[string]any{"url": apiURL("keycloak-api/"), "auth": "oauth2", "client_id": "reader-app",
		"token_url": "http://" + srcKeycloak + ":8180/realms/nbsandbox/protocol/openid-connect/token"}, "reader-test-secret")
	wrong := connection(t, "kc-wrong", "http", map[string]any{"url": apiURL("keycloak-api/"), "auth": "oauth2", "client_id": "reader-app",
		"token_url": "http://" + srcKeycloak + ":8180/realms/nbsandbox/protocol/openid-connect/token"}, "not-the-secret")
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"kc.ipynb": codeNotebook(`import nbsandbox
out = {"ok": nbsandbox.http("kc").get("orders").status_code}
try:
    nbsandbox.http("wrong").get("orders")
    out["wrong"] = "allowed"
except nbsandbox.APIError as e:
    out["wrong"] = str(e)
nbsandbox.output(out)`)})
	bind(t, sp, "dev", map[string]map[string]string{"dev": {"kc": kc, "wrong": wrong}})
	run := startRun(t, "alice", sp, "kc.ipynb", nil)
	res := runResult(t, "alice", sp, run.ID)
	if res.Status != "succeeded" {
		t.Fatalf("run: %+v", res)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != 200.0 {
		t.Errorf("with Keycloak's token: %v", out["ok"])
	}
	if s, _ := out["wrong"].(string); !strings.Contains(s, "401") {
		t.Errorf("a wrong client secret: %v, want the token URL's 401", out["wrong"])
	}
}
