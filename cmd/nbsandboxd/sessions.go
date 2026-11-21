// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/packages"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// sessionPackages gives a session its space's package set, as a run gets
// it: built from requirements.txt (waiting while it builds), unpacked on
// this worker. A set that can't be built leaves the session without it,
// with a note saying why.
func sessionPackages(p *packages.Service, sp *spaces.Service, imageID func() string) func(context.Context, string) (string, string, error) {
	return func(ctx context.Context, spaceID string) (string, string, error) {
		if !p.Enabled() {
			return "", "", nil
		}
		req, ok := sp.ReadSpaceFileContext(ctx, spaceID, "requirements.txt", packages.MaxRequirementsBytes+1)
		if !ok {
			return "", "", nil
		}
		key, err := p.ForSpace(ctx, spaceID, req, imageID())
		if err != nil || key == "" {
			return "", "", err
		}
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			ps, err := p.Status(ctx, key)
			if err != nil {
				return "", "", err
			}
			switch ps.Status {
			case store.PackagesReady:
				dir, err := p.Ensure(ctx, key)
				if err == nil {
					p.RecordSpaceLock(ctx, spaceID, ps)
				}
				return dir, "", err
			case store.PackagesFailed:
				return "", "the session started without the space's packages: they could not be built (" + ps.Error + ")", nil
			}
			select {
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-t.C:
			}
		}
	}
}
