// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
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

// browser is a cookie-keeping HTTP client, as a browser signing in.
func browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: httpClient.Transport}
}

var kcLoginForm = regexp.MustCompile(`<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)

// uiSignIn signs user in to the web UI through Keycloak's real login page,
// and returns the browser and the page it lands on.
func uiSignIn(t *testing.T, user string) (*http.Client, string) {
	t.Helper()
	b := browser(t)
	resp, err := b.Get(uiURL + "/ui/login?return=" + url.QueryEscape("/#/after"))
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m := kcLoginForm.FindSubmatch(page)
	if resp.StatusCode != http.StatusOK || m == nil || !strings.HasPrefix(resp.Request.URL.String(), keycloakURL) {
		t.Fatalf("expected Keycloak's login page, got %d at %s", resp.StatusCode, resp.Request.URL)
	}
	resp, err = b.PostForm(html.UnescapeString(string(m[1])), url.Values{"username": {user}, "password": {user + "-test-password"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Request.URL.String(), uiURL+"/") {
		t.Fatalf("after Keycloak's login: %d at %s", resp.StatusCode, resp.Request.URL)
	}
	return b, resp.Request.URL.String()
}

// uiDo calls the UI's API as the signed-in browser, as the UI's scripts do.
func uiDo(t *testing.T, b *http.Client, method, path string, body io.Reader, ct string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, uiURL+path, body)
	req.Header.Set("X-Nbsandbox-UI", "1")
	req.Header.Set("Origin", uiURL)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// A user signs in to the web UI through Keycloak's login page and runs a
// notebook there; the space and the run are the same caller's as with an
// API token. Signing out ends the session and Keycloak's.
func TestKeycloakWebUISignIn(t *testing.T) {
	needKeycloak(t)
	b, landed := uiSignIn(t, "kc-alice")
	if landed != uiURL+"/#/after" && landed != uiURL+"/" {
		t.Errorf("landed on %s, want the return path", landed)
	}
	code, body := uiDo(t, b, "GET", "/ui/api/session", nil, "")
	var info struct{ Mode, User, Name string }
	_ = json.Unmarshal(body, &info)
	tok := userToken(t, "kc-alice", allScopes)
	sub, _ := claims(t, tok)["sub"].(string)
	if code != http.StatusOK || info.Mode != "oidc" || info.User != "kc:"+sub || info.Name != "Alice Test" {
		t.Fatalf("session: %d %s (want user kc:%s)", code, body, sub)
	}

	// A space, an upload and a run through the UI.
	code, body = uiDo(t, b, "POST", "/ui/api/v1/spaces", strings.NewReader(`{"name":"`+t.Name()+`"}`), "application/json")
	var sp struct{ ID string }
	if _ = json.Unmarshal(body, &sp); code != http.StatusCreated || sp.ID == "" {
		t.Fatalf("create space: %d %s", code, body)
	}
	t.Cleanup(func() {
		_ = bearer("").DeleteSpace(context.Background(), &apiclient.Identity{Token: userToken(t, "kc-alice", allScopes)}, sp.ID)
	})
	nb, err := os.ReadFile(repoRoot() + "/tests/workloads/smoke.ipynb")
	if err != nil {
		t.Fatal(err)
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	fw, _ := mw.CreateFormFile("file", "smoke.ipynb")
	_, _ = fw.Write(nb)
	_ = mw.Close()
	if code, body = uiDo(t, b, "POST", "/ui/api/v1/spaces/"+sp.ID+"/files", &form, mw.FormDataContentType()); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, body)
	}
	code, body = uiDo(t, b, "POST", "/ui/api/v1/spaces/"+sp.ID+"/runs", strings.NewReader(`{"notebook":"smoke.ipynb"}`), "application/json")
	var run struct{ ID string }
	if _ = json.Unmarshal(body, &run); code != http.StatusAccepted && code != http.StatusCreated || run.ID == "" {
		t.Fatalf("run: %d %s", code, body)
	}
	// The same caller with an API token sees the space and the run.
	done, err := bearer("").WaitRun(context.Background(), &apiclient.Identity{Token: tok}, sp.ID, run.ID, 3*time.Minute)
	if err != nil || done.Status != "succeeded" {
		t.Fatalf("the UI's run, seen with alice's token: %+v %v", done, err)
	}
	// Without the UI's header, a change is refused (CSRF).
	req, _ := http.NewRequest("POST", uiURL+"/ui/api/v1/spaces", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a change without the UI's header: %d", resp.StatusCode)
	}

	// Sign out: the UI's session ends, then Keycloak's, which returns to
	// the signed-out page.
	code, body = uiDo(t, b, "POST", "/ui/logout", nil, "")
	var out struct{ Next string }
	if _ = json.Unmarshal(body, &out); code != http.StatusOK || !strings.HasPrefix(out.Next, keycloakURL) {
		t.Fatalf("logout: %d %s", code, body)
	}
	resp, err = b.Get(out.Next)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Request.URL.String() != uiURL+"/ui/signed-out" || !strings.Contains(string(page), "signed out") {
		t.Errorf("after Keycloak's sign-out: %d at %s", resp.StatusCode, resp.Request.URL)
	}
	if code, _ = uiDo(t, b, "GET", "/ui/api/session", nil, ""); code != http.StatusUnauthorized {
		t.Errorf("after signing out: %d", code)
	}
	// Keycloak's session is gone too: signing in shows its login page again.
	resp, err = b.Get(uiURL + "/ui/login")
	if err != nil {
		t.Fatal(err)
	}
	page, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	finding(t, "keycloak-web-ui", "sign in to the web UI through Keycloak's login page, run a notebook there, check it with an API token, sign out",
		"the UI user is the token's caller (same space and run); a change without the UI's header is refused; signing out ends both sessions",
		"run "+done.Status, kcLoginForm.Match(page))
	if !kcLoginForm.Match(page) {
		t.Error("Keycloak still signed in after the UI's sign-out")
	}
}

// A user signed out at Keycloak (here by an administrator) loses their UI
// session within the introspection cache.
func TestKeycloakWebUISessionRevoked(t *testing.T) {
	needKeycloak(t)
	b, _ := uiSignIn(t, "kc-bob")
	if code, body := uiDo(t, b, "GET", "/ui/api/v1/spaces", nil, ""); code != http.StatusOK {
		t.Fatalf("signed in: %d %s", code, body)
	}
	// Keycloak's administrator signs in to the master realm.
	resp, err := httpClient.PostForm(keycloakURL+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"keycloak-test-only"}})
	if err != nil {
		t.Fatal(err)
	}
	var at struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&at)
	_ = resp.Body.Close()
	admin := at.AccessToken
	if admin == "" {
		t.Fatalf("admin sign-in: %d", resp.StatusCode)
	}
	adminCall := func(method, path string) []byte {
		req, _ := http.NewRequest(method, keycloakURL+"/admin/realms/nbsandbox"+path, nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			t.Fatalf("admin %s %s: %d %s", method, path, resp.StatusCode, out)
		}
		return out
	}
	var users []struct{ ID string }
	_ = json.Unmarshal(adminCall("GET", "/users?exact=true&username=kc-bob"), &users)
	if len(users) != 1 {
		t.Fatalf("kc-bob: %v", users)
	}
	adminCall("POST", "/users/"+users[0].ID+"/logout")

	deadline := time.Now().Add(15 * time.Second)
	var code int
	for time.Now().Before(deadline) {
		if code, _ = uiDo(t, b, "GET", "/ui/api/v1/spaces", nil, ""); code == http.StatusUnauthorized {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	finding(t, "keycloak-web-ui-revoked", "an administrator signs a user out at Keycloak while they have a web UI session",
		"the UI session ends (401) within the introspection cache (2 s here)", fmt.Sprintf("HTTP %d", code), code == http.StatusUnauthorized)
	if code != http.StatusUnauthorized {
		t.Fatalf("UI session still accepted 15 s after Keycloak's sign-out: %d", code)
	}
}
