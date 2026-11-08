// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// CheckLanguage refuses a notebook written for a kernel other than Python.
// The runner image has only the Python kernel and always uses it, so an R or
// Julia notebook would otherwise run as Python and fail with a misleading
// syntax error in its first cell. A notebook that names no language or
// kernel is treated as Python.
func CheckLanguage(nb []byte) error {
	var doc struct {
		Metadata struct {
			Kernelspec struct {
				Name     string `json:"name"`
				Language string `json:"language"`
			} `json:"kernelspec"`
			LanguageInfo struct {
				Name string `json:"name"`
			} `json:"language_info"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(nb, &doc); err != nil {
		return &spaces.InvalidError{Msg: "notebook is not valid JSON"}
	}
	md := doc.Metadata
	lang := md.Kernelspec.Language
	if lang == "" {
		lang = md.LanguageInfo.Name
	}
	if lang == "" {
		lang = md.Kernelspec.Name // e.g. "python3", "ir", "julia-1.10"
	}
	if lang == "" || strings.HasPrefix(strings.ToLower(strings.TrimSpace(lang)), "python") {
		return nil
	}
	return &spaces.InvalidError{Msg: fmt.Sprintf(
		"this notebook is written for %q, but nbsandbox runs Python notebooks only: change the notebook's kernel to Python 3", lang)}
}
