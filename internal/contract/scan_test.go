// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func notebook(cells ...string) []byte {
	type cell struct {
		CellType string `json:"cell_type"`
		Source   string `json:"source"`
	}
	nb := struct {
		Cells []cell `json:"cells"`
	}{}
	for _, c := range cells {
		if strings.HasPrefix(c, "md:") {
			nb.Cells = append(nb.Cells, cell{"markdown", c[3:]})
		} else {
			nb.Cells = append(nb.Cells, cell{"code", c})
		}
	}
	b, _ := json.Marshal(nb)
	return b
}

func TestScan(t *testing.T) {
	for name, c := range map[string]struct {
		cells        []string
		output, emit []int
	}{
		"plain":            {[]string{"import nbsandbox", "nbsandbox.output({'a': 1})"}, []int{1}, nil},
		"alias":            {[]string{"import pandas as pd, nbsandbox as nb", "md:# results", "for x in xs:\n    nb.emit(x)"}, nil, []int{2}},
		"from import":      {[]string{"from nbsandbox import emit, output as out", "emit(1)", "out(2)"}, []int{2}, []int{1}},
		"from parens":      {[]string{"from nbsandbox import (emit)", "emit (1)"}, nil, []int{1}},
		"strings comments": {[]string{"import nbsandbox", "s = 'nbsandbox.emit(1)'\n# nbsandbox.emit(2)\nt = \"\"\"\nnbsandbox.emit(3)\n\"\"\"", "nbsandbox.output(1)"}, []int{2}, nil},
		"not imported":     {[]string{"nbsandbox.emit(1)"}, nil, nil},
		"other module":     {[]string{"import nbsandbox, foo", "foo.emit(1)", "x.nbsandbox.emit(2)"}, nil, nil},
		"magics":           {[]string{"%pip install x\nimport nbsandbox; nbsandbox.emit(1)"}, nil, []int{0}},
		"attribute only":   {[]string{"import nbsandbox", "f = nbsandbox.emit"}, nil, nil},
	} {
		got, err := Scan(notebook(c.cells...))
		if err != nil || !slices.Equal(got.Output, c.output) || !slices.Equal(got.Emit, c.emit) {
			t.Errorf("%s: %+v %v, want output %v emit %v", name, got, err, c.output, c.emit)
		}
	}
	if _, err := Scan([]byte("{")); err == nil {
		t.Error("invalid JSON scanned")
	}
}

func TestResolveModes(t *testing.T) {
	stream := mustSpec(t, `{"output": {"mode": "stream", "item": {"type": "object"}}}`)
	single := mustSpec(t, `{"output": {"mode": "single", "schema": {"type": "number"}}}`)
	plain := mustSpec(t, `{"output": {"type": "number"}}`)
	for name, c := range map[string]struct {
		c     *Contract
		calls Calls
		mode  string
		err   string
	}{
		"none, no calls":           {nil, Calls{}, ModeSingle, ""},
		"none, output":             {nil, Calls{Output: []int{2}}, ModeSingle, ""},
		"none, emit":               {nil, Calls{Emit: []int{3}}, ModeStream, ""},
		"both":                     {nil, Calls{Output: []int{1}, Emit: []int{3, 4}}, "", "nbsandbox.output (cell 1) and nbsandbox.emit (cells 3, 4)"},
		"stream declared, output":  {stream, Calls{Output: []int{5}}, "", "mode is stream, but cell 5 calls nbsandbox.output"},
		"stream declared, nothing": {stream, Calls{}, ModeStream, ""},
		"single declared, emit":    {single, Calls{Emit: []int{0}}, "", "mode is single, but cell 0 calls nbsandbox.emit"},
		"plain schema, emit":       {plain, Calls{Emit: []int{1}}, "", "a schema for one value"},
		"plain schema, output":     {plain, Calls{Output: []int{1}}, ModeSingle, ""},
	} {
		got, err := Resolve(c.c, c.calls)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: %v, want an error with %q", name, err, c.err)
			}
		case err != nil || got.Mode != c.mode:
			t.Errorf("%s: %+v %v, want %s", name, got, err, c.mode)
		}
	}
	if !stream.Stream() || !stream.ModeDeclared || plain.ModeDeclared || (*Contract)(nil).Stream() {
		t.Error("mode flags")
	}
	if err := stream.ValidateItem(json.RawMessage(`[1]`)); err == nil {
		t.Error("an item of the wrong type passed")
	}
	if err := stream.ValidateItem(json.RawMessage(`{"a": 1}`)); err != nil {
		t.Error(err)
	}
	if c := (*Contract)(nil).WithMode("", false); c.Mode != ModeSingle {
		t.Errorf("default mode %q", c.Mode)
	}
	for _, bad := range []string{`{"output": {"mode": "batch"}}`, `{"output": {"mode": "stream", "schema": {}}}`} {
		if _, err := FromSpec(mustRawSpec(t, bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if c, err := FromNotebook([]byte(`{"metadata": {"nbsandbox": {"contract": {"output": {"mode": "stream", "item": {}}}}}}`)); err != nil || !c.Stream() {
		t.Errorf("from notebook %+v %v", c, err)
	}
}

func mustRawSpec(t *testing.T, s string) Spec {
	t.Helper()
	var sp Spec
	if err := json.Unmarshal([]byte(s), &sp); err != nil {
		t.Fatal(err)
	}
	return sp
}

func mustSpec(t *testing.T, s string) *Contract {
	t.Helper()
	c, err := FromSpec(mustRawSpec(t, s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
