// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import "testing"

// Whatever a notebook writes to its events file, a line is either refused or
// becomes an event of a known kind with values in range.
func FuzzParseRunEvent(f *testing.F) {
	for _, s := range []string{`{"kind":"item","item":{"a":1}}`, `{"kind":"item","item":1,"total":0}`,
		`{"kind":"progress","fraction":0.5,"message":"x"}`, `{"kind":"progress","fraction":2}`,
		`{"kind":"cell","cell_index":3,"state":"finished","outcome":"ok","duration_seconds":1.5}`, `{"kind":"other"}`, `[]`, ``} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		ev, err := parseRunEvent(line)
		if err != nil {
			return
		}
		switch ev.Kind {
		case "item":
			if len(ev.Item) == 0 || (ev.Total != nil && *ev.Total < 1) {
				t.Fatalf("bad item accepted: %+v", ev)
			}
		case "progress":
			if ev.Fraction == nil || *ev.Fraction < 0 || *ev.Fraction > 1 || len(ev.Message) > 1000 {
				t.Fatalf("bad progress accepted: %+v", ev)
			}
		case "cell":
			if ev.CellIndex == nil || *ev.CellIndex < 0 {
				t.Fatalf("bad cell event accepted: %+v", ev)
			}
		default:
			t.Fatalf("unknown kind accepted: %q", ev.Kind)
		}
	})
}
