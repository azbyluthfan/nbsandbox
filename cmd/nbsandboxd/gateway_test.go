// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestGatewayCommand(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	var out bytes.Buffer
	if err := runGateway(context.Background(), nil, env(nil), &out); err == nil || !strings.Contains(err.Error(), "NBSANDBOX_GATEWAY_KEY") {
		t.Fatalf("no key: %v", err)
	}
	const key = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := runGateway(ctx, []string{"-listen", "127.0.0.1:0"}, env(map[string]string{"NBSANDBOX_GATEWAY_KEY": key}), &out)
	if err != nil || !strings.Contains(out.String(), "gateway listening") || strings.Contains(out.String(), key) {
		t.Fatalf("serve: %v\n%s", err, out.String())
	}
}
