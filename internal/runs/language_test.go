// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"strings"
	"testing"
)

func TestCheckLanguage(t *testing.T) {
	for metadata, ok := range map[string]bool{
		`{}`: true, // no kernel named: Python
		`{"kernelspec": {"name": "python3", "display_name": "Python 3", "language": "python"}}`: true,
		`{"kernelspec": {"name": "python3"}}`:                                                   true,
		`{"language_info": {"name": "python", "version": "3.13.1"}}`:                            true,
		`{"kernelspec": {"name": "conda-env-ml-py", "language": "Python"}}`:                     true,
		`{"kernelspec": {"name": "ir", "display_name": "R", "language": "R"}}`:                  false,
		`{"kernelspec": {"name": "julia-1.10"}}`:                                                false,
		`{"language_info": {"name": "scala"}}`:                                                  false,
		// The kernel's language wins over its name.
		`{"kernelspec": {"name": "python3", "language": "R"}}`: false,
	} {
		nb := `{"nbformat": 4, "cells": [], "metadata": ` + metadata + `}`
		err := CheckLanguage([]byte(nb))
		if (err == nil) != ok {
			t.Errorf("%s: %v, want ok=%v", metadata, err, ok)
		}
		if err != nil && !strings.Contains(err.Error(), "Python notebooks only") {
			t.Errorf("%s: unclear message %q", metadata, err)
		}
	}
	if CheckLanguage([]byte(`{`)) == nil {
		t.Error("bad JSON accepted")
	}
}
