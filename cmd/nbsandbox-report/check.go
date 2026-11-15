// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/check"
	"github.com/azbyluthfan/nbsandbox/internal/report"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

const checkUsage = `Usage: nbsandbox-report check -url URL -user-a NAME -user-b NAME [flags]

Runs the isolation, resource and failure checks against a running nbsandbox
deployment through its API, as two users, and writes the isolation report for
that deployment. It creates its own spaces (nbsandbox-check-...) and deletes
them at the end. It never restarts or stops anything.

Credentials come from the environment, a password or a token per user:
  NBSANDBOX_CHECK_A_PASSWORD or NBSANDBOX_CHECK_A_TOKEN
  NBSANDBOX_CHECK_B_PASSWORD or NBSANDBOX_CHECK_B_TOKEN

Flags:
`

// runCheckCommand implements `nbsandbox-report check`. It returns the exit
// code: 0 all checks passed, 1 a check failed or couldn't run, 2 bad usage.
func runCheckCommand(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.Usage = func() { _, _ = fmt.Fprint(fs.Output(), checkUsage); fs.PrintDefaults() }
	base := fs.String("url", "", "the deployment's base URL (required), e.g. https://nbsandbox.example.com")
	userA := fs.String("user-a", "", "the first user (the one that probes); required")
	userB := fs.String("user-b", "", "a second, different user (the neighbour); required")
	out := fs.String("out", "isolation-report.md", "report file")
	findingsDir := fs.String("findings", "", "also keep the raw findings in this folder")
	kubeconfig := fs.String("kubeconfig", "", "Kubernetes deployments: a kubeconfig with read access, for the probe's extra targets and cluster facts")
	ns := fs.String("namespace", "nbsandbox", "with -kubeconfig: nbsandbox's namespace")
	runsNS := fs.String("runs-namespace", "nbsandbox-runs", "with -kubeconfig: the runs' namespace")
	var targets, only listFlag
	fs.Var(&targets, "target", "host:port a run must not reach (repeatable), in addition to the internet and the metadata service")
	fs.Var(&only, "only", "run only this check (repeatable): "+checkNames())
	dataDir := fs.String("data-dir", "", "the server's data folder, if known: the probe also tries its paths")
	skipSlow := fs.Bool("skip-slow", false, "skip time-limit, which takes the deployment's run timeout")
	caFile := fs.String("ca-file", "", "trust this CA certificate (PEM) for the deployment's TLS")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" || *userA == "" || *userB == "" || *userA == *userB {
		fmt.Fprintln(os.Stderr, "nbsandbox-report check: -url, -user-a and -user-b are required, and the users must differ")
		fs.Usage()
		return 2
	}
	a, errA := identity(*userA, "A")
	b, errB := identity(*userB, "B")
	if err := errors.Join(errA, errB); err != nil {
		fmt.Fprintln(os.Stderr, "nbsandbox-report check:", err)
		return 2
	}
	u, err := url.Parse(*base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintln(os.Stderr, "nbsandbox-report check: -url must be an http(s) URL")
		return 2
	}
	if u.Scheme == "http" && !isLocal(u.Hostname()) {
		fmt.Fprintln(os.Stderr, "nbsandbox-report check: refusing to send credentials over plain http to", u.Host)
		return 2
	}
	dt, _ := http.DefaultTransport.(*http.Transport)
	transport := dt.Clone()
	if *caFile != "" {
		pem, err := os.ReadFile(*caFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "nbsandbox-report check:", err)
			return 2
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			fmt.Fprintln(os.Stderr, "nbsandbox-report check: no certificate in", *caFile)
			return 2
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
	}
	e := &check.Env{
		Client:       &check.Client{BaseURL: strings.TrimSuffix(*base, "/"), HTTP: &http.Client{Timeout: 60 * time.Second, Transport: transport}},
		A:            a,
		B:            b,
		DataDir:      *dataDir,
		ExtraTargets: targets,
		Logf:         logf,
	}
	if err := e.Load(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "nbsandbox-report check:", err)
		return 1
	}
	facts := report.Facts{Runtime: e.Runtime, Backend: "docker", NbsandboxRuntime: e.Info, Checked: u.Scheme + "://" + u.Host,
		NotChecked: map[string]string{}, RecordedAt: time.Now().UTC()}
	if e.Kubernetes {
		facts.Backend = "kubernetes"
		for _, id := range report.NotOnKubernetes {
			facts.NotChecked[id] = "not applicable on Kubernetes (stops Docker Compose services)"
		}
	}
	if *kubeconfig != "" {
		k, err := check.ConnectKube(ctx, *kubeconfig, *ns, *runsNS)
		if err != nil {
			fmt.Fprintln(os.Stderr, "nbsandbox-report check:", err)
			return 1
		}
		e.ExtraTargets = append(e.ExtraTargets, k.Targets...)
		e.RunLeft = k.RunPod
		facts.Cluster = fmt.Sprintf("Kubernetes %s; NetworkPolicies in %s: %s", k.Version, *runsNS, strings.Join(k.Policies, ", "))
		if len(k.Policies) == 0 {
			facts.Cluster += "none"
		}
	}
	logf("checking %s (%s, runtime %s, run timeout %s) as %s and %s", facts.Checked, facts.Backend, e.Runtime, e.RunTimeout, a.Name, b.Name)

	for id, why := range check.NotChecked {
		facts.NotChecked[id] = why
	}
	run := report.Run{Runtime: e.Runtime, Facts: facts, Findings: map[string]report.Finding{}}
	failed := false
	for _, c := range check.All() {
		if len(only) > 0 && !contains(only, c.Name) {
			for _, id := range c.Findings {
				run.Facts.NotChecked[id] = "not selected (-only)"
			}
			continue
		}
		if c.Slow && *skipSlow {
			for _, id := range c.Findings {
				run.Facts.NotChecked[id] = "skipped (-skip-slow): takes the deployment's run timeout"
			}
			continue
		}
		logf("%s ...", c.Name)
		findings, err := c.Run(ctx, e)
		for _, f := range findings {
			run.Findings[f.ID] = f
			mark := "pass"
			if !f.Pass {
				mark, failed = "FAIL", true
			}
			logf("  %s: %s (%s)", f.ID, mark, f.Observed)
		}
		if err != nil {
			failed = true
			logf("  %s couldn't run: %v", c.Name, err)
		}
		if ctx.Err() != nil {
			logf("interrupted; the spaces it created are deleted")
			return 1
		}
	}

	if *findingsDir != "" {
		if err := writeFindings(*findingsDir, run); err != nil {
			fmt.Fprintln(os.Stderr, "nbsandbox-report check:", err)
			return 1
		}
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nbsandbox-report check:", err)
		return 1
	}
	if err := report.RenderFor(f, []report.Run{run}, time.Now(), "nbsandbox-report check"); err != nil {
		_ = f.Close()
		fmt.Fprintln(os.Stderr, "nbsandbox-report check:", err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "nbsandbox-report check:", err)
		return 1
	}
	missing := run.Missing()
	fmt.Printf("%s: %d findings, missing %v; wrote %s\n", run.Runtime, len(run.Findings), missing, *out)
	if failed || len(missing) > 0 {
		return 1
	}
	return 0
}

// identity reads a user's password or token from the environment.
func identity(name, which string) (*check.Identity, error) {
	pw := os.Getenv("NBSANDBOX_CHECK_" + which + "_PASSWORD")
	tok := os.Getenv("NBSANDBOX_CHECK_" + which + "_TOKEN")
	if (pw == "") == (tok == "") {
		return nil, fmt.Errorf("set exactly one of NBSANDBOX_CHECK_%s_PASSWORD and NBSANDBOX_CHECK_%s_TOKEN for %s", which, which, name)
	}
	return &check.Identity{Name: name, Password: pw, Token: tok}, nil
}

func isLocal(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost")
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func checkNames() string {
	var names []string
	for _, c := range check.All() {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// writeFindings saves the findings in the layout report.Load reads.
func writeFindings(dir string, run report.Run) error {
	d := filepath.Join(dir, run.Runtime)
	if err := os.MkdirAll(d, 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(run.Facts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(d, "_facts.json"), b, 0o600); err != nil {
		return err
	}
	for id, f := range run.Findings {
		b, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(d, id+".json"), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}
