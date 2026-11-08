// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Command nbsandbox-report renders docs/isolation-report.md from the findings
// the integration tests write.
//
//	nbsandbox-report -in bin/findings -out docs/isolation-report.md [-require]
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/report"
)

func main() {
	in := flag.String("in", "bin/findings", "findings directory (<runtime>/<ID>.json)")
	out := flag.String("out", "docs/isolation-report.md", "report file")
	require := flag.Bool("require", false, "fail if a required test is missing or failed")
	target := flag.String("target", "", "the make target named in the report (default: make report, or make report-k8s)")
	flag.Parse()

	runs, err := report.Load(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nbsandbox-report:", err)
		os.Exit(1)
	}
	// Host paths in the findings would name whoever ran the tests.
	if home, err := os.UserHomeDir(); err == nil && len(home) > 1 {
		report.Redact(runs, home, "~")
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nbsandbox-report:", err)
		os.Exit(1)
	}
	if err := report.RenderFor(f, runs, time.Now(), *target); err != nil {
		fmt.Fprintln(os.Stderr, "nbsandbox-report:", err)
		os.Exit(1)
	}
	_ = f.Close()

	bad := false
	for _, r := range runs {
		var failed []string
		for id, fd := range r.Findings {
			if !fd.Pass {
				failed = append(failed, id)
			}
		}
		fmt.Printf("%s: %d findings, missing %v, failed %v\n", r.Runtime, len(r.Findings), r.Missing(), failed)
		bad = bad || len(r.Missing()) > 0 || len(failed) > 0
	}
	fmt.Println("wrote", *out)
	if *require && bad {
		fmt.Fprintln(os.Stderr, "nbsandbox-report: required tests missing or failed")
		os.Exit(1)
	}
}
