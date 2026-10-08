package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrMAXCommentsUnavailable = errors.New("MAX comments require a published channel post")
	ErrMAXCommentUncertain    = errors.New("MAX comment operation outcome is uncertain")
	ErrMAXCommentBusy         = errors.New("MAX comment operation is in progress")
	ErrMAXCommentValidation   = errors.New("invalid MAX comment request")
	maxCommentIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)
	maxCommentRequestPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{16,80}$`)
)

const MAXCommentsPageLimit = 100
const maxCommentOperationLease = 2 * time.Minute
const maxCommentColumns = `message_id,text,sender_user_id,sender_name,sender_is_bot,text_editable,reply_to_message_id,created_at,updated_at,observed_at,deleted_at,version`
const maxCommentOperationColumns = `operation_id,client_request_id,kind,state,message_id,request_hash,write_started,bot_user_id,expected_version,desired_text,desired_format,desired_reply_to,claimed_at,finished_at`

// MAXPostComment is an observed provider comment, never an editorial comment.
// Private provider identity is retained for exact bot authorship checks and
// distinct-author aggregates; it is not exposed as a browser identifier.
type MAXPostComment struct {
	MessageID        string     `json:"message_id"`
	Text             string     `json:"text"`
	SenderUserID     string     `json:"-"`
	SenderName       string     `json:"sender_name"`
	SenderIsBot      bool       `json:"sender_is_bot"`
	TextEditable     bool       `json:"-"`
	ReplyToMessageID string     `json:"reply_to_message_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	ObservedAt       time.Time  `json:"-"`
	DeletedAt        *time.Time `json:"deleted_at"`
	Version          int64      `json:"version"`
}

type MAXCommentOperation struct {
	OperationID     string     `json:"operation_id"`
	ClientRequestID string     `json:"client_request_id"`
	Kind            string     `json:"kind"`
	State           string     `json:"state"`
	MessageID       string     `json:"message_id,omitempty"`
	RequestHash     string     `json:"-"`
	WriteStarted    bool       `json:"-"`
	BotUserID       string     `json:"-"`
	ExpectedVersion int64      `json:"-"`
	DesiredText     string     `json:"-"`
	DesiredFormat   string     `json:"-"`
	DesiredReplyTo  string     `json:"-"`
	ClaimedAt       time.Time  `json:"-"`
	FinishedAt      *time.Time `json:"-"`
}

type MAXCommentMetrics struct {
	ObservedTotal         *int64   `json:"observed_total"`
	ObservedInbound       *int64   `json:"observed_inbound"`
	ObservedReplies       *int64   `json:"observed_replies"`
	ObservedDeleted       *int64   `json:"observed_deleted"`
	UniqueAuthors         *int64   `json:"unique_authors"`
	ResponseRate          *float64 `json:"response_rate"`
	MedianResponseSeconds *float64 `json:"median_response_seconds"`
	Total                 *int64   `json:"total"` // MAX does not document a complete total counter.
}

type MAXCommentCoverage struct {
	Kind         string     `json:"kind"`
	Complete     bool       `json:"complete"`
	PageLimit    int        `json:"page_limit"`
	LastSyncedAt *time.Time `json:"last_synced_at"`
	Truncated    bool       `json:"truncated"`
}

type MAXCommentsSnapshot struct {
	PostID        int64                 `json:"post_id"`
	RootMessageID string                `json:"root_message_id"`
	ChannelID     int64                 `json:"-"`
	Comments      []MAXPostComment      `json:"comments"`
	Operations    []MAXCommentOperation `json:"operations"`
	Coverage      MAXCommentCoverage    `json:"coverage"`
	Metrics       MAXCommentMetrics     `json:"metrics"`
}

type MAXCommentContext struct {
	Post    Post
	Channel Channel
}
type MAXCommentSyncClaim struct {
	MAXCommentContext
	Generation int64
	ClaimedAt  time.Time
}
type MAXCommentWriteRequest struct {
	ClientRequestID, RequestHash, Kind, MessageID, Text, Format, ReplyToMessageID, BotUserID string
	ExpectedVersion                                                                          int64
}

func ValidMAXCommentID(id string) bool        { return maxCommentIDPattern.MatchString(id) }
func ValidMAXCommentRequestID(id string) bool { return maxCommentRequestPattern.MatchString(id) }

// Parent-first locking serializes with membership revocation/archival before
// reading the role. The publication MID is rechecked on every write phase.
func maxCommentContextTx(ctx context.Context, tx *sql.Tx, actor, workspace string, postID int64, write bool) (MAXCommentContext, error) {
	if _, err := lockActiveWorkspaceForMAXHistoryWrite(ctx, tx, workspace); err != nil {
		return MAXCommentContext{}, err
	}
	if write {
		if err := requireWorkspaceRole(ctx, tx, actor, workspace, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
			return MAXCommentContext{}, err
		}
	} else if _, err := resolveWorkspaceAccess(ctx, tx, actor, workspace); err != nil {
		return MAXCommentContext{}, err
	}
	lock := " FOR SHARE"
	if write {
		lock = " FOR UPDATE"
	}
	post, err := scanPost(tx.QueryRowContext(ctx, `SELECT `+postColumns+` FROM posts WHERE workspace_id=$1 AND id=$2`+lock, workspace, postID))
	if err != nil {
		return MAXCommentContext{}, err
	}
	if post.Status != PostStatusPublished || !ValidMAXCommentID(post.MAXMessageID) || post.ChannelID == nil {
		return MAXCommentContext{}, ErrMAXCommentsUnavailable
	}
	channel, err := scanChannel(tx.QueryRowContext(ctx, `SELECT `+channelColumns+` FROM channels WHERE workspace_id=$1 AND id=$2 AND is_channel FOR SHARE`, workspace, *post.ChannelID))
	if err != nil {
		return MAXCommentContext{}, err
	}
	return MAXCommentContext{Post: post, Channel: channel}, nil
}

func (s *Store) GetMAXCommentContext(ctx context.Context, actor, workspace string, postID int64, write bool) (MAXCommentContext, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXCommentContext{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := maxCommentContextTx(ctx, tx, actor, workspace, postID, write)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func scanMAXComment(row scanner) (MAXPostComment, error) {
	var c MAXPostComment
	err := row.Scan(&c.MessageID, &c.Text, &c.SenderUserID, &c.SenderName, &c.SenderIsBot, &c.TextEditable, &c.ReplyToMessageID, &c.CreatedAt, &c.UpdatedAt, &c.ObservedAt, &c.DeletedAt, &c.Version)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return c, err
}
func scanMAXCommentOperation(row scanner) (MAXCommentOperation, error) {
	var o MAXCommentOperation
	err := row.Scan(&o.OperationID, &o.ClientRequestID, &o.Kind, &o.State, &o.MessageID, &o.RequestHash, &o.WriteStarted, &o.BotUserID, &o.ExpectedVersion, &o.DesiredText, &o.DesiredFormat, &o.DesiredReplyTo, &o.ClaimedAt, &o.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return o, err
}

func (s *Store) GetMAXCommentsSnapshot(ctx context.Context, actor, workspace string, postID int64, currentBotID string, now time.Time, requestedIDs ...string) (MAXCommentsSnapshot, error) {
	if len(requestedIDs) > 20 {
		return MAXCommentsSnapshot{}, ErrMAXCommentValidation
	}
	for _, id := range requestedIDs {
		if !ValidMAXCommentRequestID(id) {
			return MAXCommentsSnapshot{}, ErrMAXCommentValidation
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXCommentsSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := maxCommentContextTx(ctx, tx, actor, workspace, postID, false)
	if err != nil {
		return MAXCommentsSnapshot{}, err
	}
	result := MAXCommentsSnapshot{PostID: postID, RootMessageID: c.Post.MAXMessageID, ChannelID: c.Channel.ID, Comments: []MAXPostComment{}, Operations: []MAXCommentOperation{}, Coverage: MAXCommentCoverage{Kind: "latest_100", PageLimit: MAXCommentsPageLimit}}
	err = tx.QueryRowContext(ctx, `SELECT last_synced_at,truncated FROM max_post_comment_sync WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3`, workspace, postID, c.Post.MAXMessageID).Scan(&result.Coverage.LastSyncedAt, &result.Coverage.Truncated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	// A process that died after begin-write cannot make the thread writable by
	// letting its lease expire. Pre-write crashes are guaranteed not to send.
	_, err = tx.ExecContext(ctx, `UPDATE max_post_comment_operations SET state=CASE WHEN write_started THEN 'uncertain' ELSE 'rejected' END,finished_at=$4 WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND state='sending' AND claimed_at<$5`, workspace, postID, c.Post.MAXMessageID, now.UTC(), now.Add(-maxCommentOperationLease))
	if err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+maxCommentColumns+` FROM max_post_comments WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 ORDER BY created_at DESC,message_id LIMIT 100`, workspace, postID, c.Post.MAXMessageID)
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		item, e := scanMAXComment(rows)
		if e != nil {
			_ = rows.Close()
			return result, e
		}
		result.Comments = append(result.Comments, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return result, err
	}
	operationRows, err := tx.QueryContext(ctx, `SELECT `+maxCommentOperationColumns+` FROM max_post_comment_operations WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND (state IN ('sending','uncertain') OR client_request_id=ANY($4::text[]) OR operation_id IN (SELECT operation_id FROM max_post_comment_operations WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 ORDER BY claimed_at DESC LIMIT 10)) ORDER BY claimed_at DESC`, workspace, postID, c.Post.MAXMessageID, requestedIDs)
	if err != nil {
		return result, err
	}
	defer func() { _ = operationRows.Close() }()
	for operationRows.Next() {
		item, e := scanMAXCommentOperation(operationRows)
		if e != nil {
			_ = operationRows.Close()
			return result, e
		}
		result.Operations = append(result.Operations, item)
	}
	err = operationRows.Err()
	_ = operationRows.Close()
	if err != nil {
		return result, err
	}
	if result.Coverage.LastSyncedAt != nil {
		result.Metrics, err = getMAXCommentMetricsTx(ctx, tx, workspace, postID, c.Post.MAXMessageID, currentBotID)
		if err != nil {
			return result, err
		}
	}
	return result, tx.Commit()
}

// Metrics describe observed human comments and exact-current-bot replies.
// Deleted comments remain in historical counts, but not in active counts.
func BuildMAXCommentMetrics(items []MAXPostComment, botID string) MAXCommentMetrics {
	var total, inbound, replies, deleted int64
	authors := map[string]bool{}
	incoming := map[string]MAXPostComment{}
	first := map[string]float64{}
	for _, c := range items {
		if c.DeletedAt != nil {
			deleted++
			continue
		}
		total++
		if !c.SenderIsBot && c.SenderUserID != "" {
			inbound++
			incoming[c.MessageID] = c
			authors[c.SenderUserID] = true
		} else if botID != "" && c.SenderUserID == botID && c.ReplyToMessageID != "" {
			replies++
		}
	}
	for _, c := range items {
		if c.DeletedAt != nil || !c.SenderIsBot || botID == "" || c.SenderUserID != botID {
			continue
		}
		parent, ok := incoming[c.ReplyToMessageID]
		if !ok {
			continue
		}
		seconds := c.CreatedAt.Sub(parent.CreatedAt).Seconds()
		if seconds < 0 {
			continue
		}
		prior, seen := first[parent.MessageID]
		if !seen || seconds < prior {
			first[parent.MessageID] = seconds
		}
	}
	unique := int64(len(authors))
	m := MAXCommentMetrics{ObservedTotal: &total, ObservedInbound: &inbound, ObservedReplies: &replies, ObservedDeleted: &deleted, UniqueAuthors: &unique}
	if inbound > 0 {
		rate := float64(len(first)) / float64(inbound)
		m.ResponseRate = &rate
	}
	if len(first) > 0 {
		samples := make([]float64, 0, len(first))
		for _, n := range first {
			samples = append(samples, n)
		}
		sort.Float64s(samples)
		median := samples[len(samples)/2]
		if len(samples)%2 == 0 {
			median = (samples[len(samples)/2-1] + median) / 2
		}
		m.MedianResponseSeconds = &median
	}
	return m
}

func validateMAXComment(c MAXPostComment) error {
	if !ValidMAXCommentID(c.MessageID) || c.CreatedAt.IsZero() || c.ObservedAt.IsZero() || utf8.RuneCountInString(c.Text) > 4000 || utf8.RuneCountInString(c.SenderName) > 200 || len(c.SenderUserID) > 64 || c.ReplyToMessageID != "" && !ValidMAXCommentID(c.ReplyToMessageID) {
		return ErrMAXCommentValidation
	}
	return nil
}

func upsertMAXCommentTx(ctx context.Context, tx *sql.Tx, workspace string, postID, channelID int64, root string, c MAXPostComment) error {
	// MAX user names have no documented length bound. Bound only the display
	// label, preserving the exact private sender ID used for authorship.
	if utf8.RuneCountInString(c.SenderName) > 200 {
		c.SenderName = string([]rune(c.SenderName)[:200])
	}
	if err := validateMAXComment(c); err != nil {
		return err
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO max_post_comments(workspace_id,post_id,channel_id,root_message_id,message_id,text,sender_user_id,sender_name,sender_is_bot,text_editable,reply_to_message_id,created_at,updated_at,observed_at,deleted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT(workspace_id,post_id,root_message_id,message_id) DO UPDATE SET
text=CASE WHEN max_post_comments.deleted_at IS NULL THEN EXCLUDED.text ELSE max_post_comments.text END,
sender_user_id=CASE WHEN EXCLUDED.sender_user_id<>'' THEN EXCLUDED.sender_user_id ELSE max_post_comments.sender_user_id END,
sender_name=CASE WHEN EXCLUDED.sender_name<>'' THEN EXCLUDED.sender_name ELSE max_post_comments.sender_name END,
sender_is_bot=CASE WHEN EXCLUDED.sender_user_id<>'' THEN EXCLUDED.sender_is_bot ELSE max_post_comments.sender_is_bot END,
text_editable=EXCLUDED.text_editable,
reply_to_message_id=CASE WHEN EXCLUDED.reply_to_message_id<>'' THEN EXCLUDED.reply_to_message_id ELSE max_post_comments.reply_to_message_id END,
updated_at=GREATEST(max_post_comments.updated_at,EXCLUDED.updated_at),observed_at=EXCLUDED.observed_at,
deleted_at=COALESCE(max_post_comments.deleted_at,EXCLUDED.deleted_at),
version=max_post_comments.version+CASE WHEN (max_post_comments.deleted_at IS NULL AND max_post_comments.text IS DISTINCT FROM EXCLUDED.text) OR max_post_comments.deleted_at IS DISTINCT FROM COALESCE(max_post_comments.deleted_at,EXCLUDED.deleted_at) THEN 1 ELSE 0 END
WHERE max_post_comments.observed_at<=EXCLUDED.observed_at`, workspace, postID, channelID, root, c.MessageID, c.Text, c.SenderUserID, c.SenderName, c.SenderIsBot, c.TextEditable, c.ReplyToMessageID, c.CreatedAt.UTC(), c.UpdatedAt.UTC(), c.ObservedAt.UTC(), c.DeletedAt)
	return err
}

func (s *Store) ClaimMAXCommentSync(ctx context.Context, actor, workspace string, postID int64, now time.Time) (MAXCommentSyncClaim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXCommentSyncClaim{}, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := maxCommentContextTx(ctx, tx, actor, workspace, postID, false)
	if err != nil {
		return MAXCommentSyncClaim{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO max_post_comment_sync(workspace_id,post_id,root_message_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, workspace, postID, c.Post.MAXMessageID)
	if err != nil {
		return MAXCommentSyncClaim{}, err
	}
	var claimed, last *time.Time
	err = tx.QueryRowContext(ctx, `SELECT claimed_at,last_synced_at FROM max_post_comment_sync WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 FOR UPDATE`, workspace, postID, c.Post.MAXMessageID).Scan(&claimed, &last)
	if err != nil {
		return MAXCommentSyncClaim{}, err
	}
	if claimed != nil && claimed.After(now.Add(-time.Minute)) || last != nil && last.After(now.Add(-15*time.Second)) {
		return MAXCommentSyncClaim{}, ErrMAXCommentBusy
	}
	var generation int64
	err = tx.QueryRowContext(ctx, `UPDATE max_post_comment_sync SET generation=nextval('max_comments_sync_generation_seq'),claimed_at=$4 WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 RETURNING generation`, workspace, postID, c.Post.MAXMessageID, now.UTC()).Scan(&generation)
	if err != nil {
		return MAXCommentSyncClaim{}, err
	}
	return MAXCommentSyncClaim{MAXCommentContext: c, Generation: generation, ClaimedAt: now.UTC()}, tx.Commit()
}

func (s *Store) ApplyMAXCommentSync(ctx context.Context, actor, workspace string, claim MAXCommentSyncClaim, items []MAXPostComment, now time.Time) error {
	if len(items) > MAXCommentsPageLimit {
		return ErrMAXCommentValidation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := maxCommentContextTx(ctx, tx, actor, workspace, claim.Post.ID, false)
	if err != nil {
		return err
	}
	if c.Post.MAXMessageID != claim.Post.MAXMessageID || c.Channel.ID != claim.Channel.ID {
		return ErrConflict
	}
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT generation FROM max_post_comment_sync WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND claimed_at IS NOT NULL FOR UPDATE`, workspace, c.Post.ID, c.Post.MAXMessageID).Scan(&generation)
	if err != nil {
		return err
	}
	if generation != claim.Generation {
		return ErrConflict
	}
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item.MessageID] {
			return ErrMAXCommentValidation
		}
		seen[item.MessageID] = true
		item.ObservedAt = claim.ClaimedAt
		if err = upsertMAXCommentTx(ctx, tx, workspace, c.Post.ID, c.Channel.ID, c.Post.MAXMessageID, item); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE max_post_comment_sync SET claimed_at=NULL,last_synced_at=$4,truncated=$5 WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3`, workspace, c.Post.ID, c.Post.MAXMessageID, now.UTC(), len(items) == MAXCommentsPageLimit)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReleaseMAXCommentSync(ctx context.Context, workspace string, claim MAXCommentSyncClaim) error {
	_, err := s.db.ExecContext(ctx, `UPDATE max_post_comment_sync SET claimed_at=NULL WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND generation=$4`, workspace, claim.Post.ID, claim.Post.MAXMessageID, claim.Generation)
	return err
}

func (s *Store) ObserveMAXComment(ctx context.Context, actor, workspace string, postID int64, expectedRoot string, item MAXPostComment) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := maxCommentContextTx(ctx, tx, actor, workspace, postID, false)
	if err != nil {
		return err
	}
	if c.Post.MAXMessageID != expectedRoot {
		return ErrConflict
	}
	if err = upsertMAXCommentTx(ctx, tx, workspace, postID, c.Channel.ID, expectedRoot, item); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClaimMAXCommentOperation(ctx context.Context, actor, workspace string, postID int64, expectedRoot string, r MAXCommentWriteRequest, now time.Time) (MAXCommentOperation, bool, error) {
	if !ValidMAXCommentRequestID(r.ClientRequestID) || !directOAuthStateHashPattern.MatchString(r.RequestHash) || r.Kind != "send" && r.Kind != "edit" && r.Kind != "delete" {
		return MAXCommentOperation{}, false, ErrMAXCommentValidation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXCommentOperation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := maxCommentContextTx(ctx, tx, actor, workspace, postID, true)
	if err != nil {
		return MAXCommentOperation{}, false, err
	}
	if c.Post.MAXMessageID != expectedRoot {
		return MAXCommentOperation{}, false, ErrConflict
	}
	// maxCommentContextTx locks the post before checking outstanding operations.
	existing, err := scanMAXCommentOperation(tx.QueryRowContext(ctx, `SELECT `+maxCommentOperationColumns+` FROM max_post_comment_operations WHERE workspace_id=$1 AND post_id=$2 AND client_request_id=$3 FOR UPDATE`, workspace, postID, r.ClientRequestID))
	if err == nil {
		if existing.RequestHash != r.RequestHash {
			return existing, false, ErrConflict
		}
		return existing, false, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return existing, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE max_post_comment_operations SET state=CASE WHEN write_started THEN 'uncertain' ELSE 'rejected' END,finished_at=$4 WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND state='sending' AND claimed_at<$5`, workspace, postID, expectedRoot, now.UTC(), now.Add(-maxCommentOperationLease))
	if err != nil {
		return existing, false, err
	}
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM max_post_comment_operations WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND state IN ('sending','uncertain') AND (($4='send' AND kind='send') OR ($4<>'send' AND message_id=$5 AND kind IN ('edit','delete'))) ORDER BY claimed_at LIMIT 1`, workspace, postID, expectedRoot, r.Kind, r.MessageID).Scan(&state)
	if err == nil {
		if state == "uncertain" {
			return existing, false, ErrMAXCommentUncertain
		}
		return existing, false, ErrMAXCommentBusy
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return existing, false, err
	}
	if r.Kind != "send" {
		comment, e := scanMAXComment(tx.QueryRowContext(ctx, `SELECT `+maxCommentColumns+` FROM max_post_comments WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND message_id=$4 FOR UPDATE`, workspace, postID, expectedRoot, r.MessageID))
		if e != nil {
			return existing, false, e
		}
		if comment.DeletedAt != nil || comment.Version != r.ExpectedVersion {
			return existing, false, ErrConflict
		}
	}
	if r.ReplyToMessageID != "" {
		var found bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM max_post_comments WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND message_id=$4 AND deleted_at IS NULL)`, workspace, postID, expectedRoot, r.ReplyToMessageID).Scan(&found)
		if err != nil {
			return existing, false, err
		}
		if !found {
			return existing, false, ErrNotFound
		}
	}
	operation := newStoreID("mco_")
	_, err = tx.ExecContext(ctx, `INSERT INTO max_post_comment_operations(operation_id,workspace_id,post_id,channel_id,root_message_id,client_request_id,request_hash,actor_user_id,kind,state,message_id,expected_version,desired_text,desired_format,desired_reply_to,claimed_at,bot_user_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'sending',$10,$11,$12,$13,$14,$15,$16)`, operation, workspace, postID, c.Channel.ID, expectedRoot, r.ClientRequestID, r.RequestHash, actor, r.Kind, r.MessageID, r.ExpectedVersion, r.Text, r.Format, r.ReplyToMessageID, now.UTC(), r.BotUserID)
	if err != nil {
		return existing, false, err
	}
	if err = appendAuditEventTx(ctx, tx, AuditEvent{WorkspaceID: workspace, ActorUserID: actor, Action: "max.comment.claimed", EntityType: "post", EntityID: strconv.FormatInt(postID, 10), Metadata: mustJSON(map[string]any{"operation_id": operation, "kind": r.Kind}), CreatedAt: now.UTC()}); err != nil {
		return existing, false, err
	}
	existing, err = scanMAXCommentOperation(tx.QueryRowContext(ctx, `SELECT `+maxCommentOperationColumns+` FROM max_post_comment_operations WHERE operation_id=$1`, operation))
	if err != nil {
		return existing, false, err
	}
	return existing, true, tx.Commit()
}

func (s *Store) BeginMAXCommentWrite(ctx context.Context, actor, workspace string, postID int64, root, operation string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := maxCommentContextTx(ctx, tx, actor, workspace, postID, true)
	if err != nil {
		return err
	}
	if c.Post.MAXMessageID != root {
		return ErrConflict
	}
	o, err := scanMAXCommentOperation(tx.QueryRowContext(ctx, `SELECT `+maxCommentOperationColumns+` FROM max_post_comment_operations WHERE operation_id=$1 AND workspace_id=$2 AND post_id=$3 AND root_message_id=$4 FOR UPDATE`, operation, workspace, postID, root))
	if err != nil {
		return err
	}
	if o.Kind != "send" {
		var version int64
		err = tx.QueryRowContext(ctx, `SELECT version FROM max_post_comments WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND message_id=$4 AND deleted_at IS NULL`, workspace, postID, root, o.MessageID).Scan(&version)
		if err != nil {
			return ErrConflict
		}
		if version != o.ExpectedVersion {
			return ErrConflict
		}
	}
	if o.DesiredReplyTo != "" {
		var exists bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM max_post_comments WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND message_id=$4 AND deleted_at IS NULL)`, workspace, postID, root, o.DesiredReplyTo).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
			return ErrConflict
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE max_post_comment_operations SET write_started=TRUE WHERE operation_id=$1 AND workspace_id=$2 AND post_id=$3 AND root_message_id=$4 AND state='sending' AND NOT write_started AND claimed_at>$5`, operation, workspace, postID, root, now.Add(-maxCommentOperationLease))
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

// Finalization is private-operation fenced and detached from caller identity.
// A known result is durable even if membership changed during the HTTP call.
func (s *Store) FinishMAXCommentOperation(ctx context.Context, operation, state string, item *MAXPostComment, now time.Time) error {
	if state != "succeeded" && state != "rejected" && state != "uncertain" {
		return ErrMAXCommentValidation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var workspace, root, kind string
	var postID, channelID int64
	var messageID string
	// Match the claim's parent -> post -> operation lock order before taking
	// the operation lock. FK checks on the result row must not deadlock an
	// overlapping duplicate request that already holds the post lock.
	err = tx.QueryRowContext(ctx, `SELECT workspace_id,post_id FROM max_post_comment_operations WHERE operation_id=$1`, operation).Scan(&workspace, &postID)
	if err != nil {
		return err
	}
	var locked string
	err = tx.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR KEY SHARE`, workspace).Scan(&locked)
	if err != nil {
		return err
	}
	var lockedPost int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM posts WHERE workspace_id=$1 AND id=$2 FOR KEY SHARE`, workspace, postID).Scan(&lockedPost)
	if err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT workspace_id,post_id,channel_id,root_message_id,kind,message_id FROM max_post_comment_operations WHERE operation_id=$1 AND state IN ('sending','uncertain') FOR UPDATE`, operation).Scan(&workspace, &postID, &channelID, &root, &kind, &messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if item != nil {
		if kind != "send" && item.MessageID != messageID {
			return ErrConflict
		}
		if err = upsertMAXCommentTx(ctx, tx, workspace, postID, channelID, root, *item); err != nil {
			return err
		}
		messageID = item.MessageID
	}
	if state == "succeeded" && kind == "delete" {
		_, err = tx.ExecContext(ctx, `UPDATE max_post_comments SET deleted_at=COALESCE(deleted_at,$5),updated_at=GREATEST(updated_at,$5),observed_at=GREATEST(observed_at,$5),version=version+CASE WHEN deleted_at IS NULL THEN 1 ELSE 0 END WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND message_id=$4`, workspace, postID, root, messageID, now.UTC())
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE max_post_comment_operations SET state=$2,message_id=$3,finished_at=$4 WHERE operation_id=$1`, operation, state, messageID, now.UTC())
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Created/edited events have no documented root MID. Only an existing exact
// mapping may be updated; unknown dialog/group/channel messages are ignored.
func (s *Store) ObserveKnownMAXCommentEvent(ctx context.Context, chatID string, item MAXPostComment) error {
	if !ValidMAXCommentID(item.MessageID) {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT c.workspace_id,c.post_id,c.channel_id,c.root_message_id FROM max_post_comments c JOIN channels ch ON ch.workspace_id=c.workspace_id AND ch.id=c.channel_id JOIN posts p ON p.workspace_id=c.workspace_id AND p.id=c.post_id JOIN workspaces w ON w.id=c.workspace_id WHERE ch.max_chat_id=$1 AND c.message_id=$2 AND p.max_message_id=c.root_message_id AND w.archived_at IS NULL`, chatID, item.MessageID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type mapping struct {
		workspace, root string
		post, channel   int64
	}
	targets := []mapping{}
	for rows.Next() {
		var m mapping
		if err = rows.Scan(&m.workspace, &m.post, &m.channel, &m.root); err != nil {
			_ = rows.Close()
			return err
		}
		targets = append(targets, m)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, m := range targets {
		if _, err = lockActiveWorkspaceForMAXHistoryWrite(ctx, tx, m.workspace); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return err
		}
		if err = upsertMAXCommentTx(ctx, tx, m.workspace, m.post, m.channel, m.root, item); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ObserveMAXCommentRemoval(ctx context.Context, chatID, root, message string, eventAt time.Time) error {
	if !ValidMAXCommentID(root) || !ValidMAXCommentID(message) || eventAt.IsZero() {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT p.workspace_id,p.id,ch.id FROM posts p JOIN channels ch ON ch.workspace_id=p.workspace_id AND ch.id=p.channel_id JOIN workspaces w ON w.id=p.workspace_id WHERE ch.max_chat_id=$1 AND p.max_message_id=$2 AND w.archived_at IS NULL`, chatID, root)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type mapping struct {
		workspace     string
		post, channel int64
	}
	targets := []mapping{}
	for rows.Next() {
		var m mapping
		if err = rows.Scan(&m.workspace, &m.post, &m.channel); err != nil {
			_ = rows.Close()
			return err
		}
		targets = append(targets, m)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, m := range targets {
		if _, err = lockActiveWorkspaceForMAXHistoryWrite(ctx, tx, m.workspace); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return err
		}
		item := MAXPostComment{MessageID: message, CreatedAt: eventAt, UpdatedAt: eventAt, ObservedAt: eventAt, DeletedAt: &eventAt}
		if err = upsertMAXCommentTx(ctx, tx, m.workspace, m.post, m.channel, root, item); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE max_post_comment_operations SET state='succeeded',finished_at=$5 WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND message_id=$4 AND kind='delete' AND state='uncertain'`, m.workspace, m.post, root, message, eventAt.UTC())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) FindMAXCommentOperation(ctx context.Context, actor, workspace string, postID int64, clientRequestID string) (MAXCommentOperation, bool, error) {
	c, err := s.GetMAXCommentContext(ctx, actor, workspace, postID, false)
	if err != nil {
		return MAXCommentOperation{}, false, err
	}
	o, err := scanMAXCommentOperation(s.db.QueryRowContext(ctx, `SELECT `+maxCommentOperationColumns+` FROM max_post_comment_operations WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND client_request_id=$4`, workspace, postID, c.Post.MAXMessageID, clientRequestID))
	if errors.Is(err, ErrNotFound) {
		return o, false, nil
	}
	return o, err == nil, err
}

func (s *Store) GetMAXCommentOperation(ctx context.Context, actor, workspace string, postID int64, operationID string) (MAXCommentOperation, error) {
	c, err := s.GetMAXCommentContext(ctx, actor, workspace, postID, false)
	if err != nil {
		return MAXCommentOperation{}, err
	}
	return scanMAXCommentOperation(s.db.QueryRowContext(ctx, `SELECT `+maxCommentOperationColumns+` FROM max_post_comment_operations WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3 AND operation_id=$4`, workspace, postID, c.Post.MAXMessageID, operationID))
}

// recipient.post_id is the only documented root association on create/edit
// webhook messages. The caller passes it through without inferring a root.
func (s *Store) ObserveMAXCommentForRoot(ctx context.Context, chatID, root string, item MAXPostComment) error {
	if !ValidMAXCommentID(root) {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT p.workspace_id,p.id,ch.id FROM posts p JOIN channels ch ON ch.workspace_id=p.workspace_id AND ch.id=p.channel_id JOIN workspaces w ON w.id=p.workspace_id WHERE ch.max_chat_id=$1 AND p.max_message_id=$2 AND w.archived_at IS NULL`, chatID, root)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type mapping struct {
		workspace     string
		post, channel int64
	}
	targets := []mapping{}
	for rows.Next() {
		var m mapping
		if err = rows.Scan(&m.workspace, &m.post, &m.channel); err != nil {
			_ = rows.Close()
			return err
		}
		targets = append(targets, m)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, m := range targets {
		if _, err = lockActiveWorkspaceForMAXHistoryWrite(ctx, tx, m.workspace); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return err
		}
		if err = upsertMAXCommentTx(ctx, tx, m.workspace, m.post, m.channel, root, item); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ReconcileMAXCommentOperation(ctx context.Context, actor, workspace string, postID int64, root, operation string, item *MAXPostComment, now time.Time) error {
	if item == nil {
		return ErrMAXCommentValidation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := maxCommentContextTx(ctx, tx, actor, workspace, postID, true)
	if err != nil {
		return err
	}
	if c.Post.MAXMessageID != root {
		return ErrConflict
	}
	o, err := scanMAXCommentOperation(tx.QueryRowContext(ctx, `SELECT `+maxCommentOperationColumns+` FROM max_post_comment_operations WHERE operation_id=$1 AND workspace_id=$2 AND post_id=$3 AND root_message_id=$4 FOR UPDATE`, operation, workspace, postID, root))
	if err != nil {
		return err
	}
	if o.State != "uncertain" || o.Kind != "send" || o.DesiredFormat != "" || !item.SenderIsBot || item.SenderUserID != o.BotUserID || item.Text != o.DesiredText || item.ReplyToMessageID != o.DesiredReplyTo || item.CreatedAt.Before(o.ClaimedAt.Add(-5*time.Second)) {
		return ErrConflict
	}
	if err = upsertMAXCommentTx(ctx, tx, workspace, postID, c.Channel.ID, root, *item); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE max_post_comment_operations SET state='succeeded',message_id=$2,finished_at=$3 WHERE operation_id=$1`, operation, item.MessageID, now.UTC())
	if err != nil {
		return err
	}
	if err = appendAuditEventTx(ctx, tx, AuditEvent{WorkspaceID: workspace, ActorUserID: actor, Action: "max.comment.reconciled", EntityType: "post", EntityID: strconv.FormatInt(postID, 10), Metadata: mustJSON(map[string]any{"operation_id": operation, "message_id": item.MessageID}), CreatedAt: now.UTC()}); err != nil {
		return err
	}
	return tx.Commit()
}

func getMAXCommentMetricsTx(ctx context.Context, tx *sql.Tx, workspace string, postID int64, root, bot string) (MAXCommentMetrics, error) {
	var total, inbound, replies, deleted, unique, answered int64
	var median sql.NullFloat64
	err := tx.QueryRowContext(ctx, `WITH scoped AS (
 SELECT c.*,EXISTS(SELECT 1 FROM max_post_comment_operations o WHERE o.workspace_id=c.workspace_id AND o.post_id=c.post_id AND o.root_message_id=c.root_message_id AND o.message_id=c.message_id AND o.kind='send' AND o.state='succeeded' AND o.bot_user_id=$4) AS known_own
 FROM max_post_comments c WHERE workspace_id=$1 AND post_id=$2 AND root_message_id=$3
), first_replies AS (
 SELECT inbound.message_id,MIN(EXTRACT(EPOCH FROM reply.created_at-inbound.created_at)) AS seconds
 FROM scoped inbound JOIN scoped reply ON reply.reply_to_message_id=inbound.message_id
 WHERE inbound.deleted_at IS NULL AND NOT inbound.sender_is_bot AND inbound.sender_user_id<>''
 AND reply.deleted_at IS NULL AND ((reply.sender_is_bot AND reply.sender_user_id=$4) OR reply.known_own) AND $4<>''
 AND reply.created_at>=inbound.created_at GROUP BY inbound.message_id
)
SELECT COUNT(*) FILTER(WHERE deleted_at IS NULL),
 COUNT(*) FILTER(WHERE deleted_at IS NULL AND NOT sender_is_bot AND sender_user_id<>''),
 COUNT(*) FILTER(WHERE deleted_at IS NULL AND ((sender_is_bot AND sender_user_id=$4) OR known_own) AND $4<>'' AND reply_to_message_id<>''),
 COUNT(*) FILTER(WHERE deleted_at IS NOT NULL),
 COUNT(DISTINCT sender_user_id) FILTER(WHERE deleted_at IS NULL AND NOT sender_is_bot AND sender_user_id<>''),
 (SELECT COUNT(*) FROM first_replies),(SELECT PERCENTILE_CONT(0.5) WITHIN GROUP(ORDER BY seconds) FROM first_replies)
FROM scoped`, workspace, postID, root, bot).Scan(&total, &inbound, &replies, &deleted, &unique, &answered, &median)
	if err != nil {
		return MAXCommentMetrics{}, err
	}
	m := MAXCommentMetrics{ObservedTotal: &total, ObservedInbound: &inbound, ObservedReplies: &replies, ObservedDeleted: &deleted, UniqueAuthors: &unique}
	if inbound > 0 {
		rate := float64(answered) / float64(inbound)
		m.ResponseRate = &rate
	}
	if median.Valid {
		m.MedianResponseSeconds = &median.Float64
	}
	return m, nil
}

func MAXCommentStateError(o MAXCommentOperation) error {
	switch o.State {
	case "sending":
		return ErrMAXCommentBusy
	case "uncertain":
		return ErrMAXCommentUncertain
	case "rejected":
		return fmt.Errorf("%w: previous request was rejected", ErrConflict)
	}
	return nil
}

// Private conservative phase check used before RBAC/provider validation and
// again immediately before returning an error. A pending operation may begin
// writing concurrently, so only a rejected operation is guaranteed safe.
// It exposes no
// comment or actor data, and keeps a revoked/re-published request key from
// being described as a guaranteed new pre-write rejection.
func (s *Store) MAXCommentRequestWasWritten(ctx context.Context, workspace string, postID int64, requestID string) (bool, error) {
	var written bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM max_post_comment_operations WHERE workspace_id=$1 AND post_id=$2 AND client_request_id=$3 AND state<>'rejected')`, workspace, postID, requestID).Scan(&written)
	return written, err
}

func NormalizeMAXCommentText(text string) string { return strings.TrimSpace(text) }
