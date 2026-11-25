// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package secrets keeps connections' credentials (docs/plan-data-access.md).
// A provider either stores a value nbsandbox was given (managed) or vouches
// for one that already exists (referenced). Values go in, never out through
// the API: only the run that needs a credential gets it, mounted by the
// platform (Kubernetes) or, in development, from the built-in store.
package secrets

import (
	"context"
	"errors"
)

// Provider keeps connections' credentials.
type Provider interface {
	// Name is the provider's configured name (builtin, kubernetes).
	Name() string
	// Store writes a connection's managed credential, replacing any value it
	// had, and returns where it is kept.
	Store(ctx context.Context, connectionID string, value []byte) (ref string, err error)
	// Remove deletes a managed credential. It never deletes a referenced
	// one, and a missing one is not an error.
	Remove(ctx context.Context, ref string) error
	// Check verifies a referenced credential exists, without reading it out.
	Check(ctx context.Context, ref string) error
}

// ErrNoReferences means the provider can only keep values it is given.
var ErrNoReferences = errors.New("this secret provider can't refer to existing secrets: give the secret's value instead")

// ErrBadReference means a reference doesn't name a secret the provider
// could hold.
var ErrBadReference = errors.New("not a valid secret reference")
