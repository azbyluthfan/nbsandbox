// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/azbyluthfan/nbsandbox/internal/completions"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
)

// getCompletions is GET /v1/runtime/completions: the runner image's
// completion index for editors. It is named by its hash (the ETag), so a
// browser keeps it until the image changes.
func (a *API) getCompletions(w http.ResponseWriter, r *http.Request) {
	info, err := a.d.Runtime()
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	if info.Completions == nil || a.d.Completions == nil {
		WriteError(w, http.StatusNotFound, CodeNotFound, completions.ErrNone.Error())
		return
	}
	tag := `"` + info.Completions.SHA256 + `"`
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Vary", "Accept-Encoding")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rc, err := a.d.Completions(r.Context(), *info.Completions)
	if errors.Is(err, completions.ErrNone) {
		WriteError(w, http.StatusNotFound, CodeNotFound, err.Error())
		return
	}
	if err != nil {
		obs.Logger(r.Context()).Error("open the completion index", "error", err)
		WriteError(w, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/json")
	if acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Completions.Bytes, 10))
		_, _ = io.Copy(w, rc)
		return
	}
	zr, err := gzip.NewReader(rc)
	if err != nil {
		obs.Logger(r.Context()).Error("read the completion index", "error", err)
		WriteError(w, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	_, _ = io.CopyN(w, zr, completions.MaxJSON)
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") && strings.TrimSpace(q) != "q=0" {
			return true
		}
	}
	return false
}
