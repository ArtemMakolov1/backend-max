package app

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

// directAuthoritativeProviderValidationFailure accepts only an explicit
// per-item provider rejection. Transport, top-level API, malformed-response,
// and partial-success errors are ambiguous and must stay recoverable.
func directAuthoritativeProviderValidationFailure(err error) (string, bool) {
	var partial *yandexdirect.PartialMutationError
	if errors.As(err, &partial) {
		if partial == nil || len(partial.Results) != 1 ||
			partial.Results[0].ID > 0 ||
			len(partial.Results[0].Errors) == 0 {
			return "", false
		}
		for _, issue := range partial.Results[0].Errors {
			if issue.Code > 0 {
				return strconv.Itoa(issue.Code), true
			}
		}
		return "", false
	}
	var providerErr *yandexdirect.Error
	if !errors.As(err, &providerErr) || providerErr == nil ||
		providerErr.StatusCode != 0 || providerErr.APIErrorCode != 0 {
		return "", false
	}
	code := strings.TrimSpace(providerErr.Code)
	if len(code) == 0 || len(code) > 10 {
		return "", false
	}
	for _, char := range code {
		if char < '0' || char > '9' {
			return "", false
		}
	}
	numeric, parseErr := strconv.ParseUint(code, 10, 32)
	if parseErr != nil || numeric == 0 {
		return "", false
	}
	return code, true
}

func (a *App) failDirectSubmissionIfProviderAbsent(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
	providerErr error,
) (bool, error) {
	code, authoritative := directAuthoritativeProviderValidationFailure(providerErr)
	if !authoritative {
		return false, nil
	}
	id, err := a.directGraph.FindUnifiedCampaignByOperationMarker(
		ctx, token, material.Connection.ClientLogin,
		material.Operation.OperationMarker,
	)
	if err != nil || id != 0 {
		return false, err
	}
	_, err = a.store.FailDirectCampaignGraphOperation(
		ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
		store.DirectGraphTerminalFailureInput{
			ExpectedOperationID: material.Operation.ID,
			ExpectedClaimedAt:   material.Operation.ClaimedAt,
			ProviderOutcome:     store.DirectGraphProviderOutcomeAbsent,
			FailureCode:         code,
			ConfirmedAt:         a.now().UTC(),
		},
	)
	return true, err
}

func (a *App) failDirectUpdateIfBaselineUnchanged(
	ctx context.Context, material store.DirectGraphSubmissionMaterial,
	graph yandexdirect.CampaignGraph, providerErr error,
) (bool, error) {
	code, authoritative := directAuthoritativeProviderValidationFailure(providerErr)
	if !authoritative {
		return false, nil
	}
	hash, err := graph.Fingerprint()
	if err != nil || hash != material.Operation.ExpectedGraphHash {
		return false, err
	}
	_, err = a.store.FailDirectCampaignGraphOperation(
		ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
		store.DirectGraphTerminalFailureInput{
			ExpectedOperationID: material.Operation.ID,
			ExpectedClaimedAt:   material.Operation.ClaimedAt,
			ProviderOutcome:     store.DirectGraphProviderOutcomeBaselineUnchanged,
			FailureCode:         code,
			ConfirmedAt:         a.now().UTC(),
		},
	)
	return true, err
}
