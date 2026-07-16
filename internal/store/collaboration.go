package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	postRevisionColumns             = `id,workspace_id,post_id,revision_number,author_user_id,snapshot,created_at`
	postReviewColumns               = `id,workspace_id,post_id,revision_id,reviewer_user_id,decision,comment,created_at`
	postCommentColumns              = `id,workspace_id,post_id,revision_id,parent_id,author_user_id,body,created_at,updated_at,deleted_at,resolved_at,COALESCE(resolved_by_user_id,'')`
	userDisplayNameExpression       = `COALESCE(NULLIF(u.display_name,''),NULLIF(u.login,''),u.id)`
	NotificationKindReviewRequested = "review.requested"
	NotificationKindReviewDecided   = "review.decided"
	NotificationKindCommentCreated  = "comment.created"
)

func (s *Store) CreatePostRevision(ctx context.Context, actorUserID, workspaceID string, postID int64) (PostRevision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PostRevision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return PostRevision{}, err
	}
	revision, err := createPostRevisionTx(ctx, tx, actorUserID, workspaceID, postID, time.Now().UTC())
	if err != nil {
		return PostRevision{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "post.revision_created",
		EntityType: "post", EntityID: fmt.Sprint(postID),
		Metadata:  mustJSON(map[string]any{"revision_id": revision.ID, "revision_number": revision.Number}),
		CreatedAt: revision.CreatedAt,
	}); err != nil {
		return PostRevision{}, err
	}
	if err := tx.Commit(); err != nil {
		return PostRevision{}, err
	}
	return s.getPostRevisionWithAuthor(ctx, workspaceID, revision.ID)
}

func createPostRevisionTx(ctx context.Context, tx *sql.Tx, actorUserID, workspaceID string, postID int64, now time.Time) (PostRevision, error) {
	var snapshot []byte
	err := tx.QueryRowContext(ctx, `SELECT jsonb_build_object(
'title',p.title,'content',p.content,'format',p.format,'channel_id',p.channel_id,
'image_url',p.image_url,'image_path',p.image_path,'image_prompt',p.image_prompt,
'link_buttons',p.link_buttons,'notify',p.notify,
'disable_link_preview',p.disable_link_preview,
'attachments',COALESCE((SELECT jsonb_agg(jsonb_build_object(
    'id',pa.id,'type',pa.type,'position',pa.position,'storage_key',pa.storage_key,
    'processing_status',pa.processing_status,'size_bytes',pa.size_bytes,'mime_type',pa.mime_type,
    'width',pa.width,'height',pa.height,'duration_ms',pa.duration_ms
) ORDER BY pa.position,pa.id) FROM post_attachments pa WHERE pa.post_id=p.id),'[]'::jsonb))
FROM posts p WHERE p.workspace_id=$1 AND p.id=$2 FOR UPDATE`, workspaceID, postID).Scan(&snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return PostRevision{}, ErrNotFound
	}
	if err != nil {
		return PostRevision{}, fmt.Errorf("lock post for revision: %w", err)
	}
	var revision PostRevision
	err = tx.QueryRowContext(ctx, `INSERT INTO post_revisions(
workspace_id,post_id,revision_number,author_user_id,snapshot,created_at)
SELECT $1,$2,COALESCE(max(revision_number),0)+1,$3,$4::jsonb,$5
FROM post_revisions WHERE workspace_id=$1 AND post_id=$2
RETURNING `+postRevisionColumns, workspaceID, postID, actorUserID, string(snapshot), now.UTC()).Scan(
		&revision.ID, &revision.WorkspaceID, &revision.PostID, &revision.Number,
		&revision.AuthorUserID, &revision.Snapshot, &revision.CreatedAt)
	if err != nil {
		return PostRevision{}, fmt.Errorf("create post revision: %w", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE posts SET current_revision_id=$1,review_status='draft',updated_at=$2
WHERE workspace_id=$3 AND id=$4`, revision.ID, now.UTC(), workspaceID, postID)
	if err != nil {
		return PostRevision{}, fmt.Errorf("activate post revision: %w", err)
	}
	normalizePostRevision(&revision)
	return revision, nil
}

func (s *Store) GetCurrentPostRevision(ctx context.Context, actorUserID, workspaceID string, postID int64) (PostRevision, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return PostRevision{}, err
	}
	revision, err := scanPostRevisionWithAuthor(s.db.QueryRowContext(ctx, `SELECT r.id,r.workspace_id,r.post_id,
r.revision_number,r.author_user_id,r.snapshot,r.created_at,`+userDisplayNameExpression+`
FROM posts p JOIN post_revisions r ON r.id=p.current_revision_id
JOIN users u ON u.id=r.author_user_id
WHERE p.workspace_id=$1 AND p.id=$2`, workspaceID, postID))
	if err != nil {
		return PostRevision{}, err
	}
	return revision, nil
}

func (s *Store) ListPostRevisions(ctx context.Context, actorUserID, workspaceID string, postID int64) ([]PostRevision, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.workspace_id,r.post_id,r.revision_number,
r.author_user_id,r.snapshot,r.created_at,`+userDisplayNameExpression+` FROM post_revisions r
JOIN users u ON u.id=r.author_user_id
WHERE r.workspace_id=$1 AND r.post_id=$2 ORDER BY r.revision_number DESC`, workspaceID, postID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]PostRevision, 0)
	for rows.Next() {
		revision, err := scanPostRevisionWithAuthor(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, revision)
	}
	return result, rows.Err()
}

func (s *Store) SubmitPostForReview(ctx context.Context, actorUserID, workspaceID string, postID int64, now time.Time) (PostRevision, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PostRevision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return PostRevision{}, err
	}
	var reviewStatus string
	err = tx.QueryRowContext(ctx, `SELECT review_status FROM posts
WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, postID).Scan(&reviewStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return PostRevision{}, ErrNotFound
	}
	if err != nil {
		return PostRevision{}, err
	}
	if reviewStatus == ReviewStatusInReview {
		return PostRevision{}, fmt.Errorf("%w: post is already in review", ErrConflict)
	}
	// Always snapshot at submission time. Autosaves and attachment mutations
	// may have happened after the previously selected revision.
	revision, err := createPostRevisionTx(ctx, tx, actorUserID, workspaceID, postID, now)
	if err != nil {
		return PostRevision{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE posts SET review_status='in_review',updated_at=$1
WHERE workspace_id=$2 AND id=$3`, now.UTC(), workspaceID, postID); err != nil {
		return PostRevision{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "review.submitted",
		EntityType: "post", EntityID: fmt.Sprint(postID),
		Metadata: mustJSON(map[string]any{"revision_id": revision.ID}), CreatedAt: now,
	}); err != nil {
		return PostRevision{}, err
	}
	if err := notifyRolesTx(ctx, tx, workspaceID, actorUserID,
		[]string{WorkspaceRoleOwner, WorkspaceRoleApprover}, Notification{
			Kind: NotificationKindReviewRequested, Title: "Пост ожидает согласования",
			EntityType: "post", EntityID: fmt.Sprint(postID),
			Metadata: mustJSON(map[string]any{"revision_id": revision.ID}), CreatedAt: now,
		}); err != nil {
		return PostRevision{}, err
	}
	if err := tx.Commit(); err != nil {
		return PostRevision{}, err
	}
	return s.getPostRevisionWithAuthor(ctx, workspaceID, revision.ID)
}

func (s *Store) DecidePostReview(ctx context.Context, reviewerUserID, workspaceID string, postID, revisionID int64, decision, comment string, now time.Time) (PostReview, error) {
	if decision != ReviewDecisionApproved && decision != ReviewDecisionChangesRequested {
		return PostReview{}, errors.New("invalid review decision")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PostReview{}, err
	}
	defer func() { _ = tx.Rollback() }()
	access, err := resolveWorkspaceAccess(ctx, tx, reviewerUserID, workspaceID)
	if err != nil {
		return PostReview{}, err
	}
	if access.Member.Role != WorkspaceRoleOwner && access.Member.Role != WorkspaceRoleApprover {
		return PostReview{}, ErrNotFound
	}
	var currentRevisionID sql.NullInt64
	var reviewStatus string
	err = tx.QueryRowContext(ctx, `SELECT current_revision_id,review_status FROM posts
WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, postID).Scan(&currentRevisionID, &reviewStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return PostReview{}, ErrNotFound
	}
	if err != nil {
		return PostReview{}, err
	}
	if !currentRevisionID.Valid || currentRevisionID.Int64 != revisionID || reviewStatus != ReviewStatusInReview {
		return PostReview{}, ErrConflict
	}
	var revisionAuthor string
	if err := tx.QueryRowContext(ctx, `SELECT author_user_id FROM post_revisions
WHERE workspace_id=$1 AND post_id=$2 AND id=$3`, workspaceID, postID, revisionID).Scan(&revisionAuthor); errors.Is(err, sql.ErrNoRows) {
		return PostReview{}, ErrNotFound
	} else if err != nil {
		return PostReview{}, err
	}
	if decision == ReviewDecisionApproved && access.Workspace.RequireDistinctApprover && revisionAuthor == reviewerUserID {
		return PostReview{}, fmt.Errorf("%w: revision author cannot approve it", ErrConflict)
	}
	var review PostReview
	err = tx.QueryRowContext(ctx, `INSERT INTO post_reviews(
workspace_id,post_id,revision_id,reviewer_user_id,decision,comment,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+postReviewColumns,
		workspaceID, postID, revisionID, reviewerUserID, decision, strings.TrimSpace(comment), now.UTC()).Scan(
		&review.ID, &review.WorkspaceID, &review.PostID, &review.RevisionID,
		&review.ReviewerUserID, &review.Decision, &review.Comment, &review.CreatedAt)
	if err != nil {
		return PostReview{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE posts SET review_status=$1,updated_at=$2
WHERE workspace_id=$3 AND id=$4`, decision, now.UTC(), workspaceID, postID); err != nil {
		return PostReview{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: reviewerUserID, Action: "review." + decision,
		EntityType: "post", EntityID: fmt.Sprint(postID),
		Metadata: mustJSON(map[string]any{"revision_id": revisionID, "review_id": review.ID}), CreatedAt: now,
	}); err != nil {
		return PostReview{}, err
	}
	if revisionAuthor != reviewerUserID {
		if _, err := insertNotificationTx(ctx, tx, Notification{
			WorkspaceID: workspaceID, UserID: revisionAuthor, Kind: NotificationKindReviewDecided,
			Title: "Решение по согласованию", Body: strings.TrimSpace(comment), EntityType: "post",
			EntityID: fmt.Sprint(postID), Metadata: mustJSON(map[string]any{"decision": decision, "revision_id": revisionID}),
			CreatedAt: now,
		}); err != nil {
			return PostReview{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PostReview{}, err
	}
	return s.getPostReviewWithReviewer(ctx, workspaceID, review.ID)
}

func (s *Store) ListPostReviews(ctx context.Context, actorUserID, workspaceID string, postID int64) ([]PostReview, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.workspace_id,r.post_id,r.revision_id,
r.reviewer_user_id,r.decision,r.comment,r.created_at,`+userDisplayNameExpression+` FROM post_reviews r
JOIN users u ON u.id=r.reviewer_user_id
WHERE r.workspace_id=$1 AND r.post_id=$2 ORDER BY r.created_at DESC,r.id DESC`, workspaceID, postID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]PostReview, 0)
	for rows.Next() {
		var review PostReview
		if err := rows.Scan(&review.ID, &review.WorkspaceID, &review.PostID, &review.RevisionID,
			&review.ReviewerUserID, &review.Decision, &review.Comment, &review.CreatedAt,
			&review.ReviewerDisplayName); err != nil {
			return nil, err
		}
		review.CreatedAt = review.CreatedAt.UTC()
		result = append(result, review)
	}
	return result, rows.Err()
}

// IsCurrentRevisionApproved is safe for trusted workers after they obtained a
// workspace-scoped post. HTTP callers must resolve membership first.
func (s *Store) IsCurrentRevisionApproved(ctx context.Context, workspaceID string, postID int64) (bool, error) {
	var approved bool
	err := s.db.QueryRowContext(ctx, `SELECT p.current_revision_id IS NOT NULL
AND p.review_status='approved'
AND EXISTS(SELECT 1 FROM post_reviews r
    WHERE r.workspace_id=p.workspace_id AND r.post_id=p.id
      AND r.revision_id=p.current_revision_id AND r.decision='approved')
FROM posts p WHERE p.workspace_id=$1 AND p.id=$2`, workspaceID, postID).Scan(&approved)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return approved, nil
}

func (s *Store) CreatePostComment(ctx context.Context, actorUserID string, comment PostComment) (PostComment, error) {
	comment.Body = strings.TrimSpace(comment.Body)
	if comment.Body == "" {
		return PostComment{}, errors.New("comment body is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PostComment{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, comment.WorkspaceID,
		WorkspaceRoleOwner, WorkspaceRoleEditor, WorkspaceRoleApprover); err != nil {
		return PostComment{}, err
	}
	var postAuthor string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((
    SELECT r.author_user_id FROM post_revisions r
    WHERE r.workspace_id=p.workspace_id AND r.post_id=p.id
    ORDER BY r.revision_number DESC LIMIT 1
),'') FROM posts p WHERE p.workspace_id=$1 AND p.id=$2`,
		comment.WorkspaceID, comment.PostID).Scan(&postAuthor); errors.Is(err, sql.ErrNoRows) {
		return PostComment{}, ErrNotFound
	} else if err != nil {
		return PostComment{}, err
	}
	if comment.RevisionID != nil {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM post_revisions
WHERE workspace_id=$1 AND post_id=$2 AND id=$3`, comment.WorkspaceID, comment.PostID, *comment.RevisionID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return PostComment{}, ErrNotFound
		} else if err != nil {
			return PostComment{}, err
		}
	}
	if comment.ParentID != nil {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM post_comments
WHERE workspace_id=$1 AND post_id=$2 AND id=$3`, comment.WorkspaceID, comment.PostID, *comment.ParentID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return PostComment{}, ErrNotFound
		} else if err != nil {
			return PostComment{}, err
		}
	}
	now := comment.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	comment.AuthorUserID = actorUserID
	err = tx.QueryRowContext(ctx, `INSERT INTO post_comments(
workspace_id,post_id,revision_id,parent_id,author_user_id,body,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$7) RETURNING `+postCommentColumns,
		comment.WorkspaceID, comment.PostID, comment.RevisionID, comment.ParentID,
		actorUserID, comment.Body, now.UTC()).Scan(
		&comment.ID, &comment.WorkspaceID, &comment.PostID, &comment.RevisionID, &comment.ParentID,
		&comment.AuthorUserID, &comment.Body, &comment.CreatedAt, &comment.UpdatedAt, &comment.DeletedAt,
		&comment.ResolvedAt, &comment.ResolvedByUserID)
	if err != nil {
		return PostComment{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: comment.WorkspaceID, ActorUserID: actorUserID, Action: "comment.created",
		EntityType: "comment", EntityID: fmt.Sprint(comment.ID),
		Metadata: mustJSON(map[string]any{"post_id": comment.PostID}), CreatedAt: now,
	}); err != nil {
		return PostComment{}, err
	}
	targets := make(map[string]struct{})
	if postAuthor != "" {
		targets[postAuthor] = struct{}{}
	}
	if comment.RevisionID != nil {
		var author string
		if err := tx.QueryRowContext(ctx, `SELECT author_user_id FROM post_revisions WHERE id=$1`, *comment.RevisionID).Scan(&author); err == nil {
			targets[author] = struct{}{}
		}
	}
	if comment.ParentID != nil {
		var author string
		if err := tx.QueryRowContext(ctx, `SELECT author_user_id FROM post_comments WHERE id=$1`, *comment.ParentID).Scan(&author); err == nil {
			targets[author] = struct{}{}
		}
	}
	delete(targets, actorUserID)
	for target := range targets {
		if _, err := insertNotificationTx(ctx, tx, Notification{
			WorkspaceID: comment.WorkspaceID, UserID: target, Kind: NotificationKindCommentCreated,
			Title: "Новый комментарий", Body: comment.Body, EntityType: "post", EntityID: fmt.Sprint(comment.PostID),
			Metadata: mustJSON(map[string]any{"comment_id": comment.ID}), CreatedAt: now,
		}); errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return PostComment{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PostComment{}, err
	}
	return s.getPostCommentWithAuthors(ctx, comment.WorkspaceID, comment.ID)
}

func (s *Store) ListPostComments(ctx context.Context, actorUserID, workspaceID string, postID int64) ([]PostComment, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.workspace_id,c.post_id,c.revision_id,c.parent_id,
c.author_user_id,c.body,c.created_at,c.updated_at,c.deleted_at,c.resolved_at,
COALESCE(c.resolved_by_user_id,''),`+userDisplayNameExpression+`,
COALESCE(NULLIF(ru.display_name,''),NULLIF(ru.login,''),ru.id,'')
FROM post_comments c JOIN users u ON u.id=c.author_user_id
LEFT JOIN users ru ON ru.id=c.resolved_by_user_id
WHERE c.workspace_id=$1 AND c.post_id=$2 ORDER BY c.created_at,c.id`, workspaceID, postID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]PostComment, 0)
	for rows.Next() {
		comment, err := scanPostCommentWithAuthors(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, comment)
	}
	return result, rows.Err()
}

func (s *Store) DeletePostComment(ctx context.Context, actorUserID, workspaceID string, commentID int64, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	access, err := resolveWorkspaceAccess(ctx, tx, actorUserID, workspaceID)
	if err != nil {
		return err
	}
	var author string
	if err := tx.QueryRowContext(ctx, `SELECT author_user_id FROM post_comments
WHERE workspace_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE`, workspaceID, commentID).Scan(&author); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if author != actorUserID && access.Member.Role != WorkspaceRoleOwner {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE post_comments SET body='',updated_at=$1,deleted_at=$1
WHERE workspace_id=$2 AND id=$3`, now.UTC(), workspaceID, commentID); err != nil {
		return err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "comment.deleted",
		EntityType: "comment", EntityID: fmt.Sprint(commentID), Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResolvePostComment(ctx context.Context, actorUserID, workspaceID string, commentID int64, resolved bool, now time.Time) (PostComment, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PostComment{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return PostComment{}, err
	}
	var comment PostComment
	resolvedAt, resolvedBy := any(nil), any(nil)
	if resolved {
		resolvedAt, resolvedBy = now.UTC(), actorUserID
	}
	err = tx.QueryRowContext(ctx, `UPDATE post_comments SET resolved_at=$1,resolved_by_user_id=$2,updated_at=$3
WHERE workspace_id=$4 AND id=$5 AND deleted_at IS NULL
RETURNING `+postCommentColumns, resolvedAt, resolvedBy, now.UTC(), workspaceID, commentID).Scan(
		&comment.ID, &comment.WorkspaceID, &comment.PostID, &comment.RevisionID, &comment.ParentID,
		&comment.AuthorUserID, &comment.Body, &comment.CreatedAt, &comment.UpdatedAt, &comment.DeletedAt,
		&comment.ResolvedAt, &comment.ResolvedByUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return PostComment{}, ErrNotFound
	}
	if err != nil {
		return PostComment{}, err
	}
	action := "comment.reopened"
	if resolved {
		action = "comment.resolved"
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: action,
		EntityType: "comment", EntityID: fmt.Sprint(commentID), Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return PostComment{}, err
	}
	if err := tx.Commit(); err != nil {
		return PostComment{}, err
	}
	return s.getPostCommentWithAuthors(ctx, workspaceID, comment.ID)
}

func (s *Store) CreateAuditEvent(ctx context.Context, actorUserID string, event AuditEvent) (AuditEvent, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, event.WorkspaceID); err != nil {
		return AuditEvent{}, err
	}
	event.ActorUserID = actorUserID
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	err := s.db.QueryRowContext(ctx, `INSERT INTO audit_events(
workspace_id,actor_user_id,action,entity_type,entity_id,metadata,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7)
RETURNING id,workspace_id,COALESCE(actor_user_id,''),action,entity_type,entity_id,metadata,created_at`,
		event.WorkspaceID, event.ActorUserID, event.Action, event.EntityType, event.EntityID,
		normalizeJSONObject(event.Metadata), event.CreatedAt.UTC()).Scan(
		&event.ID, &event.WorkspaceID, &event.ActorUserID, &event.Action, &event.EntityType,
		&event.EntityID, &event.Metadata, &event.CreatedAt)
	if err != nil {
		return AuditEvent{}, fmt.Errorf("create audit event: %w", err)
	}
	event.CreatedAt = event.CreatedAt.UTC()
	if event.ActorUserID != "" {
		if err := s.db.QueryRowContext(ctx, `SELECT `+userDisplayNameExpression+` FROM users u WHERE u.id=$1`,
			event.ActorUserID).Scan(&event.ActorDisplayName); err != nil {
			return AuditEvent{}, fmt.Errorf("load audit actor: %w", err)
		}
	}
	return event, nil
}

func (s *Store) ListAuditEvents(ctx context.Context, actorUserID, workspaceID string, limit int, beforeID int64) ([]AuditEvent, error) {
	if err := requireWorkspaceRole(ctx, s.db, actorUserID, workspaceID, WorkspaceRoleOwner); err != nil {
		return nil, err
	}
	limit = boundedListLimit(limit)
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.workspace_id,COALESCE(e.actor_user_id,''),
COALESCE(NULLIF(u.display_name,''),NULLIF(u.login,''),u.id,''),
e.action,e.entity_type,e.entity_id,e.metadata,e.created_at FROM audit_events e
LEFT JOIN users u ON u.id=e.actor_user_id
WHERE e.workspace_id=$1 AND ($2::bigint=0 OR e.id<$2) ORDER BY e.id DESC LIMIT $3`, workspaceID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]AuditEvent, 0)
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(&event.ID, &event.WorkspaceID, &event.ActorUserID, &event.ActorDisplayName, &event.Action,
			&event.EntityType, &event.EntityID, &event.Metadata, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.CreatedAt = event.CreatedAt.UTC()
		result = append(result, event)
	}
	return result, rows.Err()
}

func appendAuditEventTx(ctx context.Context, tx *sql.Tx, event AuditEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_events(
workspace_id,actor_user_id,action,entity_type,entity_id,metadata,created_at)
VALUES($1,NULLIF($2,''),$3,$4,$5,$6,$7)`, event.WorkspaceID, event.ActorUserID,
		event.Action, event.EntityType, event.EntityID, normalizeJSONObject(event.Metadata), event.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

func (s *Store) CreateNotification(ctx context.Context, notification Notification) (Notification, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Notification{}, err
	}
	defer func() { _ = tx.Rollback() }()
	created, err := insertNotificationTx(ctx, tx, notification)
	if err != nil {
		return Notification{}, err
	}
	if err := tx.Commit(); err != nil {
		return Notification{}, err
	}
	return created, nil
}

func insertNotificationTx(ctx context.Context, tx *sql.Tx, notification Notification) (Notification, error) {
	if notification.CreatedAt.IsZero() {
		notification.CreatedAt = time.Now().UTC()
	}
	if notification.WorkspaceID == "" || notification.UserID == "" || notification.Kind == "" {
		return Notification{}, errors.New("notification workspace, user and kind are required")
	}
	query := `INSERT INTO notifications(
workspace_id,user_id,kind,title,body,entity_type,entity_id,metadata,dedupe_key,created_at)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10
WHERE EXISTS(SELECT 1 FROM workspace_members WHERE workspace_id=$1 AND user_id=$2)
ON CONFLICT(user_id,dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING
RETURNING id,workspace_id,user_id,kind,title,body,entity_type,entity_id,metadata,COALESCE(dedupe_key,''),read_at,created_at`
	err := tx.QueryRowContext(ctx, query, notification.WorkspaceID, notification.UserID, notification.Kind,
		notification.Title, notification.Body, notification.EntityType, notification.EntityID,
		normalizeJSONObject(notification.Metadata), notification.DedupeKey, notification.CreatedAt.UTC()).Scan(
		&notification.ID, &notification.WorkspaceID, &notification.UserID, &notification.Kind,
		&notification.Title, &notification.Body, &notification.EntityType, &notification.EntityID,
		&notification.Metadata, &notification.DedupeKey, &notification.ReadAt, &notification.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) && notification.DedupeKey != "" {
		err = tx.QueryRowContext(ctx, `SELECT id,workspace_id,user_id,kind,title,body,entity_type,entity_id,
metadata,COALESCE(dedupe_key,''),read_at,created_at FROM notifications
WHERE user_id=$1 AND dedupe_key=$2`, notification.UserID, notification.DedupeKey).Scan(
			&notification.ID, &notification.WorkspaceID, &notification.UserID, &notification.Kind,
			&notification.Title, &notification.Body, &notification.EntityType, &notification.EntityID,
			&notification.Metadata, &notification.DedupeKey, &notification.ReadAt, &notification.CreatedAt)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, ErrNotFound
	}
	if err != nil {
		return Notification{}, fmt.Errorf("create notification: %w", err)
	}
	normalizeNotification(&notification)
	return notification, nil
}

func notifyRolesTx(ctx context.Context, tx *sql.Tx, workspaceID, excludedUserID string, roles []string, template Notification) error {
	rows, err := tx.QueryContext(ctx, `SELECT user_id,role FROM workspace_members WHERE workspace_id=$1`, workspaceID)
	if err != nil {
		return err
	}
	type recipient struct{ userID, role string }
	recipients := make([]recipient, 0)
	for rows.Next() {
		var recipient recipient
		if err := rows.Scan(&recipient.userID, &recipient.role); err != nil {
			_ = rows.Close()
			return err
		}
		recipients = append(recipients, recipient)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		allowed[role] = struct{}{}
	}
	for _, recipient := range recipients {
		if recipient.userID == excludedUserID {
			continue
		}
		if _, ok := allowed[recipient.role]; !ok {
			continue
		}
		notification := template
		notification.WorkspaceID = workspaceID
		notification.UserID = recipient.userID
		if _, err := insertNotificationTx(ctx, tx, notification); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListNotifications(ctx context.Context, userID, workspaceID string, unreadOnly bool, limit int, beforeID int64) ([]Notification, error) {
	limit = boundedListLimit(limit)
	rows, err := s.db.QueryContext(ctx, `SELECT id,workspace_id,user_id,kind,title,body,entity_type,
entity_id,metadata,COALESCE(dedupe_key,''),read_at,created_at FROM notifications
WHERE user_id=$1 AND ($2='' OR workspace_id=$2) AND (NOT $3::boolean OR read_at IS NULL)
AND ($4::bigint=0 OR id<$4) ORDER BY id DESC LIMIT $5`, userID, workspaceID, unreadOnly, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]Notification, 0)
	for rows.Next() {
		notification, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, notification)
	}
	return result, rows.Err()
}

func (s *Store) MarkNotificationRead(ctx context.Context, userID string, notificationID int64, now time.Time) (Notification, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	notification, err := scanNotification(s.db.QueryRowContext(ctx, `UPDATE notifications
SET read_at=COALESCE(read_at,$1) WHERE user_id=$2 AND id=$3
RETURNING id,workspace_id,user_id,kind,title,body,entity_type,entity_id,metadata,
COALESCE(dedupe_key,''),read_at,created_at`, now.UTC(), userID, notificationID))
	if err != nil {
		return Notification{}, err
	}
	return notification, nil
}

func (s *Store) MarkAllNotificationsRead(ctx context.Context, userID, workspaceID string, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE notifications SET read_at=$1
WHERE user_id=$2 AND ($3='' OR workspace_id=$3) AND read_at IS NULL`, now.UTC(), userID, workspaceID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func scanPostRevision(row scanner) (PostRevision, error) {
	var revision PostRevision
	if err := row.Scan(&revision.ID, &revision.WorkspaceID, &revision.PostID, &revision.Number,
		&revision.AuthorUserID, &revision.Snapshot, &revision.CreatedAt); errors.Is(err, sql.ErrNoRows) {
		return PostRevision{}, ErrNotFound
	} else if err != nil {
		return PostRevision{}, err
	}
	normalizePostRevision(&revision)
	return revision, nil
}

func scanPostRevisionWithAuthor(row scanner) (PostRevision, error) {
	var revision PostRevision
	if err := row.Scan(&revision.ID, &revision.WorkspaceID, &revision.PostID, &revision.Number,
		&revision.AuthorUserID, &revision.Snapshot, &revision.CreatedAt,
		&revision.AuthorDisplayName); errors.Is(err, sql.ErrNoRows) {
		return PostRevision{}, ErrNotFound
	} else if err != nil {
		return PostRevision{}, err
	}
	normalizePostRevision(&revision)
	return revision, nil
}

func (s *Store) getPostRevisionWithAuthor(ctx context.Context, workspaceID string, revisionID int64) (PostRevision, error) {
	return scanPostRevisionWithAuthor(s.db.QueryRowContext(ctx, `SELECT r.id,r.workspace_id,r.post_id,
r.revision_number,r.author_user_id,r.snapshot,r.created_at,`+userDisplayNameExpression+`
FROM post_revisions r JOIN users u ON u.id=r.author_user_id
WHERE r.workspace_id=$1 AND r.id=$2`, workspaceID, revisionID))
}

func (s *Store) getPostReviewWithReviewer(ctx context.Context, workspaceID string, reviewID int64) (PostReview, error) {
	var review PostReview
	err := s.db.QueryRowContext(ctx, `SELECT r.id,r.workspace_id,r.post_id,r.revision_id,
r.reviewer_user_id,r.decision,r.comment,r.created_at,`+userDisplayNameExpression+`
FROM post_reviews r JOIN users u ON u.id=r.reviewer_user_id
WHERE r.workspace_id=$1 AND r.id=$2`, workspaceID, reviewID).Scan(
		&review.ID, &review.WorkspaceID, &review.PostID, &review.RevisionID,
		&review.ReviewerUserID, &review.Decision, &review.Comment, &review.CreatedAt,
		&review.ReviewerDisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return PostReview{}, ErrNotFound
	}
	if err != nil {
		return PostReview{}, err
	}
	review.CreatedAt = review.CreatedAt.UTC()
	return review, nil
}

func normalizePostRevision(revision *PostRevision) { revision.CreatedAt = revision.CreatedAt.UTC() }

func scanPostComment(row scanner) (PostComment, error) {
	var comment PostComment
	if err := row.Scan(&comment.ID, &comment.WorkspaceID, &comment.PostID, &comment.RevisionID,
		&comment.ParentID, &comment.AuthorUserID, &comment.Body, &comment.CreatedAt,
		&comment.UpdatedAt, &comment.DeletedAt, &comment.ResolvedAt, &comment.ResolvedByUserID); errors.Is(err, sql.ErrNoRows) {
		return PostComment{}, ErrNotFound
	} else if err != nil {
		return PostComment{}, err
	}
	normalizePostComment(&comment)
	return comment, nil
}

func scanPostCommentWithAuthors(row scanner) (PostComment, error) {
	var comment PostComment
	if err := row.Scan(&comment.ID, &comment.WorkspaceID, &comment.PostID, &comment.RevisionID,
		&comment.ParentID, &comment.AuthorUserID, &comment.Body, &comment.CreatedAt,
		&comment.UpdatedAt, &comment.DeletedAt, &comment.ResolvedAt, &comment.ResolvedByUserID,
		&comment.AuthorDisplayName, &comment.ResolvedByDisplayName); errors.Is(err, sql.ErrNoRows) {
		return PostComment{}, ErrNotFound
	} else if err != nil {
		return PostComment{}, err
	}
	normalizePostComment(&comment)
	return comment, nil
}

func (s *Store) getPostCommentWithAuthors(ctx context.Context, workspaceID string, commentID int64) (PostComment, error) {
	return scanPostCommentWithAuthors(s.db.QueryRowContext(ctx, `SELECT c.id,c.workspace_id,c.post_id,
c.revision_id,c.parent_id,c.author_user_id,c.body,c.created_at,c.updated_at,c.deleted_at,c.resolved_at,
COALESCE(c.resolved_by_user_id,''),`+userDisplayNameExpression+`,
COALESCE(NULLIF(ru.display_name,''),NULLIF(ru.login,''),ru.id,'')
FROM post_comments c JOIN users u ON u.id=c.author_user_id
LEFT JOIN users ru ON ru.id=c.resolved_by_user_id
WHERE c.workspace_id=$1 AND c.id=$2`, workspaceID, commentID))
}

func normalizePostComment(comment *PostComment) {
	comment.CreatedAt = comment.CreatedAt.UTC()
	comment.UpdatedAt = comment.UpdatedAt.UTC()
	if comment.DeletedAt != nil {
		value := comment.DeletedAt.UTC()
		comment.DeletedAt = &value
	}
	if comment.ResolvedAt != nil {
		value := comment.ResolvedAt.UTC()
		comment.ResolvedAt = &value
	}
}

func scanNotification(row scanner) (Notification, error) {
	var notification Notification
	if err := row.Scan(&notification.ID, &notification.WorkspaceID, &notification.UserID,
		&notification.Kind, &notification.Title, &notification.Body, &notification.EntityType,
		&notification.EntityID, &notification.Metadata, &notification.DedupeKey,
		&notification.ReadAt, &notification.CreatedAt); errors.Is(err, sql.ErrNoRows) {
		return Notification{}, ErrNotFound
	} else if err != nil {
		return Notification{}, err
	}
	normalizeNotification(&notification)
	return notification, nil
}

func normalizeNotification(notification *Notification) {
	notification.CreatedAt = notification.CreatedAt.UTC()
	if notification.ReadAt != nil {
		value := notification.ReadAt.UTC()
		notification.ReadAt = &value
	}
}

func normalizeJSONObject(value json.RawMessage) json.RawMessage {
	if len(value) == 0 || !json.Valid(value) {
		return json.RawMessage(`{}`)
	}
	var object map[string]any
	if json.Unmarshal(value, &object) != nil || object == nil {
		return json.RawMessage(`{}`)
	}
	return value
}

func boundedListLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}
