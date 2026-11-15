// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// Authenticator accepts basic auth (users.yaml), bearer tokens (OIDC), or
// both, depending on which is configured.
type Authenticator struct {
	Users *Users    // nil: basic auth is off
	OIDC  *Verifier // nil: bearer tokens are off
	Log   *slog.Logger
	// Limiter slows down password guessing (basic auth), per username and,
	// with LimitByAddress, per client address; nil: no limit.
	Limiter *LoginLimiter
	// LimitByAddress also limits failures per client address (password
	// spraying). Only meaningful when the address is the client's: a direct
	// connection, or TrustedProxies naming the proxy in front.
	LimitByAddress bool
	// TrustedProxies are the proxies whose X-Forwarded-For is believed when
	// finding the client's address (Caddy, an ingress); nil: the
	// connection's peer is the client.
	TrustedProxies *TrustedProxies
}

// Failure describes a refused request, for the API's error writer.
type Failure struct {
	Status  int    // 401, 403, or 429 (too many failed sign-ins)
	Message string // safe to show the caller
}

// Middleware authenticates the request and stores the caller in its
// context. On failure it sets the WWW-Authenticate challenges and calls
// onFail, which writes the response in the API's error format.
func (a *Authenticator) Middleware(onFail func(http.ResponseWriter, *http.Request, Failure), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithClientAddr(r.Context(), ClientIP(r.Context(), r.RemoteAddr, r.Header, a.TrustedProxies))
		p, err := a.Authenticate(ctx, r.Header.Get("Authorization"))
		if err != nil {
			if wait, limited := IsTooManyAttempts(err); limited {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
				onFail(w, r, Failure{Status: http.StatusTooManyRequests, Message: FailureMessage(err)})
				return
			}
			for _, c := range a.Challenges(err) {
				w.Header().Add("WWW-Authenticate", c)
			}
			onFail(w, r, Failure{Status: http.StatusUnauthorized, Message: FailureMessage(err)})
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(ctx, p)))
	})
}

var (
	errNoCredentials = errors.New("credentials required")
	errBadBasic      = errors.New("bad username or password")
	errWrongScheme   = errors.New("this authentication scheme is not enabled")
)

// Authenticate checks an Authorization value ("Bearer <token>" or
// "Basic <base64>"), whatever the transport carried it (an HTTP header,
// gRPC metadata), and returns the caller.
func (a *Authenticator) Authenticate(ctx context.Context, authorization string) (Principal, error) {
	scheme, rest, _ := strings.Cut(authorization, " ")
	rest = strings.TrimSpace(rest)
	switch {
	case authorization == "":
		return Principal{}, errNoCredentials
	case strings.EqualFold(scheme, "Bearer"):
		if a.OIDC == nil {
			return Principal{}, errWrongScheme
		}
		p, err := a.OIDC.Verify(ctx, rest)
		if err != nil {
			// The reason goes to the log, not to the caller.
			if a.Log != nil {
				a.Log.Info("bearer token refused", "reason", err.Error())
			}
			return Principal{}, err
		}
		return p, nil
	case strings.EqualFold(scheme, "Basic"):
		if a.Users == nil {
			return Principal{}, errWrongScheme
		}
		user, pass, ok := parseBasic(rest)
		keys := a.limitKeys(ctx, user)
		if err := a.Limiter.Check(keys...); err != nil {
			if a.Log != nil {
				a.Log.Warn("sign-in refused: too many failures", "user", user, "client", ClientAddr(ctx))
			}
			return Principal{}, err
		}
		if !ok || !a.Users.Verify(user, pass) {
			a.Limiter.Fail(keys...)
			return Principal{}, errBadBasic
		}
		a.Limiter.Reset("user:" + user)
		if a.Users.Admin(user) {
			return BasicAdmin(user), nil
		}
		return BasicPrincipal(user), nil
	default:
		return Principal{}, errWrongScheme
	}
}

// limitKeys are the login limiter's keys for a sign-in: the username, and
// the client's address when limiting by address and it is known.
func (a *Authenticator) limitKeys(ctx context.Context, user string) []string {
	keys := []string{"user:" + user}
	if addr := ClientAddr(ctx); a.LimitByAddress && addr != "" {
		keys = append(keys, "addr:"+addr)
	}
	return keys
}

// parseBasic decodes the credentials of a Basic Authorization value.
func parseBasic(v string) (user, pass string, ok bool) {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", "", false
	}
	return strings.Cut(string(b), ":")
}

// Challenges returns one WWW-Authenticate value per enabled scheme
// (RFC 9110 §11.6.1; RFC 6750 §3 for Bearer), for a refused request.
func (a *Authenticator) Challenges(err error) []string {
	var out []string
	if a.OIDC != nil {
		v := `Bearer realm="nbsandbox"`
		if errors.Is(err, ErrInvalidToken) {
			v += `, error="invalid_token"`
		}
		out = append(out, v)
	}
	if a.Users != nil {
		out = append(out, `Basic realm="nbsandbox", charset="UTF-8"`)
	}
	return out
}

// FailureMessage says why authentication failed, safely for the caller.
func FailureMessage(err error) string {
	switch {
	case errors.Is(err, ErrInvalidToken):
		return "the access token is invalid or expired"
	case errors.Is(err, errTooMany):
		return "too many failed sign-ins: try again later (Retry-After)"
	case errors.Is(err, errWrongScheme):
		return "use a supported authentication scheme (see WWW-Authenticate)"
	default:
		return "valid credentials required"
	}
}

// Forbidden sets the challenge for a token that lacks a scope (RFC 6750
// §3.1) and returns the failure to write.
func Forbidden(w http.ResponseWriter, scope string) Failure {
	w.Header().Set("WWW-Authenticate", InsufficientScope(scope))
	return Failure{Status: http.StatusForbidden, Message: "the access token lacks the scope " + scope}
}

// InsufficientScope is the challenge for a caller that lacks scope.
func InsufficientScope(scope string) string {
	return `Bearer realm="nbsandbox", error="insufficient_scope", scope="` + scope + `"`
}
