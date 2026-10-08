package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

// Retry only the read-only channel preflight while retaining the publishing
// claim. Its bounded attempts cannot retry a message request whose outcome is
// ambiguous, nor leave a failed post on the scheduler indefinitely.
func (a *App) inspectPublicationChannel(ctx context.Context, channel store.Channel) (maxclient.ChatInfo, maxclient.Membership, error) {
	var info maxclient.ChatInfo
	var membership maxclient.Membership
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			delay := time.Duration(attempt*attempt) * time.Second
			var providerErr *maxclient.Error
			if errors.As(err, &providerErr) && providerErr.RetryAfter > delay {
				if providerErr.RetryAfter > 10*time.Second {
					// Respect a long provider cooldown without keeping the claim
					// waiting indefinitely or retrying before MAX permits it.
					return maxclient.ChatInfo{}, maxclient.Membership{}, &publicationPreflightExhaustedError{cause: err}
				}
				delay = providerErr.RetryAfter
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return maxclient.ChatInfo{}, maxclient.Membership{}, ctx.Err()
			case <-timer.C:
			}
		}
		info, membership, err = a.inspectChannel(ctx, channel)
		if err == nil || !transientPublicationPreflightError(err) {
			return info, membership, err
		}
	}
	return maxclient.ChatInfo{}, maxclient.Membership{}, &publicationPreflightExhaustedError{cause: err}
}

type publicationPreflightExhaustedError struct{ cause error }

func (e *publicationPreflightExhaustedError) Error() string {
	return "MAX channel preflight retries exhausted: " + e.cause.Error()
}
func (e *publicationPreflightExhaustedError) Unwrap() error { return e.cause }

func transientPublicationPreflightError(err error) bool {
	var providerErr *maxclient.Error
	if !errors.As(err, &providerErr) {
		return false
	}
	switch providerErr.StatusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
