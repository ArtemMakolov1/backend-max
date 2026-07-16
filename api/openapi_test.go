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
		"/health":                                 {"get"},
		"/auth/yandex/start":                      {"post"},
		"/auth/max/start":                         {"post"},
		"/auth/max/{request_id}/complete":         {"post"},
		"/auth/max/{request_id}":                  {"delete"},
		"/auth/logout":                            {"post"},
		"/channels":                               {"get"},
		"/channels/discoverable":                  {"get"},
		"/channels/discoverable/refresh":          {"post"},
		"/channels/connect/observed":              {"post"},
		"/channels/connect/start":                 {"post"},
		"/channels/connect/{claim_id}":            {"get"},
		"/channels/{id}/test":                     {"post"},
		"/channels/{id}":                          {"delete"},
		"/analytics":                              {"get"},
		"/posts":                                  {"get", "post"},
		"/posts/format-content":                   {"post"},
		"/posts/{id}":                             {"get", "put", "patch", "delete"},
		"/posts/{id}/duplicate":                   {"post"},
		"/posts/{id}/generate-image":              {"post"},
		"/posts/{id}/image":                       {"post"},
		"/posts/{id}/attachments":                 {"post"},
		"/posts/{id}/attachments/{attachment_id}": {"put", "delete"},
		"/posts/{id}/attachments/order":           {"patch"},
		"/posts/{id}/publish":                     {"post"},
		"/posts/{id}/schedule":                    {"post"},
		"/posts/{id}/cancel-schedule":             {"post"},
		"/posts/{id}/update-published":            {"post"},
		"/posts/{id}/sync-max":                    {"post"},
		"/posts/{id}/pin":                         {"post", "delete"},
		"/posts/{id}/publication":                 {"delete"},
		"/research/generate":                      {"post"},
		"/integration/max":                        {"get"},
		"/integration/max/test":                   {"post"},
		"/integration/max/identity":               {"get", "post"},
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

	assertResponseRef(t, document, "/posts/{id}/attachments", "post", "201", "#/components/responses/PostAttachmentMutation")
	assertResponseRef(t, document, "/posts/{id}/attachments/{attachment_id}", "put", "200", "#/components/responses/PostAttachmentMutation")
	assertResponseRef(t, document, "/posts/{id}/attachments/{attachment_id}", "delete", "200", "#/components/responses/Post")
	assertResponseRef(t, document, "/posts/{id}/attachments/order", "patch", "200", "#/components/responses/Post")
}

func assertResponseRef(t *testing.T, document openAPIDocument, path, method, status, want string) {
	t.Helper()
	operation, ok := stringMap(document.Paths[path][method])
	if !ok {
		t.Fatalf("contract is missing %s %s", method, path)
	}
	responses, ok := stringMap(operation["responses"])
	if !ok {
		t.Fatalf("contract is missing responses for %s %s", method, path)
	}
	response, ok := stringMap(responses[status])
	if !ok {
		t.Fatalf("contract is missing response %s for %s %s", status, method, path)
	}
	got, _ := response["$ref"].(string)
	if got != want {
		t.Fatalf("response %s for %s %s = %q, want %q", status, method, path, got, want)
	}
}

func stringMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[any]any:
		converted := make(map[string]any, len(typed))
		for key, item := range typed {
			text, ok := key.(string)
			if !ok {
				return nil, false
			}
			converted[text] = item
		}
		return converted, true
	default:
		return nil, false
	}
}
