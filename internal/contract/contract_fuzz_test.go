// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package contract

import "testing"

// Whatever a notebook holds, reading its contract never panics, and a schema
// can never make nbsandboxd fetch anything (there is no loader).
func FuzzFromNotebook(f *testing.F) {
	for _, s := range []string{
		`{"metadata":{"nbsandbox":{"input":{"type":"object"},"output":{"type":"array"}}},"cells":[]}`,
		`{"metadata":{"nbsandbox":{"input":{"$ref":"https://example.com/s.json"}}}}`,
		`{"metadata":{"nbsandbox":{"output":{"mode":"stream","items":{"type":"integer"}}}}}`,
		`{"metadata":{"nbsandbox":{"input":{"pattern":"(a+)+$"}}}}`, `{}`, `[]`, ``,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, nb []byte) {
		c, err := FromNotebook(nb)
		if err != nil || c == nil {
			return
		}
		_ = c.ValidateInput([]byte(`{"a":1}`))
		_ = c.ValidateOutput([]byte(`[1,"x"]`))
	})
}
