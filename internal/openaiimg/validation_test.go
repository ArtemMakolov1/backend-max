package openaiimg

import (
	"net/http"
	"strings"
	"testing"
)

func TestValidateAPIRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request GenerateRequest
		wantErr string
	}{
		{name: "defaults", request: GenerateRequest{Prompt: "A poster"}},
		{name: "unicode prompt boundary", request: GenerateRequest{Prompt: strings.Repeat("\u044f", 2000), Size: "1024x1024", Quality: "medium"}},
		{name: "landscape high", request: GenerateRequest{Prompt: "A poster", Size: "1536x1024", Quality: "high"}},
		{name: "portrait low", request: GenerateRequest{Prompt: "A poster", Size: "1024x1536", Quality: "low"}},
		{name: "empty prompt", request: GenerateRequest{Prompt: "  "}, wantErr: "prompt is required"},
		{name: "oversized unicode prompt", request: GenerateRequest{Prompt: strings.Repeat("\u044f", 2001)}, wantErr: "2000 characters"},
		{name: "auto size", request: GenerateRequest{Prompt: "A poster", Size: "auto"}, wantErr: "image size"},
		{name: "custom size", request: GenerateRequest{Prompt: "A poster", Size: "2048x1152"}, wantErr: "image size"},
		{name: "auto quality", request: GenerateRequest{Prompt: "A poster", Quality: "auto"}, wantErr: "image quality"},
		{name: "unknown quality", request: GenerateRequest{Prompt: "A poster", Quality: "ultra"}, wantErr: "image quality"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAPIRequest(test.request)
			if test.wantErr == "" && err != nil {
				t.Fatalf("ValidateAPIRequest() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("ValidateAPIRequest() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestReusableClientValidationKeepsUpstreamCapabilities(t *testing.T) {
	t.Parallel()

	request := GenerateRequest{
		Prompt:  strings.Repeat("\u044f", 2001),
		Size:    "2048x1152",
		Quality: "auto",
	}
	if err := ValidateRequest(request); err != nil {
		t.Fatalf("ValidateRequest() rejected reusable-client request: %v", err)
	}
	client, err := New("https://api.openai.com", "key", "gpt-image-2", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Validate(request); err != nil {
		t.Fatalf("Client.Validate() rejected reusable-client request: %v", err)
	}
}
