package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrDiscoveryDraftBusy        = errors.New("content discovery draft is in progress")
	ErrDiscoveryRequestConflict  = errors.New("content discovery request id was already used with different parameters")
	ErrDiscoveryCandidateExpired = errors.New("content discovery candidate expired")
)

type DiscoveryCandidate struct {
	ID        string
	Payload   json.RawMessage
	ChannelID *int64
}
type DiscoveryTransferItem struct {
	Type         string `json:"type"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	AttachmentID int64  `json:"attachment_id,omitempty"`
}
type DiscoveryMediaTransfer struct {
	Status         string                  `json:"status"`
	RequestedCount int                     `json:"requested_count"`
	CopiedCount    int                     `json:"copied_count"`
	Items          []DiscoveryTransferItem `json:"items"`
	Warnings       []string                `json:"warnings"`
}
type DiscoveryDraftOperation struct {
	Candidate  DiscoveryCandidate
	PostID     int64
	Generation int64
	Complete   bool
	Report     DiscoveryMediaTransfer
}

func discoveryWriteAccess(ctx context.Context, tx *sql.Tx, actor, workspace string) (WorkspaceAccess, error) {
	if _, err := lockActiveWorkspaceForMAXHistoryWrite(ctx, tx, workspace); err != nil {
		return WorkspaceAccess{}, err
	}
	access, err := resolveWorkspaceAccess(ctx, tx, actor, workspace)
	if err != nil {
		return WorkspaceAccess{}, err
	}
	if access.Member.Role != WorkspaceRoleOwner && access.Member.Role != WorkspaceRoleEditor {
		return WorkspaceAccess{}, ErrNotFound
	}
	return access, nil
}

func (s *Store) SaveDiscoveryCandidates(ctx context.Context, actor, workspace string, channelID *int64, payloads []json.RawMessage, now time.Time) ([]DiscoveryCandidate, error) {
	if len(payloads) > 3 || now.IsZero() {
		return nil, errors.New("invalid discovery candidate batch")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = discoveryWriteAccess(ctx, tx, actor, workspace); err != nil {
		return nil, err
	}
	// Expiry removes unused retrieval authority, while accepted operations keep
	// their exact immutable card for durable replay. Bound every cleanup batch.
	if _, err = tx.ExecContext(ctx, `DELETE FROM content_discovery_candidates WHERE id IN (
 SELECT c.id FROM content_discovery_candidates c WHERE c.workspace_id=$1 AND c.actor_user_id=$2 AND c.expires_at<=$3
 AND NOT EXISTS(SELECT 1 FROM content_discovery_draft_operations o WHERE o.workspace_id=c.workspace_id AND o.actor_user_id=c.actor_user_id AND o.candidate_id=c.id)
 ORDER BY c.expires_at,c.id LIMIT 64)`, workspace, actor, now.UTC()); err != nil {
		return nil, err
	}
	if channelID != nil {
		var id int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE workspace_id=$1 AND id=$2 FOR KEY SHARE`, workspace, *channelID).Scan(&id); errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		} else if err != nil {
			return nil, err
		}
	}
	result := make([]DiscoveryCandidate, 0, len(payloads))
	for _, payload := range payloads {
		var object map[string]json.RawMessage
		if len(payload) > 65536 || json.Unmarshal(payload, &object) != nil || object == nil {
			return nil, errors.New("invalid discovery candidate payload")
		}
		candidate := DiscoveryCandidate{ID: newStoreID("cdc_"), Payload: append(json.RawMessage(nil), payload...), ChannelID: channelID}
		if _, err = tx.ExecContext(ctx, `INSERT INTO content_discovery_candidates(id,workspace_id,actor_user_id,channel_id,payload,created_at,expires_at) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7)`, candidate.ID, workspace, actor, nullableInt64(channelID), string(payload), now.UTC(), now.UTC().Add(24*time.Hour)); err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// Lookup uses the actor and workspace boundary even for operation replays.
func (s *Store) GetDiscoveryCandidate(ctx context.Context, actor, workspace, id string) (DiscoveryCandidate, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actor, workspace); err != nil {
		return DiscoveryCandidate{}, err
	}
	var c DiscoveryCandidate
	var channel sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id,payload,channel_id FROM content_discovery_candidates WHERE workspace_id=$1 AND actor_user_id=$2 AND id=$3`, workspace, actor, id).Scan(&c.ID, &c.Payload, &channel)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if channel.Valid {
		c.ChannelID = &channel.Int64
	}
	return c, err
}

// Claim and the initial post are committed together. A lease takeover retains
// the same post; every later attachment commit checks its generation fence.
func (s *Store) ClaimDiscoveryDraft(ctx context.Context, actor, workspace, candidateID, requestID string, channelID *int64, includeMedia bool, draft Post, now time.Time) (DiscoveryDraftOperation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiscoveryDraftOperation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	access, err := discoveryWriteAccess(ctx, tx, actor, workspace)
	if err != nil {
		return DiscoveryDraftOperation{}, err
	}
	params, _ := json.Marshal(struct {
		Candidate string
		Channel   *int64
		Media     bool
	}{candidateID, channelID, includeMedia})
	sum := sha256.Sum256(params)
	hash := hex.EncodeToString(sum[:])
	// Serialize first claims even when the operation row does not exist yet.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "maxposty:discovery:"+workspace+":"+actor+":"+requestID); err != nil {
		return DiscoveryDraftOperation{}, err
	}
	var op DiscoveryDraftOperation
	var oldHash, state string
	var postID sql.NullInt64
	var lease time.Time
	var report []byte
	err = tx.QueryRowContext(ctx, `SELECT request_hash,post_id,state,generation,lease_until,report FROM content_discovery_draft_operations WHERE workspace_id=$1 AND actor_user_id=$2 AND client_request_id=$3::uuid FOR UPDATE`, workspace, actor, requestID).Scan(&oldHash, &postID, &state, &op.Generation, &lease, &report)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return op, err
	}
	if exists {
		if oldHash != hash {
			return op, ErrDiscoveryRequestConflict
		}
		if !postID.Valid {
			return op, ErrNotFound
		}
		op.PostID = postID.Int64
		op.Complete = state == "complete"
		if err = json.Unmarshal(report, &op.Report); err != nil {
			return op, err
		}
		if !op.Complete && lease.After(now) {
			return op, ErrDiscoveryDraftBusy
		}
	}
	var channel sql.NullInt64
	var expires time.Time
	err = tx.QueryRowContext(ctx, `SELECT id,payload,channel_id,expires_at FROM content_discovery_candidates WHERE workspace_id=$1 AND actor_user_id=$2 AND id=$3`, workspace, actor, candidateID).Scan(&op.Candidate.ID, &op.Candidate.Payload, &channel, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return op, ErrNotFound
	}
	if err != nil {
		return op, err
	}
	if channel.Valid {
		op.Candidate.ChannelID = &channel.Int64
	}
	if !exists && !expires.After(now) {
		return op, ErrDiscoveryCandidateExpired
	}
	if exists {
		if !op.Complete {
			op.Generation++
			_, err = tx.ExecContext(ctx, `UPDATE content_discovery_draft_operations SET generation=$4,lease_until=$5,updated_at=$6 WHERE workspace_id=$1 AND actor_user_id=$2 AND client_request_id=$3::uuid`, workspace, actor, requestID, op.Generation, now.Add(3*time.Minute), now)
		}
		if err != nil {
			return op, err
		}
		if err = tx.Commit(); err != nil {
			return op, err
		}
		return op, nil
	}
	if channelID != nil {
		var id int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE workspace_id=$1 AND id=$2 FOR KEY SHARE`, workspace, *channelID).Scan(&id); errors.Is(err, sql.ErrNoRows) {
			return op, ErrNotFound
		} else if err != nil {
			return op, err
		}
	}
	if draft.Format != FormatMarkdown && draft.Format != FormatHTML {
		return op, errors.New("invalid discovery draft format")
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO posts(owner_id,workspace_id,title,content,format,status,channel_id,image_prompt,notify,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'draft',$6,$7,TRUE,$8,$8) RETURNING id`, access.Workspace.CompatOwnerUserID, workspace, draft.Title, draft.Content, draft.Format, nullableInt64(channelID), draft.ImagePrompt, now).Scan(&op.PostID)
	if err != nil {
		return op, err
	}
	if _, err = createPostRevisionTx(ctx, tx, actor, workspace, op.PostID, now); err != nil {
		return op, err
	}
	if err = appendAuditEventTx(ctx, tx, AuditEvent{WorkspaceID: workspace, ActorUserID: actor, Action: "post.created", EntityType: "post", EntityID: fmt.Sprint(op.PostID), Metadata: mustJSON(map[string]any{"origin": "content_discovery", "candidate_id": candidateID}), CreatedAt: now}); err != nil {
		return op, err
	}
	op.Generation = 1
	op.Report = DiscoveryMediaTransfer{Items: []DiscoveryTransferItem{}, Warnings: []string{}}
	empty, _ := json.Marshal(op.Report)
	_, err = tx.ExecContext(ctx, `INSERT INTO content_discovery_draft_operations(workspace_id,actor_user_id,client_request_id,candidate_id,request_hash,post_id,state,generation,lease_until,expected_post_updated_at,report,created_at,updated_at) VALUES($1,$2,$3::uuid,$4,$5,$6,'working',1,$7,$8,$9::jsonb,$8,$8)`, workspace, actor, requestID, candidateID, hash, op.PostID, now.Add(3*time.Minute), now, string(empty))
	if err != nil {
		return op, err
	}
	if err = tx.Commit(); err != nil {
		return op, err
	}
	return op, nil
}

// A preflight rejects known exhausted storage before any external retrieval;
// reservation after the file hash is known enforces concurrent actual usage.
func (s *Store) DiscoveryMediaCapacity(ctx context.Context, actor, workspace string, limits MediaLimits) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = discoveryWriteAccess(ctx, tx, actor, workspace); err != nil {
		return 0, err
	}
	unlimited, err := workspaceComplimentaryAccess(ctx, tx, workspace)
	if err != nil {
		return 0, err
	}
	if unlimited {
		return limits.MaxBytes, nil
	}
	var files, bytes int64
	err = tx.QueryRowContext(ctx, `SELECT asset_count,total_bytes FROM workspace_media_usage WHERE workspace_id=$1`, workspace).Scan(&files, &bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return limits.MaxBytes, nil
	}
	if err != nil {
		return 0, err
	}
	if files >= limits.MaxFiles || bytes >= limits.MaxBytes {
		return 0, ErrMediaQuotaExceeded
	}
	return limits.MaxBytes - bytes, nil
}

func (s *Store) AddDiscoveryDraftAttachment(ctx context.Context, actor, workspace, requestID string, op DiscoveryDraftOperation, attachment PostAttachment) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	access, err := discoveryWriteAccess(ctx, tx, actor, workspace)
	if err != nil {
		return 0, err
	}
	// Parent -> post -> operation is shared with lifecycle/cascade writers.
	state, err := lockPostAttachmentState(ctx, tx, access.Workspace.CompatOwnerUserID, op.PostID)
	if err != nil {
		return 0, err
	}
	if err = validateAttachmentWriteState(state); err != nil {
		return 0, err
	}
	var expected, updated time.Time
	var generation int64
	var status string
	var persistedReport []byte
	if err = tx.QueryRowContext(ctx, `SELECT updated_at FROM posts WHERE id=$1 AND workspace_id=$2 AND status='draft'`, op.PostID, workspace).Scan(&updated); errors.Is(err, sql.ErrNoRows) {
		return 0, ErrConflict
	} else if err != nil {
		return 0, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT generation,state,expected_post_updated_at,report FROM content_discovery_draft_operations WHERE workspace_id=$1 AND actor_user_id=$2 AND client_request_id=$3::uuid AND post_id=$4 FOR UPDATE`, workspace, actor, requestID, op.PostID).Scan(&generation, &status, &expected, &persistedReport); err != nil {
		return 0, err
	}
	if generation != op.Generation || status != "working" || !expected.Equal(updated) {
		return 0, ErrConflict
	}
	if err = validateAttachmentObject(ctx, tx, access.Workspace.CompatOwnerUserID, attachment); err != nil {
		return 0, err
	}
	var owned bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_assets WHERE workspace_id=$1 AND filename=$2 AND state='ready')`, workspace, attachment.StorageKey).Scan(&owned); err != nil {
		return 0, err
	}
	if !owned {
		return 0, ErrNotFound
	}
	ids, err := listAttachmentIDsTx(ctx, tx, access.Workspace.CompatOwnerUserID, op.PostID)
	if err != nil {
		return 0, err
	}
	if len(ids) >= MaxPostAttachments {
		return 0, ErrConflict
	}
	now := time.Now().UTC()
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO post_attachments(owner_id,post_id,type,position,storage_key,processing_status,size_bytes,mime_type,width,height,duration_ms,provider_meta,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'ready',$6,$7,$8,$9,$10,$11::jsonb,$12,$12) RETURNING id`, access.Workspace.CompatOwnerUserID, op.PostID, attachment.Type, len(ids), attachment.StorageKey, attachment.SizeBytes, attachment.MIMEType, nullableInt(attachment.Width), nullableInt(attachment.Height), nullableInt64(attachment.DurationMS), normalizedProviderMeta(attachment.ProviderMeta), now).Scan(&id)
	if err != nil {
		return 0, err
	}
	if err = syncPostAttachmentProjectionTx(ctx, tx, access.Workspace.CompatOwnerUserID, op.PostID, now); err != nil {
		return 0, err
	}
	if err = json.Unmarshal(persistedReport, &op.Report); err != nil {
		return 0, err
	}
	item := DiscoveryTransferItem{Type: attachment.Type, Status: "copied", AttachmentID: id}
	var provenance struct {
		PreviewOnly bool `json:"preview_only"`
	}
	if json.Unmarshal(attachment.ProviderMeta, &provenance) == nil && provenance.PreviewOnly {
		item.Reason = "preview_only"
	}
	op.Report.Items = append(op.Report.Items, item)
	report, _ := json.Marshal(op.Report)
	if _, err = tx.ExecContext(ctx, `UPDATE content_discovery_draft_operations SET expected_post_updated_at=$4,report=$5::jsonb,updated_at=$4 WHERE workspace_id=$1 AND actor_user_id=$2 AND client_request_id=$3::uuid`, workspace, actor, requestID, now, string(report)); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// Preserve already accepted outcomes even if the caller disconnects or their
// membership changes. No new post/media write is authorized by this method.
func (s *Store) FinishDiscoveryDraft(ctx context.Context, actor, workspace, requestID string, op DiscoveryDraftOperation, report DiscoveryMediaTransfer) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE content_discovery_draft_operations SET state='complete',report=$5::jsonb,updated_at=$6 WHERE workspace_id=$1 AND actor_user_id=$2 AND client_request_id=$3::uuid AND generation=$4 AND state='working'`, workspace, actor, requestID, op.Generation, string(raw), time.Now().UTC())
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrConflict
	}
	return nil
}
