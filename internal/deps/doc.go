// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package deps checks a notebook's imports, and any requirements.txt in its
// space, against the runner image manifest and reports packages the image does
// not provide.
package deps
