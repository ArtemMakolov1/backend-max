package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// These are observed active comments for the selected MAX publication, not a
// provider total. A sync page is never treated as complete MAX history.
type MAXCommentAnalyticsFields struct {
	ObservedComments         *int64     `json:"observed_comments"`
	CommentsLastSyncedAt     *time.Time `json:"comments_last_synced_at"`
	CommentsCoverageComplete bool       `json:"comments_coverage_complete"`
	CommentsTruncated        bool       `json:"comments_truncated"`
}

func (s *Store) maxCommentAnalyticsForPost(ctx context.Context, workspace string, postID int64, root string, end time.Time) (MAXCommentAnalyticsFields, error) {
	var result MAXCommentAnalyticsFields
	if root == "" {
		return result, nil
	}
	err := s.db.QueryRowContext(ctx, `SELECT
CASE WHEN sync.last_synced_at<$4 AND NOT EXISTS(SELECT 1 FROM max_post_comments future WHERE future.workspace_id=sync.workspace_id AND future.post_id=sync.post_id AND future.root_message_id=sync.root_message_id AND future.observed_at>=$4)
THEN (SELECT COUNT(*) FROM max_post_comments c WHERE c.workspace_id=sync.workspace_id AND c.post_id=sync.post_id AND c.root_message_id=sync.root_message_id AND c.deleted_at IS NULL) END,
CASE WHEN sync.last_synced_at<$4 THEN sync.last_synced_at END,CASE WHEN sync.last_synced_at<$4 THEN sync.truncated ELSE FALSE END
FROM max_post_comment_sync sync WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3`, workspace, postID, root, end).Scan(&result.ObservedComments, &result.CommentsLastSyncedAt, &result.CommentsTruncated)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	return result, err
}

func (s *Store) applyWorkspaceMAXCommentAnalytics(ctx context.Context, workspace string, end time.Time, report *AnalyticsContentReport) error {
	if len(report.Posts) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(report.Posts))
	roots := make([]string, 0, len(report.Posts))
	index := map[int64]int{}
	for i, p := range report.Posts {
		ids = append(ids, p.ID)
		roots = append(roots, p.RootMessageID)
		index[p.ID] = i
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,
CASE WHEN sync.last_synced_at<$4 AND NOT EXISTS(SELECT 1 FROM max_post_comments future WHERE future.workspace_id=$1 AND future.post_id=p.id AND future.root_message_id=p.root_message_id AND future.observed_at>=$4)
THEN (SELECT COUNT(*) FROM max_post_comments c WHERE c.workspace_id=$1 AND c.post_id=p.id AND c.root_message_id=p.root_message_id AND c.deleted_at IS NULL) END,
CASE WHEN sync.last_synced_at<$4 THEN sync.last_synced_at END,CASE WHEN sync.last_synced_at<$4 THEN COALESCE(sync.truncated,FALSE) ELSE FALSE END
FROM UNNEST($2::bigint[],$3::text[]) p(id,root_message_id)
LEFT JOIN max_post_comment_sync sync ON sync.workspace_id=$1 AND sync.post_id=p.id AND sync.root_message_id=p.root_message_id`, workspace, ids, roots, end)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	knownIDs := []int64{}
	knownRoots := []string{}
	var total int64
	for rows.Next() {
		var id int64
		var fields MAXCommentAnalyticsFields
		if err = rows.Scan(&id, &fields.ObservedComments, &fields.CommentsLastSyncedAt, &fields.CommentsTruncated); err != nil {
			_ = rows.Close()
			return err
		}
		i, ok := index[id]
		if !ok {
			continue
		}
		report.Posts[i].MAXCommentAnalyticsFields = fields
		if fields.ObservedComments != nil {
			total += *fields.ObservedComments
			knownIDs = append(knownIDs, id)
			knownRoots = append(knownRoots, report.Posts[i].RootMessageID)
			report.Summary.CommentsSyncedPosts++
			if report.Summary.CommentsLastSyncedAt == nil || fields.CommentsLastSyncedAt != nil && fields.CommentsLastSyncedAt.After(*report.Summary.CommentsLastSyncedAt) {
				report.Summary.CommentsLastSyncedAt = fields.CommentsLastSyncedAt
			}
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(knownIDs) == 0 {
		return nil
	}
	var authors int64
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT c.sender_user_id) FROM max_post_comments c JOIN UNNEST($2::bigint[],$3::text[]) p(id,root_message_id) ON p.id=c.post_id AND p.root_message_id=c.root_message_id WHERE c.workspace_id=$1 AND c.deleted_at IS NULL AND NOT c.sender_is_bot AND c.sender_user_id<>''`, workspace, knownIDs, knownRoots).Scan(&authors)
	if err != nil {
		return err
	}
	report.Summary.ObservedComments = &total
	report.Summary.ObservedCommentAuthors = &authors
	return nil
}
