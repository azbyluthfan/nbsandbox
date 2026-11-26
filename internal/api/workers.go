// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/azbyluthfan/nbsandbox/internal/compute"
)

// listWorkers is GET /v1/workers: the workers, what each runs on and how
// many runs and sessions it holds (docs/plan-compute-view.md). Any signed-in
// user: counts only, no one's runs.
func (a *API) listWorkers(w http.ResponseWriter, r *http.Request) {
	list := []compute.Worker{}
	if a.d.Workers != nil {
		ws, err := a.d.Workers(r.Context())
		if err != nil {
			writeRunError(w, r, err)
			return
		}
		list = append(list, ws...)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}
