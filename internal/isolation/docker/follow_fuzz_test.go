// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"bytes"
	"strings"
	"testing"
)

// However output is chunked, every line is at most MaxLineBytes,
// and the lines put back together equal the input (minus line endings).
func FuzzLineWriter(f *testing.F) {
	f.Add([]byte("a\nb\r\nc"), uint8(3))
	f.Add(bytes.Repeat([]byte("x"), MaxLineBytes+5), uint8(7))
	f.Fuzz(func(t *testing.T, data []byte, chunk uint8) {
		var got []string
		w := newLineWriter(func(s string) { got = append(got, s) })
		step := int(chunk%64) + 1
		for i := 0; i < len(data); i += step {
			_, _ = w.Write(data[i:min(i+step, len(data))])
		}
		w.Flush()
		for _, l := range got {
			if len(l) > MaxLineBytes {
				t.Fatalf("line of %d bytes", len(l))
			}
		}
		want := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n", "")
		joined := strings.Join(got, "")
		// A lone trailing \r is stripped at the end of a line or the stream.
		if strings.ReplaceAll(joined, "\r", "") != strings.ReplaceAll(want, "\r", "") {
			t.Fatalf("content changed: %q -> %q", data, got)
		}
	})
}
