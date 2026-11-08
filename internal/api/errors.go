// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
)

// Error codes used across the API. Every error response has the shape
// {"error": {"code": "...", "message": "..."}}.
const (
	CodeBadRequest        = "bad_request"
	CodeUnauthorized      = "unauthorized"
	CodeForbidden         = "forbidden" // the token lacks the route's scope (403)
	CodeNotFound          = "not_found"
	CodeMethodNotAllowed  = "method_not_allowed"
	CodeConflict          = "conflict"
	CodePayloadTooLarge   = "payload_too_large"
	CodeUnprocessable     = "unprocessable"
	CodeInvalidParameters = "invalid_parameters" // parameters don't match the notebook's input contract
	CodeInvalidContract   = "invalid_contract"   // the notebook's contract isn't valid
	CodeVersionMismatch   = "version_mismatch"   // the file changed since the version given (412)
	CodeVersionRequired   = "version_required"   // replacing a file needs If-Match (428)
	CodeDockerUnavailable = "docker_unavailable"
	CodeInternal          = "internal"
)

// ErrorBody is the JSON error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the content of an error response.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Problems lists where a value failed validation (invalid_parameters).
	Problems []contract.Problem `json:"problems,omitempty"`
}

// WriteError writes an error response.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}

// WriteJSON writes v as a JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
