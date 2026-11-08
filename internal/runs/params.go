// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// Parameter limits. The size limit is a setting (runs.max_parameters); these
// are its default and the most it may be set to.
const (
	DefaultMaxParamsBytes = 1 << 20
	MaxParamsBytesCeiling = 16 << 20
	MaxParamsDepth        = 32
)

var identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)

// papermill injects parameters as Python variables, so names must be valid
// identifiers and not keywords.
var pyKeywords = map[string]bool{
	"False": true, "None": true, "True": true, "and": true, "as": true, "assert": true, "async": true,
	"await": true, "break": true, "class": true, "continue": true, "def": true, "del": true, "elif": true,
	"else": true, "except": true, "finally": true, "for": true, "from": true, "global": true, "if": true,
	"import": true, "in": true, "is": true, "lambda": true, "nonlocal": true, "not": true, "or": true,
	"pass": true, "raise": true, "return": true, "try": true, "while": true, "with": true, "yield": true,
}

// NormalizeParameters validates raw parameters and returns them as canonical
// JSON (object keys sorted at every level): a JSON object whose keys are
// Python identifiers and whose values are any JSON (strings, numbers,
// booleans, null, arrays, objects), nested at most MaxParamsDepth deep and
// at most max bytes in total. papermill injects each top-level key as a
// variable; nested values become Python literals.
func NormalizeParameters(raw json.RawMessage, max int64) (string, error) {
	bad := func(format string, args ...any) error {
		return &spaces.InvalidError{Msg: "parameters: " + fmt.Sprintf(format, args...)}
	}
	if max <= 0 {
		max = DefaultMaxParamsBytes
	}
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "{}", nil
	}
	if int64(len(raw)) > max {
		return "", bad("larger than %d bytes", max)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var params map[string]any
	if err := dec.Decode(&params); err != nil {
		return "", bad("must be a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", bad("must be a single JSON object (found data after it)")
	}
	if params == nil {
		return "{}", nil
	}
	for k, v := range params {
		if !identRE.MatchString(k) || pyKeywords[k] {
			return "", bad("%q is not a valid Python variable name", k)
		}
		if err := checkDepth(v, 1); err != nil {
			return "", bad("%q: %v", k, err)
		}
	}
	out, err := json.Marshal(params) // map keys are sorted: canonical
	if err != nil {
		return "", err
	}
	if int64(len(out)) > max {
		return "", bad("larger than %d bytes", max)
	}
	return string(out), nil
}

// checkDepth rejects values nested deeper than MaxParamsDepth (deep nesting
// costs papermill and Python's parser more than it is worth, and is never a
// real model input).
func checkDepth(v any, depth int) error {
	if depth > MaxParamsDepth {
		return fmt.Errorf("nested deeper than %d levels", MaxParamsDepth)
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if err := checkDepth(e, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, e := range x {
			if err := checkDepth(e, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// bodyHash identifies a submission for idempotency.
func bodyHash(notebook, canonicalParams string) string {
	h := sha256.Sum256([]byte(notebook + "\x00" + canonicalParams))
	return hex.EncodeToString(h[:])
}
