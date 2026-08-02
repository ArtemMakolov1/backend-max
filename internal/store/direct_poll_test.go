package store

import (
	"testing"
	"time"
)

func TestDirectProviderNextCheckAtUsesLifecycleCadence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		status string
		delay  time.Duration
	}{
		{status: "moderation", delay: 5 * time.Minute},
		{status: "provider_draft", delay: 15 * time.Minute},
		{status: "accepted", delay: 15 * time.Minute},
		{status: "active", delay: 30 * time.Minute},
		{status: "suspended", delay: time.Hour},
		{status: "rejected", delay: 6 * time.Hour},
	} {
		test := test
		t.Run(test.status, func(t *testing.T) {
			t.Parallel()
			if got := directProviderNextCheckAt(test.status, now); !got.Equal(now.Add(test.delay)) {
				t.Fatalf("next check = %s, want %s", got, now.Add(test.delay))
			}
		})
	}
}
