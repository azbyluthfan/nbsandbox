// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import "github.com/azbyluthfan/nbsandbox/internal/tarfs"

// The run's files go in and out of Pods as tar streams (internal/tarfs).
var (
	writeTar = tarfs.Write
	readTar  = tarfs.Read
	clearDir = tarfs.ClearDir
)

// ErrTooLarge: the files copied out of a Pod are larger than allowed.
var ErrTooLarge = tarfs.ErrTooLarge
