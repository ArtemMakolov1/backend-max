package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Store) ListWorkspaceCalendar(
	ctx context.Context,
	actorUserID, workspaceID string,
	from, to time.Time,
	channelID *int64,
) ([]CalendarItem, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	from, to = from.UTC(), to.UTC()
	if from.IsZero() || to.IsZero() || !to.After(from) || to.Sub(from) > 366*24*time.Hour {
		return nil, errors.New("calendar range must be positive and no longer than 366 days")
	}
	rows, err := s.db.QueryContext(ctx, `WITH active_variants AS (
    SELECT cv.id,cv.workspace_id,cv.campaign_id,cv.post_id,cv.planned_at,cv.updated_at
    FROM campaign_variants cv
    JOIN campaigns cp ON cp.workspace_id=cv.workspace_id AND cp.id=cv.campaign_id
    WHERE cp.archived_at IS NULL
), calendar_items AS (
    SELECT COALESCE(cv.id,'post:' || p.id::text) AS item_id,
           CASE WHEN cv.id IS NULL THEN 'post' ELSE 'campaign_variant' END AS kind,
           COALESCE(cv.campaign_id,'') AS campaign_id,COALESCE(cv.id,'') AS variant_id,
           p.id AS post_id,c.id AS channel_id,c.title AS channel_title,p.title,
           p.scheduled_at AS occurs_at,
           p.status,COALESCE(p.review_status,'') AS review_status,
           p.updated_at AS post_updated_at,cv.updated_at AS variant_updated_at
    FROM posts p
    JOIN channels c ON c.workspace_id=p.workspace_id AND c.id=p.channel_id
    LEFT JOIN active_variants cv ON cv.workspace_id=p.workspace_id AND cv.post_id=p.id
    WHERE p.workspace_id=$1
      AND p.scheduled_at>=$2 AND p.scheduled_at<$3
      AND ($4::bigint IS NULL OR c.id=$4)
    UNION ALL
    SELECT COALESCE(cv.id,'post:' || p.id::text),
           CASE WHEN cv.id IS NULL THEN 'post' ELSE 'campaign_variant' END,
           COALESCE(cv.campaign_id,''),COALESCE(cv.id,''),
           p.id,c.id,c.title,p.title,p.published_at,p.status,
           COALESCE(p.review_status,''),p.updated_at,cv.updated_at
    FROM posts p
    JOIN channels c ON c.workspace_id=p.workspace_id AND c.id=p.channel_id
    LEFT JOIN active_variants cv ON cv.workspace_id=p.workspace_id AND cv.post_id=p.id
    WHERE p.workspace_id=$1 AND p.scheduled_at IS NULL
      AND p.published_at>=$2 AND p.published_at<$3
      AND ($4::bigint IS NULL OR c.id=$4)
    UNION ALL
    SELECT cv.id,'campaign_variant',cv.campaign_id,cv.id,p.id,c.id,c.title,p.title,
           cv.planned_at,p.status,COALESCE(p.review_status,''),p.updated_at,cv.updated_at
    FROM active_variants cv
    JOIN posts p ON p.workspace_id=cv.workspace_id AND p.id=cv.post_id
    JOIN channels c ON c.workspace_id=p.workspace_id AND c.id=p.channel_id
    WHERE p.workspace_id=$1 AND p.scheduled_at IS NULL AND p.published_at IS NULL
      AND cv.planned_at>=$2 AND cv.planned_at<$3
      AND ($4::bigint IS NULL OR c.id=$4)
    UNION ALL
    SELECT cv.id,'campaign_variant',cv.campaign_id,cv.id,NULL,c.id,c.title,cv.title,
           cv.planned_at,cv.status,'',NULL,cv.updated_at
    FROM campaign_variants cv
    JOIN campaigns cp ON cp.workspace_id=cv.workspace_id AND cp.id=cv.campaign_id
    JOIN channels c ON c.workspace_id=cv.workspace_id AND c.id=cv.channel_id
    WHERE cv.workspace_id=$1 AND cv.post_id IS NULL AND cp.archived_at IS NULL
      AND cv.planned_at>=$2 AND cv.planned_at<$3
      AND ($4::bigint IS NULL OR c.id=$4)
)
SELECT item_id,kind,campaign_id,variant_id,post_id,channel_id,channel_title,title,
       occurs_at,status,review_status,post_updated_at,variant_updated_at
FROM calendar_items ORDER BY occurs_at,channel_id,item_id`, workspaceID, from, to, nullableInt64(channelID))
	if err != nil {
		return nil, fmt.Errorf("list workspace calendar: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]CalendarItem, 0)
	for rows.Next() {
		var item CalendarItem
		if err := rows.Scan(&item.ID, &item.Kind, &item.CampaignID, &item.VariantID,
			&item.PostID, &item.ChannelID, &item.ChannelTitle, &item.Title, &item.OccursAt,
			&item.Status, &item.ReviewStatus, &item.PostUpdatedAt, &item.VariantUpdatedAt); err != nil {
			return nil, err
		}
		item.OccursAt = item.OccursAt.UTC()
		if item.PostUpdatedAt != nil {
			value := item.PostUpdatedAt.UTC()
			item.PostUpdatedAt = &value
		}
		if item.VariantUpdatedAt != nil {
			value := item.VariantUpdatedAt.UTC()
			item.VariantUpdatedAt = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

type lockedSchedulePost struct {
	ID                int64
	Status            string
	UpdatedAt         time.Time
	CurrentRevisionID *int64
	ChannelID         *int64
	Title             string
	Content           string
	Format            string
	ImageURL          string
	AttachmentCount   int
	AttachmentsReady  bool
	AttachmentOrderOK bool
	LinkButtonCount   int
}

func (s *Store) RescheduleWorkspacePost(
	ctx context.Context,
	actorUserID, workspaceID string,
	postID int64,
	scheduledAt, expectedUpdatedAt, now time.Time,
) (Post, error) {
	if scheduledAt.IsZero() || !scheduledAt.UTC().After(now.UTC()) {
		return Post{}, errors.New("scheduled_at must be in the future")
	}
	if expectedUpdatedAt.IsZero() {
		return Post{}, errors.New("expected_updated_at is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Post{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Post{}, err
	}
	approvalRequired, err := workspaceApprovalRequiredTx(ctx, tx, workspaceID)
	if err != nil {
		return Post{}, err
	}
	post, err := lockSchedulePost(ctx, tx, workspaceID, postID)
	if err != nil {
		return Post{}, err
	}
	if !post.UpdatedAt.Equal(expectedUpdatedAt.UTC()) {
		return Post{}, fmt.Errorf("%w: post changed in another session", ErrConflict)
	}
	if conflict := validateLockedSchedulePost(ctx, tx, workspaceID, post, scheduledAt, now, approvalRequired); conflict != nil {
		if conflict.Code == "approval_required" {
			return Post{}, ErrCampaignApprovalRequired
		}
		return Post{}, fmt.Errorf("%w: %s", ErrConflict, conflict.Message)
	}
	updatedAt := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE posts SET status='scheduled',scheduled_at=$1,last_error='',updated_at=$2
WHERE workspace_id=$3 AND id=$4 AND updated_at=$5 AND status IN ('draft','failed','scheduled')`,
		scheduledAt.UTC(), updatedAt, workspaceID, postID, expectedUpdatedAt.UTC())
	if err != nil {
		return Post{}, fmt.Errorf("reschedule workspace post: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Post{}, fmt.Errorf("%w: post changed before it could be rescheduled", ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE campaign_variants
SET planned_at=$1,status='scheduled',updated_at=$2 WHERE workspace_id=$3 AND post_id=$4`,
		scheduledAt.UTC(), updatedAt, workspaceID, postID); err != nil {
		return Post{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "post.calendar_rescheduled",
		EntityType: "post", EntityID: fmt.Sprint(postID),
		Metadata: mustJSON(map[string]any{"scheduled_at": scheduledAt.UTC()}), CreatedAt: updatedAt,
	}); err != nil {
		return Post{}, err
	}
	if err := tx.Commit(); err != nil {
		return Post{}, err
	}
	return s.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID)
}

func (s *Store) BatchScheduleCampaign(
	ctx context.Context,
	actorUserID, workspaceID, campaignID string,
	items []CampaignScheduleItem,
	now time.Time,
) (Campaign, error) {
	if len(items) == 0 || len(items) > 200 {
		return Campaign{}, errors.New("1 to 200 campaign schedule items are required")
	}
	items = append([]CampaignScheduleItem(nil), items...)
	sortCampaignScheduleItems(items)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Campaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Campaign{}, err
	}
	approvalRequired, err := workspaceApprovalRequiredTx(ctx, tx, workspaceID)
	if err != nil {
		return Campaign{}, err
	}
	var campaignExists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM campaigns
WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL`, workspaceID, campaignID).Scan(&campaignExists); errors.Is(err, sql.ErrNoRows) {
		return Campaign{}, ErrNotFound
	} else if err != nil {
		return Campaign{}, err
	}
	seen := make(map[string]struct{}, len(items))
	conflicts := make([]CampaignItemConflict, 0)
	type scheduleCandidate struct {
		request CampaignScheduleItem
		postID  int64
		post    lockedSchedulePost
	}
	candidates := make([]scheduleCandidate, 0, len(items))
	for _, item := range items {
		item.VariantID = strings.TrimSpace(item.VariantID)
		if item.VariantID == "" {
			conflicts = append(conflicts, CampaignItemConflict{Code: "variant_id_required", Message: "variant_id is required"})
			continue
		}
		if _, duplicate := seen[item.VariantID]; duplicate {
			conflicts = append(conflicts, CampaignItemConflict{VariantID: item.VariantID, Code: "duplicate_variant", Message: "variant appears more than once"})
			continue
		}
		seen[item.VariantID] = struct{}{}
		if item.ExpectedUpdatedAt.IsZero() {
			conflicts = append(conflicts, CampaignItemConflict{VariantID: item.VariantID, Code: "expected_updated_at_required", Message: "reload the campaign before scheduling"})
			continue
		}
		var postID *int64
		var plannedAt time.Time
		err := tx.QueryRowContext(ctx, `SELECT post_id,planned_at FROM campaign_variants
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3`, workspaceID, campaignID, item.VariantID).Scan(&postID, &plannedAt)
		if errors.Is(err, sql.ErrNoRows) {
			conflicts = append(conflicts, CampaignItemConflict{VariantID: item.VariantID, Code: "not_found", Message: "campaign variant was not found"})
			continue
		}
		if err != nil {
			return Campaign{}, err
		}
		if postID == nil {
			conflicts = append(conflicts, CampaignItemConflict{VariantID: item.VariantID, Code: "not_materialized", Message: "create a post draft for this variant first"})
			continue
		}
		if item.ScheduledAt.IsZero() {
			item.ScheduledAt = plannedAt.UTC()
		}
		candidates = append(candidates, scheduleCandidate{request: item, postID: *postID})
	}
	// A publication lifecycle transaction owns the post before its linked
	// variant and campaign. Lock every post in the same order before touching
	// either parent row, preventing the inverse campaign -> variant -> post wait.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].postID < candidates[j].postID })
	for index := range candidates {
		candidate := &candidates[index]
		post, err := lockSchedulePost(ctx, tx, workspaceID, candidate.postID)
		if err != nil {
			postID := candidate.postID
			conflicts = append(conflicts, CampaignItemConflict{VariantID: candidate.request.VariantID, PostID: &postID, Code: "post_not_found", Message: "materialized post was not found"})
			continue
		}
		candidate.post = post
		if !post.UpdatedAt.Equal(candidate.request.ExpectedUpdatedAt.UTC()) {
			postID := candidate.postID
			conflicts = append(conflicts, CampaignItemConflict{VariantID: candidate.request.VariantID, PostID: &postID, Code: "stale_post", Message: "post changed in another session"})
			continue
		}
		if conflict := validateLockedSchedulePost(ctx, tx, workspaceID, post, candidate.request.ScheduledAt, now, approvalRequired); conflict != nil {
			conflict.VariantID = candidate.request.VariantID
			conflicts = append(conflicts, *conflict)
		}
	}
	if len(conflicts) != 0 {
		return Campaign{}, &CampaignScheduleError{Items: conflicts}
	}
	// Lock and revalidate the variant mapping only after all post locks are held.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].request.VariantID < candidates[j].request.VariantID })
	for index := range candidates {
		candidate := &candidates[index]
		var currentPostID *int64
		err := tx.QueryRowContext(ctx, `SELECT post_id FROM campaign_variants
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3 FOR UPDATE`, workspaceID, campaignID,
			candidate.request.VariantID).Scan(&currentPostID)
		if errors.Is(err, sql.ErrNoRows) {
			postID := candidate.postID
			conflicts = append(conflicts, CampaignItemConflict{VariantID: candidate.request.VariantID, PostID: &postID,
				Code: "stale_variant", Message: "campaign variant changed in another session"})
		} else if err != nil {
			return Campaign{}, err
		} else if currentPostID == nil || *currentPostID != candidate.postID {
			postID := candidate.postID
			conflicts = append(conflicts, CampaignItemConflict{VariantID: candidate.request.VariantID, PostID: &postID,
				Code: "stale_variant", Message: "campaign variant changed in another session"})
		}
	}
	if len(conflicts) != 0 {
		return Campaign{}, &CampaignScheduleError{Items: conflicts}
	}
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM campaigns
WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL FOR UPDATE`, workspaceID, campaignID).Scan(&campaignExists); errors.Is(err, sql.ErrNoRows) {
		return Campaign{}, ErrNotFound
	} else if err != nil {
		return Campaign{}, err
	}
	updatedAt := time.Now().UTC()
	for _, item := range candidates {
		result, err := tx.ExecContext(ctx, `UPDATE posts
SET status='scheduled',scheduled_at=$1,last_error='',updated_at=$2
WHERE workspace_id=$3 AND id=$4 AND updated_at=$5 AND status IN ('draft','failed','scheduled')`,
			item.request.ScheduledAt.UTC(), updatedAt, workspaceID, item.post.ID, item.request.ExpectedUpdatedAt.UTC())
		if err != nil {
			return Campaign{}, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return Campaign{}, &CampaignScheduleError{Items: []CampaignItemConflict{{
				VariantID: item.request.VariantID, PostID: &item.post.ID,
				Code: "stale_post", Message: "post changed before the batch committed",
			}}}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE campaign_variants
SET planned_at=$1,status='scheduled',updated_at=$2
WHERE workspace_id=$3 AND campaign_id=$4 AND id=$5`, item.request.ScheduledAt.UTC(), updatedAt,
			workspaceID, campaignID, item.request.VariantID); err != nil {
			return Campaign{}, err
		}
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "campaign.scheduled",
		EntityType: "campaign", EntityID: campaignID,
		Metadata: mustJSON(map[string]any{"post_count": len(candidates)}), CreatedAt: updatedAt,
	}); err != nil {
		return Campaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return Campaign{}, err
	}
	return s.GetCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func workspaceApprovalRequiredTx(ctx context.Context, tx *sql.Tx, workspaceID string) (bool, error) {
	var required bool
	if err := tx.QueryRowContext(ctx, `SELECT approval_required FROM workspaces
WHERE id=$1 AND archived_at IS NULL FOR SHARE`, workspaceID).Scan(&required); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, err
	}
	return required, nil
}

func lockSchedulePost(ctx context.Context, tx *sql.Tx, workspaceID string, postID int64) (lockedSchedulePost, error) {
	var post lockedSchedulePost
	post.ID = postID
	if err := tx.QueryRowContext(ctx, `SELECT p.status,p.updated_at,p.current_revision_id,p.channel_id,
p.title,p.content,p.format,p.image_url,
(SELECT count(*) FROM post_attachments pa
    WHERE pa.workspace_id=p.workspace_id AND pa.post_id=p.id),
NOT EXISTS(
    SELECT 1 FROM post_attachments pa
    WHERE pa.workspace_id=p.workspace_id AND pa.post_id=p.id
      AND pa.processing_status<>'ready'
),
COALESCE((
    SELECT min(pa.position)=0 AND max(pa.position)=count(*)-1
    FROM post_attachments pa
    WHERE pa.workspace_id=p.workspace_id AND pa.post_id=p.id
    HAVING count(*)>0
),TRUE),jsonb_array_length(p.link_buttons)
FROM posts p WHERE p.workspace_id=$1 AND p.id=$2 FOR UPDATE OF p`, workspaceID, postID).Scan(
		&post.Status, &post.UpdatedAt, &post.CurrentRevisionID, &post.ChannelID,
		&post.Title, &post.Content, &post.Format, &post.ImageURL, &post.AttachmentCount,
		&post.AttachmentsReady, &post.AttachmentOrderOK, &post.LinkButtonCount); errors.Is(err, sql.ErrNoRows) {
		return lockedSchedulePost{}, ErrNotFound
	} else if err != nil {
		return lockedSchedulePost{}, err
	}
	post.UpdatedAt = post.UpdatedAt.UTC()
	return post, nil
}

func validateLockedSchedulePost(
	ctx context.Context,
	tx *sql.Tx,
	workspaceID string,
	post lockedSchedulePost,
	scheduledAt, now time.Time,
	approvalRequired bool,
) *CampaignItemConflict {
	conflict := func(code, message string) *CampaignItemConflict {
		return &CampaignItemConflict{PostID: &post.ID, Code: code, Message: message}
	}
	if scheduledAt.IsZero() || !scheduledAt.UTC().After(now.UTC()) {
		return conflict("time_not_future", "scheduled_at must be in the future")
	}
	if post.Status != PostStatusDraft && post.Status != PostStatusFailed && post.Status != PostStatusScheduled {
		return conflict("invalid_status", "post cannot be scheduled from its current status")
	}
	if post.AttachmentCount > 0 && !post.AttachmentsReady {
		return conflict("attachment_not_ready", "all post attachments must be ready before scheduling")
	}
	if post.AttachmentCount > 0 && !post.AttachmentOrderOK {
		return conflict("invalid_attachments", "post attachments must have a contiguous zero-based order")
	}
	attachmentLimit := MaxPostAttachments
	if post.LinkButtonCount > 0 {
		attachmentLimit = MaxPostAttachmentsWithKeyboard
	}
	if post.AttachmentCount > attachmentLimit {
		return conflict("invalid_attachments", "post has too many media attachments")
	}
	if !ValidFormat(post.Format) || utf8.RuneCountInString(post.Content) > 4000 ||
		(strings.TrimSpace(post.Content) == "" && post.AttachmentCount == 0 && post.ImageURL == "") {
		return conflict("invalid_content", "post content is not ready for MAX publication")
	}
	if post.ChannelID == nil {
		return conflict("channel_required", "post channel is required")
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT active FROM channels
WHERE workspace_id=$1 AND id=$2`, workspaceID, *post.ChannelID).Scan(&active); err != nil || !active {
		return conflict("channel_unavailable", "post channel is missing or inactive")
	}
	if approvalRequired {
		if post.CurrentRevisionID == nil {
			return conflict("approval_required", "current post revision must be approved")
		}
		var approved bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM post_reviews WHERE workspace_id=$1 AND post_id=$2 AND revision_id=$3 AND decision='approved'
)`, workspaceID, post.ID, *post.CurrentRevisionID).Scan(&approved); err != nil || !approved {
			return conflict("approval_required", "current post revision must be approved")
		}
	}
	return nil
}
