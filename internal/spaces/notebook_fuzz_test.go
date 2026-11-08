// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"bytes"
	"testing"
)

// Uploaded notebooks are checked without panicking, whatever they hold.
func FuzzValidateNotebook(f *testing.F) {
	for _, s := range []string{`{"nbformat":4,"cells":[]}`, `{"nbformat":3,"cells":[]}`, `{"cells":[]}`, `[`, ``,
		`{"nbformat":4,"cells":[` + string(bytes.Repeat([]byte(`[`), 20000)) + `]}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = ValidateNotebook(bytes.NewReader(b))
	})
}
