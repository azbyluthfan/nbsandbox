// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"encoding/json"
	"testing"
)

// Whatever arrives, parameters are either rejected or come out as
// a JSON object of valid Python names that round-trips, within the size cap.
func FuzzNormalizeParameters(f *testing.F) {
	for _, s := range []string{`{}`, `{"a":1}`, `{"a":[1,"x",true]}`, `{"class":1}`, `[1]`, `{"a":{"b":1}}`, `null`, ``, `{"a":1e400}`,
		`{"a":{"b":[{"c":null}]}}`, `{"a":[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		const limit = 64 << 10
		out, err := NormalizeParameters(json.RawMessage(raw), limit)
		if err != nil {
			return
		}
		if len(out) > limit {
			t.Fatalf("accepted %d bytes", len(out))
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("output is not a JSON object: %q", out)
		}
		for k := range m {
			if !identRE.MatchString(k) || pyKeywords[k] {
				t.Fatalf("accepted name %q", k)
			}
		}
		again, err := NormalizeParameters(json.RawMessage(out), limit)
		if err != nil || again != out {
			t.Fatalf("not stable: %q -> %q (%v)", out, again, err)
		}
	})
}
