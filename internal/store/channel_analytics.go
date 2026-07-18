package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const MaxChannelAnalyticsDays = 366

// ChannelAnalytics is the tenant-scoped, user-facing report for one connected
// channel. Missing MAX counters are represented as nil rather than zero so the
// client never presents an upstream data gap as a real zero.
type ChannelAnalytics struct {
	Channel ChannelAnalyticsChannel `json:"channel"`
	Period  AnalyticsPeriod         `json:"period"`
	Summary AnalyticsSummary        `json:"summary"`
	Daily   []AnalyticsDailyPoint   `json:"daily"`
	Posts   []AnalyticsPost         `json:"posts"`
}

type ChannelAnalyticsChannel struct {
	ID                int64  `json:"id"`
	Title             string `json:"title"`
	IconURL           string `json:"icon_url,omitempty"`
	ParticipantsCount int    `json:"participants_count"`
}

type AnalyticsPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type AnalyticsSummary struct {
	PostsTotal          int64  `json:"posts_total"`
	PublishedPosts      int64  `json:"published_posts"`
	TotalViews          *int64 `json:"total_views"`
	ParticipantsCurrent *int   `json:"participants_current"`
	ParticipantsChange  *int   `json:"participants_change"`
	ViewsChange         *int64 `json:"views_change"`
}

// AnalyticsDailyPoint contains observations only. Views is the change between
// comparable snapshots of the same MAX publications captured in the period;
// a publication's first observed counter is never presented as growth.
// ViewsTotal is the latest known cumulative total on the date. Dates with no
// MAX observation are not manufactured as zero-valued points.
type AnalyticsDailyPoint struct {
	Date               string `json:"date"`
	Views              *int64 `json:"views"`
	ViewsTotal         *int64 `json:"views_total"`
	ParticipantsCount  *int   `json:"participants_count"`
	ParticipantsChange *int   `json:"participants_change"`
}

type AnalyticsPost struct {
	ID               int64      `json:"id"`
	Title            string     `json:"title"`
	PublishedAt      *time.Time `json:"published_at"`
	Views            *int64     `json:"views"`
	MAXMessageURL    string     `json:"max_message_url,omitempty"`
	MAXStatsSyncedAt *time.Time `json:"max_stats_synced_at"`
	PublicationState string     `json:"publication_state"`
	RemovedFromMAX   bool       `json:"removed_from_max"`
}

type analyticsViewObservation struct {
	PostID       int64
	MAXMessageID string
	Views        int64
	CapturedAt   time.Time
}

type analyticsPublicationKey struct {
	PostID       int64
	MAXMessageID string
}

// GetChannelAnalyticsForUser returns one bounded channel report and authorizes
// the parent channel before reading any posts or snapshots. The date interval
// is inclusive and interpreted as UTC calendar days.
func (s *Store) GetChannelAnalyticsForUser(ctx context.Context, userID string, channelID int64,
	fromDay, toDay time.Time,
) (ChannelAnalytics, error) {
	if strings.TrimSpace(userID) == "" || channelID <= 0 {
		return ChannelAnalytics{}, errors.New("channel owner and positive channel ID are required")
	}
	if fromDay.IsZero() || toDay.IsZero() {
		return ChannelAnalytics{}, errors.New("analytics date range is required")
	}
	fromDay, toDay = utcDate(fromDay), utcDate(toDay)
	if toDay.Before(fromDay) {
		return ChannelAnalytics{}, errors.New("analytics end date must not precede start date")
	}
	if days := int(toDay.Sub(fromDay)/(24*time.Hour)) + 1; days > MaxChannelAnalyticsDays {
		return ChannelAnalytics{}, fmt.Errorf("analytics range must not exceed %d days", MaxChannelAnalyticsDays)
	}

	channel, err := s.GetChannelForUser(ctx, userID, channelID)
	if err != nil {
		return ChannelAnalytics{}, err
	}
	toExclusive := toDay.AddDate(0, 0, 1)
	report := ChannelAnalytics{
		Channel: ChannelAnalyticsChannel{
			ID: channel.ID, Title: channel.Title, IconURL: channel.IconURL,
			ParticipantsCount: channel.ParticipantsCount,
		},
		Period: AnalyticsPeriod{From: fromDay.Format(time.DateOnly), To: toDay.Format(time.DateOnly)},
		Daily:  make([]AnalyticsDailyPoint, 0),
		Posts:  make([]AnalyticsPost, 0),
	}

	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM posts
WHERE owner_id = ? AND channel_id = ?
  AND created_at >= ? AND created_at < ?`,
		userID, channelID, fromDay, toExclusive).Scan(&report.Summary.PostsTotal); err != nil {
		return ChannelAnalytics{}, fmt.Errorf("count channel analytics posts: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT post.id, post.title, post.published_at,
       COALESCE(latest.views,
           CASE WHEN post.max_stats_synced_at < ? THEN post.max_views END),
       post.max_message_url,
       COALESCE(latest.captured_at,
           CASE WHEN post.max_stats_synced_at < ? THEN post.max_stats_synced_at END),
       CASE
           WHEN post.status = ? AND post.max_message_id <> '' THEN 'published'
           ELSE 'removed'
       END AS publication_state
FROM posts AS post
LEFT JOIN LATERAL (
    SELECT snapshot.views, snapshot.captured_at
    FROM post_view_snapshots AS snapshot
    WHERE snapshot.owner_id = post.owner_id
      AND snapshot.post_id = post.id
      AND snapshot.captured_at < ?
      AND (post.max_message_id = '' OR snapshot.max_message_id = post.max_message_id)
    ORDER BY snapshot.captured_at DESC, snapshot.id DESC
    LIMIT 1
) AS latest ON TRUE
WHERE post.owner_id = ? AND post.channel_id = ?
  AND post.published_at >= ? AND post.published_at < ?
ORDER BY post.published_at DESC, post.id DESC`,
		toExclusive, toExclusive, PostStatusPublished, toExclusive, userID, channelID, fromDay, toExclusive)
	if err != nil {
		return ChannelAnalytics{}, fmt.Errorf("list channel analytics posts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var totalViews int64
	var knownViews int
	for rows.Next() {
		var post AnalyticsPost
		var publishedAt, statsSyncedAt sql.NullTime
		var views sql.NullInt64
		if err := rows.Scan(&post.ID, &post.Title, &publishedAt, &views, &post.MAXMessageURL,
			&statsSyncedAt, &post.PublicationState); err != nil {
			return ChannelAnalytics{}, fmt.Errorf("scan channel analytics post: %w", err)
		}
		post.PublishedAt = parseNullableTime(publishedAt)
		post.MAXStatsSyncedAt = parseNullableTime(statsSyncedAt)
		if views.Valid {
			value := views.Int64
			post.Views = &value
			totalViews += value
			knownViews++
		}
		post.RemovedFromMAX = post.PublicationState == "removed"
		if post.RemovedFromMAX {
			// MAX URLs are live publication links. Do not expose a stale link as
			// actionable after the publication has been removed.
			post.MAXMessageURL = ""
		}
		report.Posts = append(report.Posts, post)
	}
	if err := rows.Err(); err != nil {
		return ChannelAnalytics{}, fmt.Errorf("list channel analytics posts: %w", err)
	}
	report.Summary.PublishedPosts = int64(len(report.Posts))
	if knownViews > 0 {
		report.Summary.TotalViews = &totalViews
	}

	viewObservations, err := s.listChannelAnalyticsViewObservations(ctx, userID, channelID, fromDay, toExclusive)
	if err != nil {
		return ChannelAnalytics{}, err
	}
	participantHistory, err := s.ListChannelParticipantSnapshotsForUser(ctx, userID, channelID, fromDay, toDay)
	if err != nil {
		return ChannelAnalytics{}, err
	}
	participantBaseline, err := s.latestParticipantSnapshotBefore(ctx, userID, channelID, fromDay)
	if err != nil {
		return ChannelAnalytics{}, err
	}

	report.Daily, report.Summary.ViewsChange = buildAnalyticsDaily(
		viewObservations, participantHistory, participantBaseline,
	)
	participantsCurrent := channel.ParticipantsCount
	if len(participantHistory) > 0 {
		participantsCurrent = participantHistory[len(participantHistory)-1].ParticipantsCount
	} else if participantBaseline != nil {
		participantsCurrent = participantBaseline.ParticipantsCount
	}
	report.Summary.ParticipantsCurrent = &participantsCurrent
	if len(participantHistory) > 0 {
		if participantBaseline != nil {
			change := participantHistory[len(participantHistory)-1].ParticipantsCount - participantBaseline.ParticipantsCount
			report.Summary.ParticipantsChange = &change
		} else if len(participantHistory) >= 2 {
			change := participantHistory[len(participantHistory)-1].ParticipantsCount - participantHistory[0].ParticipantsCount
			report.Summary.ParticipantsChange = &change
		}
	}
	return report, nil
}

func (s *Store) listChannelAnalyticsViewObservations(ctx context.Context, userID string, channelID int64,
	fromDay, toExclusive time.Time,
) ([]analyticsViewObservation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT snapshot.post_id, snapshot.max_message_id, snapshot.views, snapshot.captured_at
FROM post_view_snapshots AS snapshot
JOIN posts AS post ON post.owner_id = snapshot.owner_id AND post.id = snapshot.post_id
WHERE snapshot.owner_id = ? AND post.channel_id = ?
  AND post.published_at >= ? AND post.published_at < ?
  AND snapshot.captured_at >= ? AND snapshot.captured_at < ?
  AND (post.max_message_id = '' OR snapshot.max_message_id = post.max_message_id)
ORDER BY snapshot.captured_at, snapshot.id`,
		userID, channelID, fromDay, toExclusive, fromDay, toExclusive)
	if err != nil {
		return nil, fmt.Errorf("list channel analytics view observations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]analyticsViewObservation, 0)
	for rows.Next() {
		var observation analyticsViewObservation
		if err := rows.Scan(&observation.PostID, &observation.MAXMessageID, &observation.Views,
			&observation.CapturedAt); err != nil {
			return nil, fmt.Errorf("scan channel analytics view observation: %w", err)
		}
		observation.CapturedAt = observation.CapturedAt.UTC()
		result = append(result, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list channel analytics view observations: %w", err)
	}
	return result, nil
}

func (s *Store) latestParticipantSnapshotBefore(ctx context.Context, userID string, channelID int64,
	before time.Time,
) (*ChannelParticipantSnapshot, error) {
	var snapshot ChannelParticipantSnapshot
	var observedOn time.Time
	err := s.db.QueryRowContext(ctx, `
SELECT history.observed_on, history.participants_count, history.captured_at
FROM channel_participant_snapshots AS history
JOIN channels AS channel ON channel.id = history.channel_id
WHERE channel.owner_id = ? AND channel.id = ? AND history.observed_on < ?
ORDER BY history.observed_on DESC, history.captured_at DESC
LIMIT 1`, userID, channelID, before.Format(time.DateOnly)).Scan(
		&observedOn, &snapshot.ParticipantsCount, &snapshot.CapturedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get channel analytics participant baseline: %w", err)
	}
	snapshot.ObservedOn = observedOn.UTC().Format(time.DateOnly)
	snapshot.CapturedAt = snapshot.CapturedAt.UTC()
	return &snapshot, nil
}

func buildAnalyticsDaily(viewObservations []analyticsViewObservation,
	participantHistory []ChannelParticipantSnapshot,
	participantBaseline *ChannelParticipantSnapshot,
) ([]AnalyticsDailyPoint, *int64) {
	points := make(map[string]*AnalyticsDailyPoint)
	viewDays := make(map[string][]analyticsViewObservation)
	for _, observation := range viewObservations {
		day := observation.CapturedAt.UTC().Format(time.DateOnly)
		viewDays[day] = append(viewDays[day], observation)
	}
	viewDayKeys := make([]string, 0, len(viewDays))
	for day := range viewDays {
		viewDayKeys = append(viewDayKeys, day)
	}
	sort.Strings(viewDayKeys)

	publicationViews := make(map[analyticsPublicationKey]int64)
	var viewsChange int64
	var hasComparableViews bool
	for _, day := range viewDayKeys {
		var dayChange int64
		var hasComparableDayViews bool
		for _, observation := range viewDays[day] {
			key := analyticsPublicationKey{PostID: observation.PostID, MAXMessageID: observation.MAXMessageID}
			if previous, exists := publicationViews[key]; exists {
				// MAX counters are cumulative. Compare only observations of the
				// same publication, otherwise the first snapshot of a newly
				// published post would be incorrectly counted as period growth.
				delta := observation.Views - previous
				dayChange += delta
				viewsChange += delta
				hasComparableDayViews = true
				hasComparableViews = true
			}
			// Keep the latest observation, including a possible upstream
			// correction. Taking MAX() would conceal real counter changes.
			publicationViews[key] = observation.Views
		}
		var total int64
		for _, views := range publicationViews {
			total += views
		}
		point := &AnalyticsDailyPoint{Date: day, ViewsTotal: int64Pointer(total)}
		if hasComparableDayViews {
			point.Views = int64Pointer(dayChange)
		}
		points[day] = point
	}

	previousParticipant := participantBaseline
	for index := range participantHistory {
		snapshot := participantHistory[index]
		point := points[snapshot.ObservedOn]
		if point == nil {
			point = &AnalyticsDailyPoint{Date: snapshot.ObservedOn}
			points[snapshot.ObservedOn] = point
		}
		value := snapshot.ParticipantsCount
		point.ParticipantsCount = &value
		if previousParticipant != nil && analyticsDaysAreConsecutive(
			previousParticipant.ObservedOn, snapshot.ObservedOn,
		) {
			change := snapshot.ParticipantsCount - previousParticipant.ParticipantsCount
			point.ParticipantsChange = &change
		}
		previousParticipant = &participantHistory[index]
	}

	days := make([]string, 0, len(points))
	for day := range points {
		days = append(days, day)
	}
	sort.Strings(days)
	result := make([]AnalyticsDailyPoint, 0, len(days))
	for _, day := range days {
		result = append(result, *points[day])
	}
	var observedViewsChange *int64
	if hasComparableViews {
		observedViewsChange = int64Pointer(viewsChange)
	}
	return result, observedViewsChange
}

func analyticsDaysAreConsecutive(previous, current string) bool {
	previousDay, previousErr := time.Parse(time.DateOnly, previous)
	currentDay, currentErr := time.Parse(time.DateOnly, current)
	return previousErr == nil && currentErr == nil && currentDay.Equal(previousDay.AddDate(0, 0, 1))
}

func int64Pointer(value int64) *int64 {
	copy := value
	return &copy
}
