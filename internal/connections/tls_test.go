// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package connections

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

func testCAPEM(t *testing.T) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now(),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func db(typ, host string, extra map[string]any) Input {
	s := map[string]any{"host": host, "database": "crm", "user": "reader"}
	for k, v := range extra {
		s[k] = v
	}
	return Input{Name: "c" + strings.ReplaceAll(host, ".", "-"), Type: typ, Settings: s, Secret: SecretInput{Value: ptr("pw")}}
}

func TestTLSSettings(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t)
	ca := testCAPEM(t)
	key := "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIJ+DYvh6SEqVTm50DFtMDoQikTmiCqirVv9mWG9qfSnF\n-----END PRIVATE KEY-----\n"
	for name, in := range map[string]Input{
		"an unknown mode":         db(TypePostgreSQL, "a.internal", map[string]any{"tls": "maybe"}),
		"a ca without verify":     db(TypePostgreSQL, "b.internal", map[string]any{"tls": "require", "ca": ca}),
		"a ca that isn't PEM":     db(TypePostgreSQL, "c.internal", map[string]any{"tls": "verify", "ca": "not a certificate"}),
		"a key for a ca":          db(TypePostgreSQL, "d.internal", map[string]any{"tls": "verify", "ca": key}),
		"a ca with more after it": db(TypePostgreSQL, "e.internal", map[string]any{"tls": "verify", "ca": ca + "junk"}),
		"sqlserver can't verify":  db(TypeSQLServer, "f.internal", map[string]any{"tls": "verify"}),
		"mysql verify by address": db(TypeMySQL, "10.0.0.5", map[string]any{"tls": "verify"}),
		"an s3 ca without https":  {Name: "g", Type: TypeS3, Settings: map[string]any{"endpoint": "minio.internal", "bucket": "b", "use_ssl": false, "ca": ca}},
		"an s3 ca that isn't PEM": {Name: "h", Type: TypeS3, Settings: map[string]any{"endpoint": "minio.internal", "bucket": "b", "ca": "x"}},
		"a ca over 64 KiB":        db(TypePostgreSQL, "i.internal", map[string]any{"tls": "verify", "ca": strings.Repeat(ca, 200)}),
	} {
		if _, err := s.Create(ctx, "alice", in); !isInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The default is prefer; verify takes a CA (or the system's).
	v, err := s.Create(ctx, "alice", db(TypePostgreSQL, "pg.internal", nil))
	if err != nil || v.Settings["tls"] != TLSPrefer {
		t.Fatalf("default: %+v %v", v, err)
	}
	for _, in := range []Input{
		db(TypePostgreSQL, "pg2.internal", map[string]any{"tls": "verify", "ca": ca + ca}),
		db(TypePostgreSQL, "10.0.0.6", map[string]any{"tls": "verify"}),
		db(TypeMySQL, "my.internal", map[string]any{"tls": "verify", "ca": ca}),
		db(TypeSQLServer, "ms.internal", map[string]any{"tls": "require"}),
		{Name: "lake", Type: TypeS3, Settings: map[string]any{"endpoint": "minio.internal:9000", "bucket": "b", "ca": ca}},
	} {
		if _, err := s.Create(ctx, "alice", in); err != nil {
			t.Errorf("%s %v: %v", in.Type, in.Settings, err)
		}
	}
}

// MySQL with tls verify: its host name resolves to a loopback address of the
// fetcher's own, where its forwarder listens.
func TestFetcherHostAliases(t *testing.T) {
	ctx := context.Background()
	s, db_, _ := newService(t)
	s.d.Fetcher = FetcherSettings{Image: "fetcher:1", GatewayAddress: "nbsandbox-gateway:3128", GatewayKey: gwKey}
	now := time.Now()
	if err := db_.CreateSpace(ctx, store.Space{ID: "s1", Owner: "alice", Name: "s1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for n, in := range map[string]Input{
		"a": db(TypeMySQL, "my.internal", map[string]any{"tls": "verify"}),
		"b": db(TypeMySQL, "My.Internal", map[string]any{"tls": "verify", "port": 3307}), // the same host: one address
		"c": db(TypeMySQL, "other.internal", map[string]any{"tls": "verify"}),
		"d": db(TypeMySQL, "plain.internal", map[string]any{"tls": "require"}),
		"e": db(TypePostgreSQL, "pg.internal", map[string]any{"tls": "verify"}),
	} {
		in.Name = "conn-" + n
		v, err := s.Create(ctx, "alice", in)
		if err != nil {
			t.Fatal(err)
		}
		names[n] = v.ID
	}
	f, err := s.Fetcher(ctx, "r1", "alice", names, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"my.internal": "127.0.0.2", "other.internal": "127.0.0.3"}
	if len(f.Hosts) != len(want) || f.Hosts["my.internal"] != want["my.internal"] || f.Hosts["other.internal"] != want["other.internal"] {
		t.Errorf("hosts: %v", f.Hosts)
	}
	var cfg fetcherConfig
	if err := json.Unmarshal(f.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	listen := map[string]string{}
	for _, src := range cfg.Sources {
		listen[src.Name] = src.Listen
	}
	if listen["a"] != "127.0.0.2" || listen["b"] != "127.0.0.2" || listen["c"] != "127.0.0.3" || listen["d"] != "" || listen["e"] != "" {
		t.Errorf("listen: %v", listen)
	}
}
