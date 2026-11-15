// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package uiauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/audit"
	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// loginTTL: how long a sign-in at the provider may take.
const loginTTL = 10 * time.Minute

// login starts signing in: the form (basic), or a redirect to the
// provider's sign-in page (oidc).
func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	ret := localPath(r.URL.Query().Get("return"))
	if s.cfg.Mode == ModeBasic {
		msg := ""
		switch r.URL.Query().Get("error") {
		case "bad":
			msg = "Wrong username or password."
		case "limited":
			msg = "Too many failed sign-ins. Try again in a minute."
			if m, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && m > 1 {
				msg = "Too many failed sign-ins. Try again in " + strconv.Itoa(m) + " minutes."
			}
		}
		s.page(w, http.StatusOK, pageData{Title: "Sign in", Form: true, Return: ret, Error: msg})
		return
	}
	p, err := s.endpoints(r.Context())
	if err != nil {
		s.d.Log.Warn("web UI sign-in: the identity provider can't be reached", "err", err)
		s.page(w, http.StatusBadGateway, pageData{Title: "Can't sign in", Error: "The identity provider can't be reached right now.", Retry: ret})
		return
	}
	state, verifier, nonce := random(32), random(32), random(32)
	err = s.d.Store.CreateUILogin(r.Context(), store.UILogin{StateHash: hash(state), Verifier: verifier, Nonce: nonce,
		ReturnTo: ret, ExpiresAt: s.now().Add(loginTTL)})
	if err != nil {
		s.d.Log.Error("web UI sign-in: recording it failed", "err", err)
		s.page(w, http.StatusInternalServerError, pageData{Title: "Can't sign in", Error: "Something went wrong. Try again.", Retry: ret})
		return
	}
	// The state also goes in a cookie: the callback must come back to the
	// browser that started the sign-in, so nobody can sign a victim in to
	// the attacker's account with a link.
	s.setCookie(w, s.loginCookieName(), state, int(loginTTL.Seconds()))
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {s.cfg.ClientID},
		"redirect_uri":          {s.redirectURI()},
		"scope":                 {strings.Join(s.cfg.Scopes, " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	target, _ := url.Parse(p.Authorization)
	extra := target.Query()
	for k, v := range q {
		extra[k] = v
	}
	target.RawQuery = extra.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (s *Service) redirectURI() string { return s.origin + "/ui/callback" }

// callback finishes signing in at the provider.
func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fail := func(status int, msg, logMsg string, args ...any) {
		s.d.Log.Info("web UI sign-in refused: "+logMsg, args...)
		s.page(w, status, pageData{Title: "Can't sign in", Error: msg, Retry: "/"})
	}
	s.setCookie(w, s.loginCookieName(), "", -1)
	if e := q.Get("error"); e != "" {
		fail(http.StatusUnauthorized, "The identity provider didn't sign you in ("+e+").", "the provider answered with an error",
			"error", e, "description", q.Get("error_description"))
		return
	}
	state := q.Get("state")
	c, err := r.Cookie(s.loginCookieName())
	if state == "" || err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		fail(http.StatusBadRequest, "This sign-in wasn't started in this browser, or took too long.", "state doesn't match this browser's")
		return
	}
	l, err := s.d.Store.TakeUILogin(r.Context(), hash(state), s.now())
	if err != nil {
		fail(http.StatusBadRequest, "This sign-in has expired or was already used.", "unknown or expired state", "err", err)
		return
	}
	resp, err := s.tokenRequest(r.Context(), map[string]string{
		"grant_type": "authorization_code", "code": q.Get("code"), "redirect_uri": s.redirectURI(), "code_verifier": l.Verifier,
	})
	if err != nil {
		fail(http.StatusBadGateway, "The identity provider didn't accept the sign-in.", "code exchange failed", "err", err)
		return
	}
	if resp.IDToken == "" {
		fail(http.StatusBadGateway, "The identity provider sent no ID token (is openid among the scopes?).", "no ID token")
		return
	}
	claims, err := s.d.Verifier.IDToken(r.Context(), resp.IDToken, s.cfg.ClientID, l.Nonce)
	if err != nil {
		fail(http.StatusUnauthorized, "The identity provider's answer didn't check out.", "ID token refused", "err", err)
		return
	}
	p, err := s.d.Verifier.Verify(r.Context(), resp.AccessToken)
	if err != nil {
		fail(http.StatusUnauthorized, "The access token isn't one nbsandbox accepts (check the UI client's scopes and audience).",
			"access token refused", "err", err)
		return
	}
	if !p.Has(auth.ScopeRead) {
		fail(http.StatusForbidden, "Your account wasn't granted "+auth.ScopeRead+".", "no read scope", "user", p.ID)
		return
	}
	name := p.Name
	for _, k := range []string{"name", "preferred_username", "email"} {
		if v, _ := claims[k].(string); v != "" && name == "" {
			name = v
		}
	}
	t := tokens{Access: resp.AccessToken, Refresh: resp.RefreshToken, ID: resp.IDToken}
	if err := s.startSession(r.Context(), w, p, name, &t, resp.expiry(s.now())); err != nil {
		s.d.Log.Error("web UI sign-in: recording the session failed", "err", err)
		s.page(w, http.StatusInternalServerError, pageData{Title: "Can't sign in", Error: "Something went wrong. Try again.", Retry: l.ReturnTo})
		return
	}
	s.d.Log.Info("web UI sign-in", "user", p.ID, "method", "oidc", "client", p.Client)
	s.d.Audit.Record(auth.WithPrincipal(r.Context(), p), audit.Entry{Action: audit.ActionSignIn, Status: http.StatusSeeOther})
	http.Redirect(w, r, l.ReturnTo, http.StatusSeeOther)
}

// basicLogin checks the form against users.yaml, with the API's login
// limiter.
func (s *Service) basicLogin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		s.page(w, http.StatusForbidden, pageData{Title: "Can't sign in", Error: "Sign in from the web UI's own page.", Retry: "/"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user, pass, ret := r.PostForm.Get("username"), r.PostForm.Get("password"), localPath(r.PostForm.Get("return"))
	back := func(e string, extra ...string) {
		q := url.Values{"error": {e}, "return": {ret}}
		if len(extra) == 2 {
			q.Set(extra[0], extra[1])
		}
		http.Redirect(w, r, "/ui/login?"+q.Encode(), http.StatusSeeOther)
	}
	a := s.d.Basic
	ctx := auth.WithClientAddr(r.Context(), auth.ClientIP(r.Context(), r.RemoteAddr, r.Header, a.TrustedProxies))
	cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	p, err := a.Authenticate(ctx, "Basic "+cred)
	if wait, limited := auth.IsTooManyAttempts(err); limited {
		back("limited", "wait", strconv.Itoa(int((wait+time.Minute-1)/time.Minute)))
		return
	}
	if err != nil {
		s.d.Log.Info("web UI sign-in refused", "user", user, "method", "basic")
		back("bad")
		return
	}
	if err := s.startSession(r.Context(), w, p, p.Name, nil, time.Time{}); err != nil {
		s.d.Log.Error("web UI sign-in: recording the session failed", "err", err)
		back("bad")
		return
	}
	s.d.Log.Info("web UI sign-in", "user", p.ID, "method", "basic")
	s.d.Audit.Record(auth.WithPrincipal(r.Context(), p), audit.Entry{Action: audit.ActionSignIn, Status: http.StatusSeeOther})
	http.Redirect(w, r, ret, http.StatusSeeOther) //nolint:gosec // ret went through localPath: a path on this site only
}

// logout ends the session and says where to go next: the provider's
// sign-out (so the user is signed out there too), or the sign-in page.
func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	if !s.fromUI(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{
			"code": "forbidden", "message": "sign out from the web UI's own pages"}})
		return
	}
	next := "/ui/login"
	if s.cfg.Mode == ModeOIDC {
		next = "/ui/signed-out"
	}
	sess, err := s.session(r.Context(), r)
	if err == nil {
		_ = s.d.Store.DeleteUISession(r.Context(), sess.IDHash)
		s.d.Log.Info("web UI sign-out", "user", sess.Principal)
		s.d.Audit.Record(r.Context(), audit.Entry{Actor: sess.Principal, Method: sess.Method, Action: audit.ActionSignOut, Status: http.StatusOK})
		if sess.Method == "oidc" && s.cfg.Mode == ModeOIDC {
			if u := s.endSessionURL(r, sess); u != "" {
				next = u
			}
		}
	}
	s.setCookie(w, s.cookieName(), "", -1)
	writeJSON(w, http.StatusOK, map[string]string{"next": next})
}

// endSessionURL is the provider's sign-out (RP-initiated logout), coming
// back to /ui/signed-out; "" if the provider has none.
func (s *Service) endSessionURL(r *http.Request, sess store.UISession) string {
	p, err := s.endpoints(r.Context())
	if err != nil || p.EndSession == "" {
		return ""
	}
	u, _ := url.Parse(p.EndSession)
	q := u.Query()
	q.Set("client_id", s.cfg.ClientID)
	q.Set("post_logout_redirect_uri", s.origin+"/ui/signed-out")
	if t, err := s.open(sess); err == nil && t.ID != "" {
		q.Set("id_token_hint", t.ID)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Service) signedOut(w http.ResponseWriter, _ *http.Request) {
	s.page(w, http.StatusOK, pageData{Title: "Signed out", Message: "You're signed out of nbsandbox.", SignIn: true})
}

type pageData struct {
	Title   string
	Error   string
	Message string
	Form    bool   // the users.yaml form
	Return  string // where the form goes afterwards
	Retry   string // a "Try again" link back to this path
	SignIn  bool   // a "Sign in" link
}

// The pages are plain HTML with no script, styled by /login.css.
var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · nbsandbox</title>
<link rel="stylesheet" href="/login.css">
</head>
<body>
<main class="card">
<h1>nbsandbox</h1>
{{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
{{if .Message}}<p>{{.Message}}</p>{{end}}
{{if .Form}}
<form method="post" action="/ui/login">
<label>Username <input name="username" autocomplete="username" required autofocus></label>
<label>Password <input name="password" type="password" autocomplete="current-password" required></label>
<input type="hidden" name="return" value="{{.Return}}">
<button type="submit">Sign in</button>
</form>
{{end}}
{{if .Retry}}<p><a href="/ui/login?return={{.Retry}}">Try again</a></p>{{end}}
{{if .SignIn}}<p><a href="/ui/login">Sign in</a></p>{{end}}
</main>
</body>
</html>
`))

// pageCSP: styles from this origin, the form posts only here, nothing else.
const pageCSP = "default-src 'none'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

func (s *Service) page(w http.ResponseWriter, status int, d pageData) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := pageTmpl.Execute(w, d); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		s.d.Log.Debug("web UI page", "err", err)
	}
}
