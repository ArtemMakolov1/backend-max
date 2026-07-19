package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"maxpilot/backend/internal/openaiimg"
)

type guardrailImageClient struct {
	validateCalls int
	validateErr   error
}

func (*guardrailImageClient) Generate(context.Context, openaiimg.GenerateRequest) (openaiimg.Result, error) {
	return openaiimg.Result{}, nil
}

func (c *guardrailImageClient) Validate(openaiimg.GenerateRequest) error {
	c.validateCalls++
	return c.validateErr
}

func TestValidateImageRequestAppliesAPIPolicyBeforeClientValidation(t *testing.T) {
	t.Parallel()

	invalidRequests := []openaiimg.GenerateRequest{
		{Prompt: strings.Repeat("\u044f", 2001)},
		{Prompt: "A poster", Size: "auto"},
		{Prompt: "A poster", Size: "2048x1152"},
		{Prompt: "A poster", Quality: "auto"},
	}
	for _, request := range invalidRequests {
		client := &guardrailImageClient{}
		application := New(nil, nil, nil, client, nil, nil)
		if err := application.ValidateImageRequest(request); err == nil {
			t.Fatalf("ValidateImageRequest(%#v) returned no error", request)
		}
		if client.validateCalls != 0 {
			t.Fatalf("client validation calls = %d, want 0", client.validateCalls)
		}
	}

	clientErr := errors.New("model-specific validation")
	client := &guardrailImageClient{validateErr: clientErr}
	application := New(nil, nil, nil, client, nil, nil)
	err := application.ValidateImageRequest(openaiimg.GenerateRequest{
		Prompt: "A poster", Size: "1536x1024", Quality: "high",
	})
	if !errors.Is(err, clientErr) {
		t.Fatalf("ValidateImageRequest() error = %v, want client error", err)
	}
	if client.validateCalls != 1 {
		t.Fatalf("client validation calls = %d, want 1", client.validateCalls)
	}
}
