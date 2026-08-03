package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	MAXHistoryImportStatusInProgress = "in_progress"
	MAXHistoryImportStatusComplete   = "complete"
	MAXHistoryImportStatusFailed     = "failed"

	maxHistoryRemoteURLLimit = 4096
)

const maxHistoryImportColumns = `workspace_id,channel_id,generation,run_id,status,cursor_from,
expected_count,processed_count,imported_count,existing_count,skipped_count,
claimed_at,completed_at,previous_completed_at,error_code,created_at,updated_at`

// MAXHistoryImport is the durable progress and fencing snapshot for one
// workspace channel. Channel is returned by Claim so the provider request is
// bound to the same tenant-scoped channel row that was locked by the store.
type MAXHistoryImport struct {
	WorkspaceID         string     `json:"workspace_id"`
	ChannelID           int64      `json:"channel_id"`
	Channel             Channel    `json:"channel"`
	Generation          int64      `json:"generation"`
	RunID               int64      `json:"-"`
	Status              string     `json:"status"`
	CursorFrom          *int64     `json:"cursor_from,omitempty"`
	ExpectedCount       int64      `json:"expected_count"`
	ProcessedCount      int64      `json:"processed_count"`
	ImportedCount       int64      `json:"imported_count"`
	ExistingCount       int64      `json:"existing_count"`
	SkippedCount        int64      `json:"skipped_count"`
	ClaimedAt           *time.Time `json:"claimed_at,omitempty"`
	CompletedAt         *time.Time `json:"completed_at,omitempty"`
	PreviousCompletedAt *time.Time `json:"previous_completed_at,omitempty"`
	ErrorCode           string     `json:"error_code,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// MAXHistoryItem is the normalized, provider-independent representation of a
// MAX history message. Raw provider JSON and media tokens never leave the
// store through Post's public JSON representation.
type MAXHistoryItem struct {
	Title       string
	Content     string
	URL         string
	MessageID   string
	PublishedAt time.Time
	Views       *int64
	SenderIsBot *bool
	RoundTrip   bool
	LinkButtons []LinkButton
	Raw         json.RawMessage
	Attachments []MAXHistoryAttachment
}

// MAXHistoryAttachment retains the provider token required to edit or
// republish a supported remote MAX attachment without pretending that it is a
// local media object.
type MAXHistoryAttachment struct {
	Type          string
	ProviderToken string
	RemoteURL     string
	MIMEType      string
	SizeBytes     int64
	Width         *int
	Height        *int
	DurationMS    *int64
	ProviderMeta  json.RawMessage
}

// ClaimMAXHistoryImport acquires a durable provider-call lease. A completed
// run starts from the newest page with cleared counters. Failed, expired and
// page-boundary (unclaimed in_progress) runs resume their existing cursor.
// Every successful claim receives a new global generation, fencing all older
// workers even if their provider response arrives later.
func (s *Store) ClaimMAXHistoryImport(
	ctx context.Context,
	actorUserID, workspaceID string,
	channelID int64,
	now time.Time,
	lease time.Duration,
) (MAXHistoryImport, error) {
	if strings.TrimSpace(actorUserID) == "" || strings.TrimSpace(workspaceID) == "" ||
		channelID <= 0 || now.IsZero() || lease <= 0 {
		return MAXHistoryImport{}, errors.New("MAX history claim requires actor, workspace, channel, now and a positive lease")
	}
	now = now.UTC().Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXHistoryImport{}, err
	}
	defer func() { _ = tx.Rollback() }()

	access, err := resolveWorkspaceAccess(ctx, tx, actorUserID, workspaceID)
	if err != nil {
		return MAXHistoryImport{}, err
	}
	if !access.Capabilities.EditContent {
		return MAXHistoryImport{}, ErrNotFound
	}
	if _, err := lockActiveWorkspaceForMAXHistoryWrite(ctx, tx, workspaceID); err != nil {
		return MAXHistoryImport{}, err
	}
	channel, err := scanChannel(tx.QueryRowContext(ctx, `SELECT `+channelColumns+`
FROM channels
WHERE workspace_id=$1 AND id=$2 AND active AND is_channel
FOR UPDATE`, workspaceID, channelID))
	if err != nil {
		return MAXHistoryImport{}, err
	}

	current, err := scanMAXHistoryImport(tx.QueryRowContext(ctx, `SELECT `+maxHistoryImportColumns+`
FROM channel_history_imports
WHERE workspace_id=$1 AND channel_id=$2
FOR UPDATE`, workspaceID, channelID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return MAXHistoryImport{}, err
	}
	if err == nil && current.ClaimedAt != nil && current.ClaimedAt.After(now.Add(-lease)) {
		return MAXHistoryImport{}, ErrConflict
	}

	var generation, candidateRunID int64
	if err := tx.QueryRowContext(ctx, `SELECT nextval('max_history_import_generation_seq'),
nextval('max_history_import_run_seq')`).Scan(&generation, &candidateRunID); err != nil {
		return MAXHistoryImport{}, fmt.Errorf("allocate MAX history import generation: %w", err)
	}
	expectedCount := int64(channel.MessagesCount)
	if errors.Is(err, ErrNotFound) {
		_, err = tx.ExecContext(ctx, `INSERT INTO channel_history_imports(
workspace_id,channel_id,generation,run_id,status,cursor_from,expected_count,
processed_count,imported_count,existing_count,skipped_count,claimed_at,
completed_at,previous_completed_at,error_code,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,NULL,$6,0,0,0,0,$7,NULL,NULL,'',$7,$7)`,
			workspaceID, channelID, generation, candidateRunID,
			MAXHistoryImportStatusInProgress, expectedCount, now)
		if err != nil {
			return MAXHistoryImport{}, fmt.Errorf("create MAX history import: %w", err)
		}
	} else {
		runID := current.RunID
		cursorFrom := current.CursorFrom
		processedCount := current.ProcessedCount
		importedCount := current.ImportedCount
		existingCount := current.ExistingCount
		skippedCount := current.SkippedCount
		previousCompletedAt := current.PreviousCompletedAt
		if current.ExpectedCount > expectedCount {
			expectedCount = current.ExpectedCount
		}
		if current.Status == MAXHistoryImportStatusComplete {
			runID = candidateRunID
			cursorFrom = nil
			processedCount = 0
			importedCount = 0
			existingCount = 0
			skippedCount = 0
			previousCompletedAt = current.CompletedAt
			expectedCount = int64(channel.MessagesCount)
		}
		_, err = tx.ExecContext(ctx, `UPDATE channel_history_imports SET
generation=$1,run_id=$2,status=$3,cursor_from=$4,expected_count=$5,
processed_count=$6,imported_count=$7,existing_count=$8,skipped_count=$9,
claimed_at=$10,completed_at=NULL,previous_completed_at=$11,error_code='',updated_at=$10
WHERE workspace_id=$12 AND channel_id=$13`, generation, runID, MAXHistoryImportStatusInProgress,
			nullableInt64(cursorFrom), expectedCount, processedCount, importedCount, existingCount, skippedCount,
			now, nullableTime(previousCompletedAt), workspaceID, channelID)
		if err != nil {
			return MAXHistoryImport{}, fmt.Errorf("resume MAX history import: %w", err)
		}
	}

	claimed, err := scanMAXHistoryImport(tx.QueryRowContext(ctx, `SELECT `+maxHistoryImportColumns+`
FROM channel_history_imports WHERE workspace_id=$1 AND channel_id=$2`, workspaceID, channelID))
	if err != nil {
		return MAXHistoryImport{}, err
	}
	claimed.Channel = channel
	if err := tx.Commit(); err != nil {
		return MAXHistoryImport{}, fmt.Errorf("commit MAX history import claim: %w", err)
	}
	return claimed, nil
}

// ApplyMAXHistoryPage atomically materializes one claimed provider page,
// advances its cursor, creates collaboration revisions, and writes one bulk
// audit event. It releases the page lease even when more pages remain, so the
// next HTTP batch can immediately claim a fresh fenced generation.
func (s *Store) ApplyMAXHistoryPage(
	ctx context.Context,
	actorUserID, workspaceID string,
	generation int64,
	items []MAXHistoryItem,
	nextFrom *int64,
	complete bool,
	now time.Time,
) (MAXHistoryImport, error) {
	if strings.TrimSpace(actorUserID) == "" || strings.TrimSpace(workspaceID) == "" || now.IsZero() {
		return MAXHistoryImport{}, errors.New("MAX history page requires actor, workspace and now")
	}
	if generation <= 0 {
		return MAXHistoryImport{}, ErrConflict
	}
	if !complete && nextFrom == nil {
		return MAXHistoryImport{}, errors.New("an incomplete MAX history page requires next_from")
	}
	if nextFrom != nil && *nextFrom < 0 {
		return MAXHistoryImport{}, errors.New("MAX history next_from must not be negative")
	}
	normalized, err := normalizeMAXHistoryItems(items)
	if err != nil {
		return MAXHistoryImport{}, err
	}
	now = now.UTC().Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXHistoryImport{}, err
	}
	defer func() { _ = tx.Rollback() }()

	access, err := resolveWorkspaceAccess(ctx, tx, actorUserID, workspaceID)
	if err != nil {
		return MAXHistoryImport{}, err
	}
	if !access.Capabilities.EditContent {
		return MAXHistoryImport{}, ErrNotFound
	}
	compatOwnerID, err := lockActiveWorkspaceForMAXHistoryWrite(ctx, tx, workspaceID)
	if err != nil {
		return MAXHistoryImport{}, err
	}
	channelID, err := maxHistoryChannelIDForGeneration(ctx, tx, workspaceID, generation)
	if err != nil {
		return MAXHistoryImport{}, err
	}
	channel, err := scanChannel(tx.QueryRowContext(ctx, `SELECT `+channelColumns+`
FROM channels
WHERE workspace_id=$1 AND id=$2 AND active AND is_channel
FOR UPDATE`, workspaceID, channelID))
	if err != nil {
		return MAXHistoryImport{}, err
	}
	progress, err := scanMAXHistoryImport(tx.QueryRowContext(ctx, `SELECT `+maxHistoryImportColumns+`
FROM channel_history_imports
WHERE workspace_id=$1 AND channel_id=$2 AND generation=$3
FOR UPDATE`, workspaceID, channelID, generation))
	if errors.Is(err, ErrNotFound) || progress.Status != MAXHistoryImportStatusInProgress || progress.ClaimedAt == nil {
		return MAXHistoryImport{}, ErrConflict
	}
	if err != nil {
		return MAXHistoryImport{}, err
	}
	var publicationInProgress bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM posts
WHERE workspace_id=$1 AND channel_id=$2 AND status=$3
)`, workspaceID, channelID, PostStatusPublishing).Scan(&publicationInProgress); err != nil {
		return MAXHistoryImport{}, fmt.Errorf("check active MAX publication before history import: %w", err)
	}
	if publicationInProgress {
		return MAXHistoryImport{}, fmt.Errorf("%w: channel has a MAX publication in progress", ErrConflict)
	}

	var pageImported, pageExisting, pageSkipped int64
	priorRunOverlap := false
	for index := range normalized {
		item := normalized[index]
		classification, importErr := applyMAXHistoryItemTx(
			ctx, tx, actorUserID, compatOwnerID, workspaceID, channelID,
			progress.RunID, item, now,
		)
		if importErr != nil {
			return MAXHistoryImport{}, fmt.Errorf("apply MAX history item %d: %w", index, importErr)
		}
		switch classification {
		case maxHistoryItemImported:
			pageImported++
		case maxHistoryItemExisting:
			pageExisting++
		case maxHistoryItemPriorRunMapping:
			pageExisting++
			priorRunOverlap = true
		case maxHistoryItemSkipped:
			pageSkipped++
		case maxHistoryItemBoundaryRepeat:
			// The MAX cursor is inclusive. The message at the page boundary
			// is refreshed above but is not processed for a second time.
		}
	}

	pageProcessed := pageImported + pageExisting + pageSkipped
	processedCount := progress.ProcessedCount + pageProcessed
	importedCount := progress.ImportedCount + pageImported
	existingCount := progress.ExistingCount + pageExisting
	skippedCount := progress.SkippedCount + pageSkipped
	expectedCount := progress.ExpectedCount
	if processedCount > expectedCount {
		expectedCount = processedCount
	}
	overlapComplete := !complete && progress.PreviousCompletedAt != nil && priorRunOverlap
	effectiveComplete := complete || overlapComplete
	if overlapComplete {
		// An incremental run stops at the first message already seen by the
		// preceding completed run. Its expected work is the scanned delta, not
		// the channel's all-time message count.
		expectedCount = processedCount
	}
	status := MAXHistoryImportStatusInProgress
	var completedAt any
	cursorFrom := nextFrom
	if effectiveComplete {
		status = MAXHistoryImportStatusComplete
		completedAt = now
		cursorFrom = nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE channel_history_imports SET
status=$1,cursor_from=$2,expected_count=$3,processed_count=$4,
imported_count=$5,existing_count=$6,skipped_count=$7,claimed_at=NULL,
completed_at=$8,error_code='',updated_at=$9
WHERE workspace_id=$10 AND channel_id=$11 AND generation=$12
  AND status=$13 AND claimed_at IS NOT NULL`, status, nullableInt64(cursorFrom), expectedCount,
		processedCount, importedCount, existingCount, skippedCount, completedAt, now,
		workspaceID, channelID, generation, MAXHistoryImportStatusInProgress)
	if err != nil {
		return MAXHistoryImport{}, fmt.Errorf("advance MAX history import: %w", err)
	}

	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID,
		ActorUserID: actorUserID,
		Action:      "max.history.page_imported",
		EntityType:  "channel",
		EntityID:    fmt.Sprint(channelID),
		Metadata: mustJSON(map[string]any{
			"generation": generation, "run_id": progress.RunID, "complete": effectiveComplete,
			"provider_complete": complete, "prior_run_overlap": priorRunOverlap,
			"page_processed_count": pageProcessed, "page_imported_count": pageImported,
			"page_existing_count": pageExisting, "page_skipped_count": pageSkipped,
			"processed_count": processedCount, "imported_count": importedCount,
			"existing_count": existingCount, "skipped_count": skippedCount,
		}),
		CreatedAt: now,
	}); err != nil {
		return MAXHistoryImport{}, err
	}

	updated, err := scanMAXHistoryImport(tx.QueryRowContext(ctx, `SELECT `+maxHistoryImportColumns+`
FROM channel_history_imports
WHERE workspace_id=$1 AND channel_id=$2 AND generation=$3`, workspaceID, channelID, generation))
	if err != nil {
		return MAXHistoryImport{}, err
	}
	updated.Channel = channel
	if err := tx.Commit(); err != nil {
		return MAXHistoryImport{}, fmt.Errorf("commit MAX history page: %w", err)
	}
	return updated, nil
}

// ReleaseMAXHistoryImport records a provider/application failure only for the
// exact still-claimed generation. A later claim resumes its cursor and
// counters rather than restarting the channel.
func (s *Store) ReleaseMAXHistoryImport(
	ctx context.Context,
	actorUserID, workspaceID string,
	generation int64,
	errorCode string,
	now time.Time,
) error {
	errorCode = strings.TrimSpace(errorCode)
	if strings.TrimSpace(actorUserID) == "" || strings.TrimSpace(workspaceID) == "" ||
		generation <= 0 || errorCode == "" || now.IsZero() {
		return ErrConflict
	}
	if len(errorCode) > 128 {
		return errors.New("MAX history error_code must not exceed 128 bytes")
	}
	now = now.UTC().Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	access, err := resolveWorkspaceAccess(ctx, tx, actorUserID, workspaceID)
	if err != nil {
		return err
	}
	if !access.Capabilities.EditContent {
		return ErrNotFound
	}
	if _, err := lockActiveWorkspaceForMAXHistoryWrite(ctx, tx, workspaceID); err != nil {
		return err
	}
	var channelID int64
	err = tx.QueryRowContext(ctx, `SELECT channel_id FROM channel_history_imports
WHERE workspace_id=$1 AND generation=$2`, workspaceID, generation).Scan(&channelID)
	if errors.Is(err, sql.ErrNoRows) {
		// A newer claim replaced this generation. Releasing an old worker is
		// deliberately a no-op and cannot affect the new lease.
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve MAX history generation for release: %w", err)
	}
	var lockedChannelID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM channels
WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, channelID).Scan(&lockedChannelID); errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	} else if err != nil {
		return fmt.Errorf("lock MAX history channel for release: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE channel_history_imports SET
status=$1,claimed_at=NULL,completed_at=$2,error_code=$3,updated_at=$2
WHERE workspace_id=$4 AND channel_id=$5 AND generation=$6
  AND status=$7 AND claimed_at IS NOT NULL`, MAXHistoryImportStatusFailed, now, errorCode,
		workspaceID, channelID, generation, MAXHistoryImportStatusInProgress)
	if err != nil {
		return fmt.Errorf("release MAX history import: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		// Apply may already have committed and released this exact lease.
		// Best-effort cleanup must stay idempotent in that uncertain outcome.
		return nil
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit MAX history import release: %w", err)
	}
	return nil
}

type maxHistoryItemClassification uint8

const (
	maxHistoryItemImported maxHistoryItemClassification = iota + 1
	maxHistoryItemExisting
	maxHistoryItemPriorRunMapping
	maxHistoryItemSkipped
	maxHistoryItemBoundaryRepeat
)

func applyMAXHistoryItemTx(
	ctx context.Context,
	tx *sql.Tx,
	actorUserID, ownerID, workspaceID string,
	channelID, runID int64,
	item MAXHistoryItem,
	now time.Time,
) (maxHistoryItemClassification, error) {
	var mappedPostID, mappedRunID int64
	err := tx.QueryRowContext(ctx, `SELECT post_id,last_import_run_id
FROM max_history_messages
WHERE workspace_id=$1 AND channel_id=$2 AND max_message_id=$3
FOR UPDATE`, workspaceID, channelID, item.MessageID).Scan(&mappedPostID, &mappedRunID)
	if err == nil {
		if mappedRunID == runID {
			return maxHistoryItemBoundaryRepeat, nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE max_history_messages SET
last_import_run_id=$1,raw=$2::jsonb,imported_at=$3
WHERE workspace_id=$4 AND channel_id=$5 AND max_message_id=$6`, runID, string(item.Raw), now,
			workspaceID, channelID, item.MessageID)
		if err != nil {
			return 0, fmt.Errorf("refresh existing MAX history mapping: %w", err)
		}
		return maxHistoryItemPriorRunMapping, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("lookup MAX history mapping: %w", err)
	}

	var existingPostID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM posts
WHERE workspace_id=$1 AND channel_id=$2 AND max_message_id=$3
FOR UPDATE`, workspaceID, channelID, item.MessageID).Scan(&existingPostID)
	if err == nil {
		if err := insertMAXHistoryMappingTx(ctx, tx, workspaceID, channelID, existingPostID, runID, item, now); err != nil {
			return 0, err
		}
		return maxHistoryItemExisting, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("lookup existing MAX publication: %w", err)
	}

	linkButtonsJSON, err := marshalLinkButtons(item.LinkButtons)
	if err != nil {
		return 0, err
	}
	imageURL := ""
	for _, attachment := range item.Attachments {
		if attachment.Type == PostAttachmentImage {
			imageURL = attachment.RemoteURL
			break
		}
	}
	var statsSyncedAt any
	if item.Views != nil {
		statsSyncedAt = now
	}
	var senderIsBot any
	if item.SenderIsBot != nil {
		senderIsBot = *item.SenderIsBot
	}
	var postID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO posts(
owner_id,workspace_id,title,content,format,status,channel_id,image_url,image_path,image_prompt,link_buttons,
notify,disable_link_preview,scheduled_at,max_message_id,max_message_url,max_views,max_stats_synced_at,
max_is_pinned,origin,max_history_attachments_complete,max_sender_is_bot,last_error,published_at,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'','',$9,FALSE,FALSE,NULL,$10,$11,$12,$13,
FALSE,$14,$15,$16,'',$17,$17,$17)
ON CONFLICT DO NOTHING
RETURNING id`, ownerID, workspaceID, item.Title, item.Content, FormatMarkdown, PostStatusPublished,
		channelID, imageURL, linkButtonsJSON, item.MessageID, item.URL, nullableInt64(item.Views), statsSyncedAt,
		PostOriginMAXHistory, item.RoundTrip, senderIsBot, item.PublishedAt).Scan(&postID)
	if errors.Is(err, sql.ErrNoRows) {
		if lookupErr := tx.QueryRowContext(ctx, `SELECT id FROM posts
WHERE workspace_id=$1 AND channel_id=$2 AND max_message_id=$3
FOR UPDATE`, workspaceID, channelID, item.MessageID).Scan(&existingPostID); lookupErr != nil {
			return 0, fmt.Errorf("resolve concurrent MAX publication collision: %w", lookupErr)
		}
		if err := insertMAXHistoryMappingTx(ctx, tx, workspaceID, channelID, existingPostID, runID, item, now); err != nil {
			return 0, err
		}
		return maxHistoryItemExisting, nil
	}
	if err != nil {
		return 0, fmt.Errorf("create MAX history post: %w", err)
	}

	for position, attachment := range item.Attachments {
		_, err := tx.ExecContext(ctx, `INSERT INTO max_history_post_attachments(
owner_id,workspace_id,post_id,type,position,processing_status,size_bytes,mime_type,
width,height,duration_ms,provider_token,remote_url,provider_meta,error_code,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb,'',$15,$15)`,
			ownerID, workspaceID, postID, attachment.Type, position,
			AttachmentStatusReady, attachment.SizeBytes, attachment.MIMEType,
			nullableInt(attachment.Width), nullableInt(attachment.Height), nullableInt64(attachment.DurationMS),
			attachment.ProviderToken, attachment.RemoteURL, string(attachment.ProviderMeta), now)
		if err != nil {
			return 0, fmt.Errorf("create MAX history attachment: %w", err)
		}
	}
	if _, err := createPostRevisionTx(ctx, tx, actorUserID, workspaceID, postID, now); err != nil {
		return 0, err
	}
	if err := insertMAXHistoryMappingTx(ctx, tx, workspaceID, channelID, postID, runID, item, now); err != nil {
		return 0, err
	}
	return maxHistoryItemImported, nil
}

func insertMAXHistoryMappingTx(
	ctx context.Context,
	tx *sql.Tx,
	workspaceID string,
	channelID, postID, runID int64,
	item MAXHistoryItem,
	now time.Time,
) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO max_history_messages(
workspace_id,channel_id,post_id,max_message_id,last_import_run_id,raw,imported_at)
VALUES($1,$2,$3,$4,$5,$6::jsonb,$7)`, workspaceID, channelID, postID,
		item.MessageID, runID, string(item.Raw), now)
	if err != nil {
		return fmt.Errorf("create MAX history mapping: %w", err)
	}
	return nil
}

func maxHistoryChannelIDForGeneration(
	ctx context.Context,
	tx *sql.Tx,
	workspaceID string,
	generation int64,
) (int64, error) {
	var channelID int64
	err := tx.QueryRowContext(ctx, `SELECT channel_id FROM channel_history_imports
WHERE workspace_id=$1 AND generation=$2`, workspaceID, generation).Scan(&channelID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, fmt.Errorf("resolve MAX history generation: %w", err)
	}
	return channelID, nil
}

func lockActiveWorkspaceForMAXHistoryWrite(
	ctx context.Context,
	tx *sql.Tx,
	workspaceID string,
) (string, error) {
	var compatOwnerID string
	err := tx.QueryRowContext(ctx, `SELECT compat_owner_user_id FROM workspaces
WHERE id=$1 AND archived_at IS NULL
FOR KEY SHARE`, workspaceID).Scan(&compatOwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock workspace for MAX history write: %w", err)
	}
	if strings.TrimSpace(compatOwnerID) == "" {
		return "", fmt.Errorf("%w: workspace has no compatibility owner", ErrConflict)
	}
	return compatOwnerID, nil
}

func scanMAXHistoryImport(row scanner) (MAXHistoryImport, error) {
	var result MAXHistoryImport
	var cursorFrom sql.NullInt64
	var claimedAt, completedAt, previousCompletedAt sql.NullTime
	err := row.Scan(
		&result.WorkspaceID, &result.ChannelID, &result.Generation, &result.RunID, &result.Status, &cursorFrom,
		&result.ExpectedCount, &result.ProcessedCount, &result.ImportedCount,
		&result.ExistingCount, &result.SkippedCount, &claimedAt, &completedAt,
		&previousCompletedAt, &result.ErrorCode, &result.CreatedAt, &result.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return MAXHistoryImport{}, ErrNotFound
	}
	if err != nil {
		return MAXHistoryImport{}, fmt.Errorf("scan MAX history import: %w", err)
	}
	if cursorFrom.Valid {
		value := cursorFrom.Int64
		result.CursorFrom = &value
	}
	result.ClaimedAt = parseNullableTime(claimedAt)
	result.CompletedAt = parseNullableTime(completedAt)
	result.PreviousCompletedAt = parseNullableTime(previousCompletedAt)
	result.CreatedAt = result.CreatedAt.UTC()
	result.UpdatedAt = result.UpdatedAt.UTC()
	return result, nil
}

func normalizeMAXHistoryItems(items []MAXHistoryItem) ([]MAXHistoryItem, error) {
	normalized := make([]MAXHistoryItem, len(items))
	seen := make(map[string]struct{}, len(items))
	for index, source := range items {
		item := source
		item.MessageID = strings.TrimSpace(item.MessageID)
		if item.MessageID == "" {
			return nil, fmt.Errorf("MAX history item %d has no message_id", index)
		}
		if _, duplicate := seen[item.MessageID]; duplicate {
			return nil, fmt.Errorf("MAX history page contains duplicate message_id %q", item.MessageID)
		}
		seen[item.MessageID] = struct{}{}
		if item.PublishedAt.IsZero() {
			return nil, fmt.Errorf("MAX history item %d has no published_at", index)
		}
		item.PublishedAt = item.PublishedAt.UTC().Truncate(time.Microsecond)
		item.Title = strings.TrimSpace(item.Title)
		item.URL = strings.TrimSpace(item.URL)
		if item.URL != "" {
			value, err := normalizeMAXHistoryRemoteURL(item.URL)
			if err != nil {
				return nil, fmt.Errorf("MAX history item %d URL: %w", index, err)
			}
			item.URL = value
		}
		if item.Views != nil {
			if *item.Views < 0 {
				return nil, fmt.Errorf("MAX history item %d has negative views", index)
			}
			value := *item.Views
			item.Views = &value
		}
		if item.SenderIsBot != nil {
			value := *item.SenderIsBot
			item.SenderIsBot = &value
		}
		raw, err := normalizeMAXHistoryObject(item.Raw)
		if err != nil {
			return nil, fmt.Errorf("MAX history item %d raw: %w", index, err)
		}
		item.Raw = raw
		item.LinkButtons = normalizeLinkButtons(append([]LinkButton(nil), item.LinkButtons...))
		if err := ValidateLinkButtonsForPublish(item.LinkButtons); err != nil {
			return nil, fmt.Errorf("MAX history item %d: %w", index, err)
		}
		mediaLimit := MaxPostAttachments
		if len(item.LinkButtons) > 0 {
			mediaLimit = MaxPostAttachmentsWithKeyboard
		}
		if len(item.Attachments) > mediaLimit {
			return nil, fmt.Errorf("MAX history item %d has more than %d media attachments", index, mediaLimit)
		}
		item.Attachments = append([]MAXHistoryAttachment(nil), item.Attachments...)
		for attachmentIndex := range item.Attachments {
			attachment, err := normalizeMAXHistoryAttachment(item.Attachments[attachmentIndex])
			if err != nil {
				return nil, fmt.Errorf("MAX history item %d attachment %d: %w", index, attachmentIndex, err)
			}
			item.Attachments[attachmentIndex] = attachment
		}
		normalized[index] = item
	}
	return normalized, nil
}

func normalizeMAXHistoryAttachment(attachment MAXHistoryAttachment) (MAXHistoryAttachment, error) {
	attachment.Type = strings.TrimSpace(attachment.Type)
	if attachment.Type != PostAttachmentImage && attachment.Type != PostAttachmentVideo {
		return MAXHistoryAttachment{}, errors.New("type must be image or video")
	}
	attachment.ProviderToken = strings.TrimSpace(attachment.ProviderToken)
	if attachment.ProviderToken == "" {
		return MAXHistoryAttachment{}, errors.New("provider token is required")
	}
	remoteURL, err := normalizeMAXHistoryRemoteURL(attachment.RemoteURL)
	if err != nil {
		return MAXHistoryAttachment{}, err
	}
	attachment.RemoteURL = remoteURL
	attachment.MIMEType = strings.TrimSpace(attachment.MIMEType)
	if attachment.MIMEType == "" {
		if attachment.Type == PostAttachmentImage {
			attachment.MIMEType = "image/jpeg"
		} else {
			attachment.MIMEType = "video/mp4"
		}
	}
	if attachment.Type == PostAttachmentImage && !strings.HasPrefix(attachment.MIMEType, "image/") {
		return MAXHistoryAttachment{}, errors.New("image attachment must use an image MIME type")
	}
	if attachment.Type == PostAttachmentVideo && !strings.HasPrefix(attachment.MIMEType, "video/") {
		return MAXHistoryAttachment{}, errors.New("video attachment must use a video MIME type")
	}
	if attachment.SizeBytes < 0 {
		return MAXHistoryAttachment{}, errors.New("size_bytes must not be negative")
	}
	if attachment.Width != nil {
		if *attachment.Width <= 0 {
			return MAXHistoryAttachment{}, errors.New("width must be positive")
		}
		value := *attachment.Width
		attachment.Width = &value
	}
	if attachment.Height != nil {
		if *attachment.Height <= 0 {
			return MAXHistoryAttachment{}, errors.New("height must be positive")
		}
		value := *attachment.Height
		attachment.Height = &value
	}
	if attachment.DurationMS != nil {
		if *attachment.DurationMS < 0 {
			return MAXHistoryAttachment{}, errors.New("duration_ms must not be negative")
		}
		value := *attachment.DurationMS
		attachment.DurationMS = &value
	}
	meta, err := normalizeMAXHistoryObject(attachment.ProviderMeta)
	if err != nil {
		return MAXHistoryAttachment{}, fmt.Errorf("provider metadata: %w", err)
	}
	attachment.ProviderMeta = meta
	return attachment, nil
}

func normalizeMAXHistoryObject(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var object map[string]any
	if err := json.Unmarshal(value, &object); err != nil || object == nil {
		return nil, errors.New("must be a JSON object")
	}
	return append(json.RawMessage(nil), value...), nil
}

func normalizeMAXHistoryRemoteURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxHistoryRemoteURLLimit || strings.IndexFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) >= 0 {
		return "", errors.New("remote URL is required and must be browser-safe")
	}
	parsed, err := url.Parse(value)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" ||
		parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("remote URL must be an absolute HTTPS URL without userinfo or fragment")
	}
	return parsed.String(), nil
}
