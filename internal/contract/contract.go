// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package contract reads a notebook's optional contract and validates
// parameters and outputs against it.
//
// The contract lives in the notebook's own metadata, so it travels with the
// notebook and is snapshotted with it (a function's publish request may give
// one instead):
//
//	"metadata": {"nbsandbox": {"contract": {"input": <JSON Schema>, "output": <output>}}}
//
// The output says how the notebook returns its result:
//
//	<JSON Schema>                                  one value (nbsandbox.output), matching it
//	{"mode": "single", "schema": <JSON Schema>}    the same, spelled out
//	{"mode": "stream", "item": <JSON Schema>}      a sequence of items (nbsandbox.emit), each matching it
//
// Schemas are JSON Schema 2020-12 and optional. Without a contract, any
// JSON is accepted, and the mode is inferred from the calls the notebook
// makes (see Scan).
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// printer renders validation messages ("maximum: got 99, want 24").
var printer = message.NewPrinter(language.English)

// MaxSchemaBytes bounds each schema, so a contract can't make validation
// expensive.
const MaxSchemaBytes = 256 << 10

// Output modes.
const (
	ModeSingle = "single" // one value: nbsandbox.output
	ModeStream = "stream" // a sequence of items: nbsandbox.emit
)

// Contract is a notebook's compiled contract. A nil *Contract accepts
// everything, in single mode.
type Contract struct {
	// InputJSON is the input schema; OutputJSON the schema of the value
	// (single mode) or of each item (stream mode); nil when absent.
	InputJSON, OutputJSON json.RawMessage
	// Mode is single or stream; ModeDeclared says the contract named it
	// (otherwise it is inferred from the notebook's calls).
	Mode          string
	ModeDeclared  bool
	input, output *jsonschema.Schema
}

// Spec is a contract as written: in notebook metadata or a publish request.
type Spec struct {
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
}

// GetMode is the output mode (single for a nil contract).
func (c *Contract) GetMode() string {
	if c == nil || c.Mode == "" {
		return ModeSingle
	}
	return c.Mode
}

// Stream reports whether the contract returns a sequence of items.
func (c *Contract) Stream() bool { return c != nil && c.Mode == ModeStream }

// Error is a contract problem the caller must fix: an invalid contract, or a
// value that doesn't match it.
type Error struct {
	Msg      string
	Problems []Problem // where a value failed, if it did
}

// Problem is one validation failure.
type Problem struct {
	Pointer string `json:"pointer"` // JSON pointer into the value ("" = the whole value)
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Msg }

// FromNotebook reads the contract from a notebook's metadata. It returns
// nil, nil when the notebook has none.
func FromNotebook(nb []byte) (*Contract, error) {
	spec, err := SpecFromNotebook(nb)
	if err != nil || spec == nil {
		return nil, err
	}
	return FromSpec(*spec)
}

// SpecFromNotebook reads the contract as written in a notebook's metadata
// (nil when there is none).
func SpecFromNotebook(nb []byte) (*Spec, error) {
	var doc struct {
		Metadata struct {
			Nbsandbox struct {
				Contract *Spec `json:"contract"`
			} `json:"nbsandbox"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(nb, &doc); err != nil {
		return nil, &Error{Msg: "notebook is not valid JSON"}
	}
	return doc.Metadata.Nbsandbox.Contract, nil
}

// FromSpec compiles a written contract.
func FromSpec(s Spec) (*Contract, error) {
	mode, declared, schema, err := parseOutput(s.Output)
	if err != nil {
		return nil, err
	}
	c, err := Compile(s.Input, schema)
	if err != nil {
		return nil, err
	}
	c.Mode, c.ModeDeclared = mode, declared
	return c, nil
}

// parseOutput reads the output part: a plain schema (single mode), or an
// object with "mode".
func parseOutput(raw json.RawMessage) (mode string, declared bool, schema json.RawMessage, err error) {
	raw = bytes.TrimSpace(raw)
	var obj map[string]json.RawMessage
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &obj) != nil {
		return ModeSingle, false, raw, nil
	}
	m, ok := obj["mode"]
	if !ok {
		return ModeSingle, false, raw, nil // a plain schema
	}
	if err := json.Unmarshal(m, &mode); err != nil || (mode != ModeSingle && mode != ModeStream) {
		return "", false, nil, &Error{Msg: `contract output "mode" must be "single" or "stream"`}
	}
	key := map[string]string{ModeSingle: "schema", ModeStream: "item"}[mode]
	for k := range obj {
		if k != "mode" && k != key {
			return "", false, nil, &Error{Msg: fmt.Sprintf("contract output in %s mode takes %q (a JSON Schema), not %q", mode, key, k)}
		}
	}
	return mode, true, obj[key], nil
}

// Compile builds a single-mode contract from its two schemas (either may be
// empty).
func Compile(input, output json.RawMessage) (*Contract, error) {
	c := &Contract{Mode: ModeSingle}
	var err error
	if c.InputJSON, c.input, err = compile("input", input); err != nil {
		return nil, err
	}
	if c.OutputJSON, c.output, err = compile("output", output); err != nil {
		return nil, err
	}
	return c, nil
}

// WithMode returns the contract in the given mode (stored contracts keep the
// mode beside the schemas).
func (c *Contract) WithMode(mode string, declared bool) *Contract {
	if c == nil {
		c = &Contract{}
	}
	out := *c
	out.Mode, out.ModeDeclared = mode, declared
	if out.Mode == "" {
		out.Mode = ModeSingle
	}
	return &out
}

func compile(which string, raw json.RawMessage) (json.RawMessage, *jsonschema.Schema, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	if len(raw) > MaxSchemaBytes {
		return nil, nil, &Error{Msg: fmt.Sprintf("contract %s schema is larger than %d bytes", which, MaxSchemaBytes)}
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, &Error{Msg: fmt.Sprintf("contract %s schema is not valid JSON", which)}
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	// Schemas may only refer to themselves: no fetching from files or the
	// network.
	c.UseLoader(noLoader{})
	url := "nbsandbox:///contract/" + which + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, nil, &Error{Msg: fmt.Sprintf("contract %s schema: %v", which, err)}
	}
	sch, err := c.Compile(url)
	if err != nil {
		return nil, nil, &Error{Msg: fmt.Sprintf("contract %s schema is not a valid JSON Schema: %v", which, oneLine(err))}
	}
	return append(json.RawMessage(nil), raw...), sch, nil
}

type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schemas can't load %s: only references inside the same schema are allowed", url)
}

// HasInput and HasOutput report which schemas the contract declares.
func (c *Contract) HasInput() bool  { return c != nil && c.input != nil }
func (c *Contract) HasOutput() bool { return c != nil && c.output != nil }

// ValidateInput checks a run's parameters (a JSON object).
func (c *Contract) ValidateInput(params json.RawMessage) error {
	if !c.HasInput() {
		return nil
	}
	return validate(c.input, params, "parameters don't match the notebook's input contract")
}

// ValidateOutput checks the value a run returned; nil means it returned none.
func (c *Contract) ValidateOutput(out json.RawMessage) error {
	if !c.HasOutput() {
		return nil
	}
	if out == nil {
		return &Error{Msg: "the notebook's contract declares an output, but the notebook didn't call nbsandbox.output"}
	}
	return validate(c.output, out, "the output doesn't match the notebook's output contract")
}

// ValidateItem checks one item of a streamed output.
func (c *Contract) ValidateItem(item json.RawMessage) error {
	if !c.HasOutput() {
		return nil
	}
	return validate(c.output, item, "an item doesn't match the notebook's output contract")
}

func validate(sch *jsonschema.Schema, raw json.RawMessage, msg string) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return &Error{Msg: "not valid JSON"}
	}
	err = sch.Validate(v)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return &Error{Msg: msg + ": " + oneLine(err)}
	}
	problems := leaves(ve)
	if len(problems) > 20 {
		problems = problems[:20]
	}
	parts := make([]string, 0, len(problems))
	for _, p := range problems {
		where := p.Pointer
		if where == "" {
			where = "(the whole value)"
		}
		parts = append(parts, where+": "+p.Message)
	}
	return &Error{Msg: msg + ": " + strings.Join(parts, "; "), Problems: problems}
}

// leaves flattens a validation error tree into its most specific failures.
func leaves(ve *jsonschema.ValidationError) []Problem {
	var out []Problem
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, Problem{Pointer: pointer(e.InstanceLocation), Message: oneLine(e.ErrorKind.LocalizedString(printer))})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Pointer < out[j].Pointer })
	return out
}

func pointer(loc []string) string {
	if len(loc) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range loc {
		b.WriteString("/")
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

func oneLine(v any) string {
	return strings.Join(strings.Fields(fmt.Sprint(v)), " ")
}
