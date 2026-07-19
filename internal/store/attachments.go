package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AddPostAttachmentForUser appends or inserts one already quota-accounted S3
// object and returns the refreshed post. position < 0 appends the attachment.
func (s *Store) AddPostAttachmentForUser(ctx context.Context, userID string, postID int64, attachment PostAttachment) (Post, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Post{}, fmt.Errorf("begin attachment insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := lockPostAttachmentState(ctx, tx, userID, postID)
	if err != nil {
		return Post{}, err
	}
	if err := validateAttachmentWriteState(state.status); err != nil {
		return Post{}, err
	}
	if err := validateAttachmentObject(ctx, tx, userID, attachment); err != nil {
		return Post{}, err
	}
	ids, err := listAttachmentIDsTx(ctx, tx, userID, postID)
	if err != nil {
		return Post{}, err
	}
	if len(ids) >= maxAttachmentsForButtons(state.linkButtons) {
		return Post{}, fmt.Errorf("%w: this post already has the maximum number of media attachments", ErrConflict)
	}

	position := attachment.Position
	if position < 0 || position > len(ids) {
		position = len(ids)
	}
	now := time.Now().UTC()
	var id int64
	err = tx.QueryRowContext(ctx, bindSQL(`INSERT INTO post_attachments
(owner_id, post_id, type, position, storage_key, processing_status, size_bytes, mime_type,
 width, height, duration_ms, provider_token, provider_token_expires_at, provider_meta, error_code, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`),
		userID, postID, attachment.Type, len(ids), attachment.StorageKey, normalizedProcessingStatus(attachment.ProcessingStatus),
		attachment.SizeBytes, attachment.MIMEType, nullableInt(attachment.Width), nullableInt(attachment.Height), nullableInt64(attachment.DurationMS),
		attachment.ProviderToken, nullableTime(attachment.ProviderExpires), normalizedProviderMeta(attachment.ProviderMeta), attachment.ErrorCode,
		now, now).Scan(&id)
	if err != nil {
		return Post{}, fmt.Errorf("insert post attachment: %w", err)
	}
	ids = append(ids, id)
	if position < len(ids)-1 {
		copy(ids[position+1:], ids[position:len(ids)-1])
		ids[position] = id
	}
	if err := writeAttachmentOrderTx(ctx, tx, userID, postID, ids); err != nil {
		return Post{}, err
	}
	if err := syncPostAttachmentProjectionTx(ctx, tx, userID, postID, now); err != nil {
		return Post{}, err
	}
	if err := tx.Commit(); err != nil {
		return Post{}, fmt.Errorf("commit attachment insert: %w", err)
	}
	return s.GetPost(ctx, postID)
}

// ReplacePostAttachmentForUser swaps one object without changing its stable
// attachment id or position. Other gallery entries are not rewritten.
func (s *Store) ReplacePostAttachmentForUser(ctx context.Context, userID string, postID, attachmentID int64, replacement PostAttachment) (Post, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Post{}, fmt.Errorf("begin attachment replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := lockPostAttachmentState(ctx, tx, userID, postID)
	if err != nil {
		return Post{}, err
	}
	if err := validateAttachmentWriteState(state.status); err != nil {
		return Post{}, err
	}
	if err := validateAttachmentObject(ctx, tx, userID, replacement); err != nil {
		return Post{}, err
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, bindSQL(`UPDATE post_attachments SET
type=?, storage_key=?, processing_status=?, size_bytes=?, mime_type=?, width=?, height=?, duration_ms=?,
provider_token=?, provider_token_expires_at=?, provider_meta=?, error_code=?, updated_at=?
WHERE owner_id=? AND post_id=? AND id=?`), replacement.Type, replacement.StorageKey,
		normalizedProcessingStatus(replacement.ProcessingStatus), replacement.SizeBytes, replacement.MIMEType,
		nullableInt(replacement.Width), nullableInt(replacement.Height), nullableInt64(replacement.DurationMS), replacement.ProviderToken,
		nullableTime(replacement.ProviderExpires), normalizedProviderMeta(replacement.ProviderMeta), replacement.ErrorCode, now,
		userID, postID, attachmentID)
	if err != nil {
		return Post{}, fmt.Errorf("replace post attachment: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Post{}, ErrNotFound
	}
	if err := syncPostAttachmentProjectionTx(ctx, tx, userID, postID, now); err != nil {
		return Post{}, err
	}
	if err := tx.Commit(); err != nil {
		return Post{}, fmt.Errorf("commit attachment replacement: %w", err)
	}
	return s.GetPost(ctx, postID)
}

// ReplaceFirstImageAttachmentForUser preserves the legacy single-image API:
// it replaces only the first image, or inserts one at the start when absent.
func (s *Store) ReplaceFirstImageAttachmentForUser(ctx context.Context, userID string, postID int64, replacement PostAttachment) (Post, error) {
	attachments, err := s.ListPostAttachmentsForUser(ctx, userID, postID)
	if err != nil {
		return Post{}, err
	}
	for _, attachment := range attachments {
		if attachment.Type == PostAttachmentImage {
			return s.ReplacePostAttachmentForUser(ctx, userID, postID, attachment.ID, replacement)
		}
	}
	replacement.Position = 0
	return s.AddPostAttachmentForUser(ctx, userID, postID, replacement)
}

// ReplaceFirstImageAttachmentAndPromptIfUnchanged atomically applies the
// result of a long-running image upload/generation to the exact post revision
// that initiated it. A concurrent autosave or lifecycle transition makes the
// snapshot stale and returns ErrConflict before either the attachment or the
// prompt is changed.
func (s *Store) ReplaceFirstImageAttachmentAndPromptIfUnchanged(
	ctx context.Context,
	current Post,
	replacement PostAttachment,
	prompt string,
) (Post, error) {
	if replacement.Type != PostAttachmentImage {
		return Post{}, errors.New("generated post media must be an image attachment")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Post{}, fmt.Errorf("begin atomic image replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, err := lockPostAttachmentState(ctx, tx, current.UserID, current.ID)
	if err != nil {
		return Post{}, err
	}
	if err := validateAttachmentWriteState(state.status); err != nil {
		return Post{}, err
	}
	if state.status != current.Status || !state.updatedAt.UTC().Equal(current.UpdatedAt.UTC()) {
		return Post{}, fmt.Errorf("%w: post changed while the image was being prepared", ErrConflict)
	}
	if err := validateAttachmentObject(ctx, tx, current.UserID, replacement); err != nil {
		return Post{}, err
	}

	ids, err := listAttachmentIDsTx(ctx, tx, current.UserID, current.ID)
	if err != nil {
		return Post{}, err
	}
	var firstImageID int64
	err = tx.QueryRowContext(ctx, bindSQL(`SELECT id FROM post_attachments
WHERE owner_id=? AND post_id=? AND type=? ORDER BY position, id LIMIT 1`),
		current.UserID, current.ID, PostAttachmentImage).Scan(&firstImageID)
	now := time.Now().UTC()
	switch {
	case err == nil:
		result, updateErr := tx.ExecContext(ctx, bindSQL(`UPDATE post_attachments SET
type=?, storage_key=?, processing_status=?, size_bytes=?, mime_type=?, width=?, height=?, duration_ms=?,
provider_token=?, provider_token_expires_at=?, provider_meta=?, error_code=?, updated_at=?
WHERE owner_id=? AND post_id=? AND id=?`), replacement.Type, replacement.StorageKey,
			normalizedProcessingStatus(replacement.ProcessingStatus), replacement.SizeBytes, replacement.MIMEType,
			nullableInt(replacement.Width), nullableInt(replacement.Height), nullableInt64(replacement.DurationMS), replacement.ProviderToken,
			nullableTime(replacement.ProviderExpires), normalizedProviderMeta(replacement.ProviderMeta), replacement.ErrorCode, now,
			current.UserID, current.ID, firstImageID)
		if updateErr != nil {
			return Post{}, fmt.Errorf("replace first post image: %w", updateErr)
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return Post{}, ErrNotFound
		}
	case errors.Is(err, sql.ErrNoRows):
		if len(ids) >= maxAttachmentsForButtons(state.linkButtons) {
			return Post{}, fmt.Errorf("%w: this post already has the maximum number of media attachments", ErrConflict)
		}
		var id int64
		insertErr := tx.QueryRowContext(ctx, bindSQL(`INSERT INTO post_attachments
(owner_id, post_id, type, position, storage_key, processing_status, size_bytes, mime_type,
 width, height, duration_ms, provider_token, provider_token_expires_at, provider_meta, error_code, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`),
			current.UserID, current.ID, replacement.Type, len(ids), replacement.StorageKey,
			normalizedProcessingStatus(replacement.ProcessingStatus), replacement.SizeBytes, replacement.MIMEType,
			nullableInt(replacement.Width), nullableInt(replacement.Height), nullableInt64(replacement.DurationMS),
			replacement.ProviderToken, nullableTime(replacement.ProviderExpires), normalizedProviderMeta(replacement.ProviderMeta),
			replacement.ErrorCode, now, now).Scan(&id)
		if insertErr != nil {
			return Post{}, fmt.Errorf("insert first post image: %w", insertErr)
		}
		ids = append(ids, id)
		copy(ids[1:], ids[:len(ids)-1])
		ids[0] = id
		if err := writeAttachmentOrderTx(ctx, tx, current.UserID, current.ID, ids); err != nil {
			return Post{}, err
		}
	default:
		return Post{}, fmt.Errorf("find first post image: %w", err)
	}

	if err := syncPostAttachmentProjectionTx(ctx, tx, current.UserID, current.ID, now); err != nil {
		return Post{}, err
	}
	result, err := tx.ExecContext(ctx, bindSQL(`UPDATE posts SET image_prompt=? WHERE owner_id=? AND id=?`),
		prompt, current.UserID, current.ID)
	if err != nil {
		return Post{}, fmt.Errorf("update image prompt: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Post{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return Post{}, fmt.Errorf("commit atomic image replacement: %w", err)
	}
	return s.GetPost(ctx, current.ID)
}

func (s *Store) ReorderPostAttachmentsForUser(ctx context.Context, userID string, postID int64, orderedIDs []int64) (Post, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Post{}, fmt.Errorf("begin attachment reorder: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := lockPostAttachmentState(ctx, tx, userID, postID)
	if err != nil {
		return Post{}, err
	}
	if err := validateAttachmentWriteState(state.status); err != nil {
		return Post{}, err
	}
	currentIDs, err := listAttachmentIDsTx(ctx, tx, userID, postID)
	if err != nil {
		return Post{}, err
	}
	if !sameAttachmentIDs(currentIDs, orderedIDs) {
		return Post{}, errors.New("ordered_attachment_ids must contain every attachment exactly once")
	}
	if err := writeAttachmentOrderTx(ctx, tx, userID, postID, orderedIDs); err != nil {
		return Post{}, err
	}
	if err := syncPostAttachmentProjectionTx(ctx, tx, userID, postID, time.Now().UTC()); err != nil {
		return Post{}, err
	}
	if err := tx.Commit(); err != nil {
		return Post{}, fmt.Errorf("commit attachment reorder: %w", err)
	}
	return s.GetPost(ctx, postID)
}

func (s *Store) DeletePostAttachmentForUser(ctx context.Context, userID string, postID, attachmentID int64) (Post, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Post{}, fmt.Errorf("begin attachment deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := lockPostAttachmentState(ctx, tx, userID, postID)
	if err != nil {
		return Post{}, err
	}
	if err := validateAttachmentWriteState(state.status); err != nil {
		return Post{}, err
	}
	result, err := tx.ExecContext(ctx, bindSQL(`DELETE FROM post_attachments
WHERE owner_id=? AND post_id=? AND id=?`), userID, postID, attachmentID)
	if err != nil {
		return Post{}, fmt.Errorf("delete post attachment: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Post{}, ErrNotFound
	}
	ids, err := listAttachmentIDsTx(ctx, tx, userID, postID)
	if err != nil {
		return Post{}, err
	}
	if err := writeAttachmentOrderTx(ctx, tx, userID, postID, ids); err != nil {
		return Post{}, err
	}
	if err := syncPostAttachmentProjectionTx(ctx, tx, userID, postID, time.Now().UTC()); err != nil {
		return Post{}, err
	}
	if err := tx.Commit(); err != nil {
		return Post{}, fmt.Errorf("commit attachment deletion: %w", err)
	}
	return s.GetPost(ctx, postID)
}

func (s *Store) ListPostAttachmentsForUser(ctx context.Context, userID string, postID int64) ([]PostAttachment, error) {
	if _, err := s.getPostForUserWithoutAttachments(ctx, userID, postID); err != nil {
		return nil, err
	}
	return queryPostAttachments(ctx, s.db, `WHERE owner_id=? AND post_id=? ORDER BY position, id`, userID, postID)
}

// CachePostAttachmentProviderToken stores the reusable opaque MAX upload
// token only when the attachment still refers to the exact object that was
// uploaded. Provider cache writes deliberately do not advance updated_at:
// they are internal delivery metadata and must not make an unchanged editor
// snapshot stale.
func (s *Store) CachePostAttachmentProviderToken(
	ctx context.Context,
	userID string,
	postID, attachmentID int64,
	expectedStorageKey string,
	expectedUpdatedAt time.Time,
	token string,
) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("provider token is required")
	}
	result, err := s.db.ExecContext(ctx, bindSQL(`UPDATE post_attachments
SET provider_token=?, provider_token_expires_at=NULL
WHERE owner_id=? AND post_id=? AND id=? AND storage_key=? AND updated_at=?
  AND processing_status=?`), token, userID, postID, attachmentID, expectedStorageKey,
		expectedUpdatedAt.UTC(), AttachmentStatusReady)
	if err != nil {
		return fmt.Errorf("cache post attachment provider token: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("%w: attachment changed while its MAX upload token was being cached", ErrConflict)
	}
	return nil
}

func (s *Store) hydratePostAttachments(ctx context.Context, posts []Post) error {
	if len(posts) == 0 {
		return nil
	}
	args := make([]any, 0, len(posts))
	placeholders := make([]string, len(posts))
	for index := range posts {
		placeholders[index] = "?"
		args = append(args, posts[index].ID)
		posts[index].Attachments = []PostAttachment{}
	}
	attachments, err := queryPostAttachments(ctx, s.db,
		`WHERE post_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY post_id, position, id`, args...)
	if err != nil {
		return err
	}
	byPost := make(map[int64][]PostAttachment, len(posts))
	for _, attachment := range attachments {
		byPost[attachment.PostID] = append(byPost[attachment.PostID], attachment)
	}
	for index := range posts {
		if list := byPost[posts[index].ID]; list != nil {
			posts[index].Attachments = list
		}
	}
	return nil
}

type attachmentRows interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryPostAttachments(ctx context.Context, queryer attachmentRows, suffix string, args ...any) ([]PostAttachment, error) {
	rows, err := queryer.QueryContext(ctx, bindSQL(`SELECT id, owner_id, post_id, type, position, storage_key,
processing_status, size_bytes, mime_type, width, height, duration_ms, provider_token,
provider_token_expires_at, provider_meta, error_code, created_at, updated_at
FROM post_attachments `+suffix), args...)
	if err != nil {
		return nil, fmt.Errorf("list post attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	attachments := make([]PostAttachment, 0)
	for rows.Next() {
		attachment, err := scanPostAttachment(rows)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

func scanPostAttachment(row scanner) (PostAttachment, error) {
	var attachment PostAttachment
	var width, height, duration sql.NullInt64
	var providerExpires sql.NullTime
	var providerMeta []byte
	if err := row.Scan(&attachment.ID, &attachment.OwnerID, &attachment.PostID, &attachment.Type, &attachment.Position,
		&attachment.StorageKey, &attachment.ProcessingStatus, &attachment.SizeBytes, &attachment.MIMEType,
		&width, &height, &duration, &attachment.ProviderToken, &providerExpires, &providerMeta, &attachment.ErrorCode,
		&attachment.CreatedAt, &attachment.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PostAttachment{}, ErrNotFound
		}
		return PostAttachment{}, fmt.Errorf("scan post attachment: %w", err)
	}
	if width.Valid {
		value := int(width.Int64)
		attachment.Width = &value
	}
	if height.Valid {
		value := int(height.Int64)
		attachment.Height = &value
	}
	if duration.Valid {
		value := duration.Int64
		attachment.DurationMS = &value
	}
	attachment.ProviderExpires = parseNullableTime(providerExpires)
	attachment.ProviderMeta = append(json.RawMessage(nil), providerMeta...)
	attachment.URL = "/media/" + url.PathEscape(attachment.StorageKey)
	attachment.CreatedAt = attachment.CreatedAt.UTC()
	attachment.UpdatedAt = attachment.UpdatedAt.UTC()
	return attachment, nil
}

type lockedPostAttachmentState struct {
	status      string
	linkButtons []byte
	updatedAt   time.Time
}

func lockPostAttachmentState(ctx context.Context, tx *sql.Tx, userID string, postID int64) (lockedPostAttachmentState, error) {
	var state lockedPostAttachmentState
	err := tx.QueryRowContext(ctx, bindSQL(`SELECT status, link_buttons, updated_at FROM posts
WHERE owner_id=? AND id=? FOR UPDATE`), userID, postID).Scan(&state.status, &state.linkButtons, &state.updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return lockedPostAttachmentState{}, ErrNotFound
	}
	if err != nil {
		return lockedPostAttachmentState{}, fmt.Errorf("lock post attachments: %w", err)
	}
	return state, nil
}

func validateAttachmentWriteState(status string) error {
	if status == PostStatusPublishing {
		return fmt.Errorf("%w: post is currently publishing", ErrConflict)
	}
	return nil
}

func maxAttachmentsForButtons(encoded []byte) int {
	var buttons []json.RawMessage
	if json.Unmarshal(encoded, &buttons) == nil && len(buttons) > 0 {
		return MaxPostAttachmentsWithKeyboard
	}
	return MaxPostAttachments
}

func validateAttachmentObject(ctx context.Context, tx *sql.Tx, userID string, attachment PostAttachment) error {
	if attachment.Type != PostAttachmentImage && attachment.Type != PostAttachmentVideo {
		return errors.New("attachment type must be image or video")
	}
	if strings.TrimSpace(attachment.StorageKey) == "" || attachment.SizeBytes < 0 || strings.TrimSpace(attachment.MIMEType) == "" {
		return errors.New("attachment storage key, MIME type and non-negative size are required")
	}
	if attachment.Type == PostAttachmentImage && !strings.HasPrefix(attachment.MIMEType, "image/") {
		return errors.New("image attachment must use an image MIME type")
	}
	if attachment.Type == PostAttachmentVideo && !strings.HasPrefix(attachment.MIMEType, "video/") {
		return errors.New("video attachment must use a video MIME type")
	}
	var ready bool
	if err := tx.QueryRowContext(ctx, bindSQL(`SELECT EXISTS(SELECT 1 FROM media_assets
WHERE owner_id=? AND filename=? AND state='ready')`), userID, attachment.StorageKey).Scan(&ready); err != nil {
		return fmt.Errorf("check attachment media ownership: %w", err)
	}
	if !ready {
		return ErrNotFound
	}
	return nil
}

func listAttachmentIDsTx(ctx context.Context, tx *sql.Tx, userID string, postID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, bindSQL(`SELECT id FROM post_attachments
WHERE owner_id=? AND post_id=? ORDER BY position, id FOR UPDATE`), userID, postID)
	if err != nil {
		return nil, fmt.Errorf("lock post attachment order: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func writeAttachmentOrderTx(ctx context.Context, tx *sql.Tx, userID string, postID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	// The unique order constraint is immediate. Move rows outside the valid
	// gallery range first, then assign the compact 0..n-1 positions.
	if _, err := tx.ExecContext(ctx, bindSQL(`UPDATE post_attachments SET position=position+1000
WHERE owner_id=? AND post_id=?`), userID, postID); err != nil {
		return fmt.Errorf("stage post attachment order: %w", err)
	}
	for position, id := range ids {
		result, err := tx.ExecContext(ctx, bindSQL(`UPDATE post_attachments SET position=?, updated_at=CURRENT_TIMESTAMP
WHERE owner_id=? AND post_id=? AND id=?`), position, userID, postID, id)
		if err != nil {
			return fmt.Errorf("write post attachment order: %w", err)
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrNotFound
		}
	}
	return nil
}

func syncPostAttachmentProjectionTx(ctx context.Context, tx *sql.Tx, userID string, postID int64, now time.Time) error {
	var key string
	err := tx.QueryRowContext(ctx, bindSQL(`SELECT storage_key FROM post_attachments
WHERE owner_id=? AND post_id=? AND type='image' AND processing_status='ready'
ORDER BY position, id LIMIT 1`), userID, postID).Scan(&key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read first image attachment: %w", err)
	}
	imageURL := ""
	if key != "" {
		imageURL = "/media/" + url.PathEscape(key)
	}
	if _, err := tx.ExecContext(ctx, bindSQL(`UPDATE posts SET image_url=?, image_path=?, updated_at=?
WHERE owner_id=? AND id=?`), imageURL, key, now.UTC(), userID, postID); err != nil {
		return fmt.Errorf("sync legacy image projection: %w", err)
	}
	return nil
}

func sameAttachmentIDs(current, ordered []int64) bool {
	if len(current) != len(ordered) {
		return false
	}
	left := append([]int64(nil), current...)
	right := append([]int64(nil), ordered...)
	sort.Slice(left, func(i, j int) bool { return left[i] < left[j] })
	sort.Slice(right, func(i, j int) bool { return right[i] < right[j] })
	for index := range left {
		if left[index] != right[index] || (index > 0 && right[index] == right[index-1]) {
			return false
		}
	}
	return true
}

func normalizedProcessingStatus(status string) string {
	if strings.TrimSpace(status) == "" {
		return AttachmentStatusReady
	}
	return status
}

func normalizedProviderMeta(value json.RawMessage) string {
	if len(value) == 0 || !json.Valid(value) {
		return "{}"
	}
	return string(value)
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
