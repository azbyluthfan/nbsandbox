// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package connections

import (
	"fmt"
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
	}, secretIs: "the secret access key"},
	TypeAzureBlob: {fields: map[string]field{
		"account":   {kind: "string", required: true},
		"container": {kind: "string", required: true},
		"endpoint":  {kind: "string"},                           // default: <account>.blob.core.windows.net
		"auth":      {kind: "string", def: "connection_string"}, // what the secret is: connection_string, account_key or sas
	}, secretIs: "the connection string, account key or SAS token"},
	TypeHTTP: {fields: map[string]field{ //nolint:gosec // a header's name and a prefix, not credentials
		"url":         {kind: "string", required: true},
		"auth_header": {kind: "string", def: "Authorization"},
		"auth_prefix": {kind: "string", def: "Bearer "},
	}, secretIs: "the token or API key"},
}

func dbSpec(port int) typeSpec {
	return typeSpec{fields: map[string]field{
		"host":     {kind: "string", required: true},
		"port":     {kind: "int", def: port},
		"database": {kind: "string", required: true},
		"user":     {kind: "string", required: true},
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
		return hostOnly(str(s, "host"))
	case TypeS3:
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
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
			return "", invalid("url must be an http(s) URL without credentials in it, got %q", s["url"])
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

func hostOnly(h string) (string, error) {
	h = strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), "."))
	if h == "" || strings.ContainsAny(h, "/@ ?#") {
		return "", invalid("%q is not a host name or address", h)
	}
	return h, nil
}
