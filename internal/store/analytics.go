package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProductAnalyticsSnapshot is a PII-free aggregate suitable for low-cardinality
// Prometheus gauges. Active-user periods are rolling UTC calendar days: 1, 7,
// and 30 days including the day containing the supplied observation time.
//
// Funnel values are progressive: every stage is a subset of the preceding
// stage. This keeps conversion ratios meaningful even if legacy data contains
// posts or channels that were created before explicit MAX identity linking.
type ProductAnalyticsSnapshot struct {
	DailyActiveUsers              int64
	WeeklyActiveUsers             int64
	MonthlyActiveUsers            int64
	RegisteredUsers               int64
	MAXLinkedUsers                int64
	ChannelConnectedUsers         int64
	PostCreatedUsers              int64
	PostScheduledOrPublishedUsers int64
	PostPublishedUsers            int64
	PublishedPosts                int64
	PublishedPostsWithMedia       int64
	PublishedPostsWithMultiple    int64
	PublishedPostsWithVideo       int64
	PublishedPostsWithMixedMedia  int64
}

// TouchUserActivity records only that an authenticated account was active on
// a UTC calendar day. Repeated calls for the same account and day update one
// row and retain the latest observation. The pseudonymous account ID is stored
// for distinct counting, but no routes, request contents, IPs or user agents.
func (s *Store) TouchUserActivity(ctx context.Context, userID string, at time.Time) error {
	if strings.TrimSpace(userID) == "" {
		return errors.New("user id is required")
	}
	if at.IsZero() {
		return errors.New("activity time is required")
	}
	at = at.UTC()
	activityDate := at.Format(time.DateOnly)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO user_activity_daily(owner_id, activity_date, last_seen_at)
VALUES (?, ?::date, ?)
ON CONFLICT(owner_id, activity_date) DO UPDATE SET
    last_seen_at = GREATEST(user_activity_daily.last_seen_at, excluded.last_seen_at)`, userID, activityDate, at)
	if err != nil {
		return fmt.Errorf("touch user activity: %w", err)
	}
	return nil
}

// ProductAnalyticsSnapshot returns global aggregate counts only. It never
// returns tenant IDs and all funnel labels are intended to be fixed by the
// collector.
func (s *Store) ProductAnalyticsSnapshot(ctx context.Context, now time.Time) (ProductAnalyticsSnapshot, error) {
	if now.IsZero() {
		return ProductAnalyticsSnapshot{}, errors.New("current time is required")
	}
	today := now.UTC().Truncate(24 * time.Hour)
	// Product analytics is refreshed independently by Prometheus, so retention
	// is enforced even when no user makes a new request.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM user_activity_daily WHERE activity_date <= (?::date - 35)`,
		today.Format(time.DateOnly)); err != nil {
		return ProductAnalyticsSnapshot{}, fmt.Errorf("delete expired user activity: %w", err)
	}
	weekStart := today.AddDate(0, 0, -6)
	monthStart := today.AddDate(0, 0, -29)

	var snapshot ProductAnalyticsSnapshot
	err := s.db.QueryRowContext(ctx, `
WITH
active_users AS (
    SELECT
        COUNT(DISTINCT owner_id) FILTER (WHERE activity_date >= ?::date) AS dau,
        COUNT(DISTINCT owner_id) FILTER (WHERE activity_date >= ?::date) AS wau,
        COUNT(DISTINCT owner_id) FILTER (WHERE activity_date >= ?::date) AS mau
    FROM user_activity_daily
),
registered AS (
    SELECT id AS owner_id FROM users
),
max_linked AS (
    SELECT registered.owner_id
    FROM registered
    WHERE EXISTS (
        SELECT 1 FROM max_identity_links
        WHERE max_identity_links.owner_id = registered.owner_id
    )
),
channel_connected AS (
    SELECT max_linked.owner_id
    FROM max_linked
    WHERE EXISTS (
        SELECT 1 FROM channels
        WHERE channels.owner_id = max_linked.owner_id AND channels.active
    )
),
post_created AS (
    SELECT channel_connected.owner_id
    FROM channel_connected
    WHERE EXISTS (
        SELECT 1 FROM posts
        WHERE posts.owner_id = channel_connected.owner_id
    )
),
post_scheduled_or_published AS (
    SELECT post_created.owner_id
    FROM post_created
    WHERE EXISTS (
        SELECT 1 FROM posts
        WHERE posts.owner_id = post_created.owner_id
          AND (
              posts.status IN ('scheduled', 'publishing', 'published')
              OR posts.published_at IS NOT NULL
          )
    )
),
post_published AS (
    SELECT post_scheduled_or_published.owner_id
    FROM post_scheduled_or_published
    WHERE EXISTS (
        SELECT 1 FROM posts
        WHERE posts.owner_id = post_scheduled_or_published.owner_id
          AND posts.published_at IS NOT NULL
    )
),
published_post_media AS (
    SELECT
        posts.id,
        COUNT(post_attachments.id) AS attachment_count,
        COUNT(post_attachments.id) FILTER (WHERE post_attachments.type = 'image') AS image_count,
        COUNT(post_attachments.id) FILTER (WHERE post_attachments.type = 'video') AS video_count
    FROM posts
    LEFT JOIN post_attachments
      ON post_attachments.owner_id = posts.owner_id
     AND post_attachments.post_id = posts.id
     AND post_attachments.processing_status = 'ready'
    WHERE posts.published_at IS NOT NULL
    GROUP BY posts.id
),
published_post_media_totals AS (
    SELECT
        COUNT(*) AS total,
        COUNT(*) FILTER (WHERE attachment_count > 0) AS with_media,
        COUNT(*) FILTER (WHERE attachment_count > 1) AS with_multiple,
        COUNT(*) FILTER (WHERE video_count > 0) AS with_video,
        COUNT(*) FILTER (WHERE image_count > 0 AND video_count > 0) AS with_mixed_media
    FROM published_post_media
)
SELECT
    active_users.dau,
    active_users.wau,
    active_users.mau,
    (SELECT COUNT(*) FROM registered),
    (SELECT COUNT(*) FROM max_linked),
    (SELECT COUNT(*) FROM channel_connected),
    (SELECT COUNT(*) FROM post_created),
    (SELECT COUNT(*) FROM post_scheduled_or_published),
    (SELECT COUNT(*) FROM post_published),
    published_post_media_totals.total,
    published_post_media_totals.with_media,
    published_post_media_totals.with_multiple,
    published_post_media_totals.with_video,
    published_post_media_totals.with_mixed_media
FROM active_users
CROSS JOIN published_post_media_totals`, today.Format(time.DateOnly), weekStart.Format(time.DateOnly), monthStart.Format(time.DateOnly)).Scan(
		&snapshot.DailyActiveUsers,
		&snapshot.WeeklyActiveUsers,
		&snapshot.MonthlyActiveUsers,
		&snapshot.RegisteredUsers,
		&snapshot.MAXLinkedUsers,
		&snapshot.ChannelConnectedUsers,
		&snapshot.PostCreatedUsers,
		&snapshot.PostScheduledOrPublishedUsers,
		&snapshot.PostPublishedUsers,
		&snapshot.PublishedPosts,
		&snapshot.PublishedPostsWithMedia,
		&snapshot.PublishedPostsWithMultiple,
		&snapshot.PublishedPostsWithVideo,
		&snapshot.PublishedPostsWithMixedMedia,
	)
	if err != nil {
		return ProductAnalyticsSnapshot{}, fmt.Errorf("get product analytics: %w", err)
	}
	return snapshot, nil
}
