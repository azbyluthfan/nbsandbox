// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAuthDefaultsToBasic(t *testing.T) {
	s, err := Load("", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Auth.BasicEnabled() || s.Auth.OIDC != nil {
		t.Fatalf("default auth: basic %v, oidc %+v", s.Auth.BasicEnabled(), s.Auth.OIDC)
	}
}

func TestAuthOIDCFromFileAndEnv(t *testing.T) {
	s, err := Load(writeFile(t, `
auth:
  basic: false
  oidc:
    issuer: https://login.microsoftonline.com/t/v2.0
    audience: aaaa
    id_prefix: entra
    id_claims: [tid, oid]
`), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if s.Auth.BasicEnabled() || s.Auth.OIDC.IDPrefix != "entra" || len(s.Auth.OIDC.IDClaims) != 2 {
		t.Fatalf("from file: %+v %+v", s.Auth, s.Auth.OIDC)
	}
	s, err = Load("", envMap(map[string]string{
		"NBSANDBOX_OIDC_ISSUER": "https://kc.example/realms/x", "NBSANDBOX_OIDC_AUDIENCE": "nbsandbox",
		"NBSANDBOX_OIDC_ID_PREFIX": "kc", "NBSANDBOX_OIDC_ID_CLAIMS": "sub", "NBSANDBOX_AUTH_BASIC": "false",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.Auth.BasicEnabled() || s.Auth.OIDC == nil || s.Auth.OIDC.Issuer != "https://kc.example/realms/x" || s.Auth.OIDC.IDClaims[0] != "sub" {
		t.Fatalf("from env: %+v %+v", s.Auth, s.Auth.OIDC)
	}
}

func TestAuthRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"nobody can sign in":    "auth:\n  basic: false\n",
		"oidc without audience": "auth:\n  oidc:\n    issuer: https://idp.example\n    id_prefix: x\n    id_claims: [sub]\n",
		"email as the id":       "auth:\n  oidc:\n    issuer: https://idp.example\n    audience: a\n    id_prefix: x\n    id_claims: [email]\n",
		"http issuer":           "auth:\n  oidc:\n    issuer: http://idp.example\n    audience: a\n    id_prefix: x\n    id_claims: [sub]\n",
	} {
		if _, err := Load(writeFile(t, body), noEnv); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load("", envMap(map[string]string{"NBSANDBOX_AUTH_BASIC": "maybe"})); err == nil || !strings.Contains(err.Error(), "NBSANDBOX_AUTH_BASIC") {
		t.Fatalf("bad NBSANDBOX_AUTH_BASIC: %v", err)
	}
}
