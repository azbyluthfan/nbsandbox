// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

// A resume point is a non-negative line number, or refused.
func FuzzResumePoint(f *testing.F) {
	for _, s := range []string{"0", "17", "-1", "9223372036854775808", "1e3", " 5", ""} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, header, after string) {
		r := httptest.NewRequest("GET", "/x?after="+url.QueryEscape(after), nil)
		if header != "" {
			r.Header.Set("Last-Event-ID", header)
		}
		if n, err := resumePoint(r); err == nil && n < 0 {
			t.Fatalf("Last-Event-ID %q after %q -> %d", header, after, n)
		}
	})
}
