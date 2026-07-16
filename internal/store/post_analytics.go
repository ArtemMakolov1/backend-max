package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	MaxPostAnalyticsSeriesPoints = 1000

	PostAnalyticsPublicationPublished   = "published"
	PostAnalyticsPublicationRemoved     = "removed"
	PostAnalyticsPublicationUnpublished = "unpublished"

	PostAnalyticsAudienceSnapshotAtPublish = "snapshot_at_publish"
	PostAnalyticsAudienceCurrentChannel    = "current_channel"
	PostAnalyticsAudienceMissing           = "missing"
)

// PostAnalyticsReport is the detailed, workspace-scoped view of one local
// post. A local post can be published more than once, so every metric and
// series point belongs to one selected MAX message ID internally.
type PostAnalyticsReport struct {
	Post    PostAnalyticsPost    `json:"post"`
	Period  AnalyticsPeriod      `json:"period"`
	Summary PostAnalyticsSummary `json:"summary"`
	Series  []PostAnalyticsPoint `json:"series"`
}

type PostAnalyticsPost struct {
	ID               int64      `json:"id"`
	Title            string     `json:"title"`
	ChannelID        *int64     `json:"channel_id"`
	ChannelTitle     string     `json:"channel_title"`
	PublishedAt      *time.Time `json:"published_at"`
	PublicationState string     `json:"publication_state"`
	RemovedFromMAX   bool       `json:"removed_from_max"`
	MAXMessageURL    string     `json:"max_message_url"`
	MAXStatsSyncedAt *time.Time `json:"max_stats_synced_at"`
}

type PostAnalyticsSummary struct {
	Views                *int64     `json:"views"`
	ViewsChange          *int64     `json:"views_change"`
	Audience             *int       `json:"audience"`
	AudienceSource       string     `json:"audience_source"`
	ViewsPer1KAudience   *float64   `json:"views_per_1k_audience"`
	LifetimeViewsPerHour *float64   `json:"lifetime_views_per_hour"`
	ObservedViewsPerHour *float64   `json:"observed_views_per_hour"`
	Observations         int        `json:"observations"`
	FirstObservedAt      *time.Time `json:"first_observed_at"`
	LastObservedAt       *time.Time `json:"last_observed_at"`
	CorrectionDetected   bool       `json:"correction_detected"`
	SeriesTruncated      bool       `json:"series_truncated"`
}

type PostAnalyticsPoint struct {
	CapturedAt           time.Time `json:"captured_at"`
	Views                int64     `json:"views"`
	Delta                *int64    `json:"delta"`
	IntervalViewsPerHour *float64  `json:"interval_views_per_hour"`
	Correction           bool      `json:"correction"`
}

type postAnalyticsObservation struct {
	Views      int64
	CapturedAt time.Time
}

// GetWorkspacePostAnalytics returns an inclusive UTC-day report for one post.
// The active MAX message is preferred when it existed by the period end. For
// earlier periods, the latest historical publication observed by then wins.
func (s *Store) GetWorkspacePostAnalytics(
	ctx context.Context,
	actorUserID, workspaceID string,
	postID int64,
	fromDay, toDay time.Time,
) (PostAnalyticsReport, error) {
	if strings.TrimSpace(actorUserID) == "" || strings.TrimSpace(workspaceID) == "" || postID <= 0 {
		return PostAnalyticsReport{}, errors.New("workspace member, workspace and positive post ID are required")
	}
	if fromDay.IsZero() || toDay.IsZero() {
		return PostAnalyticsReport{}, errors.New("post analytics date range is required")
	}
	fromDay, toDay = utcDate(fromDay), utcDate(toDay)
	if toDay.Before(fromDay) {
		return PostAnalyticsReport{}, errors.New("post analytics end date must not precede start date")
	}
	if days := int(toDay.Sub(fromDay)/(24*time.Hour)) + 1; days > MaxChannelAnalyticsDays {
		return PostAnalyticsReport{}, fmt.Errorf("post analytics range must not exceed %d days", MaxChannelAnalyticsDays)
	}

	post, err := s.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return PostAnalyticsReport{}, err
	}
	report := PostAnalyticsReport{
		Post: PostAnalyticsPost{
			ID: post.ID, Title: post.Title, ChannelID: post.ChannelID,
			PublicationState: PostAnalyticsPublicationUnpublished,
		},
		Period:  AnalyticsPeriod{From: fromDay.Format(time.DateOnly), To: toDay.Format(time.DateOnly)},
		Summary: PostAnalyticsSummary{AudienceSource: PostAnalyticsAudienceMissing},
		Series:  make([]PostAnalyticsPoint, 0),
	}

	var channel *Channel
	if post.ChannelID != nil {
		found, getErr := s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, *post.ChannelID)
		if getErr != nil {
			return PostAnalyticsReport{}, getErr
		}
		channel = &found
		report.Post.ChannelTitle = found.Title
	}
	toExclusive := toDay.AddDate(0, 0, 1)
	currentMessageID := strings.TrimSpace(post.MAXMessageID)
	hasActivePublication := post.Status == PostStatusPublished && currentMessageID != ""
	currentPublicationInPeriod := hasActivePublication && (post.PublishedAt == nil ||
		post.PublishedAt.UTC().Before(toExclusive))
	selectedMessageID := ""
	selectedCurrentPublication := false
	var latest postAnalyticsObservation
	latestKnown := false
	if currentPublicationInPeriod {
		selectedMessageID = currentMessageID
		selectedCurrentPublication = true
		latest, latestKnown, err = s.latestPostAnalyticsObservation(
			ctx, workspaceID, post.UserID, post.ID, selectedMessageID, toExclusive,
		)
	} else {
		selectedMessageID, latest, latestKnown, err = s.latestHistoricalPostAnalyticsPublication(
			ctx, workspaceID, post.UserID, post.ID, toExclusive,
		)
	}
	if err != nil {
		return PostAnalyticsReport{}, err
	}

	// Successful metadata syncs normally create an immutable snapshot in the
	// same transaction. Keep a timestamped metadata fallback for legacy rows
	// created before that invariant existed.
	if !latestKnown && selectedCurrentPublication && post.MAXViews != nil && post.MAXStatsSyncedAt != nil &&
		post.MAXStatsSyncedAt.UTC().Before(toExclusive) {
		latest = postAnalyticsObservation{Views: *post.MAXViews, CapturedAt: post.MAXStatsSyncedAt.UTC()}
		latestKnown = true
	}

	selectedUsesCurrentMetadata := selectedCurrentPublication
	if !selectedCurrentPublication && latestKnown && post.PublishedAt != nil &&
		!post.PublishedAt.UTC().After(latest.CapturedAt) {
		selectedUsesCurrentMetadata = selectedMessageID == currentMessageID ||
			(!hasActivePublication && currentMessageID == "")
	}
	switch {
	case selectedCurrentPublication:
		report.Post.PublicationState = PostAnalyticsPublicationPublished
		report.Post.MAXMessageURL = post.MAXMessageURL
	case selectedMessageID != "":
		report.Post.PublicationState = PostAnalyticsPublicationRemoved
		report.Post.RemovedFromMAX = true
	case post.PublishedAt != nil && !hasActivePublication:
		report.Post.PublicationState = PostAnalyticsPublicationRemoved
		report.Post.RemovedFromMAX = true
	}

	effectivePublishedAt := post.PublishedAt
	effectiveStatsSyncedAt := post.MAXStatsSyncedAt
	if selectedMessageID != "" && !selectedUsesCurrentMetadata {
		effectivePublishedAt = nil
		capturedAt := latest.CapturedAt
		effectiveStatsSyncedAt = &capturedAt
	} else if selectedMessageID == "" && hasActivePublication && !currentPublicationInPeriod {
		// The current publication happened after the requested period and no
		// older publication was observed by its end.
		effectivePublishedAt = nil
		effectiveStatsSyncedAt = nil
	}
	if effectiveStatsSyncedAt != nil && !effectiveStatsSyncedAt.UTC().Before(toExclusive) {
		if latestKnown {
			capturedAt := latest.CapturedAt
			effectiveStatsSyncedAt = &capturedAt
		} else {
			effectiveStatsSyncedAt = nil
		}
	}
	report.Post.PublishedAt = effectivePublishedAt
	report.Post.MAXStatsSyncedAt = effectiveStatsSyncedAt
	audiencePost := post
	audiencePost.PublishedAt = effectivePublishedAt
	if err := s.populatePostAnalyticsAudience(ctx, workspaceID, audiencePost, channel, &report.Summary); err != nil {
		return PostAnalyticsReport{}, err
	}

	if latestKnown {
		views := latest.Views
		report.Summary.Views = &views
		if report.Summary.Audience != nil && *report.Summary.Audience > 0 {
			metric := roundAnalyticsMetric(float64(views) * 1000 / float64(*report.Summary.Audience))
			report.Summary.ViewsPer1KAudience = &metric
		}
		if effectivePublishedAt != nil {
			hours := latest.CapturedAt.Sub(effectivePublishedAt.UTC()).Hours()
			if hours > 0 {
				metric := roundAnalyticsMetric(float64(views) / hours)
				report.Summary.LifetimeViewsPerHour = &metric
			}
		}
	}

	if selectedMessageID == "" {
		return report, nil
	}
	observations, truncated, err := s.listPostAnalyticsObservations(
		ctx, workspaceID, post.UserID, post.ID, selectedMessageID, fromDay, toExclusive,
	)
	if err != nil {
		return PostAnalyticsReport{}, err
	}
	report.Summary.SeriesTruncated = truncated
	report.Series = buildPostAnalyticsSeries(observations, &report.Summary)
	return report, nil
}

func (s *Store) populatePostAnalyticsAudience(
	ctx context.Context,
	workspaceID string,
	post Post,
	channel *Channel,
	summary *PostAnalyticsSummary,
) error {
	if channel == nil {
		return nil
	}
	if post.PublishedAt != nil {
		var audience int
		err := s.db.QueryRowContext(ctx, `
SELECT snapshot.participants_count
FROM channel_participant_snapshots AS snapshot
JOIN channels AS channel ON channel.id=snapshot.channel_id
WHERE channel.workspace_id=? AND channel.id=? AND snapshot.captured_at<=?
ORDER BY snapshot.captured_at DESC,snapshot.observed_on DESC
LIMIT 1`, workspaceID, channel.ID, post.PublishedAt.UTC()).Scan(&audience)
		switch {
		case err == nil:
			summary.Audience = &audience
			summary.AudienceSource = PostAnalyticsAudienceSnapshotAtPublish
			return nil
		case errors.Is(err, sql.ErrNoRows):
		default:
			return fmt.Errorf("get post analytics audience at publish: %w", err)
		}
	}
	audience := channel.ParticipantsCount
	summary.Audience = &audience
	summary.AudienceSource = PostAnalyticsAudienceCurrentChannel
	return nil
}

func (s *Store) latestHistoricalPostAnalyticsPublication(
	ctx context.Context,
	workspaceID string,
	ownerID string,
	postID int64,
	toExclusive time.Time,
) (string, postAnalyticsObservation, bool, error) {
	var messageID string
	var observation postAnalyticsObservation
	err := s.db.QueryRowContext(ctx, `
SELECT max_message_id,views,captured_at
FROM post_view_snapshots
WHERE workspace_id=? AND owner_id=? AND post_id=? AND max_message_id<>'' AND captured_at<?
ORDER BY captured_at DESC,id DESC
LIMIT 1`, workspaceID, ownerID, postID, toExclusive).Scan(
		&messageID, &observation.Views, &observation.CapturedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return "", postAnalyticsObservation{}, false, nil
	}
	if err != nil {
		return "", postAnalyticsObservation{}, false, fmt.Errorf("select historical post analytics publication: %w", err)
	}
	observation.CapturedAt = observation.CapturedAt.UTC()
	return messageID, observation, true, nil
}

func (s *Store) latestPostAnalyticsObservation(
	ctx context.Context,
	workspaceID string,
	ownerID string,
	postID int64,
	messageID string,
	toExclusive time.Time,
) (postAnalyticsObservation, bool, error) {
	var observation postAnalyticsObservation
	err := s.db.QueryRowContext(ctx, `
SELECT views,captured_at
FROM post_view_snapshots
WHERE workspace_id=? AND owner_id=? AND post_id=? AND max_message_id=? AND captured_at<?
ORDER BY captured_at DESC,id DESC
LIMIT 1`, workspaceID, ownerID, postID, messageID, toExclusive).Scan(&observation.Views, &observation.CapturedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return postAnalyticsObservation{}, false, nil
	}
	if err != nil {
		return postAnalyticsObservation{}, false, fmt.Errorf("get latest post analytics observation: %w", err)
	}
	observation.CapturedAt = observation.CapturedAt.UTC()
	return observation, true, nil
}

func (s *Store) listPostAnalyticsObservations(
	ctx context.Context,
	workspaceID string,
	ownerID string,
	postID int64,
	messageID string,
	fromInclusive, toExclusive time.Time,
) ([]postAnalyticsObservation, bool, error) {
	anchor, hasAnchor, err := s.postAnalyticsAnchorObservation(
		ctx, workspaceID, ownerID, postID, messageID, fromInclusive,
	)
	if err != nil {
		return nil, false, err
	}
	seriesCapacity := MaxPostAnalyticsSeriesPoints
	if hasAnchor {
		seriesCapacity--
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT views,captured_at
FROM post_view_snapshots
WHERE workspace_id=? AND owner_id=? AND post_id=? AND max_message_id=?
  AND captured_at>=? AND captured_at<?
ORDER BY captured_at DESC,id DESC
LIMIT ?`, workspaceID, ownerID, postID, messageID, fromInclusive, toExclusive, seriesCapacity+1)
	if err != nil {
		return nil, false, fmt.Errorf("list post analytics observations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]postAnalyticsObservation, 0)
	for rows.Next() {
		var observation postAnalyticsObservation
		if err := rows.Scan(&observation.Views, &observation.CapturedAt); err != nil {
			return nil, false, fmt.Errorf("scan post analytics observation: %w", err)
		}
		observation.CapturedAt = observation.CapturedAt.UTC()
		result = append(result, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list post analytics observations: %w", err)
	}
	bounded, truncated := boundPostAnalyticsObservations(result, seriesCapacity)
	if hasAnchor {
		bounded = append([]postAnalyticsObservation{anchor}, bounded...)
	}
	return bounded, truncated, nil
}

func (s *Store) postAnalyticsAnchorObservation(
	ctx context.Context,
	workspaceID, ownerID string,
	postID int64,
	messageID string,
	fromExclusive time.Time,
) (postAnalyticsObservation, bool, error) {
	var observation postAnalyticsObservation
	err := s.db.QueryRowContext(ctx, `
SELECT views,captured_at
FROM post_view_snapshots
WHERE workspace_id=? AND owner_id=? AND post_id=? AND max_message_id=? AND captured_at<?
ORDER BY captured_at DESC,id DESC
LIMIT 1`, workspaceID, ownerID, postID, messageID, fromExclusive).Scan(
		&observation.Views, &observation.CapturedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return postAnalyticsObservation{}, false, nil
	}
	if err != nil {
		return postAnalyticsObservation{}, false, fmt.Errorf("get post analytics anchor observation: %w", err)
	}
	observation.CapturedAt = observation.CapturedAt.UTC()
	return observation, true, nil
}

// boundPostAnalyticsObservations receives the database's newest-first result,
// drops the one-row truncation sentinel and prepares an ascending chart series.
func boundPostAnalyticsObservations(result []postAnalyticsObservation, limit int) ([]postAnalyticsObservation, bool) {
	truncated := len(result) > limit
	if truncated {
		result = result[:limit]
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result, truncated
}

func buildPostAnalyticsSeries(
	observations []postAnalyticsObservation,
	summary *PostAnalyticsSummary,
) []PostAnalyticsPoint {
	series := make([]PostAnalyticsPoint, 0, len(observations))
	summary.Observations = len(observations)
	if len(observations) == 0 {
		return series
	}
	firstObservedAt := observations[0].CapturedAt
	lastObservedAt := observations[len(observations)-1].CapturedAt
	summary.FirstObservedAt = &firstObservedAt
	summary.LastObservedAt = &lastObservedAt

	for index, observation := range observations {
		point := PostAnalyticsPoint{CapturedAt: observation.CapturedAt, Views: observation.Views}
		if index > 0 {
			previous := observations[index-1]
			delta := observation.Views - previous.Views
			point.Delta = &delta
			point.Correction = delta < 0
			if point.Correction {
				summary.CorrectionDetected = true
			} else if hours := observation.CapturedAt.Sub(previous.CapturedAt).Hours(); hours > 0 {
				rate := roundAnalyticsMetric(float64(delta) / hours)
				point.IntervalViewsPerHour = &rate
			}
		}
		series = append(series, point)
	}

	if len(observations) >= 2 {
		change := observations[len(observations)-1].Views - observations[0].Views
		summary.ViewsChange = &change
		if hours := observations[len(observations)-1].CapturedAt.Sub(observations[0].CapturedAt).Hours(); change >= 0 && !summary.CorrectionDetected && hours > 0 {
			rate := roundAnalyticsMetric(float64(change) / hours)
			summary.ObservedViewsPerHour = &rate
		}
	}
	return series
}
