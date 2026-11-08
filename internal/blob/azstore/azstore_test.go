// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package azstore

import (
	"context"
	"os"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/blob/blobtest"
)

// The shared blob suite against Azure Blob Storage, normally Azurite (`make
// test-blob` starts it and sets the connection string). Skipped otherwise.
func TestAzurePassesSuite(t *testing.T) {
	cs := os.Getenv("NBSANDBOX_TEST_AZURE_CONNECTION_STRING")
	if cs == "" {
		t.Skip("set NBSANDBOX_TEST_AZURE_CONNECTION_STRING (make test-blob) to run against Azure Blob Storage or Azurite")
	}
	s, err := New(context.Background(), Config{ConnectionString: cs, Container: "nbsandbox-test", Prefix: "suite/", CreateContainer: true})
	if err != nil {
		t.Fatal(err)
	}
	blobtest.Run(t, func(*testing.T) blob.Store { return s })
}

func TestConfigRefusals(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no container":     {AccountURL: "https://a.blob.core.windows.net"},
		"no credentials":   {Container: "c"},
		"http account url": {AccountURL: "http://a.blob.core.windows.net", Container: "c"},
		"uppercase prefix": {AccountURL: "https://a.blob.core.windows.net", Container: "c", Prefix: "Data/"},
	} {
		if _, err := New(context.Background(), cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
