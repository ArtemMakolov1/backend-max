package store

import "time"

// Compare observed counters close to the end of the first day. A missing
// observation is left unknown; lifetime views and extrapolation are never
// substituted. The 23–24 hour sampling tolerance is exposed in the UI.
func applyAnalyticsComparisonWindow(posts []AnalyticsContentPost, observations []analyticsViewObservation, asOf time.Time) []AnalyticsContentPost {
	byPost := make(map[int64][]analyticsViewObservation)
	for _, observation := range observations {
		byPost[observation.PostID] = append(byPost[observation.PostID], observation)
	}
	comparison := make([]AnalyticsContentPost, len(posts))
	for index := range posts {
		post := &posts[index]
		post.ComparisonWindowHours = 24
		post.Score = nil
		post.ComparisonViews, post.ComparisonCapturedAt = nil, nil
		comparison[index] = *post
		comparison[index].ViewsPer1KAudience = nil
		comparison[index].ViewsPerHour = nil
		if post.PublishedAt == nil || post.RemovedFromMAX || post.Audience <= 0 || asOf.Before(post.PublishedAt.Add(24*time.Hour)) {
			continue
		}
		start, end := post.PublishedAt.Add(23*time.Hour), post.PublishedAt.Add(24*time.Hour)
		var latest *analyticsViewObservation
		for _, observation := range byPost[post.ID] {
			if observation.Views < 0 || observation.CapturedAt.Before(start) || observation.CapturedAt.After(end) || observation.CapturedAt.After(asOf) {
				continue
			}
			if latest == nil || observation.CapturedAt.After(latest.CapturedAt) {
				copyObservation := observation
				latest = &copyObservation
			}
		}
		if latest == nil {
			continue
		}
		views, capturedAt := latest.Views, latest.CapturedAt
		post.ComparisonViews, post.ComparisonCapturedAt = &views, &capturedAt
		normalized := roundAnalyticsMetric(float64(views) * 1000 / float64(post.Audience))
		rate := roundAnalyticsMetric(float64(views) / capturedAt.Sub(*post.PublishedAt).Hours())
		score, _ := analyticsContentScore(&normalized, &rate)
		score = roundAnalyticsMetric(score)
		post.Score = &score
		comparison[index] = *post
		comparison[index].ViewsPer1KAudience, comparison[index].ViewsPerHour = &normalized, &rate
	}
	return comparison
}
