package store

import (
	"testing"
	"time"
)

func TestAnalyticsComparisonUsesFirstDayInsteadOfLifetimeAndLeavesGapsUnknown(t *testing.T) {
	published := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	views := int64(10000)
	posts := []AnalyticsContentPost{
		{ID: 1, PublishedAt: &published, Audience: 100, Views: &views},
		{ID: 2, PublishedAt: &published, Audience: 100, Views: &views},
		{ID: 3, PublishedAt: &published, Audience: 100, RemovedFromMAX: true},
	}
	observations := []analyticsViewObservation{
		{PostID: 1, Views: 50, CapturedAt: published.Add(23 * time.Hour)},
		{PostID: 1, Views: 100, CapturedAt: published.Add(24 * time.Hour)},
		{PostID: 1, Views: 10000, CapturedAt: published.Add(72 * time.Hour)},
		{PostID: 2, Views: 500, CapturedAt: published.Add(22 * time.Hour)},
		{PostID: 3, Views: 500, CapturedAt: published.Add(24 * time.Hour)},
	}
	compared := applyAnalyticsComparisonWindow(posts, observations, published.Add(96*time.Hour))
	if posts[0].ComparisonViews == nil || *posts[0].ComparisonViews != 100 || *posts[0].Views != 10000 || compared[0].Score == nil || *compared[0].ViewsPer1KAudience != 1000 || *compared[0].ViewsPerHour != 4.17 {
		t.Fatalf("comparison = %+v; stored = %+v", compared[0], posts[0])
	}
	if compared[1].Score != nil || compared[2].Score != nil || posts[1].ComparisonViews != nil {
		t.Fatal("missing or removed post was ranked")
	}
	applyAnalyticsComparisonWindow(posts[:1], observations, published.Add(23*time.Hour))
	if posts[0].Score != nil {
		t.Fatal("immature post was ranked")
	}
}

func TestBestTimeNeedsRepeatedComparableSlotsAndTenPosts(t *testing.T) {
	metric, score := 100.0, 10.0
	posts := []AnalyticsContentPost{}
	for i := 0; i < 10; i++ {
		published := time.Date(2026, 7, 6, 9+i%2, 0, 0, 0, time.UTC).AddDate(0, 0, (i/2)*7)
		posts = append(posts, AnalyticsContentPost{ID: int64(i + 1), PublishedAt: &published, ViewsPer1KAudience: &metric, ViewsPerHour: &metric, Score: &score})
	}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, best := buildAnalyticsContentHeatmap(posts[:9], now, 0); best != nil {
		t.Fatal("recommendation from nine posts")
	}
	if _, best := buildAnalyticsContentHeatmap(posts, now, 0); best == nil || best.SampleSize != 5 || best.TotalSampleSize != 10 || best.ComparisonWindowHours != 24 {
		t.Fatalf("best = %+v", best)
	}
	for i := range posts {
		posts[i].PublishedAt = posts[0].PublishedAt
	}
	if _, best := buildAnalyticsContentHeatmap(posts, now, 0); best != nil {
		t.Fatal("recommendation without an alternative comparable slot")
	}
}
