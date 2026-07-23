package contract_test

import (
	"os"
	"testing"

	"go.yaml.in/yaml/v2"
)

type openAPIDocument struct {
	OpenAPI    string                    `yaml:"openapi"`
	Paths      map[string]map[string]any `yaml:"paths"`
	Components map[string]any            `yaml:"components"`
}

// Keep the public browser contract in lockstep with the routes mounted by
// internal/api.Server. Independently consumable compatibility aliases remain
// documented as deprecated until their migration window closes.
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
		"/health":                                     {"get"},
		"/plans":                                      {"get"},
		"/webhooks/yookassa":                          {"post"},
		"/auth/yandex/start":                          {"post"},
		"/auth/max/start":                             {"post"},
		"/auth/max/{request_id}/complete":             {"post"},
		"/auth/max/{request_id}":                      {"delete"},
		"/auth/logout":                                {"post"},
		"/channels":                                   {"get"},
		"/channels/discoverable":                      {"get"},
		"/channels/discoverable/refresh":              {"post"},
		"/channels/connect/observed":                  {"post"},
		"/channels/connect/start":                     {"post"},
		"/channels/connect/{claim_id}":                {"get"},
		"/channels/{id}/test":                         {"post"},
		"/channels/{id}/description/suggest":          {"post"},
		"/channels/{id}":                              {"delete"},
		"/analytics":                                  {"get"},
		"/posts":                                      {"get", "post"},
		"/posts/format-content":                       {"post"},
		"/posts/suggest-image-prompt":                 {"post"},
		"/posts/{id}":                                 {"get", "put", "patch", "delete"},
		"/posts/{id}/duplicate":                       {"post"},
		"/posts/{id}/generate-image":                  {"post"},
		"/posts/{id}/image":                           {"post"},
		"/posts/{id}/attachments":                     {"post"},
		"/posts/{id}/attachments/{attachment_id}":     {"put", "delete"},
		"/posts/{id}/attachments/order":               {"patch"},
		"/posts/{id}/publish":                         {"post"},
		"/posts/{id}/schedule":                        {"post", "put", "delete"},
		"/posts/{id}/cancel-schedule":                 {"post"},
		"/posts/{id}/update-published":                {"post"},
		"/posts/{id}/sync":                            {"post"},
		"/posts/{id}/sync-max":                        {"post"},
		"/posts/{id}/pin":                             {"post", "delete"},
		"/posts/{id}/publication":                     {"delete"},
		"/posts/{id}/delete-publication":              {"post"},
		"/advertising/direct/oauth/callback":          {"get"},
		"/images/generate":                            {"post"},
		"/research/generate":                          {"post"},
		"/integration/max":                            {"get"},
		"/integration/max/test":                       {"post"},
		"/integration/max/identity":                   {"get", "post"},
		"/integrations/max":                           {"get"},
		"/integrations/max/test":                      {"post"},
		"/workspaces":                                 {"get", "post"},
		"/workspace-invitations/{token}/accept":       {"post"},
		"/workspaces/{workspace_id}":                  {"get", "patch", "delete"},
		"/workspaces/{workspace_id}/billing":          {"get"},
		"/workspaces/{workspace_id}/billing/checkout": {"post"},
		"/workspaces/{workspace_id}/billing/cancellation-intent":                                    {"post"},
		"/workspaces/{workspace_id}/billing/retention-offer":                                        {"post"},
		"/workspaces/{workspace_id}/billing/cancel-confirm":                                         {"post"},
		"/workspaces/{workspace_id}/billing/resume":                                                 {"post"},
		"/workspaces/{workspace_id}/billing/payment-method/detach":                                  {"post"},
		"/workspaces/{workspace_id}/advertising/direct":                                             {"get"},
		"/workspaces/{workspace_id}/advertising/direct/connect/start":                               {"post"},
		"/workspaces/{workspace_id}/advertising/direct/connect/complete":                            {"post"},
		"/workspaces/{workspace_id}/advertising/direct/connection":                                  {"delete"},
		"/workspaces/{workspace_id}/advertising/direct/campaigns":                                   {"get", "post"},
		"/workspaces/{workspace_id}/advertising/direct/campaigns/suggest":                           {"post"},
		"/workspaces/{workspace_id}/advertising/direct/campaigns/{campaign_id}":                     {"patch"},
		"/workspaces/{workspace_id}/advertising/direct/campaigns/{campaign_id}/auto-launch-consent": {"post", "delete"},
		"/workspaces/{workspace_id}/advertising/direct/campaigns/{campaign_id}/submit":              {"post"},
		"/workspaces/{workspace_id}/advertising/direct/campaigns/{campaign_id}/launch":              {"post"},
		"/workspaces/{workspace_id}/transfer-ownership":                                             {"post"},
		"/workspaces/{workspace_id}/members":                                                        {"get", "post"},
		"/workspaces/{workspace_id}/members/{user_id}":                                              {"patch", "delete"},
		"/workspaces/{workspace_id}/invitations":                                                    {"get", "post"},
		"/workspaces/{workspace_id}/invitations/{invitation_id}":                                    {"delete"},
		"/workspaces/{workspace_id}/audit":                                                          {"get"},
		"/workspaces/{workspace_id}/brand-kit":                                                      {"get", "put", "patch"},
		"/workspaces/{workspace_id}/brand-kit/suggest":                                              {"post"},
		"/workspaces/{workspace_id}/channel-templates":                                              {"get", "post"},
		"/workspaces/{workspace_id}/channel-templates/{template_id}":                                {"get", "put", "patch", "delete"},
		"/workspaces/{workspace_id}/analytics":                                                      {"get"},
		"/workspaces/{workspace_id}/analytics/content":                                              {"get"},
		"/workspaces/{workspace_id}/analytics/content/posts/{post_id}":                              {"get"},
		"/workspaces/{workspace_id}/analytics/content/posts/{post_id}/variation":                    {"post"},
		"/workspaces/{workspace_id}/analytics/content/posts/{post_id}/repeat":                       {"post"},
		"/workspaces/{workspace_id}/calendar":                                                       {"get"},
		"/workspaces/{workspace_id}/calendar/posts/{post_id}":                                       {"put"},
		"/workspaces/{workspace_id}/campaigns":                                                      {"get", "post"},
		"/workspaces/{workspace_id}/campaigns/{campaign_id}":                                        {"get", "patch", "delete"},
		"/workspaces/{workspace_id}/campaigns/{campaign_id}/variants":                               {"post"},
		"/workspaces/{workspace_id}/campaigns/{campaign_id}/variants/{variant_id}":                  {"patch", "delete"},
		"/workspaces/{workspace_id}/campaigns/{campaign_id}/materialize":                            {"post"},
		"/workspaces/{workspace_id}/campaigns/{campaign_id}/schedule":                               {"post"},
		"/workspaces/{workspace_id}/channels":                                                       {"get", "post"},
		"/workspaces/{workspace_id}/channels/connect/start":                                         {"post"},
		"/workspaces/{workspace_id}/channels/connect/{claim_id}":                                    {"get"},
		"/workspaces/{workspace_id}/channels/{channel_id}":                                          {"get", "patch", "delete"},
		"/workspaces/{workspace_id}/channels/{channel_id}/description/suggest":                      {"post"},
		"/workspaces/{workspace_id}/channels/{channel_id}/test":                                     {"post"},
		"/workspaces/{workspace_id}/posts":                                                          {"get", "post"},
		"/workspaces/{workspace_id}/posts/format-content":                                           {"post"},
		"/workspaces/{workspace_id}/posts/suggest-image-prompt":                                     {"post"},
		"/workspaces/{workspace_id}/research/generate":                                              {"post"},
		"/workspaces/{workspace_id}/images/generate":                                                {"post"},
		"/workspaces/{workspace_id}/media":                                                          {"post"},
		"/workspaces/{workspace_id}/media/{filename}":                                               {"get"},
		"/workspaces/{workspace_id}/posts/{post_id}":                                                {"get", "patch", "put", "delete"},
		"/workspaces/{workspace_id}/posts/{post_id}/duplicate":                                      {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/schedule":                                       {"post", "delete"},
		"/workspaces/{workspace_id}/posts/{post_id}/publish":                                        {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/update-published":                               {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/sync-max":                                       {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/pin":                                            {"post", "delete"},
		"/workspaces/{workspace_id}/posts/{post_id}/publication":                                    {"delete"},
		"/workspaces/{workspace_id}/posts/{post_id}/view-history":                                   {"get"},
		"/workspaces/{workspace_id}/posts/{post_id}/generate-image":                                 {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/image":                                          {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/attachments":                                    {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/attachments/{attachment_id}":                    {"put", "delete"},
		"/workspaces/{workspace_id}/posts/{post_id}/attachments/order":                              {"patch"},
		"/workspaces/{workspace_id}/posts/{post_id}/revisions":                                      {"get"},
		"/workspaces/{workspace_id}/posts/{post_id}/reviews":                                        {"get"},
		"/workspaces/{workspace_id}/posts/{post_id}/review":                                         {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/review/submit":                                  {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/review/approve":                                 {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/review/request-changes":                         {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/reviews/{revision_id}/decision":                 {"post"},
		"/workspaces/{workspace_id}/posts/{post_id}/comments":                                       {"get", "post"},
		"/workspaces/{workspace_id}/posts/{post_id}/comments/{comment_id}":                          {"patch", "delete"},
		"/notifications":                   {"get", "patch"},
		"/notifications/{notification_id}": {"patch"},
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
	assertResponseSchemaRef(t, document, "/workspaces/{workspace_id}/analytics/content/posts/{post_id}", "get", "200", "#/components/schemas/WorkspacePostAnalyticsEnvelope")
	assertResponseSchemaRef(t, document, "/workspaces/{workspace_id}/billing", "get", "200", "#/components/schemas/WorkspaceBilling")
	assertRequestSchemaRef(t, document, "/workspaces/{workspace_id}/billing/checkout", "post", "#/components/schemas/BillingCheckoutRequest")
	assertRequestSchemaRef(t, document, "/workspaces/{workspace_id}/billing/payment-method/detach", "post", "#/components/schemas/BillingDetachPaymentMethodRequest")
	assertRequestSchemaRef(t, document, "/images/generate", "post", "#/components/schemas/GenerateImageInput")
	assertRequestSchemaRef(t, document, "/posts/{id}/generate-image", "post", "#/components/schemas/GeneratePostImageInput")
	assertRequestSchemaRef(t, document, "/workspaces/{workspace_id}/posts/{post_id}/generate-image", "post", "#/components/schemas/GeneratePostImageInput")
	assertResponseSchemaRef(t, document, "/workspaces/{workspace_id}/advertising/direct", "get", "200", "#/components/schemas/DirectIntegrationEnvelope")
	assertRequestSchemaRef(t, document, "/workspaces/{workspace_id}/advertising/direct/connect/complete", "post", "#/components/schemas/DirectOAuthCompleteRequest")
	assertResponseSchemaRef(t, document, "/workspaces/{workspace_id}/advertising/direct/connect/complete", "post", "200", "#/components/schemas/DirectOAuthCompleteEnvelope")
	assertRequestSchemaRef(t, document, "/workspaces/{workspace_id}/advertising/direct/campaigns", "post", "#/components/schemas/DirectCampaignDraftRequest")
	assertRequestSchemaRef(t, document, "/workspaces/{workspace_id}/advertising/direct/campaigns/suggest", "post", "#/components/schemas/DirectCampaignSuggestionRequest")
	assertRequestSchemaRef(t, document, "/workspaces/{workspace_id}/advertising/direct/campaigns/{campaign_id}", "patch", "#/components/schemas/DirectCampaignPatchRequest")
	assertRequestSchemaRef(t, document, "/workspaces/{workspace_id}/advertising/direct/campaigns/{campaign_id}/auto-launch-consent", "post", "#/components/schemas/DirectAutoLaunchConsentRequest")
	assertRequestSchemaRef(t, document, "/workspaces/{workspace_id}/advertising/direct/campaigns/{campaign_id}/submit", "post", "#/components/schemas/DirectVersionRequest")
	assertRequestSchemaRef(t, document, "/workspaces/{workspace_id}/advertising/direct/campaigns/{campaign_id}/launch", "post", "#/components/schemas/DirectLaunchRequest")
	assertResponseRef(t, document, "/workspaces/{workspace_id}/advertising/direct/campaigns", "post", "422", "#/components/responses/ValidationProblem")
	assertResponseRef(t, document, "/workspaces/{workspace_id}/advertising/direct/campaigns/{campaign_id}", "patch", "422", "#/components/responses/ValidationProblem")
	assertSchemaRequiredProperty(t, document, "DirectIntegration", "auto_launch_enabled")
	assertSchemaRequiredProperty(t, document, "DirectIntegration", "max_campaign_weekly_budget_minor")
	assertSchemaRequiredProperty(t, document, "DirectIntegration", "max_workspace_weekly_budget_minor")
	assertSchemaRequiredProperty(t, document, "DirectOAuthStart", "expires_at")
	assertSchemaRequiredProperty(t, document, "DirectOAuthStart", "flow")
	assertSchemaOptionalProperty(t, document, "DirectOAuthStart", "state")
	assertSchemaRequiredProperty(t, document, "DirectOAuthCompleteRequest", "code")
	assertSchemaRequiredProperty(t, document, "DirectOAuthCompleteRequest", "state")
	assertSchemaRequiredProperty(t, document, "DirectConnection", "read_only")
	assertSchemaOptionalProperty(t, document, "DirectConnection", "error_code")
	assertSchemaRequiredProperty(t, document, "DirectCampaign", "provider_campaign_id")
	assertSchemaRequiredProperty(t, document, "DirectCampaign", "launch_state")
	assertSchemaRequiredProperty(t, document, "DirectCampaign", "setup_warning_code")
	assertSchemaEnumValue(t, document, "DirectCampaignStatus", "provider_draft")
	assertSchemaEnumValue(t, document, "DirectLaunchState", "failed")
	for _, capability := range []string{
		"ads.read", "ads.write", "ads.approve", "ads.launch",
		"ads.budget.manage", "ads.credentials.manage",
	} {
		assertSchemaEnumValue(t, document, "DirectCapability", capability)
		assertSchemaEnumValue(t, document, "WorkspaceCapability", capability)
	}
	assertSchemaPropertyType(t, document, "DirectCampaign", "provider_campaign_id", "string")
	assertSchemaPropertyType(t, document, "DirectCampaign", "provider_campaign_id", "null")
	assertSchemaRequiredProperty(t, document, "WorkspaceBilling", "monthly_enforcement_enabled")
	assertSchemaRequiredProperty(t, document, "WorkspaceBilling", "image_credit_costs")
	assertSchemaRequiredProperty(t, document, "WorkspaceBilling", "checkout_enabled")
	assertSchemaRequiredProperty(t, document, "WorkspaceBilling", "features")
	assertSchemaRequiredProperty(t, document, "WorkspaceBilling", "billing_actions")
	assertSchemaRequiredProperty(t, document, "BillingCheckoutRequest", "recurring_consent")
	assertSchemaRequiredProperty(t, document, "BillingCheckoutRequest", "recurring_consent_version")
	assertSchemaRequiredProperty(t, document, "BillingCatalogEntry", "recurring_consent_text")
	assertSchemaRequiredProperty(t, document, "BillingCatalogEntry", "recurring_consent_version")
	assertSchemaRequiredProperty(t, document, "BillingDetachPaymentMethodRequest", "confirmation")
	assertSchemaOptionalProperty(t, document, "BillingCheckout", "confirmation_url")
	assertSchemaRequiredProperty(t, document, "ImageCreditCosts", "low")
	assertSchemaRequiredProperty(t, document, "ImageCreditCosts", "medium")
	assertSchemaRequiredProperty(t, document, "ImageCreditCosts", "high")
	assertSchemaRequiredProperty(t, document, "GenerateImageInput", "prompt")
	assertSchemaOptionalProperty(t, document, "GenerateImageInput", "size")
	assertSchemaOptionalProperty(t, document, "GenerateImageInput", "quality")
	assertSchemaOptionalProperty(t, document, "GeneratePostImageInput", "prompt")
	assertSchemaRequiredProperty(t, document, "WorkspacePostAnalyticsSummary", "series_truncated")
}

func assertSchemaEnumValue(t *testing.T, document openAPIDocument, schemaName, want string) {
	t.Helper()
	schemas, ok := stringMap(document.Components["schemas"])
	if !ok {
		t.Fatal("contract components are missing schemas")
	}
	schema, ok := stringMap(schemas[schemaName])
	if !ok {
		t.Fatalf("contract is missing schema %s", schemaName)
	}
	values, ok := schema["enum"].([]any)
	if !ok {
		t.Fatalf("schema %s is missing enum values", schemaName)
	}
	for _, value := range values {
		if value == want {
			return
		}
	}
	t.Fatalf("schema %s enum does not contain %q", schemaName, want)
}

func assertSchemaPropertyType(
	t *testing.T, document openAPIDocument, schemaName, propertyName, want string,
) {
	t.Helper()
	schemas, ok := stringMap(document.Components["schemas"])
	if !ok {
		t.Fatal("contract components are missing schemas")
	}
	schema, ok := stringMap(schemas[schemaName])
	if !ok {
		t.Fatalf("contract is missing schema %s", schemaName)
	}
	properties, ok := stringMap(schema["properties"])
	if !ok {
		t.Fatalf("schema %s is missing properties", schemaName)
	}
	property, ok := stringMap(properties[propertyName])
	if !ok {
		t.Fatalf("schema %s is missing property %s", schemaName, propertyName)
	}
	switch value := property["type"].(type) {
	case string:
		if value == want {
			return
		}
	case []any:
		for _, item := range value {
			if item == want {
				return
			}
		}
	}
	t.Fatalf("schema %s property %s type does not contain %q", schemaName, propertyName, want)
}

func assertSchemaRequiredProperty(t *testing.T, document openAPIDocument, schemaName, propertyName string) {
	t.Helper()
	schemas, ok := stringMap(document.Components["schemas"])
	if !ok {
		t.Fatal("contract components are missing schemas")
	}
	schema, ok := stringMap(schemas[schemaName])
	if !ok {
		t.Fatalf("contract is missing schema %s", schemaName)
	}
	properties, ok := stringMap(schema["properties"])
	if !ok || properties[propertyName] == nil {
		t.Fatalf("schema %s is missing property %s", schemaName, propertyName)
	}
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("schema %s is missing required properties", schemaName)
	}
	for _, item := range required {
		if item == propertyName {
			return
		}
	}
	t.Fatalf("schema %s does not require property %s", schemaName, propertyName)
}

func assertSchemaOptionalProperty(t *testing.T, document openAPIDocument, schemaName, propertyName string) {
	t.Helper()
	schemas, ok := stringMap(document.Components["schemas"])
	if !ok {
		t.Fatal("contract components are missing schemas")
	}
	schema, ok := stringMap(schemas[schemaName])
	if !ok {
		t.Fatalf("contract is missing schema %s", schemaName)
	}
	properties, ok := stringMap(schema["properties"])
	if !ok || properties[propertyName] == nil {
		t.Fatalf("schema %s is missing property %s", schemaName, propertyName)
	}
	required, _ := schema["required"].([]any)
	for _, item := range required {
		if item == propertyName {
			t.Fatalf("schema %s unexpectedly requires property %s", schemaName, propertyName)
		}
	}
}

func assertRequestSchemaRef(t *testing.T, document openAPIDocument, path, method, want string) {
	t.Helper()
	operation, ok := stringMap(document.Paths[path][method])
	if !ok {
		t.Fatalf("contract is missing %s %s", method, path)
	}
	requestBody, ok := stringMap(operation["requestBody"])
	if !ok {
		t.Fatalf("contract is missing request body for %s %s", method, path)
	}
	content, ok := stringMap(requestBody["content"])
	if !ok {
		t.Fatalf("request body for %s %s is missing content", method, path)
	}
	mediaType, ok := stringMap(content["application/json"])
	if !ok {
		t.Fatalf("request body for %s %s is missing application/json", method, path)
	}
	schema, ok := stringMap(mediaType["schema"])
	if !ok {
		t.Fatalf("request body for %s %s is missing schema", method, path)
	}
	got, _ := schema["$ref"].(string)
	if got != want {
		t.Fatalf("request schema for %s %s = %q, want %q", method, path, got, want)
	}
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

func assertResponseSchemaRef(t *testing.T, document openAPIDocument, path, method, status, want string) {
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
	content, ok := stringMap(response["content"])
	if !ok {
		t.Fatalf("response %s for %s %s is missing content", status, method, path)
	}
	mediaType, ok := stringMap(content["application/json"])
	if !ok {
		t.Fatalf("response %s for %s %s is missing application/json", status, method, path)
	}
	schema, ok := stringMap(mediaType["schema"])
	if !ok {
		t.Fatalf("response %s for %s %s is missing schema", status, method, path)
	}
	got, _ := schema["$ref"].(string)
	if got != want {
		t.Fatalf("response schema %s for %s %s = %q, want %q", status, method, path, got, want)
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
