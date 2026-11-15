// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package uiauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/crypto/bcrypt"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// memStore keeps sign-ins and sessions in memory.
type memStore struct {
	mu       sync.Mutex
	logins   map[string]store.UILogin
	sessions map[string]store.UISession
}

func newMemStore() *memStore {
	return &memStore{logins: map[string]store.UILogin{}, sessions: map[string]store.UISession{}}
}

func (m *memStore) CreateUILogin(_ context.Context, l store.UILogin) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logins[string(l.StateHash)] = l
	return nil
}

func (m *memStore) TakeUILogin(_ context.Context, h []byte, now time.Time) (store.UILogin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.logins[string(h)]
	delete(m.logins, string(h))
	if !ok || !now.Before(l.ExpiresAt) {
		return store.UILogin{}, store.ErrNotFound
	}
	return l, nil
}

func (m *memStore) CreateUISession(_ context.Context, s store.UISession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[string(s.IDHash)] = s
	return nil
}

func (m *memStore) UISession(_ context.Context, h []byte) (store.UISession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[string(h)]
	if !ok {
		return store.UISession{}, store.ErrNotFound
	}
	return s, nil
}

func (m *memStore) TouchUISession(_ context.Context, h []byte, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[string(h)]; ok && at.After(s.LastSeenAt) {
		s.LastSeenAt = at
		m.sessions[string(h)] = s
	}
	return nil
}

func (m *memStore) UpdateUISessionTokens(_ context.Context, h, t []byte, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[string(h)]; ok {
		s.Tokens, s.AccessExpires = t, exp
		m.sessions[string(h)] = s
	}
	return nil
}

func (m *memStore) DeleteUISession(_ context.Context, h []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, string(h))
	return nil
}

func (m *memStore) DeleteExpiredUI(_ context.Context, now, idle, created time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for k, l := range m.logins {
		if !now.Before(l.ExpiresAt) {
			delete(m.logins, k)
		}
	}
	for k, s := range m.sessions {
		if s.LastSeenAt.Before(idle) || s.CreatedAt.Before(created) {
			delete(m.sessions, k)
			n++
		}
	}
	return n, nil
}

// only returns the single session (tests sign in once).
func (m *memStore) only(t *testing.T) store.UISession {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(m.sessions))
	}
	for _, s := range m.sessions {
		return s
	}
	return store.UISession{}
}

func (m *memStore) edit(f func(*store.UISession)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		f(&s)
		m.sessions[k] = s
	}
}

// fakeIDP is an identity provider with the authorization code flow, PKCE,
// refresh tokens and sign-out.
type fakeIDP struct {
	t   *testing.T
	srv *httptest.Server
	key jose.JSONWebKey

	mu        sync.Mutex
	codes     map[string]codeGrant
	refreshes map[string]bool // valid refresh tokens
	refreshed atomic.Int32

	sub       string
	scope     string
	audience  string // of access tokens
	expiresIn int64
	badNonce  bool
	deny      string // an error to send back instead of a code
}

type codeGrant struct{ nonce, challenge, redirect string }

func newFakeIDP(t *testing.T) *fakeIDP {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIDP{t: t, key: jose.JSONWebKey{Key: k, KeyID: "k1", Algorithm: string(jose.RS256), Use: "sig"},
		codes: map[string]codeGrant{}, refreshes: map[string]bool{},
		sub: "alice-sub", scope: "nbsandbox.read nbsandbox.runs", audience: "nbsandbox", expiresIn: 300}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		u := p.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": u, "jwks_uri": u + "/keys",
			"authorization_endpoint": u + "/authorize?tenant=x", "token_endpoint": u + "/token", "end_session_endpoint": u + "/logout"})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{p.key.Public()}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("tenant") != "x" || q.Get("client_id") != "nbsandbox-ui" || q.Get("response_type") != "code" ||
			q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "openid") {
			http.Error(w, "bad authorization request: "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		back, _ := url.Parse(q.Get("redirect_uri"))
		bq := url.Values{"state": {q.Get("state")}}
		if p.deny != "" {
			bq.Set("error", p.deny)
		} else {
			code := base64.RawURLEncoding.EncodeToString([]byte(q.Get("state")))[:20]
			p.mu.Lock()
			p.codes[code] = codeGrant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
			p.mu.Unlock()
			bq.Set("code", code)
		}
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f := r.PostForm
		fail := func(e string) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": e})
		}
		if f.Get("client_id") != "nbsandbox-ui" || f.Get("client_secret") != "ui-secret" {
			fail("invalid_client")
			return
		}
		nonce := ""
		switch f.Get("grant_type") {
		case "authorization_code":
			p.mu.Lock()
			g, ok := p.codes[f.Get("code")]
			delete(p.codes, f.Get("code"))
			p.mu.Unlock()
			sum := sha256.Sum256([]byte(f.Get("code_verifier")))
			if !ok || g.redirect != f.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
				fail("invalid_grant")
				return
			}
			nonce = g.nonce
			if p.badNonce {
				nonce = "someone-else's"
			}
		case "refresh_token":
			p.mu.Lock()
			ok := p.refreshes[f.Get("refresh_token")]
			delete(p.refreshes, f.Get("refresh_token")) // rotated
			p.mu.Unlock()
			if !ok {
				fail("invalid_grant")
				return
			}
			p.refreshed.Add(1)
		default:
			fail("unsupported_grant_type")
			return
		}
		now := time.Now()
		access := p.sign(map[string]any{"iss": p.srv.URL, "aud": p.audience, "sub": p.sub, "scope": p.scope,
			"azp": "nbsandbox-ui", "iat": now.Unix(), "exp": now.Add(time.Duration(p.expiresIn) * time.Second).Unix()})
		refresh := "rt-" + base64.RawURLEncoding.EncodeToString([]byte(now.String()))
		p.mu.Lock()
		p.refreshes[refresh] = true
		p.mu.Unlock()
		out := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": p.expiresIn, "refresh_token": refresh}
		if nonce != "" {
			out["id_token"] = p.sign(map[string]any{"iss": p.srv.URL, "aud": "nbsandbox-ui", "sub": p.sub, "nonce": nonce,
				"name": "Alice Example", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIDP) sign(claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		p.t.Fatal(err)
	}
	b, _ := json.Marshal(claims)
	jws, err := signer.Sign(b)
	if err != nil {
		p.t.Fatal(err)
	}
	s, _ := jws.CompactSerialize()
	return s
}

// revokeRefreshTokens: the user signed out at the provider.
func (p *fakeIDP) revokeRefreshTokens() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshes = map[string]bool{}
}

// ui is the web UI's listener with sign-in, and a browser.
type ui struct {
	t       *testing.T
	s       *Service
	srv     *httptest.Server
	store   *memStore
	browser *http.Client
}

func newUI(t *testing.T, mode Mode, idp *fakeIDP) *ui {
	t.Helper()
	u := &ui{t: t, store: newMemStore()}
	mux := http.NewServeMux()
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	cfg := Config{Mode: mode, PublicURL: u.srv.URL, Idle: time.Hour, Max: 12 * time.Hour,
		ClientID: "nbsandbox-ui", ClientSecret: "ui-secret", Scopes: []string{"openid", "nbsandbox.read", "nbsandbox.runs"}}
	d := Deps{Store: u.store}
	if idp != nil {
		v, err := auth.NewVerifier(auth.OIDCConfig{Issuer: idp.srv.URL, Audience: "nbsandbox", IDPrefix: "kc", IDClaims: []string{"sub"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		d.Verifier = v
	}
	hashed, _ := bcrypt.GenerateFromPassword([]byte("alice-password"), bcrypt.MinCost)
	users, err := auth.ParseUsers([]byte("users:\n  - username: alice\n    password_hash: " + string(hashed) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	d.Basic = &auth.Authenticator{Users: users, Limiter: auth.NewLoginLimiter(3, time.Minute)}
	if u.s, err = New(cfg, d); err != nil {
		t.Fatal(err)
	}
	u.s.Routes(mux)
	onFail := func(w http.ResponseWriter, _ *http.Request, f auth.Failure) { w.WriteHeader(f.Status) }
	mux.Handle("GET /ui/api/session", u.s.Require(onFail, SessionInfo(mode)))
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.From(r.Context())
		_, _ = io.WriteString(w, p.ID)
	})
	mux.Handle("/ui/api/v1/echo", u.s.Require(onFail, echo))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "the app") })
	jar, _ := cookiejar.New(nil)
	u.browser = &http.Client{Jar: jar}
	return u
}

// reply is a response, read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	URL        *url.URL // where it ended, after redirects
}

func (u *ui) do(method, path string, body io.Reader, h map[string]string) reply {
	u.t.Helper()
	req, _ := http.NewRequest(method, u.srv.URL+path, body)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := u.browser.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{StatusCode: resp.StatusCode, Header: resp.Header, Body: b, URL: resp.Request.URL}
}

func (u *ui) get(path string) (int, string) {
	r := u.do("GET", path, nil, nil)
	return r.StatusCode, string(r.Body)
}

func (u *ui) signIn() reply {
	u.t.Helper()
	return u.do("GET", "/ui/login?return=/spaces/abc", nil, nil)
}

func TestOIDCSignInAndOut(t *testing.T) {
	idp := newFakeIDP(t)
	u := newUI(t, ModeOIDC, idp)
	if code, _ := u.get("/ui/api/v1/echo"); code != 401 {
		t.Fatalf("before signing in: %d", code)
	}
	resp := u.signIn()
	if resp.StatusCode != 200 || string(resp.Body) != "the app" || resp.URL.Path != "/spaces/abc" {
		t.Fatalf("sign-in ended at %s: %d %s", resp.URL, resp.StatusCode, resp.Body)
	}
	if code, body := u.get("/ui/api/v1/echo"); code != 200 || body != "kc:alice-sub" {
		t.Fatalf("signed in: %d %q", code, body)
	}
	code, body := u.get("/ui/api/session")
	var info struct {
		Mode, User, Name, Method string
		Scopes                   []string
	}
	_ = json.Unmarshal([]byte(body), &info)
	if code != 200 || info.User != "kc:alice-sub" || info.Name != "Alice Example" || info.Method != "oidc" || len(info.Scopes) != 2 {
		t.Fatalf("session: %d %s", code, body)
	}
	// The browser holds only a random ID; the store only its hash and
	// encrypted tokens.
	sess := u.store.only(t)
	for _, c := range u.browser.Jar.Cookies(mustURL(u.srv.URL)) {
		if c.Name == "nbsandbox-ui" && (len(c.Value) != 43 || bytes.Contains(sess.Tokens, []byte(c.Value))) {
			t.Errorf("cookie %q", c.Value)
		}
	}
	if bytes.Contains(sess.Tokens, []byte("eyJ")) {
		t.Error("tokens stored in the clear")
	}
	// Signing out: the provider's sign-out, with the ID token as a hint.
	resp = u.do("POST", "/ui/logout", nil, map[string]string{Header: "1", "Origin": u.srv.URL})
	var out struct{ Next string }
	_ = json.Unmarshal(resp.Body, &out)
	next, _ := url.Parse(out.Next)
	if resp.StatusCode != 200 || !strings.HasPrefix(out.Next, idp.srv.URL+"/logout?") ||
		next.Query().Get("post_logout_redirect_uri") != u.srv.URL+"/ui/signed-out" || next.Query().Get("id_token_hint") == "" {
		t.Fatalf("logout: %d %s", resp.StatusCode, out.Next)
	}
	if code, _ := u.get("/ui/api/v1/echo"); code != 401 {
		t.Errorf("after signing out: %d", code)
	}
	if len(u.store.sessions) != 0 {
		t.Error("session kept after signing out")
	}
	if code, body := u.get("/ui/signed-out"); code != 200 || !strings.Contains(body, "signed out") {
		t.Errorf("signed-out page: %d", code)
	}
}

func mustURL(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}

// The access token is refreshed before it expires; when the provider
// refuses (the user signed out there), the session ends.
func TestOIDCRefreshAndRevocation(t *testing.T) {
	idp := newFakeIDP(t)
	idp.expiresIn = 30 // within refreshBefore: every request refreshes
	u := newUI(t, ModeOIDC, idp)
	u.signIn()
	for range 2 {
		if code, body := u.get("/ui/api/v1/echo"); code != 200 || body != "kc:alice-sub" {
			t.Fatalf("refreshed: %d %q", code, body)
		}
	}
	if n := idp.refreshed.Load(); n != 2 {
		t.Errorf("%d refreshes, want 2", n)
	}
	idp.revokeRefreshTokens()
	if code, _ := u.get("/ui/api/v1/echo"); code != 401 {
		t.Fatalf("after revocation at the provider: %d", code)
	}
	if len(u.store.sessions) != 0 {
		t.Error("the session must end when refresh fails")
	}
}

func TestOIDCSignInRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mod    func(*fakeIDP)
		status int
	}{
		"user cancelled":         {func(p *fakeIDP) { p.deny = "access_denied" }, 401},
		"ID token's nonce":       {func(p *fakeIDP) { p.badNonce = true }, 401},
		"token for another API":  {func(p *fakeIDP) { p.audience = "other-api" }, 401},
		"no nbsandbox.read":      {func(p *fakeIDP) { p.scope = "nbsandbox.runs" }, 403},
		"token lives over a day": {func(p *fakeIDP) { p.expiresIn = 90000 }, 401},
	} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIDP(t)
			tc.mod(idp)
			u := newUI(t, ModeOIDC, idp)
			if resp := u.signIn(); resp.StatusCode != tc.status {
				t.Fatalf("sign-in: %d, want %d", resp.StatusCode, tc.status)
			}
			if len(u.store.sessions) != 0 {
				t.Error("a session was created")
			}
		})
	}
}

// The callback must come back to the browser that started the sign-in, and
// each sign-in is used once.
func TestOIDCCallbackBoundToBrowser(t *testing.T) {
	idp := newFakeIDP(t)
	u := newUI(t, ModeOIDC, idp)
	// Start a sign-in, but stop at the provider's redirect back.
	var callback string
	u.browser.CheckRedirect = func(r *http.Request, _ []*http.Request) error {
		if r.URL.Path == "/ui/callback" {
			callback = r.URL.String()
			return http.ErrUseLastResponse
		}
		return nil
	}
	u.signIn()
	if callback == "" {
		t.Fatal("no callback")
	}
	// Another browser (the victim's) opens the attacker's callback link.
	victim := &http.Client{}
	resp, err := victim.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("callback in another browser: %d", resp.StatusCode)
	}
	// The right browser, twice: the second is refused.
	u.browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cb := strings.TrimPrefix(callback, u.srv.URL)
	if resp := u.do("GET", cb, nil, nil); resp.StatusCode != 303 || resp.Header.Get("Location") != "/spaces/abc" {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// Changes must come from the UI's own pages: its header, and its origin.
func TestCSRF(t *testing.T) {
	u := newUI(t, ModeOIDC, newFakeIDP(t))
	u.signIn()
	for name, tc := range map[string]struct {
		h    map[string]string
		want int
	}{
		"no header":         {map[string]string{"Origin": u.srv.URL}, 403},
		"another site":      {map[string]string{Header: "1", "Origin": "https://evil.example"}, 403},
		"the UI":            {map[string]string{Header: "1", "Origin": u.srv.URL}, 200},
		"the UI, no Origin": {map[string]string{Header: "1"}, 200},
	} {
		if resp := u.do("POST", "/ui/api/v1/echo", nil, tc.h); resp.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d", name, resp.StatusCode, tc.want)
		}
	}
	if resp := u.do("POST", "/ui/logout", nil, map[string]string{"Origin": u.srv.URL}); resp.StatusCode != 403 {
		t.Errorf("logout without the header: %d", resp.StatusCode)
	}
	if len(u.store.sessions) != 1 {
		t.Error("a cross-site sign-out ended the session")
	}
}

func TestSessionTimeouts(t *testing.T) {
	u := newUI(t, ModeOIDC, newFakeIDP(t))
	u.signIn()
	u.store.edit(func(s *store.UISession) { s.LastSeenAt = time.Now().Add(-61 * time.Minute) })
	if code, _ := u.get("/ui/api/v1/echo"); code != 401 {
		t.Errorf("idle over an hour: %d", code)
	}
	u.signIn()
	u.store.edit(func(s *store.UISession) { s.CreatedAt = time.Now().Add(-13 * time.Hour) })
	if code, _ := u.get("/ui/api/v1/echo"); code != 401 {
		t.Errorf("older than 12 hours: %d", code)
	}
	// Cleanup removes what has expired.
	u.signIn()
	u.store.edit(func(s *store.UISession) { s.LastSeenAt = time.Now().Add(-2 * time.Hour) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u.s.Run(ctx, time.Hour)
	if len(u.store.sessions) != 0 {
		t.Error("cleanup kept an idle session")
	}
}

func TestBasicSignIn(t *testing.T) {
	u := newUI(t, ModeBasic, nil)
	u.browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if code, body := u.get("/ui/login?return=/spaces/x"); code != 200 || !strings.Contains(body, `name="password"`) ||
		!strings.Contains(body, `value="/spaces/x"`) {
		t.Fatalf("form: %d %s", code, body)
	}
	post := func(user, pass, origin string) reply {
		form := url.Values{"username": {user}, "password": {pass}, "return": {"/spaces/x"}}
		return u.do("POST", "/ui/login", strings.NewReader(form.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": origin})
	}
	if resp := post("alice", "alice-password", "https://evil.example"); resp.StatusCode != 403 {
		t.Fatalf("form posted from another site: %d", resp.StatusCode)
	}
	if resp := post("alice", "wrong", u.srv.URL); resp.StatusCode != 303 || !strings.Contains(resp.Header.Get("Location"), "error=bad") {
		t.Fatalf("wrong password: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp := post("alice", "alice-password", u.srv.URL)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/spaces/x" {
		t.Fatalf("sign-in: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, body := u.get("/ui/api/v1/echo"); code != 200 || body != "alice" {
		t.Fatalf("signed in: %d %q", code, body)
	}
	// The login limiter applies to the form too.
	for range 3 {
		post("alice", "wrong", u.srv.URL)
	}
	if resp := post("alice", "alice-password", u.srv.URL); !strings.Contains(resp.Header.Get("Location"), "error=limited") {
		t.Errorf("after 3 failures: %s", resp.Header.Get("Location"))
	}
	if code, body := u.get("/ui/login?error=limited&wait=3"); code != 200 || !strings.Contains(body, "3 minutes") {
		t.Errorf("limited page: %s", body)
	}
	// Signing out goes back to the form.
	resp = u.do("POST", "/ui/logout", nil, map[string]string{Header: "1"})
	var out struct{ Next string }
	_ = json.Unmarshal(resp.Body, &out)
	if out.Next != "/ui/login" {
		t.Errorf("next after sign-out: %q", out.Next)
	}
}

// A user removed from users.yaml loses their session.
func TestBasicUserRemoved(t *testing.T) {
	u := newUI(t, ModeBasic, nil)
	form := url.Values{"username": {"alice"}, "password": {"alice-password"}}
	u.do("POST", "/ui/login", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": u.srv.URL})
	if code, _ := u.get("/ui/api/v1/echo"); code != 200 {
		t.Fatalf("signed in: %d", code)
	}
	empty, _ := auth.ParseUsers([]byte("users: []\n"))
	u.s.d.Basic.Users = empty
	if code, _ := u.get("/ui/api/v1/echo"); code != 401 {
		t.Errorf("removed user: %d", code)
	}
}

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{
		"/spaces/a?x=1": "/spaces/a?x=1", "": "/", "spaces": "/", "//evil.example": "/", "/\\evil.example": "/",
		"https://evil.example/": "/", "/a\r\nSet-Cookie: x": "/",
	} {
		if got := localPath(in); got != want {
			t.Errorf("localPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{Mode: ModeOIDC, PublicURL: "https://nb.example.com", Idle: time.Hour, Max: 12 * time.Hour,
		ClientID: "ui", ClientSecret: "s", Scopes: []string{"openid"}}
	if err := ok.Validate("ui"); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"no openid":        func(c *Config) { c.Scopes = []string{"nbsandbox.read"} },
		"no client":        func(c *Config) { c.ClientID = "" },
		"plain http":       func(c *Config) { c.PublicURL = "http://nb.example.com" },
		"with a path":      func(c *Config) { c.PublicURL = "https://nb.example.com/ui" },
		"idle over max":    func(c *Config) { c.Idle = 13 * time.Hour },
		"a month":          func(c *Config) { c.Max = 31 * 24 * time.Hour; c.Idle = time.Hour },
		"unknown mode":     func(c *Config) { c.Mode = "magic" },
		"no public url":    func(c *Config) { c.PublicURL = "" },
		"no secret":        func(c *Config) { c.ClientSecret = "" },
		"negative idle":    func(c *Config) { c.Idle = -time.Second },
		"zero max":         func(c *Config) { c.Max = 0 },
		"query in url":     func(c *Config) { c.PublicURL = "https://nb.example.com?x=1" },
		"not a url at all": func(c *Config) { c.PublicURL = "::" },
	} {
		c := ok
		mod(&c)
		if err := c.Validate("ui"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(Config{Mode: ModeOIDC, PublicURL: "https://nb.example.com", Idle: time.Hour, Max: time.Hour,
		ClientID: "ui", ClientSecret: "s", Scopes: []string{"openid"}}, Deps{}); err == nil {
		t.Error("oidc without a verifier")
	}
	if s, err := New(Config{Mode: ModeNone}, Deps{}); s != nil || err != nil {
		t.Error("none: no service")
	}
	if !errors.Is(errNoSession, errNoSession) {
		t.Fatal()
	}
}
