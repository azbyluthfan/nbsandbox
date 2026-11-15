// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package packages builds a space's extra Python packages (its
// requirements.txt) into a package set: a directory of installed wheels,
// built once per (requirements, runner image) and mounted read-only into the
// space's runs. Runs keep no network: only the build container reaches the
// package index, and it installs wheels only, so no package code runs while
// building.
package packages

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// MaxRequirements bounds requirements.txt: its size and its lines.
const (
	MaxRequirementsBytes = 64 << 10
	MaxRequirementLines  = 200
)

// Requirement is one line of requirements.txt.
type Requirement struct {
	Name   string   // PEP 503 normalised
	Line   string   // the requirement without hashes, whitespace normalised
	Hashes []string // sha256:<hex>, if given
}

// Requirements is a parsed requirements.txt.
type Requirements []Requirement

// ParseError says which line of requirements.txt is refused, and why.
type ParseError struct {
	Line int
	Msg  string
}

func (e *ParseError) Error() string {
	if e.Line == 0 {
		return "requirements.txt: " + e.Msg
	}
	return fmt.Sprintf("requirements.txt line %d: %s", e.Line, e.Msg)
}

var (
	// name[extras] specifiers ; markers. No URLs, paths or options: the
	// characters they need (/ : @ -) aren't allowed where they could appear.
	reqRE = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?)` + // name
		`\s*(\[\s*[A-Za-z0-9._-]+(?:\s*,\s*[A-Za-z0-9._-]+)*\s*\])?` + // extras
		`\s*((?:(?:===|==|!=|~=|<=|>=|<|>)\s*[A-Za-z0-9.*+!_-]+\s*,?\s*)*)` + // specifiers
		`(?:;\s*([A-Za-z0-9_.<>=!~'" ()]+))?$`) // markers
	hashRE = regexp.MustCompile(`^--hash=(sha256:[0-9a-f]{64})$`)
	normRE = regexp.MustCompile(`[-_.]+`)
)

// Normalize is PEP 503 name normalisation.
func Normalize(name string) string { return normRE.ReplaceAllString(strings.ToLower(name), "-") }

// Parse reads requirements.txt. Only requirement lines (name, extras,
// version specifiers, environment markers) and --hash options are accepted:
// index and find-links options, URLs, paths, editable installs, nested
// files and any other option are refused, so a space can't point the
// build at another address.
func Parse(text []byte) (Requirements, error) {
	if len(text) > MaxRequirementsBytes {
		return nil, &ParseError{Msg: fmt.Sprintf("larger than %d KiB", MaxRequirementsBytes>>10)}
	}
	var out Requirements
	seen := map[string]int{}
	sc := bufio.NewScanner(strings.NewReader(string(text)))
	n, start := 0, 0
	var logical strings.Builder
	for sc.Scan() {
		n++
		line := sc.Text()
		if logical.Len() == 0 {
			start = n
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			line = ""
		}
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, `\`) { // continued on the next line
			logical.WriteString(strings.TrimSuffix(line, `\`) + " ")
			continue
		}
		logical.WriteString(line)
		full := strings.TrimSpace(logical.String())
		logical.Reset()
		if full == "" {
			continue
		}
		r, err := parseLine(full)
		if err != nil {
			return nil, &ParseError{Line: start, Msg: err.Error()}
		}
		if prev, dup := seen[r.Name]; dup {
			return nil, &ParseError{Line: start, Msg: fmt.Sprintf("%s is already on line %d", r.Name, prev)}
		}
		seen[r.Name] = start
		out = append(out, r)
		if len(out) > MaxRequirementLines {
			return nil, &ParseError{Msg: fmt.Sprintf("more than %d requirements", MaxRequirementLines)}
		}
	}
	if logical.Len() > 0 {
		return nil, &ParseError{Line: start, Msg: "ends with a line continuation"}
	}
	return out, sc.Err()
}

func parseLine(s string) (Requirement, error) {
	fields := strings.Fields(s)
	var hashes, rest []string
	for _, f := range fields {
		if strings.HasPrefix(f, "--hash") {
			m := hashRE.FindStringSubmatch(f)
			if m == nil {
				return Requirement{}, fmt.Errorf("%q: only --hash=sha256:<64 hex digits> is accepted", f)
			}
			hashes = append(hashes, m[1])
			continue
		}
		if strings.HasPrefix(f, "-") && len(rest) == 0 {
			return Requirement{}, fmt.Errorf("the option %q is not accepted (the package index is nbsandbox's setting; only requirement lines and --hash are allowed)", f)
		}
		rest = append(rest, f)
	}
	if len(rest) == 0 {
		return Requirement{}, fmt.Errorf("--hash without a requirement")
	}
	req := strings.Join(rest, " ")
	m := reqRE.FindStringSubmatch(req)
	if m == nil {
		return Requirement{}, fmt.Errorf("%q is not a plain requirement (name, extras, versions, markers); URLs, paths and direct references are not accepted", req)
	}
	if err := checkMarker(m[4]); err != nil {
		return Requirement{}, err
	}
	slices.Sort(hashes)
	return Requirement{Name: Normalize(m[1]), Line: req, Hashes: slices.Compact(hashes)}, nil
}

// markerWords are the words an environment marker may use (PEP 508).
var markerWords = map[string]bool{
	"python_version": true, "python_full_version": true, "os_name": true, "sys_platform": true, "platform_release": true,
	"platform_system": true, "platform_version": true, "platform_machine": true, "platform_python_implementation": true,
	"implementation_name": true, "implementation_version": true, "extra": true, "and": true, "or": true, "in": true, "not": true,
}

var (
	quotedRE = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	wordRE   = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
)

// checkMarker accepts only PEP 508 marker variables and operators outside
// quoted strings.
func checkMarker(marker string) error {
	if marker == "" {
		return nil
	}
	for _, w := range wordRE.FindAllString(quotedRE.ReplaceAllString(marker, `""`), -1) {
		if !markerWords[w] {
			return fmt.Errorf("the environment marker %q uses %q, which is not a marker variable", marker, w)
		}
	}
	return nil
}

// Canonical is the requirements as one text, in a fixed order, so the same
// requirements always give the same key however the file is written.
func (rs Requirements) Canonical() string {
	lines := make([]string, 0, len(rs))
	for _, r := range rs {
		l := r.Line
		for _, h := range r.Hashes {
			l += " --hash=" + h
		}
		lines = append(lines, l)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n") + "\n"
}

// Names are the requirements' normalised distribution names.
func (rs Requirements) Names() []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

// Key identifies a package set: the same requirements built against the same
// runner image on the same platform give the same set.
func Key(canonical, runnerImageID, platform string) string {
	h := sha256.New()
	for _, part := range []string{"nbsandbox package set v1", canonical, runnerImageID, platform} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Allowed checks names against an allow-list (normalised; empty = any) and
// returns the ones that aren't on it.
func Allowed(names, allowList []string) []string {
	if len(allowList) == 0 {
		return nil
	}
	ok := map[string]bool{}
	for _, a := range allowList {
		ok[Normalize(a)] = true
	}
	var out []string
	for _, n := range names {
		if !ok[Normalize(n)] {
			out = append(out, n)
		}
	}
	return out
}
