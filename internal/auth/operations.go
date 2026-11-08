// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

// Op names an API operation, whatever the transport: an HTTP route and the
// gRPC method that do the same thing name the same Op, so they need the same
// scope. A test checks both transports cover the same operations.
type Op string

// The operations. gRPC methods have these names.
const (
	OpCreateSpace Op = "CreateSpace"
	OpListSpaces  Op = "ListSpaces"
	OpGetSpace    Op = "GetSpace"
	OpUpdateSpace Op = "UpdateSpace"
	OpDeleteSpace Op = "DeleteSpace"

	OpUploadFiles  Op = "UploadFiles"
	OpListFiles    Op = "ListFiles"
	OpGetFile      Op = "GetFile"
	OpPutFile      Op = "PutFile"
	OpMoveFile     Op = "MoveFile"
	OpDeleteFile   Op = "DeleteFile"
	OpCreateFolder Op = "CreateFolder"

	OpListFunctions       Op = "ListFunctions"
	OpGetFunction         Op = "GetFunction"
	OpDeleteFunction      Op = "DeleteFunction"
	OpPublishFunction     Op = "PublishFunction"
	OpGetFunctionVersion  Op = "GetFunctionVersion"
	OpGetFunctionContract Op = "GetFunctionContract"
	OpInvokeFunction      Op = "InvokeFunction"

	OpGetRuntime     Op = "GetRuntime"
	OpGetCompletions Op = "GetCompletions"

	OpCreateRun       Op = "CreateRun"
	OpListRuns        Op = "ListRuns"
	OpGetRun          Op = "GetRun"
	OpCancelRun       Op = "CancelRun"
	OpGetRunResult    Op = "GetRunResult"
	OpDeleteRun       Op = "DeleteRun"
	OpGetRunNotebook  Op = "GetRunNotebook"
	OpGetRunHTML      Op = "GetRunHTML"
	OpListArtifacts   Op = "ListArtifacts"
	OpGetArtifact     Op = "GetArtifact"
	OpPromoteArtifact Op = "PromoteArtifact"
	OpGetRunLog       Op = "GetRunLog"
	OpFollowRunLog    Op = "FollowRunLog"

	OpFollowRunEvents   Op = "FollowRunEvents"
	OpFollowSpaceEvents Op = "FollowSpaceEvents"

	OpAdminListSpaces  Op = "AdminListSpaces"
	OpAdminUpdateSpace Op = "AdminUpdateSpace"
	OpAdminDeleteSpace Op = "AdminDeleteSpace"

	// gRPC only: REST serves these on the same route as their twin, as a
	// stream when asked (Accept: text/event-stream).
	OpFollowRunResult      Op = "FollowRunResult"
	OpInvokeFunctionStream Op = "InvokeFunctionStream"
)

// twins maps a gRPC-only streaming operation to the REST operation that
// serves it too.
var twins = map[Op]Op{
	OpFollowRunResult:      OpGetRunResult,
	OpInvokeFunctionStream: OpInvokeFunction,
}

// RESTTwin is the operation whose REST route also serves op ("" when op
// has a route of its own).
func (op Op) RESTTwin() Op { return twins[op] }

// opScopes is the scope each operation needs.
var opScopes = map[Op]string{
	OpCreateSpace: ScopeSpacesWrite,
	OpListSpaces:  ScopeRead,
	OpGetSpace:    ScopeRead,
	OpUpdateSpace: ScopeSpacesWrite,
	OpDeleteSpace: ScopeSpacesWrite,

	OpUploadFiles:  ScopeFilesWrite,
	OpListFiles:    ScopeRead,
	OpGetFile:      ScopeRead,
	OpPutFile:      ScopeFilesWrite,
	OpMoveFile:     ScopeFilesWrite,
	OpDeleteFile:   ScopeFilesWrite,
	OpCreateFolder: ScopeFilesWrite,

	OpListFunctions:       ScopeRead,
	OpGetFunction:         ScopeRead,
	OpDeleteFunction:      ScopeFunctionsPublish,
	OpPublishFunction:     ScopeFunctionsPublish,
	OpGetFunctionVersion:  ScopeRead,
	OpGetFunctionContract: ScopeRead,
	OpInvokeFunction:      ScopeFunctionsInvoke,

	OpGetRuntime:     ScopeRead,
	OpGetCompletions: ScopeRead,

	OpCreateRun:       ScopeRuns,
	OpListRuns:        ScopeRead,
	OpGetRun:          ScopeRead,
	OpCancelRun:       ScopeRuns,
	OpGetRunResult:    ScopeRead,
	OpDeleteRun:       ScopeRuns,
	OpGetRunNotebook:  ScopeRead,
	OpGetRunHTML:      ScopeRead,
	OpListArtifacts:   ScopeRead,
	OpGetArtifact:     ScopeRead,
	OpPromoteArtifact: ScopeFilesWrite,
	OpGetRunLog:       ScopeRead,
	OpFollowRunLog:    ScopeRead,

	OpFollowRunEvents:   ScopeRead,
	OpFollowSpaceEvents: ScopeRead,

	OpAdminListSpaces:  ScopeAdmin,
	OpAdminUpdateSpace: ScopeAdmin,
	OpAdminDeleteSpace: ScopeAdmin,

	OpFollowRunResult:      ScopeRead,
	OpInvokeFunctionStream: ScopeFunctionsInvoke,
}

// Scope returns the scope op needs ("" for an unknown operation, which
// callers must refuse).
func (op Op) Scope() string { return opScopes[op] }

// AllOps lists every operation, for tests.
func AllOps() []Op {
	out := make([]Op, 0, len(opScopes))
	for op := range opScopes {
		out = append(out, op)
	}
	return out
}
