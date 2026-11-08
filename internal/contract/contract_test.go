// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const nbWith = `{"nbformat": 4, "cells": [], "metadata": {"nbsandbox": {"contract": {
  "input":  {"type": "object", "required": ["region"], "properties": {
      "region": {"type": "string"},
      "options": {"type": "object", "properties": {"months": {"type": "integer", "minimum": 1}}}}},
  "output": {"type": "object", "required": ["forecast"], "properties": {"forecast": {"type": "array", "items": {"type": "number"}}}}
}}}}`

func TestFromNotebook(t *testing.T) {
	c, err := FromNotebook([]byte(nbWith))
	if err != nil || !c.HasInput() || !c.HasOutput() {
		t.Fatalf("contract %+v %v", c, err)
	}
	none, err := FromNotebook([]byte(`{"nbformat": 4, "cells": [], "metadata": {}}`))
	if err != nil || none != nil || none.HasInput() || none.ValidateInput(json.RawMessage(`{"x":1}`)) != nil ||
		none.ValidateOutput(nil) != nil {
		t.Fatal("a notebook without a contract must accept everything")
	}
	if _, err := FromNotebook([]byte(`{`)); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

func TestValidateInput(t *testing.T) {
	c, _ := FromNotebook([]byte(nbWith))
	if err := c.ValidateInput(json.RawMessage(`{"region": "jakarta", "options": {"months": 6}}`)); err != nil {
		t.Fatal(err)
	}
	err := c.ValidateInput(json.RawMessage(`{"options": {"months": 0}}`))
	var ce *Error
	if !errors.As(err, &ce) || len(ce.Problems) != 2 {
		t.Fatalf("want two problems, got %v", err)
	}
	pointers := ce.Problems[0].Pointer + " " + ce.Problems[1].Pointer
	if pointers != " /options/months" { // missing "region" is reported at the root
		t.Fatalf("pointers %q (%v)", pointers, err)
	}
	if !strings.Contains(err.Error(), "region") {
		t.Fatalf("message doesn't name the missing property: %v", err)
	}
	if !strings.Contains(err.Error(), "minimum") || strings.Contains(err.Error(), "&{") {
		t.Fatalf("message isn't readable: %v", err)
	}
}

func TestValidateOutput(t *testing.T) {
	c, _ := FromNotebook([]byte(nbWith))
	if err := c.ValidateOutput(json.RawMessage(`{"forecast": [1, 2.5]}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateOutput(json.RawMessage(`{"forecast": ["x"]}`)); err == nil {
		t.Fatal("wrong item type accepted")
	}
	if err := c.ValidateOutput(nil); err == nil || !strings.Contains(err.Error(), "nbsandbox.output") {
		t.Fatalf("missing output: %v", err)
	}
}

func TestBadContracts(t *testing.T) {
	for name, nb := range map[string]string{
		"not a schema":     `{"metadata": {"nbsandbox": {"contract": {"input": {"type": 5}}}}}`,
		"remote reference": `{"metadata": {"nbsandbox": {"contract": {"input": {"$ref": "https://example.com/s.json"}}}}}`,
		"file reference":   `{"metadata": {"nbsandbox": {"contract": {"output": {"$ref": "file:///etc/passwd"}}}}}`,
		"too large":        `{"metadata": {"nbsandbox": {"contract": {"input": {"description": "` + strings.Repeat("x", MaxSchemaBytes) + `"}}}}}`,
	} {
		var ce *Error
		if _, err := FromNotebook([]byte(nb)); !errors.As(err, &ce) {
			t.Errorf("%s: %v, want a contract error", name, err)
		}
	}
	// Internal references are fine.
	ok := `{"metadata": {"nbsandbox": {"contract": {"input": {"$defs": {"m": {"type": "integer"}},
	  "type": "object", "properties": {"months": {"$ref": "#/$defs/m"}}}}}}}`
	c, err := FromNotebook([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if c.ValidateInput(json.RawMessage(`{"months": "six"}`)) == nil {
		t.Fatal("internal $ref not applied")
	}
}
