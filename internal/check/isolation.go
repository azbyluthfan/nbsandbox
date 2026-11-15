// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
	"github.com/azbyluthfan/nbsandbox/internal/report"
)

// The isolation suite. B's space holds a canary file and a running holder
// with a secret in memory and one in /dev/shm; A's probe tries to reach all
// of it, then B tries to reach A's run through the API.
var isolationCheck = &Check{
	Name: "isolation",
	Findings: []string{"space-paths", "canary-search", "proc-list", "memory-secret", "shared-memory", "socket-env-network",
		"privileges", "cross-user-api", "neighbour-unaffected"},
	run: runIsolation,
}

// isolationRows describe the probe's findings.
var isolationRows = []struct{ id, attempted, expected string }{
	{"space-paths", "read space B by absolute host path, by ../, and by listing / and /space/..; reach this space's other runs and published versions; list /run",
		"none of those paths exist; /run holds only this run's folder"},
	{"canary-search", "search the container filesystem for bob's canary", "not found"},
	{"proc-list", "list /proc", "only alice's own processes"},
	{"memory-secret", "search readable /proc/*/environ, cmdline and mem for the holder's in-memory secret", "not found"},
	{"shared-memory", "read /dev/shm", "holder's shared-memory secret absent"},
	{"socket-env-network", "look for the Docker socket; list environment variables; try the network", "no socket; only allowlisted variables; no network (on Kubernetes: nothing reachable, the API server, worker, kubelet and metadata service included)"},
	{"privileges", "id, CapEff, write to /usr, chmod u+s", "uid 1000; CapEff 0; NoNewPrivs; writes fail; setuid ineffective (nosuid /tmp, or no-new-privileges on Kubernetes)"},
}

// holderMargin is how much of the run timeout the holder leaves unused: its
// kernel's start counts towards the timeout, and on Kubernetes that is slow.
func (e *Env) holderMargin() time.Duration {
	if e.Kubernetes {
		return 30 * time.Second
	}
	return 8 * time.Second
}

func runIsolation(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
	canaryA, canaryB := "CANARY"+randomHex(8), randomHex(8)
	memA, memB := "MEMSECRET"+randomHex(8), randomHex(8)
	shmA, shmB := "SHMSECRET"+randomHex(8), randomHex(8)

	// B: canary and holder. The holder runs as long as it can under the run
	// timeout, to outlast the probe; it can't be released early (a run works
	// on a copy of the space, so it never sees a file uploaded later).
	spB, err := e.space(ctx, e.B, map[string][]byte{"canary.txt": []byte(canaryA + canaryB + "\n"), "holder.ipynb": probe("holder.ipynb")})
	if err != nil {
		return nil, err
	}
	holdFor := min(e.RunTimeout-e.holderMargin(), 5*time.Minute)
	holder, err := e.Client.StartRun(ctx, e.B, spB, "holder.ipynb", map[string]any{"mem_a": memA, "mem_b": memB, "shm_a": shmA, "shm_b": shmB,
		"max_seconds": int(holdFor.Seconds())})
	if err != nil {
		return nil, err
	}
	if err := e.Client.WaitLog(ctx, e.B, spB, holder.ID, "holder ready", 3*time.Minute); err != nil {
		return nil, err
	}

	// A: the probe, while the holder is running.
	spA, err := e.space(ctx, e.A, map[string][]byte{"isolation_probe.ipynb": probe("isolation_probe.ipynb")})
	if err != nil {
		return nil, err
	}
	hostPath := ""
	if e.DataDir != "" {
		hostPath = path.Join(e.DataDir, "spaces", spB, "files")
	}
	targets := e.ExtraTargets
	if targets == nil {
		targets = []string{}
	}
	run, err := e.Client.StartRun(ctx, e.A, spA, "isolation_probe.ipynb", map[string]any{
		"space_a_id": spA, "space_b_id": spB, "space_b_host_path": hostPath, "data_dir": e.DataDir,
		"canary_a": canaryA, "canary_b": canaryB, "mem_a": memA, "mem_b": memB, "shm_a": shmA, "shm_b": shmB,
		"kubernetes": e.Kubernetes, "extra_targets": targets,
	})
	if err != nil {
		return nil, err
	}
	run, err = e.Client.WaitRun(ctx, e.A, spA, run.ID, e.waitTimeout(3*time.Minute))
	if err != nil {
		return nil, err
	}
	if run.Status != "succeeded" {
		log, _ := e.Client.Log(ctx, e.A, spA, run.ID)
		return nil, fmt.Errorf("the probe run %s: %s\n%s", run.Status, run.Message, tail(log, 2000))
	}
	h, err := e.Client.GetRun(ctx, e.B, spB, holder.ID)
	if err != nil {
		return nil, fmt.Errorf("the holder's status after the probe: %w", err)
	}
	if h.Status != "running" {
		return nil, fmt.Errorf("the holder was not running during the probe: %s", h.Status)
	}

	r, err := apiclient.Expect(e.Client.Do(ctx, e.A, "GET", "/v1/spaces/"+spA+"/runs/"+run.ID+"/artifacts/findings.json", nil, nil))(http.StatusOK, "the probe's findings")
	if err != nil {
		return nil, err
	}
	var raw map[string]map[string]any
	if err := r.JSON(&raw); err != nil {
		return nil, err
	}
	var out []report.Finding
	for _, row := range isolationRows {
		f := raw[row.id]
		pass, _ := f["pass"].(bool)
		out = append(out, c.finding(row.id, row.attempted, row.expected, Summarize(row.id, f), pass))
	}

	// B reaches nothing of A's probe run through the API.
	base := "/v1/spaces/" + spA + "/runs/" + run.ID
	var codes []string
	allHidden := true
	for _, p := range []string{base, base + "/log", base + "/log/stream", base + "/output.ipynb", base + "/output.html",
		base + "/artifacts", base + "/artifacts/findings.json", "/v1/spaces/" + spA + "/files/isolation_probe.ipynb"} {
		r, err := e.Client.Do(ctx, e.B, "GET", p, nil, nil)
		if err != nil {
			return out, err
		}
		codes = append(codes, fmt.Sprint(r.Code))
		allHidden = allHidden && r.Code == http.StatusNotFound
	}
	out = append(out, c.finding("cross-user-api", "bob streams and downloads alice's run log, outputs and artifacts", "404 on every call",
		strings.Join(codes, " "), allHidden))

	// The holder must finish normally with its secrets intact.
	h, err = e.Client.WaitRun(ctx, e.B, spB, holder.ID, e.waitTimeout(2*time.Minute))
	if err != nil {
		return out, err
	}
	hlog, _ := e.Client.Log(ctx, e.B, spB, holder.ID)
	out = append(out, c.finding("neighbour-unaffected", "the holder's run after the probes", "completed normally, secrets intact", h.Status,
		h.Status == "succeeded" && strings.Contains(hlog, "holder done intact=True")))
	return out, nil
}

// Summarize turns a probe's raw findings into one line of evidence.
func Summarize(id string, f map[string]any) string {
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

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
