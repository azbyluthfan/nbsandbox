// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package s3store

import (
	"context"
	"os"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/blob/blobtest"
)

// The shared blob suite against a real S3-compatible store (`make
// test-blob` starts RustFS and sets these). Skipped otherwise.
func TestS3PassesSuite(t *testing.T) {
	ep := os.Getenv("NBSANDBOX_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("set NBSANDBOX_TEST_S3_ENDPOINT (make test-blob) to run against an S3-compatible store")
	}
	s, err := New(context.Background(), Config{
		Endpoint: ep, Bucket: "nbsandbox-test", CreateBucket: true, Prefix: "suite/",
		AccessKey: os.Getenv("NBSANDBOX_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("NBSANDBOX_TEST_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	blobtest.Run(t, func(*testing.T) blob.Store { return s })
}

func TestConfigRefusals(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no endpoint":      {Bucket: "b"},
		"no bucket":        {Endpoint: "x:9000"},
		"scheme in host":   {Endpoint: "http://x:9000", Bucket: "b"},
		"uppercase prefix": {Endpoint: "x:9000", Bucket: "b", Prefix: "Data/"},
	} {
		if _, err := New(context.Background(), cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
