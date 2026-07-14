package openaiimg

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ValidateRequest performs model-independent validation and can be used by an
// HTTP handler before it reserves an AI quota. Client.Validate adds the
// configured model's exact size rules without making an upstream request.
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
