// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, runtime, name string, v any) {
	t.Helper()
	_ = os.MkdirAll(filepath.Join(dir, runtime), 0o755)
	b, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(dir, runtime, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAndRender(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "runc", "_facts.json", Facts{Runtime: "runc", DockerInfo: `"29.9.2" "Docker Desktop"`,
		NbsandboxRuntime: map[string]any{"image": map[string]any{"ref": "nbsandbox-runner:dev", "id": "sha256:x"}, "python_version": "3.13.18"}})
	write(t, dir, "runc", "space-paths.json", Finding{ID: "space-paths", Attempted: "read space B", Expected: "absent", Observed: "absent | ok", Pass: true})
	write(t, dir, "runc", "cpu-fairness.json", Finding{ID: "cpu-fairness", Attempted: "cpu", Expected: "≤1.5×", Observed: "2.0×", Pass: false})
	write(t, dir, "runsc", "space-paths.json", Finding{ID: "space-paths", Attempted: "read space B", Expected: "absent", Observed: "absent", Pass: true})

	runs, err := Load(dir)
	if err != nil || len(runs) != 2 || runs[0].Runtime != "runc" {
		t.Fatalf("load: %v %+v", err, runs)
	}
	if m := runs[0].Missing(); len(m) != len(Required)-2 {
		t.Fatalf("missing %v", m)
	}
	var buf bytes.Buffer
	if err := Render(&buf, runs, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"# nbsandbox Isolation Report", "non-reference platform", "| runc |", "| runsc |",
		"absent \\| ok", "✅ pass", "❌ **fail**", "not run", "Observed (runsc)", "## Isolation", "## Resources",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(out, "## Log streaming") {
		t.Error("sections without findings must be left out")
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("empty dir must be an error")
	}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "runc"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "runc", "X.json"), []byte("{"), 0o644)
	if _, err := Load(dir); err == nil {
		t.Fatal("bad JSON must be an error")
	}
}

func TestRedact(t *testing.T) {
	runs := []Run{{Runtime: "runc", Findings: map[string]Finding{"x": {ID: "x", Observed: "saw /home/someone/data and /home/someone/x"}}}}
	Redact(runs, "/home/someone", "~")
	if got := runs[0].Findings["x"].Observed; got != "saw ~/data and ~/x" {
		t.Fatalf("got %q", got)
	}
	Redact(runs, "", "~") // nothing to replace
}
