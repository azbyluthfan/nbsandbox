// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package uiauth signs people in to the web UI. The UI's listener acts as a
// small backend for the browser: it signs the user in (at the identity
// provider with the authorization code flow and PKCE, or against users.yaml),
// keeps the session server-side, and gives the browser only a random session
// cookie. Tokens never reach the browser. Every request is then run as the
// caller the session's access token names, checked by the same Verifier as
// API calls, so a user is the same caller (same spaces, same scopes) in the UI
// and through the API.
package uiauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Mode is how people sign in to the UI.
type Mode string

// The modes.
const (
	ModeNone  Mode = "none"  // the user picker, no password: development, loopback only
	ModeBasic Mode = "basic" // a sign-in form against users.yaml
	ModeOIDC  Mode = "oidc"  // the identity provider
)

// Config configures signing in.
type Config struct {
	Mode Mode
	// PublicURL is the UI's address as browsers see it (https://host, or
	// http on loopback). Its origin is the only one whose requests may
	// change anything; the provider redirects back to <PublicURL>/ui/callback.
	PublicURL string
	Idle      time.Duration // signed out after this long without a request
	Max       time.Duration // and after this long in any case
	// The UI's client at the identity provider (ModeOIDC).
	ClientID     string
	ClientSecret string
	Scopes       []string
}

// Store keeps sign-ins in progress and sessions (store.DB).
type Store interface {
	CreateUILogin(ctx context.Context, l store.UILogin) error
	TakeUILogin(ctx context.Context, stateHash []byte, now time.Time) (store.UILogin, error)
	CreateUISession(ctx context.Context, s store.UISession) error
	UISession(ctx context.Context, idHash []byte) (store.UISession, error)
	TouchUISession(ctx context.Context, idHash []byte, at time.Time) error
	UpdateUISessionTokens(ctx context.Context, idHash, tokens []byte, accessExpires time.Time) error
	DeleteUISession(ctx context.Context, idHash []byte) error
	DeleteExpiredUI(ctx context.Context, now, idleBefore, createdBefore time.Time) (int64, error)
}

// Deps are what the service uses.
type Deps struct {
	Store Store
	// Basic checks users.yaml passwords (ModeBasic), with its login limiter.
	Basic *auth.Authenticator
	// Verifier checks the provider's tokens (ModeOIDC).
	Verifier *auth.Verifier
	Log      *slog.Logger
	Client   *http.Client // for the provider; default: 10 s timeout
}

// Service signs people in to the UI and finds the caller of each request.
type Service struct {
	cfg    Config
	origin string // scheme://host[:port] of PublicURL
	secure bool   // PublicURL is https: cookies are Secure and __Host-
	d      Deps
	now    func() time.Time
	aead   cipher.AEAD // encrypts stored tokens (ModeOIDC)

	provMu sync.Mutex
	prov   *provider // discovered on first use

	locks [64]sync.Mutex // per session (by hash), so one refresh at a time
}

// Validate reports every problem with cfg at once. prefix names the settings
// (e.g. "ui").
func (c Config) Validate(prefix string) error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(prefix+"."+f, a...)) }
	switch c.Mode {
	case ModeNone:
		return nil
	case ModeBasic, ModeOIDC:
	default:
		add("auth: must be none, basic or oidc, got %q", c.Mode)
		return errors.Join(errs...)
	}
	if c.PublicURL == "" {
		add("public_url: must be set when auth is %s (the address browsers open, e.g. https://nbsandbox.example.com)", c.Mode)
	} else if u, err := url.Parse(c.PublicURL); err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		add("public_url: must be an origin such as https://nbsandbox.example.com, got %q", c.PublicURL)
	} else if err := auth.CheckURL(c.PublicURL); err != nil {
		add("public_url: %v", err)
	}
	if c.Idle <= 0 || c.Max <= 0 || c.Idle > c.Max {
		add("session: idle and max must be positive, idle at most max (got %s, %s)", c.Idle, c.Max)
	}
	if c.Max > 30*24*time.Hour {
		add("session.max: at most 720h, got %s", c.Max)
	}
	if c.Mode == ModeOIDC {
		if c.ClientID == "" {
			add("oidc.client_id: must be set (the UI's client at the identity provider)")
		}
		if c.ClientSecret == "" {
			add("oidc.client_secret: must be set (NBSANDBOX_UI_OIDC_CLIENT_SECRET)")
		}
		hasOpenID := false
		for _, s := range c.Scopes {
			hasOpenID = hasOpenID || s == "openid"
		}
		if !hasOpenID {
			add("oidc.scopes: must include openid")
		}
	}
	return errors.Join(errs...)
}

// New returns the service for cfg. For ModeNone it returns nil: the UI keeps
// its user picker.
func New(cfg Config, d Deps) (*Service, error) {
	if cfg.Mode == ModeNone || cfg.Mode == "" {
		return nil, nil
	}
	if err := cfg.Validate("ui"); err != nil {
		return nil, err
	}
	u, _ := url.Parse(cfg.PublicURL)
	s := &Service{cfg: cfg, origin: u.Scheme + "://" + u.Host, secure: u.Scheme == "https", d: d, now: time.Now}
	if s.d.Log == nil {
		s.d.Log = slog.New(slog.DiscardHandler)
	}
	if s.d.Client == nil {
		s.d.Client = &http.Client{Timeout: 10 * time.Second}
	}
	switch cfg.Mode {
	case ModeBasic:
		if d.Basic == nil || d.Basic.Users == nil {
			return nil, errors.New("ui.auth basic needs a users file")
		}
	case ModeOIDC:
		if d.Verifier == nil {
			return nil, errors.New("ui.auth oidc needs auth.oidc (the identity provider)")
		}
		// The key that encrypts stored tokens comes from the client's
		// secret: rotating the secret signs everyone out.
		key, err := hkdf.Key(sha256.New, []byte(cfg.ClientSecret), nil, "nbsandbox web UI session tokens v1", 32)
		if err != nil {
			return nil, err
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		if s.aead, err = cipher.NewGCM(block); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Mode is how people sign in.
func (s *Service) Mode() Mode { return s.cfg.Mode }

// Run removes expired sign-ins and sessions every interval until ctx ends.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		now := s.now()
		if n, err := s.d.Store.DeleteExpiredUI(ctx, now, now.Add(-s.cfg.Idle), now.Add(-s.cfg.Max)); err != nil {
			if ctx.Err() == nil {
				s.d.Log.Warn("web UI: removing expired sessions failed", "err", err)
			}
		} else if n > 0 {
			s.d.Log.Debug("web UI: expired sessions removed", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Routes adds the sign-in endpoints:
//
//	GET  /ui/login       the form (basic) or a redirect to the provider (oidc)
//	POST /ui/login       the form's answer (basic)
//	GET  /ui/callback    the provider's answer (oidc)
//	POST /ui/logout      ends the session; answers {"next": where to go}
//	GET  /ui/signed-out  a page saying so
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/login", s.login)
	if s.cfg.Mode == ModeBasic {
		mux.HandleFunc("POST /ui/login", s.basicLogin)
	} else {
		mux.HandleFunc("GET /ui/callback", s.callback)
	}
	mux.HandleFunc("POST /ui/logout", s.logout)
	mux.HandleFunc("GET /ui/signed-out", s.signedOut)
}

// Header is the request header the UI's scripts send with every change. A
// page on another site can't send it without CORS, which the UI never allows.
const Header = "X-Nbsandbox-UI"

// Require runs next as the session's caller. A request that changes
// something must come from the UI's own pages (its origin and Header). On
// failure it calls onFail with 401 (sign in again) or 403.
func (s *Service) Require(onFail func(http.ResponseWriter, *http.Request, auth.Failure), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !safeMethod(r.Method) && !s.fromUI(r) {
			onFail(w, r, auth.Failure{Status: http.StatusForbidden, Message: "requests that change something must come from the web UI's own pages"})
			return
		}
		p, err := s.caller(r.Context(), r)
		if err != nil {
			if !errors.Is(err, errNoSession) {
				s.d.Log.Info("web UI session ended", "reason", err.Error())
			}
			onFail(w, r, auth.Failure{Status: http.StatusUnauthorized, Message: "sign in to the web UI"})
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

func safeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// fromUI: the request was sent by the UI's own scripts.
func (s *Service) fromUI(r *http.Request) bool {
	if r.Header.Get(Header) != "1" {
		return false
	}
	o := r.Header.Get("Origin")
	return o == "" || o == s.origin
}

// sameOrigin: a form posted from the UI's own page.
func (s *Service) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == s.origin
}

// SessionInfo answers GET /ui/api/session (behind Require).
func SessionInfo(mode Mode) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.From(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{
			"mode": mode, "user": p.ID, "name": p.Name, "method": p.Method, "scopes": p.Scopes(),
		})
	}
}

// localPath keeps only a path within the UI (no scheme, no host, not
// //host or /\host, which browsers treat as another site).
func localPath(p string) string {
	if p == "" || p[0] != '/' || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.ContainsAny(p, "\r\n\x00") {
		return "/"
	}
	if u, err := url.Parse(p); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return p
}
