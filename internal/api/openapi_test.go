// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// api/openapi.yaml documents exactly the routes the server serves. Every
// response the API tests receive is also checked against the spec's
// schemas (checkSpec), and so is every response in the integration tests.
func TestOpenAPIMatchesRouteTable(t *testing.T) {
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		OpenAPI string                          `yaml:"openapi"`
		Paths   map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Fatalf("openapi version %q", doc.OpenAPI)
	}

	documented := map[string]bool{}
	for p, ops := range doc.Paths {
		for method := range ops {
			switch method {
			case "get", "post", "put", "patch", "delete", "head", "options":
				documented[strings.ToUpper(method)+" "+p] = true
			}
		}
	}
	served := map[string]bool{}
	for _, rt := range Routes(newEnv(t).deps) {
		served[rt.Method+" "+strings.ReplaceAll(rt.Pattern, "{path...}", "{path}")] = true
	}

	var missing, extra []string
	for k := range served {
		if !documented[k] {
			missing = append(missing, k)
		}
	}
	for k := range documented {
		if !served[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("served but not in api/openapi.yaml: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("in api/openapi.yaml but not served: %v", extra)
	}
}
