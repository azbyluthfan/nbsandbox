// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// API holds the handlers' dependencies.
type API struct{ d Deps }

// Route is one entry in the route table. The table is the single source of
// truth: NewHandler registers it, the authorization matrix test walks every
// space-scoped route as the wrong user, and the OpenAPI test
// compares it with api/openapi.yaml.
type Route struct {
	Method      string
	Pattern     string // net/http pattern, e.g. /v1/spaces/{id}/files/{path...}
	Handler     http.HandlerFunc
	Auth        bool    // requires authentication (basic or bearer)
	Op          auth.Op // the operation, which names the scope the caller needs; required when Auth
	SpaceScoped bool    // has {id}: validated as a UUID, owned by the caller
	AnySpace    bool    // has {id}: validated as a UUID, any owner's (administrators)
	RunScoped   bool    // has {run_id}: validated as a UUID
	MaxBody     int64   // 0 = DefaultMaxBody
}

func (a *API) routes() []Route {
	return []Route{
		// Operational, no auth. The reverse proxy must not expose /metrics.
		{Method: "GET", Pattern: "/healthz", Handler: a.d.Health.Liveness},
		{Method: "GET", Pattern: "/readyz", Handler: a.d.Health.Readiness},
		{Method: "GET", Pattern: "/metrics", Handler: a.d.Metrics.Handler().ServeHTTP},

		// Spaces.
		{Method: "POST", Pattern: "/v1/spaces", Handler: a.createSpace, Auth: true, Op: auth.OpCreateSpace},
		{Method: "GET", Pattern: "/v1/spaces", Handler: a.listSpaces, Auth: true, Op: auth.OpListSpaces},
		{Method: "GET", Pattern: "/v1/spaces/{id}", Handler: a.getSpace, Auth: true, Op: auth.OpGetSpace, SpaceScoped: true},
		{Method: "PATCH", Pattern: "/v1/spaces/{id}", Handler: a.patchSpace, Auth: true, Op: auth.OpUpdateSpace, SpaceScoped: true},
		{Method: "DELETE", Pattern: "/v1/spaces/{id}", Handler: a.deleteSpace, Auth: true, Op: auth.OpDeleteSpace, SpaceScoped: true},

		// Files.
		{Method: "POST", Pattern: "/v1/spaces/{id}/files", Handler: a.uploadFiles, Auth: true, Op: auth.OpUploadFiles, SpaceScoped: true, MaxBody: a.d.MaxUpload},
		{Method: "GET", Pattern: "/v1/spaces/{id}/files", Handler: a.listFiles, Auth: true, Op: auth.OpListFiles, SpaceScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/files/{path...}", Handler: a.getFile, Auth: true, Op: auth.OpGetFile, SpaceScoped: true},
		{Method: "PUT", Pattern: "/v1/spaces/{id}/files/{path...}", Handler: a.putFile, Auth: true, Op: auth.OpPutFile, SpaceScoped: true, MaxBody: a.d.MaxUpload},
		{Method: "PATCH", Pattern: "/v1/spaces/{id}/files/{path...}", Handler: a.patchFile, Auth: true, Op: auth.OpMoveFile, SpaceScoped: true},
		{Method: "DELETE", Pattern: "/v1/spaces/{id}/files/{path...}", Handler: a.deleteFile, Auth: true, Op: auth.OpDeleteFile, SpaceScoped: true},
		{Method: "POST", Pattern: "/v1/spaces/{id}/folders", Handler: a.createFolder, Auth: true, Op: auth.OpCreateFolder, SpaceScoped: true},

		// Functions: published, immutable notebook versions that other
		// services invoke.
		{Method: "GET", Pattern: "/v1/spaces/{id}/functions", Handler: a.listFunctions, Auth: true, Op: auth.OpListFunctions, SpaceScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/functions/{name}", Handler: a.getFunction, Auth: true, Op: auth.OpGetFunction, SpaceScoped: true},
		{Method: "DELETE", Pattern: "/v1/spaces/{id}/functions/{name}", Handler: a.deleteFunction, Auth: true, Op: auth.OpDeleteFunction, SpaceScoped: true},
		{Method: "POST", Pattern: "/v1/spaces/{id}/functions/{name}/versions", Handler: a.publishFunction, Auth: true, Op: auth.OpPublishFunction, SpaceScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/functions/{name}/versions/{version}", Handler: a.getFunctionVersion, Auth: true, Op: auth.OpGetFunctionVersion, SpaceScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/functions/{name}/contract", Handler: a.getFunctionContract, Auth: true, Op: auth.OpGetFunctionContract, SpaceScoped: true},
		{Method: "POST", Pattern: "/v1/spaces/{id}/functions/{name}/invoke", Handler: a.invokeFunction, Auth: true, Op: auth.OpInvokeFunction, SpaceScoped: true, MaxBody: a.maxRunBody()},
		{Method: "POST", Pattern: "/v1/spaces/{id}/functions/{name}/versions/{version}/invoke", Handler: a.invokeFunction, Auth: true, Op: auth.OpInvokeFunction, SpaceScoped: true, MaxBody: a.maxRunBody()},

		// Administration: every owner's spaces (scope nbsandbox.admin).
		{Method: "GET", Pattern: "/v1/admin/spaces", Handler: a.adminListSpaces, Auth: true, Op: auth.OpAdminListSpaces},
		{Method: "PATCH", Pattern: "/v1/admin/spaces/{id}", Handler: a.adminUpdateSpace, Auth: true, Op: auth.OpAdminUpdateSpace, AnySpace: true},
		{Method: "DELETE", Pattern: "/v1/admin/spaces/{id}", Handler: a.adminDeleteSpace, Auth: true, Op: auth.OpAdminDeleteSpace, AnySpace: true},

		// Runtime.
		{Method: "GET", Pattern: "/v1/runtime", Handler: a.getRuntime, Auth: true, Op: auth.OpGetRuntime},
		{Method: "GET", Pattern: "/v1/runtime/completions", Handler: a.getCompletions, Auth: true, Op: auth.OpGetCompletions},

		// Runs, with the live log stream.
		{Method: "POST", Pattern: "/v1/spaces/{id}/runs", Handler: a.createRun, Auth: true, Op: auth.OpCreateRun, SpaceScoped: true, MaxBody: a.maxRunBody()},
		{Method: "GET", Pattern: "/v1/spaces/{id}/runs", Handler: a.listRuns, Auth: true, Op: auth.OpListRuns, SpaceScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/runs/{run_id}", Handler: a.getRun, Auth: true, Op: auth.OpGetRun, SpaceScoped: true, RunScoped: true},
		{Method: "POST", Pattern: "/v1/spaces/{id}/runs/{run_id}/cancel", Handler: a.cancelRun, Auth: true, Op: auth.OpCancelRun, SpaceScoped: true, RunScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/runs/{run_id}/result", Handler: a.getRunResultWait, Auth: true, Op: auth.OpGetRunResult, SpaceScoped: true, RunScoped: true},
		{Method: "DELETE", Pattern: "/v1/spaces/{id}/runs/{run_id}", Handler: a.deleteRun, Auth: true, Op: auth.OpDeleteRun, SpaceScoped: true, RunScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/runs/{run_id}/output.ipynb", Handler: a.getRunNotebook, Auth: true, Op: auth.OpGetRunNotebook, SpaceScoped: true, RunScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/runs/{run_id}/output.html", Handler: a.getRunHTML, Auth: true, Op: auth.OpGetRunHTML, SpaceScoped: true, RunScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/runs/{run_id}/artifacts", Handler: a.listArtifacts, Auth: true, Op: auth.OpListArtifacts, SpaceScoped: true, RunScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/runs/{run_id}/artifacts/{path...}", Handler: a.getArtifact, Auth: true, Op: auth.OpGetArtifact, SpaceScoped: true, RunScoped: true},
		{Method: "POST", Pattern: "/v1/spaces/{id}/runs/{run_id}/promote", Handler: a.promoteArtifact, Auth: true, Op: auth.OpPromoteArtifact, SpaceScoped: true, RunScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/runs/{run_id}/log", Handler: a.getRunLog, Auth: true, Op: auth.OpGetRunLog, SpaceScoped: true, RunScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/runs/{run_id}/log/stream", Handler: a.streamRunLog, Auth: true, Op: auth.OpFollowRunLog, SpaceScoped: true, RunScoped: true},

		// Status streams (SSE): one run, or every run in the space.
		{Method: "GET", Pattern: "/v1/spaces/{id}/runs/{run_id}/events", Handler: a.streamRunEvents, Auth: true, Op: auth.OpFollowRunEvents, SpaceScoped: true, RunScoped: true},
		{Method: "GET", Pattern: "/v1/spaces/{id}/events", Handler: a.streamSpaceEvents, Auth: true, Op: auth.OpFollowSpaceEvents, SpaceScoped: true},
	}
}

// Scope is the scope the route needs (its operation's).
func (rt Route) Scope() string { return rt.Op.Scope() }

// Routes exposes the table for tests in other packages.
func Routes(d Deps) []Route { return (&API{d: d}).routes() }

// requireValidSpaceID rejects a malformed {id} with 400
// before any lookup, so only well-formed IDs reach the ownership check.
func requireValidSpaceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !spaces.ValidID(r.PathValue("id")) {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "space id must be a lowercase UUID")
			return
		}
		next.ServeHTTP(w, r)
	})
}
