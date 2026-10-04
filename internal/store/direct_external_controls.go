package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var externalRequestIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func ValidDirectExternalRequestID(value string) bool {
	return externalRequestIDPattern.MatchString(value)
}

type DirectExternalOperation struct {
	ClientRequestID  string          `json:"client_request_id"`
	OperationID      string          `json:"operation_id"`
	Kind             string          `json:"kind"`
	State            string          `json:"state"`
	ProviderResultID *int64          `json:"provider_result_id,omitempty,string"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	WriteStarted     bool            `json:"-"`
	RequestHash      string          `json:"-"`
	Payload          json.RawMessage `json:"-"`
	DesiredHash      string          `json:"-"`
}

const externalOperationColumns = `client_request_id::text,operation_id,operation_kind,state,provider_result_id,created_at,updated_at,write_started,request_hash,request_payload,desired_hash`

func scanExternalOperation(row scanner) (DirectExternalOperation, error) {
	var o DirectExternalOperation
	err := row.Scan(&o.ClientRequestID, &o.OperationID, &o.Kind, &o.State, &o.ProviderResultID, &o.CreatedAt, &o.UpdatedAt, &o.WriteStarted, &o.RequestHash, &o.Payload, &o.DesiredHash)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return o, err
}
func externalOwnerKind(kind string) bool { return kind == "budget" || kind == "bid" || kind == "state" }
func externalOperationRoleTx(ctx context.Context, tx *sql.Tx, actor, workspace, kind string) error {
	if externalOwnerKind(kind) {
		return requireWorkspaceRole(ctx, tx, actor, workspace, WorkspaceRoleOwner)
	}
	return requireWorkspaceRole(ctx, tx, actor, workspace, WorkspaceRoleOwner, WorkspaceRoleEditor)
}

func (s *Store) ClaimDirectExternalOperation(ctx context.Context, actor, workspace string, campaign int64, fence DirectExternalEditFence, requestID, kind, requestHash, desiredHash string, payload json.RawMessage, now time.Time) (DirectExternalOperation, bool, error) {
	if !ValidDirectExternalRequestID(requestID) || !directOAuthStateHashPattern.MatchString(requestHash) || !directOAuthStateHashPattern.MatchString(desiredHash) || len(payload) > 65536 || !json.Valid(payload) {
		return DirectExternalOperation{}, false, ErrDirectValidation
	}
	switch kind {
	case "budget", "bid", "state", "group", "keyword", "create_group", "create_keyword":
	default:
		return DirectExternalOperation{}, false, ErrDirectValidation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectExternalOperation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = externalEditContextTx(ctx, tx, actor, workspace, fence.ConnectionID, campaign, true); err != nil {
		return DirectExternalOperation{}, false, err
	}
	if err = externalOperationRoleTx(ctx, tx, actor, workspace, kind); err != nil {
		return DirectExternalOperation{}, false, err
	}
	control, err := scanExternalControl(tx.QueryRowContext(ctx, `SELECT `+externalControlColumns+` FROM direct_external_edit_controls WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 FOR UPDATE`, workspace, fence.ConnectionID, campaign))
	if err != nil {
		return DirectExternalOperation{}, false, err
	}
	existing, err := scanExternalOperation(tx.QueryRowContext(ctx, `SELECT `+externalOperationColumns+` FROM direct_external_operation_journal WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 AND client_request_id=$4::uuid`, workspace, fence.ConnectionID, campaign, requestID))
	if err == nil {
		if existing.RequestHash != requestHash {
			return existing, false, ErrConflict
		}
		return existing, false, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return existing, false, err
	}
	if control.EditState != "idle" {
		return existing, false, ErrDirectProviderOperationBusy
	}
	if control.Version != fence.Version || control.ObservedHash != fence.ObservedHash || control.RevisionID != fence.RevisionID {
		return existing, false, ErrConflict
	}
	operation := newStoreID("dxe_")
	_, err = tx.ExecContext(ctx, `UPDATE direct_external_edit_controls SET edit_state='updating',operation_id=$4,desired_hash=$5,claimed_at=$6,version=version+1 WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3`, workspace, fence.ConnectionID, campaign, operation, desiredHash, now.UTC())
	if err != nil {
		return existing, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO direct_external_operation_journal(workspace_id,connection_id,provider_campaign_id,client_request_id,operation_id,operation_kind,request_hash,request_payload,desired_hash,created_at,updated_at) VALUES($1,$2,$3,$4::uuid,$5,$6,$7,$8::jsonb,$9,$10,$10)`, workspace, fence.ConnectionID, campaign, requestID, operation, kind, requestHash, string(payload), desiredHash, now.UTC())
	if err != nil {
		return existing, false, err
	}
	if err = appendAuditEventTx(ctx, tx, AuditEvent{WorkspaceID: workspace, ActorUserID: actor, Action: "direct.external.control_claimed", EntityType: "direct_external_campaign", EntityID: fmt.Sprint(campaign), Metadata: mustJSON(map[string]any{"connection_id": fence.ConnectionID, "operation_id": operation, "kind": kind, "request_hash": requestHash, "expected_version": fence.Version}), CreatedAt: now.UTC()}); err != nil {
		return existing, false, err
	}
	existing = DirectExternalOperation{ClientRequestID: requestID, OperationID: operation, Kind: kind, State: "updating", CreatedAt: now.UTC(), UpdatedAt: now.UTC(), RequestHash: requestHash, Payload: payload, DesiredHash: desiredHash}
	return existing, true, tx.Commit()
}

func (s *Store) BeginDirectExternalOperation(ctx context.Context, actor, workspace, connection string, campaign int64, operation, kind string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = externalEditContextTx(ctx, tx, actor, workspace, connection, campaign, true); err != nil {
		return err
	}
	if err = externalOperationRoleTx(ctx, tx, actor, workspace, kind); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE direct_external_edit_controls SET write_started=TRUE WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 AND operation_id=$4 AND edit_state='updating' AND NOT write_started AND claimed_at>$5`, workspace, connection, campaign, operation, now.Add(-directExternalEditLease))
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE direct_external_operation_journal SET write_started=TRUE,updated_at=$2 WHERE operation_id=$1 AND operation_kind=$3 AND state='updating' AND NOT write_started AND workspace_id=$4 AND connection_id=$5 AND provider_campaign_id=$6`, operation, now.UTC(), kind, workspace, connection, campaign)
	if err != nil {
		return err
	}
	n, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

// Detached acknowledgment must retain a known successful provider ID even if
// the actor is removed or the account's connection later changes.
func (s *Store) AcknowledgeDirectExternalOperation(ctx context.Context, operation string, resultID int64, now time.Time) error {
	if resultID <= 0 {
		return ErrDirectValidation
	}
	result, err := s.db.ExecContext(ctx, `UPDATE direct_external_operation_journal SET provider_result_id=$2,updated_at=$3 WHERE operation_id=$1 AND write_started AND state IN ('updating','uncertain') AND (provider_result_id IS NULL OR provider_result_id=$2)`, operation, resultID, now.UTC())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

// A worker that expired before beginning the write never contacted Direct.
// Release that claim atomically; a concurrent Begin is serialized on the same
// control row and can no longer send after this journal entry is rejected.
func (s *Store) RejectExpiredUnstartedDirectExternalOperation(ctx context.Context, workspace, connection string, campaign int64, operation string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR KEY SHARE`, workspace); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SELECT id FROM direct_connections WHERE workspace_id=$1 AND id=$2 FOR KEY SHARE`, workspace, connection); err != nil {
		return err
	}
	control, err := scanExternalControl(tx.QueryRowContext(ctx, `SELECT `+externalControlColumns+` FROM direct_external_edit_controls WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 FOR UPDATE`, workspace, connection, campaign))
	if err != nil {
		return err
	}
	if control.OperationID != operation || control.WriteStarted || control.ClaimedAt == nil || control.ClaimedAt.Add(directExternalEditLease).After(now) {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE direct_external_operation_journal SET state='rejected',updated_at=$5 WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 AND operation_id=$4 AND NOT write_started AND state='updating'`, workspace, connection, campaign, operation, now.UTC())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		_, err = tx.ExecContext(ctx, `UPDATE direct_external_edit_controls SET edit_state='idle',operation_id='',desired_hash='',claimed_at=NULL,write_started=FALSE,version=version+1,revision_id=$5 WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 AND operation_id=$4`, workspace, connection, campaign, operation, newStoreID("dxr_"))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) FinishDirectExternalOperation(ctx context.Context, workspace, connection string, campaign int64, operation, state string, now time.Time) error {
	if state != "succeeded" && state != "rejected" && state != "uncertain" {
		return ErrDirectValidation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Same parent/connection/control order as claims. Completion is private and
	// independent of the actor's current membership.
	if _, err = tx.ExecContext(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR KEY SHARE`, workspace); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SELECT id FROM direct_connections WHERE workspace_id=$1 AND id=$2 FOR KEY SHARE`, workspace, connection); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM direct_external_edit_controls WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 FOR UPDATE`, workspace, connection, campaign); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE direct_external_operation_journal SET state=$2,updated_at=$3 WHERE operation_id=$1 AND state IN ('updating','uncertain')`, operation, state, now.UTC())
	if err != nil {
		return err
	}
	uncertain := state == "uncertain"
	controlState := "idle"
	if uncertain {
		controlState = "uncertain"
	}
	_, err = tx.ExecContext(ctx, `UPDATE direct_external_edit_controls SET edit_state=$5,operation_id=CASE WHEN $5='idle' THEN '' ELSE operation_id END,desired_hash=CASE WHEN $5='idle' THEN '' ELSE desired_hash END,claimed_at=CASE WHEN $5='idle' THEN NULL ELSE claimed_at END,write_started=CASE WHEN $5='idle' THEN FALSE ELSE write_started END,version=version+CASE WHEN $5='idle' THEN 1 ELSE 0 END,revision_id=CASE WHEN $5='idle' THEN $6 ELSE revision_id END WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 AND operation_id=$4 AND edit_state<>'idle'`, workspace, connection, campaign, operation, controlState, newStoreID("dxr_"))
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ListDirectExternalOperations(ctx context.Context, actor, workspace, connection string, campaign int64) ([]DirectExternalOperation, error) {
	if _, err := s.GetDirectExternalEditContext(ctx, actor, workspace, connection, campaign, false); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+externalOperationColumns+` FROM direct_external_operation_journal WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 AND (state IN ('updating','uncertain') OR operation_id IN (SELECT operation_id FROM direct_external_operation_journal WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 ORDER BY created_at DESC LIMIT 20)) ORDER BY created_at DESC LIMIT 100`, workspace, connection, campaign)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []DirectExternalOperation{}
	for rows.Next() {
		o, err := scanExternalOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (s *Store) GetDirectExternalOperation(ctx context.Context, actor, workspace, connection string, campaign int64, requestID string) (DirectExternalOperation, error) {
	if !ValidDirectExternalRequestID(requestID) {
		return DirectExternalOperation{}, ErrDirectValidation
	}
	if _, err := s.GetDirectExternalEditContext(ctx, actor, workspace, connection, campaign, false); err != nil {
		return DirectExternalOperation{}, err
	}
	return scanExternalOperation(s.db.QueryRowContext(ctx, `SELECT `+externalOperationColumns+` FROM direct_external_operation_journal WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 AND client_request_id=$4::uuid`, workspace, connection, campaign, requestID))
}
