// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package uiauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
)

// provider is the identity provider's endpoints, from its discovery
// document.
type provider struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	EndSession    string `json:"end_session_endpoint"`
}

// endpoints returns the provider's endpoints, fetching them once.
func (s *Service) endpoints(ctx context.Context) (*provider, error) {
	s.provMu.Lock()
	defer s.provMu.Unlock()
	if s.prov != nil {
		return s.prov, nil
	}
	iss := s.d.Verifier.Issuer()
	var p provider
	if err := s.getJSON(ctx, strings.TrimSuffix(iss, "/")+"/.well-known/openid-configuration", &p); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if p.Issuer != iss {
		return nil, fmt.Errorf("discovery: the document's issuer %q is not %q", p.Issuer, iss)
	}
	for name, u := range map[string]string{"authorization_endpoint": p.Authorization, "token_endpoint": p.Token} {
		if err := auth.CheckURL(u); err != nil {
			return nil, fmt.Errorf("discovery: %s %w", name, err)
		}
	}
	if p.EndSession != "" && auth.CheckURL(p.EndSession) != nil {
		p.EndSession = "" // not usable: sign out of nbsandbox only
	}
	s.prov = &p
	return s.prov, nil
}

func (s *Service) getJSON(ctx context.Context, u string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.d.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// tokenResponse is the token endpoint's answer (RFC 6749 §5).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// expiry is when the access token expires (5 minutes if the provider
// doesn't say).
func (t tokenResponse) expiry(now time.Time) time.Time {
	if t.ExpiresIn <= 0 {
		return now.Add(5 * time.Minute)
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// tokenRequest posts a grant to the token endpoint, authenticating the UI's
// client with its secret (client_secret_post, which Entra ID and Keycloak
// both accept).
func (s *Service) tokenRequest(ctx context.Context, params map[string]string) (tokenResponse, error) {
	p, err := s.endpoints(ctx)
	if err != nil {
		return tokenResponse{}, err
	}
	form := url.Values{"client_id": {s.cfg.ClientID}, "client_secret": {s.cfg.ClientSecret}}
	for k, v := range params {
		form.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.d.Client.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var t tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return tokenResponse{}, fmt.Errorf("token endpoint: HTTP %d, not JSON", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || t.Error != "" {
		return tokenResponse{}, fmt.Errorf("token endpoint: HTTP %d: %s %s", resp.StatusCode, t.Error, t.Description)
	}
	if t.AccessToken == "" || !strings.EqualFold(t.TokenType, "bearer") {
		return tokenResponse{}, errors.New("token endpoint: no bearer access token in the answer")
	}
	return t, nil
}
