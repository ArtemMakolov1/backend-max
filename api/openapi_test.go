package contract_test

import (
	"os"
	"testing"

	"go.yaml.in/yaml/v2"
)

type openAPIDocument struct {
	OpenAPI string                    `yaml:"openapi"`
	Paths   map[string]map[string]any `yaml:"paths"`
}

// Keep the public browser contract in lockstep with the routes mounted by
// internal/api.Server. Compatibility aliases are intentionally omitted: the
// frontend must use the canonical path while the backend can keep an alias
// during a migration window.
func TestOpenAPIContainsBrowserRoutes(t *testing.T) {
	contents, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document openAPIDocument
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse OpenAPI document: %v", err)
	}
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q, want 3.1.0", document.OpenAPI)
	}

	expected := map[string][]string{
		"/health":                         {"get"},
		"/auth/yandex/start":              {"post"},
		"/auth/max/start":                 {"post"},
		"/auth/max/{request_id}/complete": {"post"},
		"/auth/max/{request_id}":          {"delete"},
		"/auth/logout":                    {"post"},
		"/channels":                       {"get"},
		"/channels/discoverable":          {"get"},
		"/channels/discoverable/refresh":  {"post"},
		"/channels/connect/observed":      {"post"},
		"/channels/connect/start":         {"post"},
		"/channels/connect/{claim_id}":    {"get"},
		"/channels/{id}/test":             {"post"},
		"/channels/{id}":                  {"delete"},
		"/analytics":                      {"get"},
		"/posts":                          {"get", "post"},
		"/posts/format-content":           {"post"},
		"/posts/{id}":                     {"get", "put", "patch", "delete"},
		"/posts/{id}/duplicate":           {"post"},
		"/posts/{id}/generate-image":      {"post"},
		"/posts/{id}/image":               {"post"},
		"/posts/{id}/publish":             {"post"},
		"/posts/{id}/schedule":            {"post"},
		"/posts/{id}/cancel-schedule":     {"post"},
		"/posts/{id}/update-published":    {"post"},
		"/posts/{id}/sync-max":            {"post"},
		"/posts/{id}/pin":                 {"post", "delete"},
		"/posts/{id}/publication":         {"delete"},
		"/research/generate":              {"post"},
		"/integration/max":                {"get"},
		"/integration/max/test":           {"post"},
		"/integration/max/identity":       {"get", "post"},
	}
	for path, methods := range expected {
		operations, ok := document.Paths[path]
		if !ok {
			t.Errorf("contract is missing path %s", path)
			continue
		}
		for _, method := range methods {
			if _, ok := operations[method]; !ok {
				t.Errorf("contract is missing %s %s", method, path)
			}
		}
	}
}
