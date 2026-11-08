// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package deps

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const label = `{"schema_version":1,"status_schema_version":2,"python_version":"3.13.18",
 "distributions":{"pandas":"3.1.1","numpy":"2.5.4","scikit-learn":"1.8.0","pyyaml":"6.0.3","python-dateutil":"4.0.0"},
 "import_names":{"pandas":["pandas"],"numpy":["numpy"],"sklearn":["scikit-learn"],"yaml":["pyyaml"],"dateutil":["python-dateutil"]},
 "stdlib":["os","sys","json","collections","importlib"]}`

type fakeFiles map[string]string

func (f fakeFiles) ReadSpaceFile(_, p string, _ int64) ([]byte, bool) {
	b, ok := f[p]
	return []byte(b), ok
}
func (f fakeFiles) SpaceEntryExists(_, p string) bool { _, ok := f[p]; return ok }

func notebook(cells ...string) []byte {
	type cell struct {
		CellType string `json:"cell_type"`
		Source   any    `json:"source"`
	}
	nb := struct {
		Cells []cell `json:"cells"`
	}{}
	nb.Cells = append(nb.Cells, cell{"markdown", "import torch  # in markdown: ignored"})
	for i, c := range cells {
		if i%2 == 1 { // exercise both source encodings
			nb.Cells = append(nb.Cells, cell{"code", strings.SplitAfter(c, "\n")})
		} else {
			nb.Cells = append(nb.Cells, cell{"code", c})
		}
	}
	b, _ := json.Marshal(nb)
	return b
}

func TestCheck(t *testing.T) {
	m, err := ParseManifest(label)
	if err != nil {
		t.Fatal(err)
	}
	nb := notebook(
		"import os, sys\nimport pandas as pd\nfrom sklearn.linear_model import LinearRegression\nimport yaml",
		"import numpy.linalg\nfrom dateutil import parser\nfrom __future__ import annotations\nfrom . import sibling",
		"%pip install torch\n!pip install jax\n# import commented\nimport torch, tensorflow as tf  # trailing comment",
		"import helpers\nfrom mypkg.sub import x\n    import scipy  # indented, inside a function\nimport torch",
	)
	files := fakeFiles{
		"helpers.py":       "",
		"notebooks/mypkg":  "",
		"requirements.txt": "pandas>=2\nnumpy==2.5.4  # pinned\nscikit_learn\n-r other.txt\n\nrequests[socks]>=2 ; python_version>'3'\n--index-url x\nPyYAML\n",
	}
	got := Check(m, files, "s1", "notebooks/model.ipynb", nb)
	var desc []string
	for _, w := range got {
		cell := "-"
		if w.Cell != nil {
			cell = fmt.Sprint(*w.Cell)
		}
		desc = append(desc, fmt.Sprintf("%s@%s:%s", w.Module, w.Source, cell))
	}
	want := "torch@notebook:2 tensorflow@notebook:2 scipy@notebook:3 requests@requirements.txt:-"
	if strings.Join(desc, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(desc, " "), want)
	}
}

func TestCheckIgnoresBrokenNotebook(t *testing.T) {
	m, _ := ParseManifest(label)
	if w := Check(m, fakeFiles{}, "s", "x.ipynb", []byte("not json")); w != nil {
		t.Fatalf("got %v", w)
	}
}

func TestParseManifest(t *testing.T) {
	if _, err := ParseManifest(""); err == nil || !strings.Contains(err.Error(), "make runner-image") {
		t.Fatalf("missing label: %v", err)
	}
	if _, err := ParseManifest("{"); err == nil {
		t.Fatal("bad JSON accepted")
	}
	if _, err := ParseManifest(`{"status_schema_version":3}`); err == nil || !strings.Contains(err.Error(), "status format 3") {
		t.Fatalf("unsupported contract: %v", err)
	}
	m, _ := ParseManifest(label)
	for mod, want := range map[string]bool{"os": true, "sklearn": true, "pandas": true, "_thread": true, "torch": false} {
		if m.Provides(mod) != want {
			t.Errorf("Provides(%q) != %v", mod, want)
		}
	}
	if !m.Installed("Scikit_Learn") || !m.Installed("python.dateutil") || m.Installed("torch") {
		t.Fatal("Installed normalisation")
	}
}
