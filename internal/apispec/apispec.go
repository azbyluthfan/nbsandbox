// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package apispec checks HTTP responses against api/openapi.yaml: the status
// code must be documented for the operation, the content type must be one
// the spec lists, and a JSON body must match its schema. Tests use it so the
// published API description can't drift from what the server sends.
package apispec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const docURL = "file:///openapi.json"

// Spec is a loaded OpenAPI document.
type Spec struct {
	doc      map[string]any
	compiler *jsonschema.Compiler
	routes   []route

	mu      sync.Mutex
	schemas map[string]*jsonschema.Schema
}

type route struct {
	template string
	segs     []string
	ops      map[string]any // lower-case method → operation object
}

// Load reads and prepares the OpenAPI document at path.
func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path) //nolint:gosec // test input chosen by the caller
	if err != nil {
		return nil, err
	}
	var y any
	if err := yaml.Unmarshal(b, &y); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Round-trip through JSON so the document has the shapes the schema
	// library expects (string keys, json.Number).
	jb, err := json.Marshal(y)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(jb))
	if err != nil {
		return nil, err
	}
	doc, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("openapi document is not an object")
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(docURL, doc); err != nil {
		return nil, err
	}
	s := &Spec{doc: doc, compiler: c, schemas: map[string]*jsonschema.Schema{}}
	paths, _ := doc["paths"].(map[string]any)
	for tmpl, p := range paths {
		ops := map[string]any{}
		if pm, ok := p.(map[string]any); ok {
			for k, op := range pm {
				switch k {
				case "get", "post", "put", "patch", "delete", "head", "options":
					ops[k] = op
				}
			}
		}
		s.routes = append(s.routes, route{template: tmpl, segs: strings.Split(strings.Trim(tmpl, "/"), "/"), ops: ops})
	}
	// Literal segments win over parameters: try the most specific first.
	sort.Slice(s.routes, func(i, j int) bool { return literals(s.routes[i].segs) > literals(s.routes[j].segs) })
	return s, nil
}

func literals(segs []string) int {
	n := 0
	for _, s := range segs {
		if !strings.HasPrefix(s, "{") {
			n++
		}
	}
	return n
}

// match finds the documented path template for a request path. A final
// {path} parameter matches the rest of the path, slashes included.
func (s *Spec) match(path string) (route, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for _, r := range s.routes {
		if matchSegs(r.segs, segs) {
			return r, true
		}
	}
	return route{}, false
}

func matchSegs(tmpl, segs []string) bool {
	for i, t := range tmpl {
		if i >= len(segs) {
			return false
		}
		if t == "{path}" && i == len(tmpl)-1 {
			return true
		}
		if !strings.HasPrefix(t, "{") && t != segs[i] {
			return false
		}
	}
	return len(tmpl) == len(segs)
}

// Covered reports whether the spec documents method and path at all.
// Requests to undocumented paths (deliberately unknown URLs in tests) are
// not validated.
func (s *Spec) Covered(method, path string) bool {
	r, ok := s.match(path)
	if !ok {
		return false
	}
	_, ok = r.ops[strings.ToLower(method)]
	return ok
}

// Validate checks one response. path is the request path without the query.
func (s *Spec) Validate(method, path string, status int, header http.Header, body []byte) error {
	r, ok := s.match(path)
	if !ok {
		return fmt.Errorf("%s %s: path not in the spec", method, path)
	}
	op, ok := r.ops[strings.ToLower(method)].(map[string]any)
	if !ok {
		return fmt.Errorf("%s %s: method not in the spec for %s", method, path, r.template)
	}
	where := fmt.Sprintf("%s %s → %d", method, r.template, status)
	responses, _ := op["responses"].(map[string]any)
	code := strconv.Itoa(status)
	resp, ok := responses[code]
	ptr := "/paths/" + escape(r.template) + "/" + strings.ToLower(method) + "/responses/" + code
	if !ok {
		resp, ok = responses["default"]
		ptr = "/paths/" + escape(r.template) + "/" + strings.ToLower(method) + "/responses/default"
	}
	if !ok {
		return fmt.Errorf("%s: status not documented", where)
	}
	respObj, ptr, err := s.deref(resp, ptr)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	content, _ := respObj["content"].(map[string]any)
	if len(content) == 0 {
		if len(bytes.TrimSpace(body)) > 0 {
			return fmt.Errorf("%s: spec documents no body, got %d bytes", where, len(body))
		}
		return nil
	}
	got, _, _ := mime.ParseMediaType(header.Get("Content-Type"))
	media, mediaKey := content[got], got
	if media == nil {
		for k, v := range content { // e.g. application/octet-stream documented as */*
			if k == "*/*" || (strings.HasSuffix(k, "/*") && strings.HasPrefix(got, strings.TrimSuffix(k, "*"))) {
				media, mediaKey = v, k
				break
			}
		}
	}
	if media == nil {
		var want []string
		for k := range content {
			want = append(want, k)
		}
		sort.Strings(want)
		return fmt.Errorf("%s: content type %q not documented (spec: %s)", where, got, strings.Join(want, ", "))
	}
	mo, _ := media.(map[string]any)
	if _, has := mo["schema"]; !has || got != "application/json" {
		return nil
	}
	sch, err := s.schema(ptr + "/content/" + escape(mediaKey) + "/schema")
	if err != nil {
		return fmt.Errorf("%s: compile schema: %w", where, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: body is not JSON: %w", where, err)
	}
	if err := sch.Validate(inst); err != nil {
		return fmt.Errorf("%s: body doesn't match the schema: %w\nbody: %s", where, err, truncate(body, 600))
	}
	return nil
}

// deref follows a response $ref ("#/components/responses/X").
func (s *Spec) deref(v any, ptr string) (map[string]any, string, error) {
	for range 5 {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, "", errors.New("response is not an object")
		}
		ref, ok := m["$ref"].(string)
		if !ok {
			return m, ptr, nil
		}
		if !strings.HasPrefix(ref, "#/") {
			return nil, "", fmt.Errorf("unsupported $ref %q", ref)
		}
		ptr = ref[1:]
		v = s.doc
		for _, p := range strings.Split(ref[2:], "/") {
			mm, ok := v.(map[string]any)
			if !ok {
				return nil, "", fmt.Errorf("bad $ref %q", ref)
			}
			v = mm[strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")]
		}
	}
	return nil, "", errors.New("$ref chain too long")
}

func (s *Spec) schema(ptr string) (*jsonschema.Schema, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sch, ok := s.schemas[ptr]; ok {
		return sch, nil
	}
	sch, err := s.compiler.Compile(docURL + "#" + ptr)
	if err != nil {
		return nil, err
	}
	s.schemas[ptr] = sch
	return sch, nil
}

func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
