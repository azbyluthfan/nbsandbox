// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package connections

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Connection types.
const (
	TypePostgreSQL = "postgresql"
	TypeMySQL      = "mysql"
	TypeSQLServer  = "sqlserver"
	TypeS3         = "s3"
	TypeAzureBlob  = "azureblob"
	TypeHTTP       = "http"
)

// An HTTP connection's auth: how the fetcher authenticates its requests.
const (
	HTTPAuthHeader = "header" // the secret in a header (auth_header, auth_prefix)
	HTTPAuthBasic  = "basic"  // user and the secret as the password
	HTTPAuthOAuth2 = "oauth2" // OAuth 2.0 client credentials; the secret is the client secret
	HTTPAuthNone   = "none"   // no credential
)

// A database connection's tls: how the fetcher talks to it.
const (
	TLSOff     = "off"     // never encrypt
	TLSPrefer  = "prefer"  // encrypt if the server offers it, without checking its certificate
	TLSRequire = "require" // always encrypt, without checking the certificate
	TLSVerify  = "verify"  // encrypt, and check the certificate and the host name
)

// field is one setting of a connection type.
type field struct {
	kind     string // "string", "int" or "bool"
	required bool
	def      any // a default, or nil
}

// typeSpec describes a connection type: its settings, whether it needs a
// secret, and what the secret is.
type typeSpec struct {
	fields       map[string]field
	secretNeeded bool
	secretIs     string // what the secret holds, for messages
}

var types = map[string]typeSpec{
	TypePostgreSQL: dbSpec(5432),
	TypeMySQL:      dbSpec(3306),
	TypeSQLServer:  dbSpec(1433),
	TypeS3: {fields: map[string]field{
		"endpoint":      {kind: "string", required: true}, // host[:port], e.g. minio.internal:9000 or s3.eu-west-1.amazonaws.com
		"region":        {kind: "string", def: "us-east-1"},
		"bucket":        {kind: "string", required: true},
		"access_key_id": {kind: "string"},
		"use_ssl":       {kind: "bool", def: true},
		"url_style":     {kind: "string", def: "vhost"}, // vhost or path (MinIO, Ceph, RustFS: path)
		"ca":            {kind: "pem"},                  // a private CA to trust as well as the system's
	}, secretIs: "the secret access key"},
	TypeAzureBlob: {fields: map[string]field{
		"account":   {kind: "string", required: true},
		"container": {kind: "string", required: true},
		"endpoint":  {kind: "string"},                           // default: <account>.blob.core.windows.net
		"auth":      {kind: "string", def: "connection_string"}, // what the secret is: connection_string, account_key or sas
	}, secretIs: "the connection string, account key or SAS token"},
	TypeHTTP: {fields: map[string]field{ //nolint:gosec // names, prefixes and URLs, not credentials
		"url":         {kind: "string", required: true},
		"auth":        {kind: "string", def: HTTPAuthHeader},
		"auth_header": {kind: "string", def: "Authorization"},
		"auth_prefix": {kind: "string", def: "Bearer "},
		"user":        {kind: "string"}, // basic
		"token_url":   {kind: "string"}, // oauth2
		"client_id":   {kind: "string"},
		"scope":       {kind: "string"},
		"audience":    {kind: "string"},
		"ca":          {kind: "pem"}, // a private CA to trust as well as the system's
	}, secretIs: "the token or API key"},
}

func dbSpec(port int) typeSpec {
	return typeSpec{fields: map[string]field{
		"host":     {kind: "string", required: true},
		"port":     {kind: "int", def: port},
		"database": {kind: "string", required: true},
		"user":     {kind: "string", required: true},
		"tls":      {kind: "string", def: TLSPrefer},
		"ca":       {kind: "pem"}, // with tls verify: the CA to trust instead of the system's
	}, secretNeeded: true, secretIs: "the password"}
}

// Types lists the connection types, sorted.
func Types() []string {
	out := make([]string, 0, len(types))
	for t := range types {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// normalize checks settings against the type and fills defaults. Unknown
// settings are refused: a secret must never hide in them.
func normalize(typ string, in map[string]any) (map[string]any, error) {
	spec, ok := types[typ]
	if !ok {
		return nil, invalid("type must be one of %s, got %q", strings.Join(Types(), ", "), typ)
	}
	out := map[string]any{}
	for k, v := range in {
		f, ok := spec.fields[k]
		if !ok {
			return nil, invalid("%s connections have no setting %q (settings: %s); a secret goes in \"secret\"", typ, k, fieldNames(spec))
		}
		val, err := coerce(k, f.kind, v)
		if err != nil {
			return nil, err
		}
		out[k] = val
	}
	for k, f := range spec.fields {
		if _, ok := out[k]; ok {
			if s, isStr := out[k].(string); !isStr || s != "" {
				continue
			}
		}
		switch {
		case f.required:
			return nil, invalid("%s connections need %q", typ, k)
		case f.def != nil:
			out[k] = f.def
		}
	}
	return out, nil
}

func fieldNames(spec typeSpec) string {
	names := make([]string, 0, len(spec.fields))
	for k := range spec.fields {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func coerce(name, kind string, v any) (any, error) {
	switch kind {
	case "string":
		s, ok := v.(string)
		if !ok || len(s) > 1024 {
			return nil, invalid("%q must be a string (at most 1024 characters)", name)
		}
		return strings.TrimSpace(s), nil
	case "int":
		switch n := v.(type) {
		case float64: // from JSON
			if n == float64(int(n)) && n >= 1 && n <= 65535 {
				return int(n), nil
			}
		case int:
			if n >= 1 && n <= 65535 {
				return n, nil
			}
		case string:
			if i, err := strconv.Atoi(n); err == nil && i >= 1 && i <= 65535 {
				return i, nil
			}
		}
		return nil, invalid("%q must be a port number (1 to 65535)", name)
	case "pem":
		s, ok := v.(string)
		if !ok || len(s) > 64<<10 {
			return nil, invalid("%q must be PEM certificates (at most 64 KiB)", name)
		}
		s = strings.TrimSpace(s)
		if s != "" {
			if err := checkPEM(s); err != nil {
				return nil, invalid("%q: %v", name, err)
			}
		}
		return s, nil
	case "bool":
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, invalid("%q must be true or false", name)
	}
	return nil, fmt.Errorf("connections: unknown field kind %q", kind)
}

// endpoint returns the host (and port, if known) a connection points to,
// for the operator's allowed list, after checking type-specific values.
func endpoint(typ string, s map[string]any) (host string, err error) {
	switch typ {
	case TypePostgreSQL, TypeMySQL, TypeSQLServer:
		h, err := hostOnly(str(s, "host"))
		if err != nil {
			return "", err
		}
		return h, checkTLS(typ, h, s)
	case TypeS3:
		if str(s, "ca") != "" {
			if ssl, _ := s["use_ssl"].(bool); !ssl {
				return "", invalid("ca is the authority for https: it needs use_ssl true")
			}
		}
		if st := str(s, "url_style"); st != "vhost" && st != "path" {
			return "", invalid("url_style must be vhost or path, got %q", st)
		}
		e := str(s, "endpoint")
		if strings.Contains(e, "://") {
			return "", invalid("endpoint is a host[:port], without a scheme (use_ssl chooses https)")
		}
		h, _, _ := strings.Cut(e, ":")
		return hostOnly(h)
	case TypeAzureBlob:
		if a := str(s, "auth"); a != "connection_string" && a != "account_key" && a != "sas" {
			return "", invalid("auth must be connection_string, account_key or sas, got %q", a)
		}
		if e := str(s, "endpoint"); e != "" {
			u, err := url.Parse(e)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
				return "", invalid("endpoint must be an http(s) URL, got %q", e)
			}
			return hostOnly(u.Hostname())
		}
		return hostOnly(str(s, "account") + ".blob.core.windows.net")
	case TypeHTTP:
		u, err := url.Parse(str(s, "url"))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return "", invalid("url must be an http(s) URL without credentials or a fragment in it, got %q", s["url"])
		}
		if str(s, "ca") != "" && u.Scheme != "https" {
			return "", invalid("ca is the authority for https: the url must be https")
		}
		if err := checkHTTPAuth(s); err != nil {
			return "", err
		}
		return hostOnly(u.Hostname())
	}
	return "", invalid("unknown type %q", typ)
}

// str is a string setting ("" if missing or not a string).
func str(s map[string]any, k string) string {
	v, _ := s[k].(string)
	return v
}

// checkTLS checks a database connection's tls and ca.
func checkTLS(typ, host string, s map[string]any) error {
	mode := str(s, "tls")
	if mode == "" {
		mode = TLSPrefer // a connection made before tls existed
	}
	switch mode {
	case TLSOff, TLSPrefer, TLSRequire, TLSVerify:
	default:
		return invalid("tls must be off, prefer, require or verify, got %q", mode)
	}
	if str(s, "ca") != "" && mode != TLSVerify {
		return invalid("ca is the authority tls verify checks the server's certificate against: it needs tls verify")
	}
	if mode == TLSVerify {
		switch {
		case typ == TypeSQLServer:
			// DuckDB's mssql extension (0.2.5) accepts any certificate.
			return invalid("sqlserver connections can't check the server's certificate yet: use tls require (encrypted, not checked)")
		case typ == TypeMySQL && net.ParseIP(host) != nil:
			// MySQL's client checks the name it connected to; an address
			// can't be pointed at the fetcher's forwarder.
			return invalid("tls verify for mysql needs the server's host name, not an address")
		}
	}
	return nil
}

// checkPEM: one or more PEM certificates, and nothing else.
func checkPEM(s string) error {
	rest, n := []byte(s), 0
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return fmt.Errorf("a %s, not a CERTIFICATE (never a key)", b.Type)
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			return fmt.Errorf("certificate %d: %w", n+1, err)
		}
		n++
	}
	if n == 0 || strings.TrimSpace(string(rest)) != "" {
		return fmt.Errorf("must be PEM certificates (-----BEGIN CERTIFICATE-----)")
	}
	return nil
}

func hostOnly(h string) (string, error) {
	h = strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), "."))
	if h == "" || strings.ContainsAny(h, "/@ ?#") {
		return "", invalid("%q is not a host name or address", h)
	}
	return h, nil
}

// checkHTTPAuth checks an HTTP connection's auth and the settings it needs.
func checkHTTPAuth(s map[string]any) error {
	only := func(auth string, keys ...string) error {
		for _, k := range keys {
			if str(s, k) != "" {
				return invalid("%s is for auth %s, not %s", k, map[string]string{"user": HTTPAuthBasic}[k]+oauthOr(k), auth)
			}
		}
		return nil
	}
	switch a := str(s, "auth"); a {
	case HTTPAuthHeader, HTTPAuthNone:
		return only(a, "user", "token_url", "client_id", "scope", "audience")
	case HTTPAuthBasic:
		if str(s, "user") == "" {
			return invalid("auth basic needs user")
		}
		return only(a, "token_url", "client_id", "scope", "audience")
	case HTTPAuthOAuth2:
		if str(s, "client_id") == "" {
			return invalid("auth oauth2 needs client_id")
		}
		u, err := url.Parse(str(s, "token_url"))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
			return invalid("auth oauth2 needs token_url, an http(s) URL without credentials in it")
		}
		return only(a, "user")
	default:
		return invalid("auth must be header, basic, oauth2 or none, got %q", a)
	}
}

func oauthOr(k string) string {
	if k == "user" {
		return ""
	}
	return HTTPAuthOAuth2
}

// tokenHost is an oauth2 HTTP connection's token URL host ("" otherwise):
// the fetcher reaches it too, so the operator must allow it as well.
func tokenHost(typ string, s map[string]any) string {
	if typ != TypeHTTP || str(s, "auth") != HTTPAuthOAuth2 {
		return ""
	}
	u, err := url.Parse(str(s, "token_url"))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// secretRule says whether a connection with these settings needs a secret
// (and what it is), or takes none.
func secretRule(typ string, s map[string]any) (needed, forbidden bool, is string) {
	spec := types[typ]
	if typ != TypeHTTP {
		return spec.secretNeeded, false, spec.secretIs
	}
	switch str(s, "auth") {
	case HTTPAuthNone:
		return false, true, ""
	case HTTPAuthBasic:
		return true, false, "the password"
	case HTTPAuthOAuth2:
		return true, false, "the client secret"
	}
	return false, false, spec.secretIs // header: optional, as before auth existed (no secret, no header)
}
