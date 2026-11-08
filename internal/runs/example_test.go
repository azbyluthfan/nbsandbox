// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
)

// The notebook in examples/ passes the checks a run makes before it starts:
// a Python notebook, a valid contract that accepts the examples' parameters
// and refuses a region the data doesn't have.
func TestExampleNotebook(t *testing.T) {
	nb, err := os.ReadFile("../../examples/notebooks/sales_report.ipynb")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLanguage(nb); err != nil {
		t.Fatal(err)
	}
	c, err := contract.FromNotebook(nb)
	if err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{`{"region":"bandung","months":4}`, `{"region":"surabaya"}`} {
		if err := c.ValidateInput(json.RawMessage(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{`{"region":"bali"}`, `{"months":3}`, `{"region":"jakarta","months":0}`} {
		if c.ValidateInput(json.RawMessage(bad)) == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}
