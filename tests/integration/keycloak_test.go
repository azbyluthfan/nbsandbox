// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
)

// End-to-end authentication against a real Keycloak
// (deploy/docker-compose.keycloak.yml, make test-keycloak). Skipped unless
// NBSANDBOX_KEYCLOAK_URL names it.

var keycloakURL = os.Getenv("NBSANDBOX_KEYCLOAK_URL")

func needKeycloak(t *testing.T) {
	t.Helper()
	if keycloakURL == "" {
		t.Skip("set NBSANDBOX_KEYCLOAK_URL (make test-keycloak)")
	}
}

// kcToken asks Keycloak's token endpoint for an access token.
func kcToken(t *testing.T, form url.Values, basicUser, basicPass string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", keycloakURL+"/realms/nbsandbox/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicUser != "" {
		req.SetBasicAuth(basicUser, basicPass)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.AccessToken == "" {
		t.Fatalf("token request %v: %d %s: %s", form.Get("grant_type"), resp.StatusCode, out.Error, out.Description)
	}
	return out.AccessToken
}

// userToken signs a test user in with the given scopes (password grant:
// test only).
func userToken(t *testing.T, user, scope string) string {
	t.Helper()
	return kcToken(t, url.Values{"grant_type": {"password"}, "client_id": {"nbsandbox-cli"}, "username": {user},
		"password": {user + "-test-password"}, "scope": {scope}}, "", "")
}

// claims decodes a token's payload (no verification: for test assertions).
func claims(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	return c
}

func bearer(string) *apiclient.Client {
	return &apiclient.Client{BaseURL: baseURL, HTTP: httpClient}
}

func asToken(t *testing.T, tok, method, path string, body any) apiclient.Response {
	t.Helper()
	c := bearer(tok)
	who := &apiclient.Identity{Token: tok}
	var r apiclient.Response
	var err error
	if body != nil {
		r, err = c.JSONCall(context.Background(), who, method, path, body)
	} else {
		r, err = c.Do(context.Background(), who, method, path, nil, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const allScopes = "nbsandbox.read nbsandbox.spaces.write nbsandbox.files.write nbsandbox.runs"

// A Keycloak user runs a notebook with a token; the space is theirs
// (kc:<sub>), the other user can't see it, and basic-auth users neither.
func TestKeycloakUserRunsANotebook(t *testing.T) {
	needKeycloak(t)
	ctx := context.Background()
	tok := userToken(t, "kc-alice", allScopes)
	who := &apiclient.Identity{Token: tok}
	client := bearer(tok)

	spaceID, err := client.CreateSpace(ctx, who, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.DeleteSpace(context.Background(), &apiclient.Identity{Token: userToken(t, "kc-alice", allScopes)}, spaceID)
	})
	nb, err := os.ReadFile(repoRoot() + "/tests/workloads/smoke.ipynb")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Upload(ctx, who, spaceID, map[string][]byte{"smoke.ipynb": nb}); err != nil {
		t.Fatal(err)
	}
	run, err := client.StartRun(ctx, who, spaceID, "smoke.ipynb", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err = client.WaitRun(ctx, who, spaceID, run.ID, 3*time.Minute)
	if err != nil || run.Status != "succeeded" {
		t.Fatalf("run %+v: %v", run, err)
	}

	// Bob's token, and alice the basic-auth user (a different caller ID),
	// see nothing of it.
	bob := userToken(t, "kc-bob", allScopes)
	if r := asToken(t, bob, "GET", "/v1/spaces/"+spaceID, nil); r.Code != http.StatusNotFound {
		t.Errorf("bob: %d", r.Code)
	}
	if r := call(t, "GET", "/v1/spaces/"+spaceID, "alice", nil, nil); r.Code != http.StatusNotFound {
		t.Errorf("basic-auth alice: %d", r.Code)
	}
	finding(t, "keycloak-user-run", "a Keycloak user creates a space and runs a notebook with an access token; another user and a basic-auth user try to read it",
		"the run succeeds; the space is the token user's; the others get 404", "run "+run.Status, true)
}

// Each endpoint needs its scope, and a token for another API is refused.
func TestKeycloakScopesAndAudience(t *testing.T) {
	needKeycloak(t)
	read := userToken(t, "kc-alice", "nbsandbox.read")
	if r := asToken(t, read, "GET", "/v1/spaces", nil); r.Code != http.StatusOK {
		t.Fatalf("list with read: %d %s", r.Code, r.Body)
	}
	r := asToken(t, read, "POST", "/v1/spaces", map[string]string{"name": "x"})
	if r.Code != http.StatusForbidden || !strings.Contains(r.Header.Get("WWW-Authenticate"), `scope="nbsandbox.spaces.write"`) {
		t.Errorf("create with read only: %d %q", r.Code, r.Header.Get("WWW-Authenticate"))
	}
	// A token for the backend only (aud nbsandbox-backend, no nbsandbox scope).
	other := userToken(t, "kc-alice", "backend")
	r = asToken(t, other, "GET", "/v1/spaces", nil)
	if r.Code != http.StatusUnauthorized || !strings.Contains(r.Header.Get("WWW-Authenticate"), "invalid_token") {
		t.Errorf("another API's token: %d %q", r.Code, r.Header.Get("WWW-Authenticate"))
	}
	// A service calling as itself with read only.
	svc := kcToken(t, url.Values{"grant_type": {"client_credentials"}}, "reader-app", "reader-test-secret")
	if r := asToken(t, svc, "GET", "/v1/runtime", nil); r.Code != http.StatusOK {
		t.Errorf("reader-app runtime: %d", r.Code)
	}
	if r := asToken(t, svc, "POST", "/v1/spaces", map[string]string{"name": "x"}); r.Code != http.StatusForbidden {
		t.Errorf("reader-app create: %d", r.Code)
	}
	finding(t, "keycloak-scopes", "call with a read-only token, a token issued for another API, and a read-only service account",
		"reads allowed; writes 403 naming the missing scope; another API's token 401", "as expected", true)
}

// A backend exchanges its user's token (issued for the backend) for one
// issued to nbsandbox (RFC 8693 token exchange) and starts a run on their
// behalf: the space is the user's, the run records the backend.
func TestKeycloakTokenExchange(t *testing.T) {
	needKeycloak(t)
	ctx := context.Background()
	subject := userToken(t, "kc-alice", "backend")
	if r := asToken(t, subject, "GET", "/v1/spaces", nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("the backend's own token must not work at nbsandbox: %d", r.Code)
	}
	exchanged := kcToken(t, url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {subject},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"audience":           {"nbsandbox"},
		"scope":              {allScopes},
	}, "nbsandbox-backend", "backend-test-secret")
	c := claims(t, exchanged)
	if c["sub"] != claims(t, subject)["sub"] {
		t.Fatalf("exchanged token is for %v, not the user", c["sub"])
	}
	who := &apiclient.Identity{Token: exchanged}
	client := bearer(exchanged)
	spaceID, err := client.CreateSpace(ctx, who, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.DeleteSpace(context.Background(), who, spaceID) })
	nb, _ := os.ReadFile(repoRoot() + "/tests/workloads/smoke.ipynb")
	if err := client.Upload(ctx, who, spaceID, map[string][]byte{"smoke.ipynb": nb}); err != nil {
		t.Fatal(err)
	}
	run, err := client.StartRun(ctx, who, spaceID, "smoke.ipynb", nil)
	if err != nil {
		t.Fatal(err)
	}
	var view struct{ Client string }
	_ = asToken(t, exchanged, "GET", "/v1/spaces/"+spaceID+"/runs/"+run.ID, nil).JSON(&view)
	if view.Client != "nbsandbox-backend" {
		t.Errorf("run client %q, want nbsandbox-backend", view.Client)
	}
	// The user's own nbsandbox token sees the space: it's theirs.
	if r := asToken(t, userToken(t, "kc-alice", "nbsandbox.read"), "GET", "/v1/spaces/"+spaceID, nil); r.Code != http.StatusOK {
		t.Errorf("the user can't see the space the backend made for them: %d", r.Code)
	}
	if _, err := client.WaitRun(ctx, who, spaceID, run.ID, 3*time.Minute); err != nil {
		t.Fatal(err)
	}
	finding(t, "keycloak-token-exchange", "a backend exchanges its user's token for an nbsandbox token (RFC 8693) and starts a run",
		"the backend's own token is refused; the exchanged one acts as the user; the run records the backend", "client "+view.Client, view.Client == "nbsandbox-backend")
}

// A token stops working soon after the user signs out at Keycloak: with
// introspection on (deploy/docker-compose.keycloak.yml, 2 s cache),
// nbsandbox asks Keycloak whether each token is still active.
func TestKeycloakRevokedTokenIsRefused(t *testing.T) {
	needKeycloak(t)
	form := url.Values{"grant_type": {"password"}, "client_id": {"nbsandbox-cli"}, "username": {"kc-alice"},
		"password": {"kc-alice-test-password"}, "scope": {"nbsandbox.read"}}
	req, _ := http.NewRequest("POST", keycloakURL+"/realms/nbsandbox/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var toks struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&toks)
	_ = resp.Body.Close()
	if toks.AccessToken == "" || toks.RefreshToken == "" {
		t.Fatalf("sign-in: %d", resp.StatusCode)
	}
	if r := asToken(t, toks.AccessToken, "GET", "/v1/spaces", nil); r.Code != http.StatusOK {
		t.Fatalf("before sign-out: %d %s", r.Code, r.Body)
	}

	// Sign out: Keycloak ends the session, and its tokens become inactive.
	out := url.Values{"client_id": {"nbsandbox-cli"}, "refresh_token": {toks.RefreshToken}}
	req, _ = http.NewRequest("POST", keycloakURL+"/realms/nbsandbox/protocol/openid-connect/logout", strings.NewReader(out.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err = httpClient.Do(req); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-out: %d", resp.StatusCode)
	}

	deadline := time.Now().Add(15 * time.Second)
	var code int
	for time.Now().Before(deadline) {
		r := asToken(t, toks.AccessToken, "GET", "/v1/spaces", nil)
		if code = r.Code; code == http.StatusUnauthorized {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	finding(t, "keycloak-revocation", "sign out at Keycloak, then call nbsandbox with the access token issued before",
		"refused (401) within the introspection cache (2 s here), long before the token expires", fmt.Sprintf("HTTP %d", code), code == http.StatusUnauthorized)
	if code != http.StatusUnauthorized {
		t.Fatalf("token still accepted 15 s after sign-out: %d", code)
	}
}
