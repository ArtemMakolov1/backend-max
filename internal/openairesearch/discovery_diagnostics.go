package openairesearch

import (
	"context"
	"errors"
)

type discoveryFailureStage string

const (
	discoveryPrecheck       discoveryFailureStage = "discovery_precheck"
	discoverySynthesis      discoveryFailureStage = "discovery_synthesis"
	discoverySynthesisRetry discoveryFailureStage = "discovery_synthesis_retry"
	discoveryPostcheck      discoveryFailureStage = "discovery_postcheck"
)

// DiscoveryDiagnostics contains only source-owned enums, never response text,
// editorial context or provider URLs. Error keeps these diagnostics private.
type DiscoveryDiagnostics struct {
	Stage            string
	ValidationReason string
}

type discoveryValidationError struct {
	message string
	reason  string
}

func (e *discoveryValidationError) Error() string { return e.message }

// DiscoveryFailureDiagnostics is the only boundary for logging the private
// discovery context. Zero or unexpected values cannot become log fragments.
func (e *Error) DiscoveryFailureDiagnostics() DiscoveryDiagnostics {
	if e == nil {
		return DiscoveryDiagnostics{}
	}
	diagnostic := e.discoveryDiagnostics
	switch discoveryFailureStage(diagnostic.Stage) {
	case discoveryPrecheck, discoverySynthesis, discoverySynthesisRetry, discoveryPostcheck:
	default:
		return DiscoveryDiagnostics{}
	}
	diagnostic.ValidationReason = safeDiscoveryValidationReason(diagnostic.ValidationReason)
	return diagnostic
}

func safeDiscoveryValidationReason(reason string) string {
	switch reason {
	case "invalid_pair_decisions", "discovery_json_invalid", "discovery_shape_invalid",
		"discovery_kind_invalid", "discovery_kind_mismatch", "discovery_source_unsafe",
		"discovery_source_unverified", "discovery_card_bounds", "discovery_draft_bounds",
		"discovery_format_unsupported", "discovery_external_links", "discovery_attribution_size",
		"discovery_source_format", "discovery_forbidden_words", "card_title_bounds", "card_summary_bounds",
		"draft_title_bounds", "draft_content_bounds", "draft_format_mismatch", "draft_image_prompt_bounds":
		return reason
	default:
		return "unknown"
	}
}

func annotateDiscoveryFailure(err error, stage discoveryFailureStage, validationErr error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		return err
	}
	// A serving provider error can be shared by a caller or test transport.
	// Adding one request's stage must not mutate that original error.
	clone := *providerErr
	clone.discoveryDiagnostics.Stage = string(stage)
	if validationErr != nil {
		clone.discoveryDiagnostics.ValidationReason = discoveryValidationReason(validationErr)
	}
	clone.discoveryDiagnostics = clone.DiscoveryFailureDiagnostics()
	return &clone
}

func discoveryValidationReason(err error) string {
	if err == nil {
		return "unknown"
	}
	var fieldErr *discoveryValidationError
	if errors.As(err, &fieldErr) {
		return safeDiscoveryValidationReason(fieldErr.reason)
	}
	// Match complete source-owned messages, never substrings or dynamically
	// formatted decoder/provider errors that could contain private fragments.
	switch err.Error() {
	case "invalid publication identity decisions":
		return "invalid_pair_decisions"
	case "invalid content discovery JSON":
		return "discovery_json_invalid"
	case "content discovery must contain one JSON object and at most three cards":
		return "discovery_shape_invalid"
	case "content discovery card kind is invalid":
		return "discovery_kind_invalid"
	case "content discovery did not honor the requested kind":
		return "discovery_kind_mismatch"
	case "content discovery contains an unsafe source":
		return "discovery_source_unsafe"
	case "content discovery source was not returned by web search":
		return "discovery_source_unverified"
	case "content discovery card exceeds its text bounds":
		return "discovery_card_bounds"
	case "content discovery draft exceeds its text bounds":
		return "discovery_draft_bounds"
	case "content discovery draft uses unsupported MAX formatting":
		return "discovery_format_unsupported"
	case "content discovery draft must not invent external links":
		return "discovery_external_links"
	case "content discovery draft with attribution exceeds the MAX post size bound":
		return "discovery_attribution_size"
	case "content discovery source cannot be safely formatted for MAX":
		return "discovery_source_format"
	case "draft title contains a forbidden word or phrase", "draft content contains a forbidden word or phrase", "draft image_prompt contains a forbidden word or phrase":
		return "discovery_forbidden_words"
	default:
		return "unknown"
	}
}
