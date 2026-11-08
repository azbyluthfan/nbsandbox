// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

// health asks /readyz and exits non-zero unless nbsandboxd is ready. The
// service image is distroless (no curl), so the container healthcheck uses
// this.
func health(args []string) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8080/readyz", "readiness URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready (%d): %s", resp.StatusCode, body)
	}
	return nil
}
