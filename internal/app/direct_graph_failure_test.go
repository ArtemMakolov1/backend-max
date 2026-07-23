package app

import (
	"errors"
	"testing"

	"maxpilot/backend/internal/yandexdirect"
)

func TestDirectAuthoritativeProviderValidationFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		code string
		ok   bool
	}{
		{
			name: "campaign per item rejection",
			err:  &yandexdirect.Error{Code: "6000"},
			code: "6000", ok: true,
		},
		{
			name: "single graph item rejection",
			err: &yandexdirect.PartialMutationError{
				Operation: "campaigns.update",
				Results: []yandexdirect.MutationResult{{
					Errors: []yandexdirect.ProviderIssue{{Code: 5007}},
				}},
			},
			code: "5007", ok: true,
		},
		{
			name: "partial success",
			err: &yandexdirect.PartialMutationError{
				Operation: "campaigns.update",
				Results: []yandexdirect.MutationResult{{
					ID: 77, Errors: []yandexdirect.ProviderIssue{{Code: 5007}},
				}},
			},
		},
		{
			name: "multiple graph results",
			err: &yandexdirect.PartialMutationError{
				Operation: "campaigns.update",
				Results: []yandexdirect.MutationResult{
					{Errors: []yandexdirect.ProviderIssue{{Code: 5007}}},
					{Errors: []yandexdirect.ProviderIssue{{Code: 6000}}},
				},
			},
		},
		{
			name: "top level API rejection",
			err: &yandexdirect.Error{
				Code: "6000", APIErrorCode: 53,
			},
		},
		{
			name: "HTTP rejection",
			err: &yandexdirect.Error{
				Code: "6000", StatusCode: 500,
			},
		},
		{name: "generic provider code", err: &yandexdirect.Error{Code: "failed"}},
		{name: "joined transport error", err: errors.Join(errors.New("timeout"))},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			code, ok := directAuthoritativeProviderValidationFailure(test.err)
			if code != test.code || ok != test.ok {
				t.Fatalf(
					"classification = (%q,%v), want (%q,%v)",
					code, ok, test.code, test.ok,
				)
			}
		})
	}
}
