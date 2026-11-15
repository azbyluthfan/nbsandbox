// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package check

import "github.com/azbyluthfan/nbsandbox/internal/apiclient"

// The API client lives in internal/apiclient; these names keep the checks
// short.
type (
	Client   = apiclient.Client
	Identity = apiclient.Identity
	Response = apiclient.Response
	Run      = apiclient.Run
)
