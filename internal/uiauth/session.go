// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package uiauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

var (
	errNoSession = errors.New("no session")
	errExpired   = errors.New("session expired")
)

// refreshBefore: an access token this close to its expiry is refreshed.
const refreshBefore = time.Minute

// touchEvery: last_seen_at is written at most this often per session.
const touchEvery = time.Minute

// tokens are the provider's tokens for a session, stored encrypted.
type tokens struct {
	Access  string `json:"a"`
	Refresh string `json:"r,omitempty"`
	ID      string `json:"i,omitempty"`
}

func (s *Service) cookieName() string {
	if s.secure {
		return "__Host-nbsandbox-ui"
	}
	return "nbsandbox-ui" // http on loopback: browsers drop __Host- cookies without Secure
}

func (s *Service) loginCookieName() string {
	if s.secure {
		return "__Host-nbsandbox-ui-login"
	}
	return "nbsandbox-ui-login"
}

func (s *Service) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	// Secure is off only when public_url is plain http, which is allowed on
	// loopback only (a browser wouldn't keep a Secure cookie there).
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, //nolint:gosec // see above
		Secure: s.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func hash(v string) []byte {
	h := sha256.Sum256([]byte(v))
	return h[:]
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// startSession records a session and sets its cookie (a browser session
// cookie: it ends with the browser, or at Idle or Max).
func (s *Service) startSession(ctx context.Context, w http.ResponseWriter, p auth.Principal, name string, t *tokens, accessExp time.Time) error {
	id := random(32)
	now := s.now()
	sess := store.UISession{IDHash: hash(id), Method: p.Method, Principal: p.ID, DisplayName: name,
		Tokens: []byte{}, AccessExpires: accessExp, CreatedAt: now, LastSeenAt: now}
	if t != nil {
		enc, err := s.seal(sess.IDHash, *t)
		if err != nil {
			return err
		}
		sess.Tokens = enc
	}
	if err := s.d.Store.CreateUISession(ctx, sess); err != nil {
		return err
	}
	s.setCookie(w, s.cookieName(), id, 0)
	return nil
}

// session returns the request's session, or errNoSession.
func (s *Service) session(ctx context.Context, r *http.Request) (store.UISession, error) {
	c, err := r.Cookie(s.cookieName())
	if err != nil || len(c.Value) != 43 {
		return store.UISession{}, errNoSession
	}
	sess, err := s.d.Store.UISession(ctx, hash(c.Value))
	if errors.Is(err, store.ErrNotFound) {
		return store.UISession{}, errNoSession
	}
	return sess, err
}

// caller finds the request's caller from its session: the users.yaml user,
// or the principal of the session's access token (refreshed when due),
// checked like any API token.
func (s *Service) caller(ctx context.Context, r *http.Request) (auth.Principal, error) {
	sess, err := s.session(ctx, r)
	if err != nil {
		return auth.Principal{}, err
	}
	now := s.now()
	end := func(reason error) (auth.Principal, error) {
		_ = s.d.Store.DeleteUISession(context.WithoutCancel(ctx), sess.IDHash)
		return auth.Principal{}, fmt.Errorf("%s: %w", sess.Principal, reason)
	}
	if now.Sub(sess.LastSeenAt) > s.cfg.Idle || now.Sub(sess.CreatedAt) > s.cfg.Max {
		return end(errExpired)
	}
	var p auth.Principal
	switch sess.Method {
	case "basic":
		users := s.basicUsers()
		if users == nil || !users.Has(sess.Principal) {
			return end(errors.New("no longer in the users file"))
		}
		if users.Admin(sess.Principal) {
			p = auth.BasicAdmin(sess.Principal)
		} else {
			p = auth.BasicPrincipal(sess.Principal)
		}
	case "oidc":
		if s.cfg.Mode != ModeOIDC {
			return end(errors.New("signed in with the provider, which is no longer the UI's sign-in"))
		}
		t, err := s.open(sess)
		if err != nil {
			return end(err)
		}
		if sess.AccessExpires.Sub(now) < refreshBefore {
			if t, err = s.refresh(ctx, sess); err != nil {
				return end(err)
			}
		}
		if p, err = s.d.Verifier.Verify(ctx, t.Access); err != nil {
			return end(err)
		}
		if p.ID != sess.Principal {
			return end(fmt.Errorf("the refreshed token names %s", p.ID))
		}
	default:
		return end(fmt.Errorf("unknown sign-in method %q", sess.Method))
	}
	if sess.DisplayName != "" {
		p.Name = sess.DisplayName
	}
	if now.Sub(sess.LastSeenAt) >= touchEvery {
		_ = s.d.Store.TouchUISession(ctx, sess.IDHash, now)
	}
	return p, nil
}

func (s *Service) basicUsers() *auth.Users {
	if s.d.Basic == nil {
		return nil
	}
	return s.d.Basic.Users
}

// refresh gets a new access token with the session's refresh token. One
// refresh per session at a time on this node; a request that waited finds
// the tokens the first one stored.
func (s *Service) refresh(ctx context.Context, sess store.UISession) (tokens, error) {
	l := &s.locks[sess.IDHash[0]%byte(len(s.locks))]
	l.Lock()
	defer l.Unlock()
	cur, err := s.d.Store.UISession(ctx, sess.IDHash)
	if err != nil {
		return tokens{}, err
	}
	t, err := s.open(cur)
	if err != nil {
		return tokens{}, err
	}
	if cur.AccessExpires.Sub(s.now()) >= refreshBefore {
		return t, nil // refreshed meanwhile
	}
	if t.Refresh == "" {
		return tokens{}, errors.New("the access token expired and the provider gave no refresh token")
	}
	resp, err := s.tokenRequest(ctx, map[string]string{"grant_type": "refresh_token", "refresh_token": t.Refresh})
	if err != nil {
		return tokens{}, fmt.Errorf("refresh: %w", err)
	}
	t.Access = resp.AccessToken
	if resp.RefreshToken != "" {
		t.Refresh = resp.RefreshToken
	}
	if resp.IDToken != "" {
		t.ID = resp.IDToken
	}
	enc, err := s.seal(sess.IDHash, t)
	if err != nil {
		return tokens{}, err
	}
	if err := s.d.Store.UpdateUISessionTokens(ctx, sess.IDHash, enc, resp.expiry(s.now())); err != nil {
		return tokens{}, err
	}
	return t, nil
}

// seal encrypts tokens for the session idHash (the hash is authenticated
// too, so a row's tokens can't be moved to another session).
func (s *Service) seal(idHash []byte, t tokens) ([]byte, error) {
	plain, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, plain, idHash), nil
}

func (s *Service) open(sess store.UISession) (tokens, error) {
	n := s.aead.NonceSize()
	if len(sess.Tokens) < n {
		return tokens{}, errors.New("no stored tokens")
	}
	plain, err := s.aead.Open(nil, sess.Tokens[:n], sess.Tokens[n:], sess.IDHash)
	if err != nil {
		return tokens{}, errors.New("stored tokens can't be decrypted (the client secret changed?)")
	}
	var t tokens
	err = json.Unmarshal(plain, &t)
	return t, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
