package api

import (
	"testing"
	"time"
)

func TestKeyedWindowLimiterSeparatesClients(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	limiter := newKeyedWindowLimiter(2, 10, time.Minute, 16)

	for attempt := 0; attempt < 2; attempt++ {
		if allowed, _ := limiter.Allow("client-a", now); !allowed {
			t.Fatalf("client-a attempt %d unexpectedly rejected", attempt+1)
		}
	}
	if allowed, _ := limiter.Allow("client-a", now); allowed {
		t.Fatal("client-a third attempt unexpectedly allowed")
	}
	if allowed, _ := limiter.Allow("client-b", now); !allowed {
		t.Fatal("client-b was blocked by client-a's rejected request")
	}
}

func TestKeyedWindowLimiterEdgeModeKeepsOnlyHighGlobalCeiling(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	limiter := newKeyedWindowLimiter(0, 3, time.Minute, 0)

	for attempt := 0; attempt < 3; attempt++ {
		if allowed, _ := limiter.Allow("shared-proxy", now); !allowed {
			t.Fatalf("edge-limited attempt %d unexpectedly rejected", attempt+1)
		}
	}
	if allowed, retryAfter := limiter.Allow("shared-proxy", now); allowed || retryAfter != time.Minute {
		t.Fatalf("global ceiling result = allowed %v, retry %s", allowed, retryAfter)
	}
}
