package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const MaxAnalyticsTimezoneOffsetMinutes = 14 * 60

// AnalyticsContentReport is the workspace-scoped analytics-to-content view.
// It keeps missing upstream counters nullable and makes the timezone used for
// weekday/hour buckets explicit.
type AnalyticsContentReport struct {
	Scope                 AnalyticsContentScope   `json:"scope"`
	Channel               ChannelAnalyticsChannel `json:"channel"`
	Period                AnalyticsPeriod         `json:"period"`
	Summary               AnalyticsContentSummary `json:"summary"`
	Daily                 []AnalyticsDailyPoint   `json:"daily"`
	Posts                 []AnalyticsContentPost  `json:"posts"`
	Heatmap               []AnalyticsHeatmapCell  `json:"heatmap"`
	BestTime              *AnalyticsBestTime      `json:"best_time"`
	TimezoneOffsetMinutes int                     `json:"timezone_offset_minutes"`
}

type AnalyticsContentScope struct {
	Kind          string `json:"kind"`
	ChannelID     *int64 `json:"channel_id"`
	ChannelTitle  string `json:"channel_title,omitempty"`
	ChannelsCount int    `json:"channels_count"`
}

type AnalyticsContentSummary struct {
	PostsTotal                   int64      `json:"posts_total"`
	PublishedPosts               int64      `json:"published_posts"`
	PublishedLast24H             int64      `json:"published_last_24h"`
	KnownViewsPosts              int        `json:"known_views_posts"`
	TotalViews                   *int64     `json:"total_views"`
	AverageReach                 *float64   `json:"average_reach"`
	ParticipantsCurrent          *int       `json:"participants_current"`
	ParticipantsChange           *int       `json:"participants_change"`
	ParticipantsChange24H        *int       `json:"participants_change_24h"`
	ParticipantsChange24HPercent *float64   `json:"participants_change_24h_percent"`
	ViewsChange                  *int64     `json:"views_change"`
	ViewsPer1KAudience           *float64   `json:"views_per_1k_audience"`
	AverageViewsPerHour          *float64   `json:"average_views_per_hour"`
	ERR24H                       *float64   `json:"err_24h"`
	ERR48H                       *float64   `json:"err_48h"`
	ERR30D                       *float64   `json:"err_30d"`
	ERR24HSample                 int        `json:"err_24h_sample"`
	ERR48HSample                 int        `json:"err_48h_sample"`
	ERR30DSample                 int        `json:"err_30d_sample"`
	PostsPerDay                  *float64   `json:"posts_per_day"`
	LastPublishedAt              *time.Time `json:"last_published_at"`
}

type AnalyticsContentPost struct {
	ID                 int64      `json:"id"`
	Title              string     `json:"title"`
	ChannelID          int64      `json:"channel_id"`
	ChannelTitle       string     `json:"channel_title"`
	Audience           int        `json:"audience"`
	PublishedAt        *time.Time `json:"published_at"`
	Views              *int64     `json:"views"`
	ViewsPer1KAudience *float64   `json:"views_per_1k_audience"`
	ViewsPerHour       *float64   `json:"views_per_hour"`
	Score              *float64   `json:"score"`
	MAXMessageURL      string     `json:"max_message_url,omitempty"`
	MAXStatsSyncedAt   *time.Time `json:"max_stats_synced_at"`
	PublicationState   string     `json:"publication_state"`
	RemovedFromMAX     bool       `json:"removed_from_max"`
	maxMessageID       string
}

type AnalyticsHeatmapCell struct {
	Weekday            int      `json:"weekday"`
	Hour               int      `json:"hour"`
	Posts              int      `json:"posts"`
	ViewsPer1KAudience *float64 `json:"views_per_1k_audience"`
	ViewsPerHour       *float64 `json:"views_per_hour"`
	Score              *float64 `json:"score"`
}

type AnalyticsBestTime struct {
	Weekday            int       `json:"weekday"`
	Hour               int       `json:"hour"`
	SampleSize         int       `json:"sample_size"`
	ViewsPer1KAudience *float64  `json:"views_per_1k_audience"`
	ViewsPerHour       *float64  `json:"views_per_hour"`
	Score              float64   `json:"score"`
	NextAt             time.Time `json:"next_at"`
}

// GetWorkspaceAnalyticsContent returns current-channel or all-channel
// analytics for one workspace membership. tzOffsetMinutes is local minus UTC;
// published timestamps are shifted by it before weekday/hour bucketing.
func (s *Store) GetWorkspaceAnalyticsContent(
	ctx context.Context,
	actorUserID, workspaceID string,
	channelID *int64,
	fromDay, toDay, asOf time.Time,
	tzOffsetMinutes int,
) (AnalyticsContentReport, error) {
	if strings.TrimSpace(actorUserID) == "" || strings.TrimSpace(workspaceID) == "" {
		return AnalyticsContentReport{}, errors.New("workspace member and workspace are required")
	}
	if fromDay.IsZero() || toDay.IsZero() || asOf.IsZero() {
		return AnalyticsContentReport{}, errors.New("analytics date range and current time are required")
	}
	if tzOffsetMinutes < -MaxAnalyticsTimezoneOffsetMinutes || tzOffsetMinutes > MaxAnalyticsTimezoneOffsetMinutes {
		return AnalyticsContentReport{}, errors.New("analytics timezone offset is out of range")
	}
	fromDay, toDay, asOf = utcDate(fromDay), utcDate(toDay), asOf.UTC()
	if toDay.Before(fromDay) {
		return AnalyticsContentReport{}, errors.New("analytics end date must not precede start date")
	}
	if days := int(toDay.Sub(fromDay)/(24*time.Hour)) + 1; days > MaxChannelAnalyticsDays {
		return AnalyticsContentReport{}, fmt.Errorf("analytics range must not exceed %d days", MaxChannelAnalyticsDays)
	}
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return AnalyticsContentReport{}, err
	}

	channels, err := s.ListChannelsForWorkspace(ctx, actorUserID, workspaceID)
	if err != nil {
		return AnalyticsContentReport{}, err
	}
	var selected *Channel
	if channelID != nil {
		if *channelID <= 0 {
			return AnalyticsContentReport{}, errors.New("channel ID must be positive")
		}
		channel, getErr := s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, *channelID)
		if getErr != nil {
			return AnalyticsContentReport{}, getErr
		}
		selected = &channel
	}

	var audienceTotal int
	for _, channel := range channels {
		if selected == nil || channel.ID == selected.ID {
			audienceTotal += channel.ParticipantsCount
		}
	}
	channelsCount := len(channels)
	scope := AnalyticsContentScope{Kind: "workspace", ChannelsCount: channelsCount}
	channelSummary := ChannelAnalyticsChannel{Title: "Все каналы", ParticipantsCount: audienceTotal}
	if selected != nil {
		id := selected.ID
		scope = AnalyticsContentScope{
			Kind: "channel", ChannelID: &id, ChannelTitle: selected.Title, ChannelsCount: 1,
		}
		channelSummary = ChannelAnalyticsChannel{
			ID: selected.ID, Title: selected.Title, IconURL: selected.IconURL,
			ParticipantsCount: selected.ParticipantsCount,
		}
	}

	report := AnalyticsContentReport{
		Scope:                 scope,
		Channel:               channelSummary,
		Period:                AnalyticsPeriod{From: fromDay.Format(time.DateOnly), To: toDay.Format(time.DateOnly)},
		Daily:                 make([]AnalyticsDailyPoint, 0),
		Posts:                 make([]AnalyticsContentPost, 0),
		Heatmap:               make([]AnalyticsHeatmapCell, 0, 7*24),
		TimezoneOffsetMinutes: tzOffsetMinutes,
	}
	currentAudience := audienceTotal
	report.Summary.ParticipantsCurrent = &currentAudience
	toExclusive := toDay.AddDate(0, 0, 1)
	analysisEnd := asOf
	if toExclusive.Before(analysisEnd) {
		analysisEnd = toExclusive
	}
	metricFrom := asOf.AddDate(0, 0, -30)
	queryFrom := fromDay
	if metricFrom.Before(queryFrom) {
		queryFrom = metricFrom
	}

	countQuery := `SELECT COUNT(*) FROM posts WHERE workspace_id=? AND created_at>=? AND created_at<?`
	countArgs := []any{workspaceID, fromDay, toExclusive}
	if selected != nil {
		countQuery += ` AND channel_id=?`
		countArgs = append(countArgs, selected.ID)
	}
	if err := s.db.QueryRowContext(ctx, countQuery, countArgs...).Scan(&report.Summary.PostsTotal); err != nil {
		return AnalyticsContentReport{}, fmt.Errorf("count workspace analytics posts: %w", err)
	}

	postQuery := `
SELECT post.id, post.title, post.channel_id, channel.title,
       COALESCE(audience_at_publish.participants_count,channel.participants_count),
       post.published_at,
       COALESCE(latest.views, CASE WHEN post.max_stats_synced_at < ? THEN post.max_views END),
       post.max_message_url,
       COALESCE(latest.captured_at, CASE WHEN post.max_stats_synced_at < ? THEN post.max_stats_synced_at END),
       CASE WHEN post.status = ? AND post.max_message_id <> '' THEN 'published' ELSE 'removed' END,
       COALESCE(latest.max_message_id,post.max_message_id)
FROM posts AS post
JOIN channels AS channel ON channel.workspace_id=post.workspace_id AND channel.id=post.channel_id
LEFT JOIN LATERAL (
    SELECT snapshot.participants_count
    FROM channel_participant_snapshots AS snapshot
    WHERE snapshot.channel_id=channel.id AND snapshot.captured_at<=post.published_at
    ORDER BY snapshot.captured_at DESC,snapshot.observed_on DESC
    LIMIT 1
) AS audience_at_publish ON TRUE
LEFT JOIN LATERAL (
    SELECT snapshot.max_message_id, snapshot.views, snapshot.captured_at
    FROM post_view_snapshots AS snapshot
    WHERE snapshot.owner_id=post.owner_id AND snapshot.post_id=post.id
      AND snapshot.captured_at < ?
      AND (post.max_message_id='' OR snapshot.max_message_id=post.max_message_id)
    ORDER BY snapshot.captured_at DESC, snapshot.id DESC
    LIMIT 1
) AS latest ON TRUE
WHERE post.workspace_id=? AND post.published_at>=? AND post.published_at<?`
	postArgs := []any{toExclusive, toExclusive, PostStatusPublished, toExclusive, workspaceID, queryFrom, toExclusive}
	if selected != nil {
		postQuery += ` AND post.channel_id=?`
		postArgs = append(postArgs, selected.ID)
	}
	postQuery += ` ORDER BY post.published_at DESC,post.id DESC`
	rows, err := s.db.QueryContext(ctx, postQuery, postArgs...)
	if err != nil {
		return AnalyticsContentReport{}, fmt.Errorf("list workspace analytics posts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var totalViews int64
	var normalizedViews int64
	var audienceExposure int64
	var viewsPerHourTotal float64
	var viewsPerHourCount int
	metricPosts := make([]AnalyticsContentPost, 0)
	for rows.Next() {
		var post AnalyticsContentPost
		var publishedAt, syncedAt sql.NullTime
		var views sql.NullInt64
		if err := rows.Scan(
			&post.ID, &post.Title, &post.ChannelID, &post.ChannelTitle, &post.Audience,
			&publishedAt, &views, &post.MAXMessageURL, &syncedAt, &post.PublicationState,
			&post.maxMessageID,
		); err != nil {
			return AnalyticsContentReport{}, fmt.Errorf("scan workspace analytics post: %w", err)
		}
		post.PublishedAt = parseNullableTime(publishedAt)
		post.MAXStatsSyncedAt = parseNullableTime(syncedAt)
		post.RemovedFromMAX = post.PublicationState == "removed"
		if post.RemovedFromMAX {
			post.MAXMessageURL = ""
		}
		if views.Valid {
			value := views.Int64
			post.Views = &value
			if post.Audience > 0 {
				normalized := roundAnalyticsMetric(float64(value) * 1000 / float64(post.Audience))
				post.ViewsPer1KAudience = &normalized
			}
			if post.PublishedAt != nil {
				ageHours := analysisEnd.Sub(post.PublishedAt.UTC()).Hours()
				if ageHours < 1 {
					ageHours = 1
				}
				rate := roundAnalyticsMetric(float64(value) / ageHours)
				post.ViewsPerHour = &rate
			}
			if score, ok := analyticsContentScore(post.ViewsPer1KAudience, post.ViewsPerHour); ok {
				rounded := roundAnalyticsMetric(score)
				post.Score = &rounded
			}
		}
		if post.PublishedAt != nil && !post.PublishedAt.Before(metricFrom) && !post.PublishedAt.After(asOf) {
			metricPosts = append(metricPosts, post)
		}
		if post.PublishedAt == nil || post.PublishedAt.Before(fromDay) || !post.PublishedAt.Before(toExclusive) {
			continue
		}
		if post.Views != nil {
			totalViews += *post.Views
			report.Summary.KnownViewsPosts++
			if post.Audience > 0 {
				normalizedViews += *post.Views
				audienceExposure += int64(post.Audience)
			}
			if post.ViewsPerHour != nil {
				viewsPerHourTotal += *post.ViewsPerHour
				viewsPerHourCount++
			}
		}
		if !post.PublishedAt.Before(asOf.Add(-24*time.Hour)) && !post.PublishedAt.After(asOf) {
			report.Summary.PublishedLast24H++
		}
		if report.Summary.LastPublishedAt == nil || post.PublishedAt.After(*report.Summary.LastPublishedAt) {
			lastPublishedAt := post.PublishedAt.UTC()
			report.Summary.LastPublishedAt = &lastPublishedAt
		}
		report.Posts = append(report.Posts, post)
	}
	if err := rows.Err(); err != nil {
		return AnalyticsContentReport{}, fmt.Errorf("list workspace analytics posts: %w", err)
	}
	report.Summary.PublishedPosts = int64(len(report.Posts))
	if report.Summary.KnownViewsPosts > 0 {
		report.Summary.TotalViews = &totalViews
		averageReach := roundAnalyticsMetric(float64(totalViews) / float64(report.Summary.KnownViewsPosts))
		report.Summary.AverageReach = &averageReach
	}
	if audienceExposure > 0 {
		metric := roundAnalyticsMetric(float64(normalizedViews) * 1000 / float64(audienceExposure))
		report.Summary.ViewsPer1KAudience = &metric
	}
	if viewsPerHourCount > 0 {
		metric := roundAnalyticsMetric(viewsPerHourTotal / float64(viewsPerHourCount))
		report.Summary.AverageViewsPerHour = &metric
	}
	activityToDay := toDay
	if currentDay := utcDate(asOf); currentDay.Before(activityToDay) {
		activityToDay = currentDay
	}
	if !activityToDay.Before(fromDay) {
		activityDays := int(activityToDay.Sub(fromDay)/(24*time.Hour)) + 1
		postsPerDay := roundAnalyticsMetric(float64(report.Summary.PublishedPosts) / float64(activityDays))
		report.Summary.PostsPerDay = &postsPerDay
	}

	reachObservations, err := s.listWorkspaceAnalyticsReachObservations(
		ctx, workspaceID, channelID, metricFrom, asOf,
	)
	if err != nil {
		return AnalyticsContentReport{}, err
	}
	applyAnalyticsERR(&report.Summary, metricPosts, reachObservations, asOf)

	observations, err := s.listWorkspaceAnalyticsViewObservations(
		ctx, workspaceID, channelID, fromDay, toExclusive,
	)
	if err != nil {
		return AnalyticsContentReport{}, err
	}
	participantHistory, err := s.listWorkspaceAnalyticsParticipantHistory(
		ctx, workspaceID, channelID, fromDay, toDay,
	)
	if err != nil {
		return AnalyticsContentReport{}, err
	}
	participantBaseline, err := s.latestWorkspaceAnalyticsParticipantSnapshotBefore(
		ctx, workspaceID, channelID, fromDay,
	)
	if err != nil {
		return AnalyticsContentReport{}, err
	}
	report.Daily, report.Summary.ViewsChange = buildAnalyticsDaily(
		observations, participantHistory, participantBaseline,
	)
	if selected != nil && len(participantHistory) > 0 {
		latestSnapshot := participantHistory[len(participantHistory)-1]
		latest := latestSnapshot.ParticipantsCount
		report.Summary.ParticipantsCurrent = &latest
		if participantBaseline != nil {
			change := latest - participantBaseline.ParticipantsCount
			report.Summary.ParticipantsChange = &change
		} else if len(participantHistory) >= 2 {
			change := latest - participantHistory[0].ParticipantsCount
			report.Summary.ParticipantsChange = &change
		}
		var previous *ChannelParticipantSnapshot
		if len(participantHistory) >= 2 {
			previous = &participantHistory[len(participantHistory)-2]
		} else {
			previous = participantBaseline
		}
		if previous != nil && latestSnapshot.ObservedOn == activityToDay.Format(time.DateOnly) &&
			analyticsDaysAreConsecutive(previous.ObservedOn, latestSnapshot.ObservedOn) {
			change := latest - previous.ParticipantsCount
			report.Summary.ParticipantsChange24H = &change
			if previous.ParticipantsCount > 0 {
				percent := roundAnalyticsMetric(float64(change) * 100 / float64(previous.ParticipantsCount))
				report.Summary.ParticipantsChange24HPercent = &percent
			}
		}
	}

	report.Heatmap, report.BestTime = buildAnalyticsContentHeatmap(
		report.Posts, asOf, tzOffsetMinutes,
	)
	return report, nil
}

func (s *Store) listWorkspaceAnalyticsViewObservations(
	ctx context.Context,
	workspaceID string,
	channelID *int64,
	fromDay, toExclusive time.Time,
) ([]analyticsViewObservation, error) {
	query := `
SELECT snapshot.post_id,snapshot.max_message_id,snapshot.views,snapshot.captured_at
FROM post_view_snapshots AS snapshot
JOIN posts AS post ON post.owner_id=snapshot.owner_id AND post.id=snapshot.post_id
WHERE post.workspace_id=? AND post.published_at>=? AND post.published_at<?
  AND snapshot.captured_at>=? AND snapshot.captured_at<?
  AND (post.max_message_id='' OR snapshot.max_message_id=post.max_message_id)`
	args := []any{workspaceID, fromDay, toExclusive, fromDay, toExclusive}
	if channelID != nil {
		query += ` AND post.channel_id=?`
		args = append(args, *channelID)
	}
	query += ` ORDER BY snapshot.captured_at,snapshot.id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workspace analytics view observations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]analyticsViewObservation, 0)
	for rows.Next() {
		var observation analyticsViewObservation
		if err := rows.Scan(&observation.PostID, &observation.MAXMessageID, &observation.Views, &observation.CapturedAt); err != nil {
			return nil, fmt.Errorf("scan workspace analytics view observation: %w", err)
		}
		observation.CapturedAt = observation.CapturedAt.UTC()
		result = append(result, observation)
	}
	return result, rows.Err()
}

type analyticsReachObservations map[analyticsPublicationKey][]analyticsViewObservation

func (s *Store) listWorkspaceAnalyticsReachObservations(
	ctx context.Context,
	workspaceID string,
	channelID *int64,
	publishedFrom, asOf time.Time,
) (analyticsReachObservations, error) {
	query := `
SELECT snapshot.post_id,snapshot.max_message_id,snapshot.views,snapshot.captured_at
FROM post_view_snapshots AS snapshot
JOIN posts AS post ON post.owner_id=snapshot.owner_id AND post.id=snapshot.post_id
WHERE post.workspace_id=? AND post.published_at>=? AND post.published_at<=?
  AND snapshot.captured_at>=post.published_at AND snapshot.captured_at<=?`
	args := []any{workspaceID, publishedFrom, asOf, asOf}
	if channelID != nil {
		query += ` AND post.channel_id=?`
		args = append(args, *channelID)
	}
	query += ` ORDER BY snapshot.captured_at,snapshot.id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workspace ERR observations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(analyticsReachObservations)
	for rows.Next() {
		var observation analyticsViewObservation
		if err := rows.Scan(
			&observation.PostID, &observation.MAXMessageID, &observation.Views, &observation.CapturedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workspace ERR observation: %w", err)
		}
		observation.CapturedAt = observation.CapturedAt.UTC()
		key := analyticsPublicationKey{PostID: observation.PostID, MAXMessageID: observation.MAXMessageID}
		result[key] = append(result[key], observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace ERR observations: %w", err)
	}
	return result, nil
}

func applyAnalyticsERR(
	summary *AnalyticsContentSummary,
	posts []AnalyticsContentPost,
	observations analyticsReachObservations,
	asOf time.Time,
) {
	if summary == nil {
		return
	}
	const observationTolerance = 2 * time.Hour
	var err24Total, err48Total, err30Total float64
	for _, post := range posts {
		if post.PublishedAt == nil || post.Audience <= 0 {
			continue
		}
		if post.Views != nil {
			err30Total += float64(*post.Views) * 100 / float64(post.Audience)
			summary.ERR30DSample++
		}
		key := analyticsPublicationKey{PostID: post.ID, MAXMessageID: post.maxMessageID}
		publicationObservations := observations[key]
		target24H := post.PublishedAt.Add(24 * time.Hour)
		if !target24H.After(asOf) {
			if views, ok := closestAnalyticsReach(publicationObservations, target24H, observationTolerance); ok {
				err24Total += float64(views) * 100 / float64(post.Audience)
				summary.ERR24HSample++
			}
		}
		target48H := post.PublishedAt.Add(48 * time.Hour)
		if !target48H.After(asOf) {
			if views, ok := closestAnalyticsReach(publicationObservations, target48H, observationTolerance); ok {
				err48Total += float64(views) * 100 / float64(post.Audience)
				summary.ERR48HSample++
			}
		}
	}
	if summary.ERR24HSample > 0 {
		value := roundAnalyticsMetric(err24Total / float64(summary.ERR24HSample))
		summary.ERR24H = &value
	}
	if summary.ERR48HSample > 0 {
		value := roundAnalyticsMetric(err48Total / float64(summary.ERR48HSample))
		summary.ERR48H = &value
	}
	if summary.ERR30DSample > 0 {
		value := roundAnalyticsMetric(err30Total / float64(summary.ERR30DSample))
		summary.ERR30D = &value
	}
}

func closestAnalyticsReach(
	observations []analyticsViewObservation,
	target time.Time,
	tolerance time.Duration,
) (int64, bool) {
	var selected int64
	closestDistance := tolerance + time.Nanosecond
	found := false
	for _, observation := range observations {
		distance := observation.CapturedAt.Sub(target)
		if distance < 0 {
			distance = -distance
		}
		if distance > tolerance || distance >= closestDistance {
			continue
		}
		selected = observation.Views
		closestDistance = distance
		found = true
	}
	return selected, found
}

func (s *Store) listWorkspaceAnalyticsParticipantHistory(
	ctx context.Context,
	workspaceID string,
	channelID *int64,
	fromDay, toDay time.Time,
) ([]ChannelParticipantSnapshot, error) {
	// A sum across channels would be misleading on days where only some
	// channels were observed. Keep the audience trend channel-specific while
	// still returning current aggregate audience for the all-channel scope.
	if channelID == nil {
		return []ChannelParticipantSnapshot{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT snapshot.observed_on,snapshot.participants_count,snapshot.captured_at
FROM channel_participant_snapshots AS snapshot
JOIN channels AS channel ON channel.id=snapshot.channel_id
WHERE channel.workspace_id=? AND channel.id=?
  AND snapshot.observed_on>=? AND snapshot.observed_on<=?
ORDER BY snapshot.observed_on,snapshot.captured_at`,
		workspaceID, *channelID, fromDay.Format(time.DateOnly), toDay.Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("list workspace analytics participant history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]ChannelParticipantSnapshot, 0)
	for rows.Next() {
		var snapshot ChannelParticipantSnapshot
		var observedOn time.Time
		if err := rows.Scan(&observedOn, &snapshot.ParticipantsCount, &snapshot.CapturedAt); err != nil {
			return nil, fmt.Errorf("scan workspace analytics participant history: %w", err)
		}
		snapshot.ObservedOn = observedOn.UTC().Format(time.DateOnly)
		snapshot.CapturedAt = snapshot.CapturedAt.UTC()
		result = append(result, snapshot)
	}
	return result, rows.Err()
}

func (s *Store) latestWorkspaceAnalyticsParticipantSnapshotBefore(
	ctx context.Context,
	workspaceID string,
	channelID *int64,
	before time.Time,
) (*ChannelParticipantSnapshot, error) {
	if channelID == nil {
		return nil, nil
	}
	var snapshot ChannelParticipantSnapshot
	var observedOn time.Time
	err := s.db.QueryRowContext(ctx, `
SELECT history.observed_on,history.participants_count,history.captured_at
FROM channel_participant_snapshots AS history
JOIN channels AS channel ON channel.id=history.channel_id
WHERE channel.workspace_id=? AND channel.id=? AND history.observed_on<?
ORDER BY history.observed_on DESC,history.captured_at DESC
LIMIT 1`, workspaceID, *channelID, before.Format(time.DateOnly)).Scan(
		&observedOn, &snapshot.ParticipantsCount, &snapshot.CapturedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace analytics participant baseline: %w", err)
	}
	snapshot.ObservedOn = observedOn.UTC().Format(time.DateOnly)
	snapshot.CapturedAt = snapshot.CapturedAt.UTC()
	return &snapshot, nil
}

type analyticsHeatmapAccumulator struct {
	posts            int
	viewsPer1K       float64
	viewsPer1KRows   int
	viewsPerHour     float64
	viewsPerHourRows int
}

func buildAnalyticsContentHeatmap(
	posts []AnalyticsContentPost,
	asOf time.Time,
	tzOffsetMinutes int,
) ([]AnalyticsHeatmapCell, *AnalyticsBestTime) {
	accumulators := make([]analyticsHeatmapAccumulator, 7*24)
	offset := time.Duration(tzOffsetMinutes) * time.Minute
	for _, post := range posts {
		if post.PublishedAt == nil {
			continue
		}
		local := post.PublishedAt.UTC().Add(offset)
		weekday := (int(local.Weekday()) + 6) % 7 // Monday=0.
		index := weekday*24 + local.Hour()
		accumulator := &accumulators[index]
		accumulator.posts++
		if post.ViewsPer1KAudience != nil {
			accumulator.viewsPer1K += *post.ViewsPer1KAudience
			accumulator.viewsPer1KRows++
		}
		if post.ViewsPerHour != nil {
			accumulator.viewsPerHour += *post.ViewsPerHour
			accumulator.viewsPerHourRows++
		}
	}

	cells := make([]AnalyticsHeatmapCell, 0, 7*24)
	bestIndex := -1
	bestScore := -1.0
	for weekday := 0; weekday < 7; weekday++ {
		for hour := 0; hour < 24; hour++ {
			index := weekday*24 + hour
			accumulator := accumulators[index]
			cell := AnalyticsHeatmapCell{Weekday: weekday, Hour: hour, Posts: accumulator.posts}
			if accumulator.viewsPer1KRows > 0 {
				metric := roundAnalyticsMetric(accumulator.viewsPer1K / float64(accumulator.viewsPer1KRows))
				cell.ViewsPer1KAudience = &metric
			}
			if accumulator.viewsPerHourRows > 0 {
				metric := roundAnalyticsMetric(accumulator.viewsPerHour / float64(accumulator.viewsPerHourRows))
				cell.ViewsPerHour = &metric
			}
			if score, ok := analyticsContentScore(cell.ViewsPer1KAudience, cell.ViewsPerHour); ok {
				rounded := roundAnalyticsMetric(score)
				cell.Score = &rounded
			}
			cells = append(cells, cell)

			score := -1.0
			if cell.Score != nil {
				score = *cell.Score
			}
			if score > bestScore || (score == bestScore && bestIndex >= 0 && cell.Posts > cells[bestIndex].Posts) {
				bestIndex, bestScore = len(cells)-1, score
			}
		}
	}
	if bestIndex < 0 || bestScore < 0 {
		return cells, nil
	}
	bestCell := cells[bestIndex]
	best := &AnalyticsBestTime{
		Weekday:            bestCell.Weekday,
		Hour:               bestCell.Hour,
		SampleSize:         bestCell.Posts,
		ViewsPer1KAudience: bestCell.ViewsPer1KAudience,
		ViewsPerHour:       bestCell.ViewsPerHour,
		Score:              bestScore,
		NextAt:             nextAnalyticsContentSlot(asOf.UTC(), tzOffsetMinutes, bestCell.Weekday, bestCell.Hour),
	}
	return cells, best
}

func nextAnalyticsContentSlot(asOf time.Time, tzOffsetMinutes, weekday, hour int) time.Time {
	offset := time.Duration(tzOffsetMinutes) * time.Minute
	localNow := asOf.UTC().Add(offset)
	localWeekday := (int(localNow.Weekday()) + 6) % 7
	daysAhead := (weekday - localWeekday + 7) % 7
	localCandidate := time.Date(
		localNow.Year(), localNow.Month(), localNow.Day()+daysAhead,
		hour, 0, 0, 0, time.UTC,
	)
	if !localCandidate.After(localNow) {
		localCandidate = localCandidate.AddDate(0, 0, 7)
	}
	return localCandidate.Add(-offset).UTC()
}

func roundAnalyticsMetric(value float64) float64 {
	return math.Round(value*100) / 100
}

func analyticsContentScore(viewsPer1K, viewsPerHour *float64) (float64, bool) {
	if viewsPer1K != nil && viewsPerHour != nil {
		if *viewsPer1K < 0 || *viewsPerHour < 0 {
			return 0, false
		}
		// Geometric mean keeps both audience efficiency and early velocity in
		// the recommendation without allowing either scale to dominate.
		return math.Sqrt(*viewsPer1K * *viewsPerHour), true
	}
	return 0, false
}

// CreateAnalyticsContentDraft creates a true draft derived from an existing
// workspace post. It intentionally never schedules or approves the copy.
func (s *Store) CreateAnalyticsContentDraft(
	ctx context.Context,
	actorUserID, workspaceID string,
	postID int64,
	kind string,
) (Post, error) {
	if kind != "variation" && kind != "repeat" {
		return Post{}, errors.New("analytics draft kind must be variation or repeat")
	}
	draft, err := s.DuplicatePostForWorkspace(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return Post{}, err
	}
	// Keep this invariant close to the analytics action even if the generic
	// duplicate implementation changes later.
	if draft.Status != PostStatusDraft || draft.ScheduledAt != nil || draft.PublishedAt != nil {
		return Post{}, fmt.Errorf("%w: analytics copy was not created as a draft", ErrConflict)
	}
	return draft, nil
}

// CreateAnalyticsRepeatPlan persists a one-variant campaign and materializes
// it into a linked draft. plannedAt therefore survives navigation and appears
// in the campaign calendar; no schedule or approval transition happens here.
func (s *Store) CreateAnalyticsRepeatPlan(
	ctx context.Context,
	actorUserID, workspaceID string,
	postID int64,
	plannedAt time.Time,
) (Post, Campaign, error) {
	plannedAt = plannedAt.UTC()
	if plannedAt.IsZero() {
		return Post{}, Campaign{}, errors.New("repeat planned time is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Post{}, Campaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Post{}, Campaign{}, err
	}
	now := time.Now().UTC()
	if !plannedAt.After(now) {
		return Post{}, Campaign{}, errors.New("planned_at must be in the future")
	}

	draftID, title, content, format, channelID, err := duplicateAnalyticsRepeatDraftTx(
		ctx, tx, actorUserID, workspaceID, postID, now,
	)
	if err != nil {
		return Post{}, Campaign{}, err
	}
	if !plannedAt.After(time.Now().UTC()) {
		return Post{}, Campaign{}, errors.New("planned_at must be in the future")
	}
	var channelActive bool
	if err := tx.QueryRowContext(ctx, `SELECT active FROM channels
WHERE workspace_id=$1 AND id=$2`, workspaceID, channelID).Scan(&channelActive); errors.Is(err, sql.ErrNoRows) {
		return Post{}, Campaign{}, ErrNotFound
	} else if err != nil {
		return Post{}, Campaign{}, err
	} else if !channelActive {
		return Post{}, Campaign{}, errors.New("campaign channel is inactive")
	}

	campaignID := newStoreID("cmp_")
	variantID := newStoreID("cv_")
	name := "Повторить в удачное время"
	description := fmt.Sprintf("Повтор публикации %d, созданный из аналитики", postID)
	variant := CampaignVariant{
		ID: variantID, WorkspaceID: workspaceID, CampaignID: campaignID,
		ChannelID: channelID, PostID: &draftID, Title: title, Content: content,
		Format: format, PlannedAt: plannedAt, Status: "materialized", CreatedBy: actorUserID,
	}
	if err := validateCampaignVariant(variant); err != nil {
		return Post{}, Campaign{}, fmt.Errorf("repeat campaign source draft: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO campaigns(
id,workspace_id,name,description,status,created_by,created_at,updated_at)
VALUES($1,$2,$3,$4,'active',$5,$6,$6)`, campaignID, workspaceID,
		name, description, actorUserID, now); err != nil {
		return Post{}, Campaign{}, mapWorkspaceWriteError("create analytics repeat campaign", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO campaign_variants(
id,workspace_id,campaign_id,channel_id,post_id,title,content,format,planned_at,status,created_by,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'materialized',$10,$11,$11)`, variantID,
		workspaceID, campaignID, channelID, draftID, title, content, format,
		plannedAt, actorUserID, now); err != nil {
		return Post{}, Campaign{}, mapWorkspaceWriteError("link analytics repeat draft", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "campaign.created_from_draft",
		EntityType: "campaign", EntityID: campaignID,
		Metadata: mustJSON(map[string]any{
			"post_id": draftID, "variant_id": variantID, "planned_at": plannedAt,
		}), CreatedAt: now,
	}); err != nil {
		return Post{}, Campaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return Post{}, Campaign{}, err
	}

	draft, err := s.GetPostForWorkspace(ctx, actorUserID, workspaceID, draftID)
	if err != nil {
		return Post{}, Campaign{}, err
	}
	campaign, err := s.GetCampaign(ctx, actorUserID, workspaceID, campaignID)
	if err != nil {
		return Post{}, Campaign{}, err
	}
	if len(campaign.Variants) != 1 || campaign.Variants[0].PostID == nil ||
		*campaign.Variants[0].PostID != draft.ID {
		return Post{}, Campaign{}, fmt.Errorf("%w: repeat campaign was not materialized", ErrConflict)
	}
	if draft.Status != PostStatusDraft || draft.ScheduledAt != nil || draft.PublishedAt != nil {
		return Post{}, Campaign{}, fmt.Errorf("%w: repeat plan bypassed the draft workflow", ErrConflict)
	}
	return draft, campaign, nil
}

func duplicateAnalyticsRepeatDraftTx(
	ctx context.Context,
	tx *sql.Tx,
	actorUserID, workspaceID string,
	sourcePostID int64,
	now time.Time,
) (int64, string, string, string, int64, error) {
	var draftID int64
	var title, content, format string
	var channelID sql.NullInt64
	err := tx.QueryRowContext(ctx, `INSERT INTO posts(
owner_id,workspace_id,title,content,format,status,channel_id,image_url,image_path,image_prompt,link_buttons,
notify,disable_link_preview,scheduled_at,max_message_id,max_message_url,max_views,max_stats_synced_at,
max_is_pinned,last_error,published_at,created_at,updated_at)
SELECT owner_id,workspace_id,trim(title || ' (копия)'),content,format,$1,channel_id,image_url,image_path,image_prompt,link_buttons,
       notify,disable_link_preview,NULL,'','',NULL,NULL,FALSE,'',NULL,$2,$2
FROM posts WHERE workspace_id=$3 AND id=$4 AND status<>$5
RETURNING id,title,content,format,channel_id`, PostStatusDraft, now, workspaceID, sourcePostID, PostStatusPublishing).Scan(
		&draftID, &title, &content, &format, &channelID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		var status string
		if lookupErr := tx.QueryRowContext(ctx, `SELECT status FROM posts
WHERE workspace_id=$1 AND id=$2`, workspaceID, sourcePostID).Scan(&status); errors.Is(lookupErr, sql.ErrNoRows) {
			return 0, "", "", "", 0, ErrNotFound
		} else if lookupErr != nil {
			return 0, "", "", "", 0, lookupErr
		}
		return 0, "", "", "", 0, fmt.Errorf("%w: source post is currently publishing", ErrConflict)
	}
	if err != nil {
		return 0, "", "", "", 0, fmt.Errorf("duplicate analytics repeat post: %w", err)
	}
	if !channelID.Valid {
		return 0, "", "", "", 0, fmt.Errorf("%w: analytics repeat source requires a channel", ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO post_attachments(
owner_id,workspace_id,post_id,type,position,storage_key,processing_status,size_bytes,mime_type,
width,height,duration_ms,provider_token,provider_token_expires_at,provider_meta,error_code,created_at,updated_at)
SELECT owner_id,workspace_id,$1,type,position,storage_key,'ready',size_bytes,mime_type,
       width,height,duration_ms,'',NULL,'{}','',$2,$2
FROM post_attachments WHERE workspace_id=$3 AND post_id=$4
ORDER BY position,id`, draftID, now, workspaceID, sourcePostID); err != nil {
		return 0, "", "", "", 0, fmt.Errorf("duplicate analytics repeat attachments: %w", err)
	}
	revision, err := createPostRevisionTx(ctx, tx, actorUserID, workspaceID, draftID, now)
	if err != nil {
		return 0, "", "", "", 0, err
	}
	for _, event := range []AuditEvent{
		{
			WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "post.revision_created",
			EntityType: "post", EntityID: fmt.Sprint(draftID),
			Metadata: mustJSON(map[string]any{
				"revision_id": revision.ID, "revision_number": revision.Number,
			}), CreatedAt: now,
		},
		{
			WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "post.duplicated",
			EntityType: "post", EntityID: fmt.Sprint(draftID),
			Metadata: mustJSON(map[string]any{"source_post_id": sourcePostID}), CreatedAt: now,
		},
	} {
		if err := appendAuditEventTx(ctx, tx, event); err != nil {
			return 0, "", "", "", 0, err
		}
	}
	return draftID, title, content, format, channelID.Int64, nil
}
