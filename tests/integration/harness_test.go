// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package integration drives a running nbsandbox stack through its API, as
// alice and bob. Run with `make test-integration`, which starts
// the dev stack with short shutdown and timeout settings.
//
//	NBSANDBOX_URL       default http://127.0.0.1:8080
//	NBSANDBOX_COMPOSE   compose file (default deploy/docker-compose.dev.yml)
//	NBSANDBOX_DATA_DIR  data directory of the stack (needed by compose)
//	NBSANDBOX_CLUSTER   set for the cluster stack (API and worker roles; make test-cluster)
//	REPORT_DIR        if set, each test writes its findings here (for make report)
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/apispec"
)

var (
	baseURL     = envOr("NBSANDBOX_URL", "http://127.0.0.1:8080")
	composeFile = envOr("NBSANDBOX_COMPOSE", filepath.Join(repoRoot(), "deploy", "docker-compose.dev.yml"))
	passwords   = map[string]string{"alice": "alice-dev-password", "bob": "bob-dev-password"}
	httpClient  = &http.Client{Timeout: 60 * time.Second}
	// clustered: API nodes behind a load balancer, separate workers. The
	// API's readiness doesn't follow Docker, and there is no web UI.
	clustered = os.Getenv("NBSANDBOX_CLUSTER") != ""
	// onKubernetes: the local Kubernetes stack (make test-k8s): runs are
	// Pods, and there are no Compose services to stop or kill.
	onKubernetes = os.Getenv("NBSANDBOX_K8S") != ""
	// kindNode is the local kind cluster's node container (make sets it
	// for the chosen network plugin).
	kindNode = envOr("NBSANDBOX_KIND_NODE", "nbsandbox-control-plane")
	// expectOpenNetwork: the cluster's network plugin doesn't enforce
	// NetworkPolicy, so no worker should become ready (make
	// test-k8s-cni-refuses).
	expectOpenNetwork = os.Getenv("NBSANDBOX_EXPECT_OPEN_NETWORK") != ""
)

// runTimeout is the stack's run time limit (make sets
// NBSANDBOX_RUN_TIMEOUT for the stack and the tests alike).
var runTimeout = func() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("NBSANDBOX_RUN_TIMEOUT")); err == nil {
		return d
	}
	return time.Minute
}()

// kubectl runs kubectl inside the local kind node.
func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	return docker(t, append([]string{"exec", kindNode, "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf"}, args...)...)
}

// needCompose skips a test that stops or kills Compose services.
func needCompose(t *testing.T) {
	t.Helper()
	if onKubernetes {
		t.Skip("stops Docker Compose services; the Kubernetes stack has none (its failures are Pod deletions)")
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// runtimeLabel is the runtime under test ("runc" or "runsc"), from /v1/runtime.
var runtimeLabel = "runc"

func TestMain(m *testing.M) {
	if err := waitReady(90 * time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "nbsandbox stack not ready at %s: %v\nstart it with `make up` (or use `make test-integration`)\n", baseURL, err)
		os.Exit(1)
	}
	if err := recordFacts(); err != nil {
		fmt.Fprintln(os.Stderr, "recording host facts:", err)
	}
	os.Exit(m.Run())
}

// recordFacts saves what the report needs to describe the platform.
func recordFacts() error {
	req, _ := http.NewRequest("GET", baseURL+"/v1/runtime", nil)
	req.SetBasicAuth("alice", passwords["alice"])
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var rt map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rt); err != nil {
		return err
	}
	if r, ok := rt["runtime"].(string); ok && r != "" {
		runtimeLabel = r
	}
	dir := os.Getenv("REPORT_DIR")
	if dir == "" {
		return nil
	}
	info, _ := exec.Command("docker", "info", "--format",
		"{{json .ServerVersion}} {{json .OperatingSystem}} {{json .KernelVersion}} {{json .CgroupVersion}} {{json .NCPU}} {{json .MemTotal}} {{json .Architecture}}").Output()
	delete(rt, "packages")
	backend := "docker"
	if onKubernetes {
		backend = "kubernetes"
	}
	facts := map[string]any{"runtime": runtimeLabel, "backend": backend, "nbsandbox_runtime": rt, "docker_info": strings.TrimSpace(string(info)),
		"recorded_at": time.Now().UTC()}
	b, _ := json.MarshalIndent(facts, "", "  ")
	_ = os.MkdirAll(filepath.Join(dir, runtimeLabel), 0o755)
	return os.WriteFile(filepath.Join(dir, runtimeLabel, "_facts.json"), b, 0o644)
}

// workerReady reports whether some worker can take runs (cluster stack: the
// API nodes' readiness doesn't follow Docker).
func workerReady() bool {
	req, _ := http.NewRequest("GET", baseURL+"/v1/runtime", nil)
	req.SetBasicAuth("alice", passwords["alice"])
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := httpClient.Get(baseURL + "/readyz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && (!clustered || expectOpenNetwork || workerReady()) {
				return nil
			}
			last = string(b)
		} else {
			last = err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("last answer: %s", last)
}

// response is an API answer.
type response struct {
	Code   int
	Body   []byte
	Header http.Header
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("not JSON (%d): %s", r.Code, r.Body)
	}
}

func call(t *testing.T, method, path, user string, body io.Reader, headers map[string]string) response {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, passwords[user])
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	checkSpec(t, method, req.URL.Path, resp, b)
	return response{Code: resp.StatusCode, Body: b, Header: resp.Header}
}

var (
	specOnce sync.Once
	apiSpec  *apispec.Spec
	specErr  error
)

// checkSpec fails the test if a response doesn't match api/openapi.yaml.
// Unclean paths (Go's router redirects them) and undocumented paths that
// tests request on purpose are skipped.
func checkSpec(t *testing.T, method, p string, resp *http.Response, body []byte) {
	t.Helper()
	specOnce.Do(func() { apiSpec, specErr = apispec.Load(filepath.Join(repoRoot(), "api", "openapi.yaml")) })
	if specErr != nil {
		t.Fatal(specErr)
	}
	if path.Clean(p) != p || !apiSpec.Covered(method, p) {
		return
	}
	if err := apiSpec.Validate(method, p, resp.StatusCode, resp.Header, body); err != nil {
		t.Errorf("response doesn't match api/openapi.yaml: %v", err)
	}
}

func jsonCall(t *testing.T, method, path, user string, body any, headers map[string]string) response {
	t.Helper()
	b, _ := json.Marshal(body)
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	return call(t, method, path, user, bytes.NewReader(b), h)
}

// space creates a space for user, removed when the test ends; at the end the
// test also checks no run container is left.
func space(t *testing.T, user string) string {
	t.Helper()
	r := jsonCall(t, "POST", "/v1/spaces", user, map[string]string{"name": t.Name()}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("create space: %d %s", r.Code, r.Body)
	}
	var sp struct{ ID string }
	r.json(t, &sp)
	t.Cleanup(func() {
		// Cancel anything still active, then delete.
		var list struct{ Items []struct{ ID, Status string } }
		call(t, "GET", "/v1/spaces/"+sp.ID+"/runs", user, nil, nil).json(t, &list)
		for _, run := range list.Items {
			if run.Status == "queued" || run.Status == "running" {
				call(t, "POST", "/v1/spaces/"+sp.ID+"/runs/"+run.ID+"/cancel", user, nil, nil)
				waitRun(t, user, sp.ID, run.ID, 30*time.Second)
			}
		}
		if r := call(t, "DELETE", "/v1/spaces/"+sp.ID, user, nil, nil); r.Code != http.StatusNoContent {
			t.Errorf("delete space: %d %s", r.Code, r.Body)
		}
		assertNoRunContainers(t)
	})
	return sp.ID
}

// upload sends workload notebooks (from tests/workloads) and extra files.
func upload(t *testing.T, user, spaceID, dir string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range files {
		if content == "" || content == "@probes" { // a workload (or probe) notebook by name
			folder := "workloads"
			if content == "@probes" {
				folder = "probes"
			}
			b, err := os.ReadFile(filepath.Join(repoRoot(), "tests", folder, name))
			if err != nil {
				t.Fatal(err)
			}
			content = string(b)
		}
		w, _ := mw.CreateFormFile("file", name)
		_, _ = w.Write([]byte(content))
	}
	_ = mw.Close()
	q := ""
	if dir != "" {
		q = "?folder=" + dir
	}
	r := call(t, "POST", "/v1/spaces/"+spaceID+"/files"+q, user, &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
	if r.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.Code, r.Body)
	}
}

// Run is the API's run view (the parts tests read).
type Run struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	ErrorKind     string          `json:"error_kind"`
	Fault         string          `json:"fault"`
	Message       string          `json:"message"`
	QueuePosition int             `json:"queue_position"`
	Parameters    json.RawMessage `json:"parameters"`
	StartedAt     *time.Time      `json:"started_at"`
	Result        *struct {
		Status   string `json:"status"`
		Rendered bool   `json:"rendered"`
		Partial  bool   `json:"partial"`
		Error    *struct {
			CellIndex     *int   `json:"cell_index"`
			ExceptionType string `json:"exception_type"`
			Message       string `json:"exception_message"`
		} `json:"error"`
		Usage struct {
			PeakMemoryBytes *int64 `json:"peak_memory_bytes"`
		} `json:"usage"`
	} `json:"result"`
}

func startRun(t *testing.T, user, spaceID, notebook string, params map[string]any) Run {
	t.Helper()
	body := map[string]any{"notebook": notebook}
	if params != nil {
		body["parameters"] = params
	}
	r := jsonCall(t, "POST", "/v1/spaces/"+spaceID+"/runs", user, body, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("start run: %d %s", r.Code, r.Body)
	}
	var run Run
	r.json(t, &run)
	return run
}

func getRun(t *testing.T, user, spaceID, runID string) Run {
	t.Helper()
	var run Run
	r := call(t, "GET", "/v1/spaces/"+spaceID+"/runs/"+runID, user, nil, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("get run: %d %s", r.Code, r.Body)
	}
	r.json(t, &run)
	return run
}

func terminal(s string) bool { return s != "queued" && s != "running" }

func waitRun(t *testing.T, user, spaceID, runID string, timeout time.Duration) Run {
	t.Helper()
	// A slower stack (a longer run timeout) gets proportionally longer.
	timeout = max(timeout, runTimeout+time.Minute)
	deadline := time.Now().Add(timeout)
	for {
		run := getRun(t, user, spaceID, runID)
		if terminal(run.Status) {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s still %s after %s", runID, run.Status, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// waitLog waits until the run's log contains want.
func waitLog(t *testing.T, user, spaceID, runID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := call(t, "GET", "/v1/spaces/"+spaceID+"/runs/"+runID+"/log", user, nil, nil)
		if strings.Contains(string(r.Body), want) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("log of %s never contained %q", runID, want)
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func compose(t *testing.T, args ...string) {
	t.Helper()
	docker(t, append([]string{"compose", "-f", composeFile}, args...)...)
}

// runContainers lists containers labelled as runs.
func runContainers(t *testing.T) []string {
	t.Helper()
	if onKubernetes {
		out := kubectl(t, "-n", "nbsandbox-runs", "get", "pods", "-l", "nbsandbox.run", "-o", "name")
		if out == "" {
			return nil
		}
		return strings.Split(out, "\n")
	}
	out := docker(t, "ps", "-a", "--filter", "label=nbsandbox.run", "--format", "{{.Names}} {{.Status}}")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// assertNoRunContainers checks, after every test, that no run container is left.
func assertNoRunContainers(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		left := runContainers(t)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("no-containers-left: run containers left behind: %v", left)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// finding records one row for the isolation report.
func finding(t *testing.T, id, attempted, expected, observed string, pass bool) {
	t.Helper()
	t.Logf("%s: %s → %s (expected %s)", id, attempted, observed, expected)
	dir := os.Getenv("REPORT_DIR")
	if dir == "" {
		return
	}
	b, _ := json.MarshalIndent(map[string]any{"id": id, "attempted": attempted, "expected": expected,
		"observed": observed, "pass": pass, "test": t.Name(), "at": time.Now().UTC()}, "", "  ")
	dir = filepath.Join(dir, runtimeLabel)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, id+".json"), b, 0o644)
}

// dockerDesktop reports whether the Docker daemon is Docker Desktop's VM.
func dockerDesktop() bool {
	out, err := exec.Command("docker", "info", "--format", "{{.OperatingSystem}}").Output()
	return err == nil && strings.Contains(string(out), "Docker Desktop")
}
