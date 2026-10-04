package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Kept separate from the public text/type projection: these records remain
// strictly server-owned and must never be embedded in an API response.
func (s *Store) ListContentAnalysisPostsForWorkspace(ctx context.Context, actor, workspaceID string, channelID int64) ([]Post, error) {
	if _, err := s.GetChannelForWorkspace(ctx, actor, workspaceID, channelID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+postColumns+` FROM posts WHERE workspace_id=? AND channel_id=? AND status=? ORDER BY published_at DESC NULLS LAST,id DESC LIMIT 10`, workspaceID, channelID, PostStatusPublished)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	posts := make([]Post, 0, 10)
	for rows.Next() {
		post, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.hydratePostAttachments(ctx, posts); err != nil {
		return nil, err
	}
	result := make([]Post, 0, 3)
	for _, post := range posts {
		if post.MAXPublishedFingerprint != "" && post.MAXPublishedFingerprint != publicationFingerprint(post) {
			continue
		}
		if len(post.Attachments) == 0 && post.ImagePath == "" && post.ImageURL == "" {
			continue
		}
		result = append(result, post)
		if len(result) == 3 {
			break
		}
	}
	return result, nil
}

func ContentAnalysisSnapshotKey(posts []Post, modelKey string) string {
	// Public JSON intentionally hides the provider fields: include them only
	// inside this irreversible hash so a replaced media source invalidates it.
	type identity struct {
		ID                                      int64
		Published, Content, ImagePath, ImageURL string
		Attachments                             []PostAttachment
		Sources                                 []string
	}
	items := make([]identity, 0, len(posts))
	for _, post := range posts {
		item := identity{ID: post.ID, Published: post.MAXMessageID, Content: post.Content, ImagePath: post.ImagePath, ImageURL: post.ImageURL, Attachments: post.Attachments}
		for _, att := range post.Attachments {
			item.Sources = append(item.Sources, fmt.Sprintf("%d:%s:%s:%s:%s:%s", att.ID, att.StorageKey, att.RemoteURL, att.ProviderToken, att.Source, att.UpdatedAt.UTC().Format(time.RFC3339Nano)))
		}
		items = append(items, item)
	}
	raw, _ := json.Marshal(struct {
		Model string
		Posts []identity
	}{modelKey, items})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Attachment records authorize the post, while the ready-object ledger
// separately enforces the exact workspace before private storage is opened.
func (s *Store) OwnsContentAnalysisMedia(ctx context.Context, actor, workspaceID, filename string) (bool, error) {
	access, err := s.ResolveWorkspaceAccess(ctx, actor, workspaceID)
	if err != nil {
		return false, err
	}
	if access.Member.Role != WorkspaceRoleOwner && access.Member.Role != WorkspaceRoleEditor {
		return false, ErrNotFound
	}
	key, err := validateMediaKey(access.Workspace.CompatOwnerUserID, filename)
	if err != nil {
		return false, err
	}
	var owned bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_assets WHERE workspace_id=? AND filename=? AND state='ready')`, workspaceID, key).Scan(&owned)
	return owned, err
}

// Claim serializes cache fills across workers. An empty claim with no result
// means another request is already analyzing this snapshot; callers fall back
// to text rather than start another paid analysis or block indefinitely.
func (s *Store) ClaimContentAnalysis(ctx context.Context, actor, workspaceID string, channelID int64, key string, now time.Time) (string, string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", "", err
	}
	claim := hex.EncodeToString(random[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireContentAnalysisScope(ctx, tx, actor, workspaceID, channelID); err != nil {
		return "", "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO content_analysis_cache(workspace_id,channel_id,snapshot_key,claim_id,claimed_until,expires_at)
VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(workspace_id,channel_id) DO NOTHING`, workspaceID, channelID, key, claim, now.Add(2*time.Minute), now)
	if err != nil {
		return "", "", err
	}
	var existingKey, existingClaim, result string
	var claimedUntil, expires time.Time
	err = tx.QueryRowContext(ctx, `SELECT snapshot_key,claim_id,claimed_until,expires_at,result_json FROM content_analysis_cache WHERE workspace_id=$1 AND channel_id=$2 FOR UPDATE`, workspaceID, channelID).Scan(&existingKey, &existingClaim, &claimedUntil, &expires, &result)
	if err != nil {
		return "", "", err
	}
	if existingKey == key && expires.After(now) && result != "" {
		if err := tx.Commit(); err != nil {
			return "", "", err
		}
		return result, "", nil
	}
	if existingKey == key && existingClaim != claim && claimedUntil.After(now) {
		if err := tx.Commit(); err != nil {
			return "", "", err
		}
		return "", "", nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE content_analysis_cache SET snapshot_key=$3,claim_id=$4,claimed_until=$5,expires_at=$6,result_json='' WHERE workspace_id=$1 AND channel_id=$2`, workspaceID, channelID, key, claim, now.Add(2*time.Minute), now)
	if err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return "", claim, nil
}

func (s *Store) CompleteContentAnalysis(ctx context.Context, actor, workspaceID string, channelID int64, key, claim, result string, expires time.Time) error {
	if len(result) > 16384 || !json.Valid([]byte(result)) {
		return errors.New("invalid content analysis cache result")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireContentAnalysisScope(ctx, tx, actor, workspaceID, channelID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE content_analysis_cache SET result_json=$5,expires_at=$6,claimed_until=CURRENT_TIMESTAMP WHERE workspace_id=$1 AND channel_id=$2 AND snapshot_key=$3 AND claim_id=$4 AND claimed_until>CURRENT_TIMESTAMP`, workspaceID, channelID, key, claim, result, expires)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

func requireContentAnalysisScope(ctx context.Context, tx *sql.Tx, actor, workspaceID string, channelID int64) error {
	if err := lockWorkspaceMembership(ctx, tx, workspaceID); err != nil {
		return err
	}
	if err := requireWorkspaceRole(ctx, tx, actor, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspaceID, channelID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
