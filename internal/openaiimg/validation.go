package openaiimg

import (
	"errors"
	"strings"
	"unicode/utf8"
)

const maxAPIImagePromptRunes = 2000

// ValidateAPIRequest applies the deliberately narrow image options exposed by
// MaxPosty's HTTP API. Keep this separate from ValidateRequest: the reusable
// OpenAI client supports additional upstream options for non-HTTP callers.
func ValidateAPIRequest(request GenerateRequest) error {
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return errors.New("image prompt is required")
	}
	if utf8.RuneCountInString(prompt) > maxAPIImagePromptRunes {
		return errors.New("image prompt must not exceed 2000 characters")
	}

	switch request.Size {
	case "", "1024x1024", "1536x1024", "1024x1536":
	default:
		return errors.New("image size must be 1024x1024, 1536x1024 or 1024x1536")
	}

	switch request.Quality {
	case "", "low", "medium", "high":
	default:
		return errors.New("image quality must be low, medium or high")
	}
	return nil
}

// ValidateRequest performs the reusable client's model-independent upstream
// validation. Client.Validate adds the configured model's exact size rules
// without making an upstream request.
func ValidateRequest(request GenerateRequest) error {
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return errors.New("image prompt is required")
	}
	if utf8.RuneCountInString(prompt) > 32000 {
		return errors.New("image prompt is too long")
	}
	if request.Quality != "" && request.Quality != "auto" && request.Quality != "low" && request.Quality != "medium" && request.Quality != "high" {
		return errors.New("image quality must be auto, low, medium or high")
	}
	return nil
}

func (c *Client) Validate(request GenerateRequest) error {
	if err := ValidateRequest(request); err != nil {
		return err
	}
	size := request.Size
	if size == "" {
		size = "1024x1024"
	}
	return validateSize(c.model, size)
}
