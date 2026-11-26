// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Finding is one test's evidence, as written by the integration tests.
type Finding struct {
	ID        string    `json:"id"`
	Attempted string    `json:"attempted"`
	Expected  string    `json:"expected"`
	Observed  string    `json:"observed"`
	Pass      bool      `json:"pass"`
	Test      string    `json:"test"`
	At        time.Time `json:"at"`
}

// Facts describe the platform a runtime's findings came from.
type Facts struct {
	Runtime          string         `json:"runtime"`
	Backend          string         `json:"backend"` // docker (also when empty) or kubernetes
	NbsandboxRuntime map[string]any `json:"nbsandbox_runtime"`
	DockerInfo       string         `json:"docker_info"`
	RecordedAt       time.Time      `json:"recorded_at"`
	// Checked is the deployment's URL when the findings come from
	// `nbsandbox-report check` (through its API) rather than the
	// integration suite.
	Checked string `json:"checked,omitempty"`
	// NotChecked are required findings a check leaves out on purpose, with
	// why (they stop services, or were skipped).
	NotChecked map[string]string `json:"not_checked,omitempty"`
	// Cluster describes the Kubernetes cluster when a check had a
	// kubeconfig (its version, the runs' NetworkPolicies).
	Cluster string `json:"cluster,omitempty"`
}

// Run is the findings of one runtime.
type Run struct {
	Runtime  string
	Facts    Facts
	Findings map[string]Finding
}

// Section groups related checks.
type Section struct {
	Title string
	IDs   []string
}

// Sections lists every test the report expects, in order. The isolation
// report proper is isolation, resources and cancellation; the rest is
// included as evidence when run.
var Sections = []Section{
	{"Isolation", []string{"space-paths", "canary-search", "proc-list", "memory-secret", "shared-memory", "socket-env-network", "privileges", "cross-user-api", "neighbour-unaffected"}},
	{"Resources", []string{"memory-limit", "cpu-fairness", "process-limit", "time-limit", "tmp-limit", "endless-output"}},
	{"Cancellation and errors", []string{"cancel-running", "cancel-queued", "cancel-at-completion", "caller-disconnect", "kernel-crash", "cell-exception", "docker-lost-mid-run", "render-failure"}},
	{"Functional and lifecycle", []string{"parameters-and-artifacts", "runtime-info", "queue-concurrency", "restart-mid-run", "graceful-shutdown", "delete-space"}},
	{"Log and event streaming", []string{"live-lines", "resume", "join-mid-run", "replay-after-finish", "slow-viewer", "through-proxy", "streamed-output"}},
	{"Interactive sessions", []string{"session-network", "session-isolation", "session-memory-limit", "session-read-only", "session-reattach"}},
	{"Web UI", []string{"web-ui"}},
	{"Compute view", []string{"compute-workers", "compute-run-usage"}},
	{"Notebook functions", []string{"function-invoke", "function-immutable", "function-errors", "read-only-invocation",
		"parallel-invocations", "status-stream", "start-latency"}},
	{"Cluster (make test-cluster)", []string{"cluster-worker-crash", "cluster-worker-stop", "cluster-api-crash", "cluster-postgres-blip"}},
	{"Connections (make test-connections, test-k8s)", []string{"connections-credentials",
		"connections-direct_db", "connections-direct_db_by_address", "connections-direct_s3", "connections-direct_s3_by_address",
		"connections-internet", "connections-gateway_with_runner_token", "connections-cross-run",
		"connections-pass_through", "connections-write", "connections-two_statements", "connections-local_file",
		"connections-other_bucket", "connections-dotdot", "connections-settings", "connections-db_role",
		"connections-shop_role", "connections-shop_pass_through", "connections-erp_role", "connections-erp_pass_through", "connections-erp_write",
		"connections-tls_pg_system", "connections-tls_pg_otherca", "connections-tls_pg_name", "connections-tls_pg_off",
		"connections-tls_my_name", "connections-tls_my_otherca", "connections-tls_lake_noca",
		"connections-fetcher_dies", "connections-session_fetcher_dies", "connections-source_restarts", "connections-gateway_restarts",
		"connections-nbsandboxd_restarts_session", "connections-run_interrupted", "connections-kubernetes_reference"}},
	{"Network plugin (make test-k8s-cni-refuses)", []string{"cni-refuses"}},
}

// Required is what the report must contain to count as complete.
var Required = []string{"space-paths", "canary-search", "proc-list", "memory-secret", "shared-memory", "socket-env-network", "privileges", "cross-user-api", "neighbour-unaffected", "memory-limit", "cpu-fairness", "process-limit", "time-limit", "tmp-limit", "endless-output",
	"cancel-running", "cancel-queued", "cancel-at-completion", "caller-disconnect", "kernel-crash", "cell-exception", "docker-lost-mid-run", "render-failure",
	"session-network", "session-isolation", "session-memory-limit", "session-read-only"}

// Load reads <dir>/<runtime>/*.json.
func Load(dir string) ([]Run, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var runs []Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r := Run{Runtime: e.Name(), Findings: map[string]Finding{}}
		files, _ := filepath.Glob(filepath.Join(dir, e.Name(), "*.json"))
		for _, f := range files {
			b, err := os.ReadFile(f) //nolint:gosec // report input chosen by the operator
			if err != nil {
				return nil, err
			}
			if filepath.Base(f) == "_facts.json" {
				if err := json.Unmarshal(b, &r.Facts); err != nil {
					return nil, fmt.Errorf("%s: %w", f, err)
				}
				continue
			}
			var fd Finding
			if err := json.Unmarshal(b, &fd); err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			r.Findings[fd.ID] = fd
		}
		if len(r.Findings) > 0 {
			runs = append(runs, r)
		}
	}
	// runc first, then runsc.
	sort.Slice(runs, func(i, j int) bool { return runs[i].Runtime < runs[j].Runtime })
	if len(runs) == 0 {
		return nil, fmt.Errorf("no findings in %s", dir)
	}
	return runs, nil
}

// NotOnKubernetes are required tests that can't run on the Kubernetes
// backend: they stop Docker Compose services. Deleting run Pods and worker
// Pods covers the same failures there.
var NotOnKubernetes = []string{"docker-lost-mid-run"}

// Kubernetes reports whether the findings came from the Kubernetes backend.
func (r Run) Kubernetes() bool { return r.Facts.Backend == "kubernetes" }

// Missing lists required tests a run lacks.
func (r Run) Missing() []string {
	var out []string
	for _, id := range Required {
		if r.Kubernetes() && slices.Contains(NotOnKubernetes, id) {
			continue
		}
		if _, skipped := r.Facts.NotChecked[id]; skipped {
			continue
		}
		if _, ok := r.Findings[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// Platform says whether the findings are from a reference platform.
func (r Run) Platform() string {
	if r.Facts.Checked != "" {
		return r.platformName() + " — **this deployment**, checked through its API at " + r.Facts.Checked
	}
	if r.Kubernetes() {
		if strings.Contains(r.Facts.DockerInfo, "Docker Desktop") {
			return "Kubernetes in kind on Docker Desktop, network policy by Cilium — **local cluster**: the controls work here; run the suite in each target cluster for its own report"
		}
		return r.platformName()
	}
	if strings.Contains(r.Facts.DockerInfo, "Docker Desktop") {
		return "Docker Desktop — **non-reference platform**: results show the controls work here, but the authoritative report comes from Linux with Docker Engine"
	}
	if r.Facts.DockerInfo == "" {
		return "unknown (no facts recorded)"
	}
	return "Linux with Docker Engine — reference platform"
}

// platformName is "Kubernetes v1.37.0" or "Docker 29.9.2 (Docker Desktop)",
// from /v1/runtime's platform fields.
func (r Run) platformName() string {
	d, _ := r.Facts.NbsandboxRuntime["docker"].(map[string]any)
	version, _ := d["version"].(string)
	os, _ := d["operating_system"].(string)
	if r.Kubernetes() {
		return strings.TrimSpace("Kubernetes " + version)
	}
	if os != "" {
		return strings.TrimSpace("Docker "+version) + " (" + os + ")"
	}
	return strings.TrimSpace("Docker " + version)
}

// Redact replaces old with repl in what the tests observed (for example
// the home folder in host paths, which names the person who ran them).
func Redact(runs []Run, old, repl string) {
	if old == "" {
		return
	}
	for _, r := range runs {
		for id, f := range r.Findings {
			f.Observed = strings.ReplaceAll(f.Observed, old, repl)
			r.Findings[id] = f
		}
	}
}

func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

// Render writes the Markdown report.
func Render(w io.Writer, runs []Run, generated time.Time) error {
	return RenderFor(w, runs, generated, "")
}

// RenderFor is Render naming the make target that produced the report
// ("" = make report, or make report-k8s for Kubernetes findings).
func RenderFor(w io.Writer, runs []Run, generated time.Time, target string) error {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	title, deflt := "nbsandbox Isolation Report", "make report"
	source := "the integration tests' findings"
	switch {
	case len(runs) > 0 && runs[0].Facts.Checked != "":
		title, deflt, source = "nbsandbox Isolation Report: "+runs[0].Facts.Checked, "nbsandbox-report check", "checks run through the deployment's API"
	case len(runs) > 0 && runs[0].Kubernetes():
		title, deflt = "nbsandbox Isolation Report: Kubernetes backend", "make report-k8s"
	case len(runs) > 0 && strings.HasPrefix(runs[0].Platform(), "Linux"):
		title = "nbsandbox Isolation Report: Linux with Docker Engine"
	}
	if target == "" {
		target = deflt
	}
	p("# %s\n\n", title)
	p("Generated %s by `"+target+"` from "+source+". Each row is a test that **ran**; the observed column is what the test saw, not what it assumed.\n\n", generated.UTC().Format("2006-01-02 15:04 MST"))

	p("## Summary\n\n| Runtime | Platform | Passed | Failed | Required tests missing |\n|---|---|---|---|---|\n")
	for _, r := range runs {
		pass, fail := 0, 0
		for _, f := range r.Findings {
			if f.Pass {
				pass++
			} else {
				fail++
			}
		}
		missing := strings.Join(r.Missing(), ", ")
		if missing == "" {
			missing = "none"
		}
		p("| %s | %s | %d | %d | %s |\n", r.Runtime, cell(r.Platform()), pass, fail, missing)
	}

	p("\n## Environment\n\n")
	for _, r := range runs {
		nr := r.Facts.NbsandboxRuntime
		img, _ := nr["image"].(map[string]any)
		limits, _ := json.Marshal(nr["limits"])
		docker, _ := nr["docker"].(map[string]any)
		p("**%s**\n\n", r.Runtime)
		switch {
		case r.Facts.Checked != "":
			p("- Platform: %v (%v)\n", docker["version"], docker["operating_system"])
			if r.Facts.Cluster != "" {
				p("- Cluster: %s\n", r.Facts.Cluster)
			}
		case r.Kubernetes():
			p("- Kubernetes: %v (%v); the Docker underneath: %s\n", docker["version"], docker["operating_system"], r.Facts.DockerInfo)
			p("- Not applicable: %s (stops Docker Compose services)\n", strings.Join(NotOnKubernetes, ", "))
		default:
			p("- Docker: %v (%v); `docker info`: %s\n", docker["version"], docker["operating_system"], r.Facts.DockerInfo)
		}
		p("- Runner image: `%v` `%v`, Python %v\n", img["ref"], img["id"], nr["python_version"])
		p("- Limits per run: `%s`\n", limits)
		p("- Facts recorded: %s\n\n", r.Facts.RecordedAt.UTC().Format(time.RFC3339))
	}

	for _, sec := range Sections {
		any := false
		for _, r := range runs {
			for _, id := range sec.IDs {
				if _, ok := r.Findings[id]; ok {
					any = true
				}
			}
		}
		if !any {
			continue
		}
		p("## %s\n\n", sec.Title)
		p("| Check | Attempted | Expected |")
		for _, r := range runs {
			p(" Observed (%s) | %s |", r.Runtime, r.Runtime)
		}
		p("\n|---|---|---|")
		for range runs {
			p("---|---|")
		}
		p("\n")
		for _, id := range sec.IDs {
			var first Finding
			for _, r := range runs {
				if f, ok := r.Findings[id]; ok {
					first = f
					break
				}
			}
			if first.ID == "" {
				continue
			}
			p("| %s | %s | %s |", id, cell(first.Attempted), cell(first.Expected))
			for _, r := range runs {
				f, ok := r.Findings[id]
				switch {
				case !ok:
					p(" — | not run |")
				case f.Pass:
					p(" %s | ✅ pass |", cell(f.Observed))
				default:
					p(" %s | ❌ **fail** |", cell(f.Observed))
				}
			}
			p("\n")
		}
		p("\n")
	}

	for _, r := range runs {
		if len(r.Facts.NotChecked) == 0 {
			continue
		}
		p("## Not checked here (%s)\n\n", r.Runtime)
		ids := make([]string, 0, len(r.Facts.NotChecked))
		for id := range r.Facts.NotChecked {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			p("- **%s**: %s\n", id, r.Facts.NotChecked[id])
		}
		p("\nThe integration suite covers these on the reference platforms ([isolation-report.md](https://github.com/azbyluthfan/nbsandbox/blob/main/docs/isolation-report.md)).\n\n")
	}

	p("## Notes\n\n")
	p("- **cpu-fairness** gates only on the Linux reference host (`NBSANDBOX_CPU_FAIRNESS_GATE=1`); elsewhere the ratio is recorded and the row passes if the test ran.\n")
	p("- **cancel-at-completion** runs 5 rounds with real containers; the 200-round race with exact timing control is a unit test (`internal/runs` TestCancelCompletionRace).\n")
	p("- **Retention** is a unit test with a fake clock (the minimum retention is one hour), so it has no row here.\n")
	p("- Known limitations are in [security/threat-model.md](security/threat-model.md).\n")

	_, err := io.WriteString(w, b.String())
	return err
}
