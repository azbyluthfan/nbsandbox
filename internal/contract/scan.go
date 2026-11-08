// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Calls are the cells (0-based, counting every cell, as error cell_index
// does) that call nbsandbox.output and nbsandbox.emit.
type Calls struct {
	Output, Emit []int
}

var (
	importModRE = regexp.MustCompile(`^\s*import\s+(.+)$`)
	fromModRE   = regexp.MustCompile(`^\s*from\s+nbsandbox\s+import\s+\(?([^)]*)\)?\s*$`)
	aliasPartRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.]*)(?:\s+as\s+([A-Za-z_][A-Za-z0-9_]*))?$`)
)

// Scan finds the calls in a notebook's code cells. It follows the usual
// ways of importing the module (import nbsandbox [as x], from nbsandbox
// import output [as y]) and ignores strings and comments. Calls made
// indirectly (through another variable, getattr, or a helper module) are
// not seen; the run itself still enforces the mode.
func Scan(nb []byte) (Calls, error) {
	var doc struct {
		Cells []struct {
			CellType string          `json:"cell_type"`
			Source   json.RawMessage `json:"source"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(nb, &doc); err != nil {
		return Calls{}, &Error{Msg: "notebook is not valid JSON"}
	}
	modules := map[string]bool{} // names the module is bound to
	funcs := map[string]string{} // name -> output | emit
	var calls Calls
	for i, c := range doc.Cells {
		if c.CellType != "code" {
			continue
		}
		src := blankStringsAndComments(cellSource(c.Source))
		var lines []string
		for _, line := range strings.Split(src, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "%") || strings.HasPrefix(t, "!") {
				continue // IPython magics and shell commands
			}
			lines = append(lines, line)
			for _, stmt := range strings.Split(line, ";") {
				bindImports(stmt, modules, funcs)
			}
		}
		code := strings.Join(lines, "\n")
		var out, emit bool
		for mod := range modules {
			m := regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])` + regexp.QuoteMeta(mod) + `\s*\.\s*(output|emit)\s*\(`)
			for _, g := range m.FindAllStringSubmatch(code, -1) {
				out, emit = out || g[1] == "output", emit || g[1] == "emit"
			}
		}
		for name, fn := range funcs {
			if regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])` + regexp.QuoteMeta(name) + `\s*\(`).MatchString(code) {
				out, emit = out || fn == "output", emit || fn == "emit"
			}
		}
		if out {
			calls.Output = append(calls.Output, i)
		}
		if emit {
			calls.Emit = append(calls.Emit, i)
		}
	}
	return calls, nil
}

// bindImports records what an import statement binds.
func bindImports(stmt string, modules map[string]bool, funcs map[string]string) {
	if m := fromModRE.FindStringSubmatch(stmt); m != nil {
		for _, part := range strings.Split(m[1], ",") {
			if p := aliasPartRE.FindStringSubmatch(strings.TrimSpace(part)); p != nil && (p[1] == "output" || p[1] == "emit") {
				name := p[1]
				if p[2] != "" {
					name = p[2]
				}
				funcs[name] = p[1]
			}
		}
		return
	}
	if m := importModRE.FindStringSubmatch(stmt); m != nil {
		for _, part := range strings.Split(m[1], ",") {
			if p := aliasPartRE.FindStringSubmatch(strings.TrimSpace(part)); p != nil && p[1] == "nbsandbox" {
				name := "nbsandbox"
				if p[2] != "" {
					name = p[2]
				}
				modules[name] = true
			}
		}
	}
}

func cellSource(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []string
	if json.Unmarshal(raw, &parts) == nil {
		return strings.Join(parts, "")
	}
	return ""
}

// blankStringsAndComments replaces the contents of string literals and
// comments with spaces (keeping line breaks), so a call written inside a
// string or comment isn't taken for one.
func blankStringsAndComments(src string) string {
	b := []byte(src)
	for i := 0; i < len(b); i++ {
		switch c := b[i]; c {
		case '#':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		case '"', '\'':
			quote := string(b[i : i+1])
			if i+2 < len(b) && b[i+1] == c && b[i+2] == c {
				quote = string(b[i : i+3])
			}
			j := i + len(quote)
			for j < len(b) {
				if b[j] == '\\' {
					j += 2
					continue
				}
				if strings.HasPrefix(string(b[j:]), quote) {
					break
				}
				if len(quote) == 1 && b[j] == '\n' {
					break // an unterminated one-line string
				}
				j++
			}
			for k := i + len(quote); k < j && k < len(b); k++ {
				if b[k] != '\n' {
					b[k] = ' '
				}
			}
			i = min(j+len(quote), len(b)) - 1
		}
	}
	return string(b)
}

// Resolve settles the contract's mode against what the notebook calls.
// Without a declared mode it is inferred: emit means stream, otherwise
// single. A notebook that calls both, or contradicts a declared mode, is
// refused, naming the cells.
func Resolve(c *Contract, calls Calls) (*Contract, error) {
	if c == nil {
		c = &Contract{Mode: ModeSingle}
	}
	out := *c
	switch {
	case len(calls.Output) > 0 && len(calls.Emit) > 0:
		return nil, &Error{Msg: fmt.Sprintf("the notebook calls nbsandbox.output (%s) and nbsandbox.emit (%s): "+
			"a notebook returns either one value or a stream of items, not both", cells(calls.Output), cells(calls.Emit))}
	case c.ModeDeclared && c.Mode == ModeStream && len(calls.Output) > 0:
		return nil, &Error{Msg: fmt.Sprintf("the contract's output mode is stream, but %s calls nbsandbox.output; "+
			"send each item with nbsandbox.emit", cells(calls.Output))}
	case c.ModeDeclared && c.Mode == ModeSingle && len(calls.Emit) > 0:
		return nil, &Error{Msg: fmt.Sprintf("the contract's output mode is single, but %s calls nbsandbox.emit; "+
			"return the value with nbsandbox.output, or declare {\"mode\": \"stream\", \"item\": ...}", cells(calls.Emit))}
	case !c.ModeDeclared && len(calls.Emit) > 0:
		if c.HasOutput() {
			return nil, &Error{Msg: fmt.Sprintf("%s calls nbsandbox.emit, but the contract's output is a schema for one value; "+
				"declare {\"mode\": \"stream\", \"item\": <schema of each item>}", cells(calls.Emit))}
		}
		out.Mode = ModeStream
	case !c.ModeDeclared:
		out.Mode = ModeSingle
	}
	return &out, nil
}

func cells(idx []int) string {
	parts := make([]string, len(idx))
	for i, n := range idx {
		parts[i] = fmt.Sprint(n)
	}
	if len(idx) == 1 {
		return "cell " + parts[0]
	}
	return "cells " + strings.Join(parts, ", ")
}
