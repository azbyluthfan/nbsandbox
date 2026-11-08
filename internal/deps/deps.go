// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package deps

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// ManifestLabel is the runner image label holding the manifest.
const ManifestLabel = "io.nbsandbox.manifest"

// SupportedStatusSchema is the runner status-file version nbsandboxd reads.
const SupportedStatusSchema = 2

// Manifest describes what the runner image provides (export_manifest.py).
type Manifest struct {
	SchemaVersion       int                 `json:"schema_version"`
	StatusSchemaVersion int                 `json:"status_schema_version"`
	PythonVersion       string              `json:"python_version"`
	Distributions       map[string]string   `json:"distributions"` // normalised name -> version
	ImportNames         map[string][]string `json:"import_names"`  // top-level module -> distributions
	Stdlib              []string            `json:"stdlib"`

	stdlib map[string]bool
	dists  map[string]bool
}

// ParseManifest reads the image label and checks the image speaks a status
// format this nbsandboxd understands.
func ParseManifest(label string) (*Manifest, error) {
	if label == "" {
		return nil, errors.New("runner image has no " + ManifestLabel + " label (build it with make runner-image)")
	}
	var m Manifest
	if err := json.Unmarshal([]byte(label), &m); err != nil {
		return nil, fmt.Errorf("runner image manifest is not valid JSON: %w", err)
	}
	if m.StatusSchemaVersion != SupportedStatusSchema {
		return nil, fmt.Errorf("runner image reports status format %d; this nbsandboxd supports %d", m.StatusSchemaVersion, SupportedStatusSchema)
	}
	m.stdlib = make(map[string]bool, len(m.Stdlib))
	for _, s := range m.Stdlib {
		m.stdlib[s] = true
	}
	m.dists = make(map[string]bool, len(m.Distributions))
	for d := range m.Distributions {
		m.dists[normalize(d)] = true
	}
	return &m, nil
}

// Provides reports whether the image can import the top-level module.
func (m *Manifest) Provides(module string) bool {
	if m.stdlib[module] || strings.HasPrefix(module, "_") {
		return true
	}
	if _, ok := m.ImportNames[module]; ok {
		return true
	}
	return m.dists[normalize(module)]
}

// Modules returns every top-level module name a notebook can import, sorted:
// the standard library and the installed packages' modules.
func (m *Manifest) Modules() []string {
	seen := make(map[string]bool, len(m.Stdlib)+len(m.ImportNames))
	for _, s := range m.Stdlib {
		seen[s] = true
	}
	for s := range m.ImportNames {
		seen[s] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Installed reports whether a distribution (requirements.txt name) is present.
func (m *Manifest) Installed(dist string) bool { return m.dists[normalize(dist)] }

var normRE = regexp.MustCompile(`[-_.]+`)

// normalize is PEP 503 name normalisation.
func normalize(name string) string { return normRE.ReplaceAllString(strings.ToLower(name), "-") }

// Warning is one dependency the image doesn't provide.
type Warning struct {
	Module string `json:"module"`
	Cell   *int   `json:"cell,omitempty"` // 0-based code-cell position in the notebook
	Source string `json:"source"`         // "notebook" or "requirements.txt"
}

// Files gives the checker read access to the space.
type Files interface {
	ReadSpaceFile(spaceID, p string, max int64) ([]byte, bool)
	SpaceEntryExists(spaceID, p string) bool
}

var (
	importRE = regexp.MustCompile(`^\s*import\s+(.+)$`)
	fromRE   = regexp.MustCompile(`^\s*from\s+([A-Za-z_][\w.]*)\s+import\b`)
	nameRE   = regexp.MustCompile(`^[A-Za-z_]\w*`)
)

// Check scans the notebook's code cells and the space's requirements.txt and
// returns what the image doesn't provide. It is best effort: imports built at
// run time (importlib, exec) aren't seen, and imports guarded by try/except
// are reported even though the notebook may handle their absence. That is
// why the default policy only warns.
func Check(m *Manifest, files Files, spaceID, notebookPath string, notebook []byte) []Warning {
	var nb struct {
		Cells []struct {
			CellType string          `json:"cell_type"`
			Source   json.RawMessage `json:"source"`
		} `json:"cells"`
	}
	if json.Unmarshal(notebook, &nb) != nil {
		return nil
	}
	nbDir := path.Dir(notebookPath)
	local := func(mod string) bool {
		for _, p := range []string{mod + ".py", mod} {
			if files.SpaceEntryExists(spaceID, p) || (nbDir != "." && files.SpaceEntryExists(spaceID, nbDir+"/"+p)) {
				return true
			}
		}
		return false
	}

	seen := map[string]bool{}
	var out []Warning
	codeCell := -1
	for i := range nb.Cells {
		c := nb.Cells[i]
		if c.CellType != "code" {
			continue
		}
		codeCell++
		for _, mod := range importsIn(sourceText(c.Source)) {
			if seen[mod] || m.Provides(mod) || local(mod) {
				continue
			}
			seen[mod] = true
			cell := codeCell
			out = append(out, Warning{Module: mod, Cell: &cell, Source: "notebook"})
		}
	}
	if req, ok := files.ReadSpaceFile(spaceID, "requirements.txt", 256<<10); ok {
		for _, name := range requirementNames(req) {
			if !m.Installed(name) && !seen["req:"+name] {
				seen["req:"+name] = true
				out = append(out, Warning{Module: name, Source: "requirements.txt"})
			}
		}
	}
	return out
}

// sourceText accepts a cell source as a string or a list of strings.
func sourceText(raw json.RawMessage) string {
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

// importsIn returns the top-level modules imported in source, in order.
func importsIn(src string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(src))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "%") || strings.HasPrefix(trimmed, "!") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if m := fromRE.FindStringSubmatch(line); m != nil {
			out = append(out, strings.SplitN(m[1], ".", 2)[0])
			continue
		}
		if m := importRE.FindStringSubmatch(line); m != nil {
			for _, part := range strings.Split(m[1], ",") {
				if name := nameRE.FindString(strings.TrimSpace(part)); name != "" {
					out = append(out, name)
				}
			}
		}
	}
	return out
}

var reqNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*`)

// requirementNames returns the distribution names in a requirements.txt.
func requirementNames(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		if name := reqNameRE.FindString(line); name != "" {
			out = append(out, normalize(name))
		}
	}
	sort.Strings(out)
	return out
}
