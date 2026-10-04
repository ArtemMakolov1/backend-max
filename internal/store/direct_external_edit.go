package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const directExternalEditLease = 2 * time.Minute
const externalControlColumns = `version,observed_hash,revision_id,bookmarked,edit_state,operation_id,write_started,claimed_at,desired_hash`

type DirectExternalEditControl struct {
	Version      int64      `json:"version"`
	ObservedHash string     `json:"observed_hash"`
	RevisionID   string     `json:"revision_id"`
	Bookmarked   bool       `json:"bookmarked"`
	EditState    string     `json:"edit_state"`
	OperationID  string     `json:"-"`
	WriteStarted bool       `json:"-"`
	ClaimedAt    *time.Time `json:"-"`
	DesiredHash  string     `json:"-"`
}

type DirectExternalEditFence struct {
	ConnectionID string `json:"expected_connection_id"`
	Version      int64  `json:"expected_version"`
	ObservedHash string `json:"expected_observed_hash"`
	RevisionID   string `json:"expected_revision_id"`
}

func scanExternalControl(row scanner) (DirectExternalEditControl, error) {
	var c DirectExternalEditControl
	err := row.Scan(&c.Version, &c.ObservedHash, &c.RevisionID, &c.Bookmarked, &c.EditState, &c.OperationID, &c.WriteStarted, &c.ClaimedAt, &c.DesiredHash)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// All provider access is tied to an active connection and a previously synced
// external ID. A managed campaign cannot bypass its graph/consent workflow.
func externalEditContextTx(ctx context.Context, tx *sql.Tx, actor, workspace, expectedConnection string, campaign int64, write bool) (DirectConnection, error) {
	if campaign <= 0 {
		return DirectConnection{}, ErrNotFound
	}
	// Membership changes and connection replacement lock the parent first.
	// Acquire its KEY SHARE before reading the role or locking a connection;
	// a blocked writer then observes the committed revocation/demotion.
	if err := lockActiveWorkspaceForDirectConnectionWrite(ctx, tx, workspace); err != nil {
		return DirectConnection{}, err
	}
	if write {
		if err := requireWorkspaceRole(ctx, tx, actor, workspace, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
			return DirectConnection{}, err
		}
	} else if _, err := resolveWorkspaceAccess(ctx, tx, actor, workspace); err != nil {
		return DirectConnection{}, err
	}
	connection, err := scanDirectConnection(tx.QueryRowContext(ctx, `SELECT `+directConnectionColumns+` FROM direct_connections WHERE workspace_id=$1 AND status='active' AND revoked_at IS NULL FOR SHARE`, workspace))
	if err != nil {
		return connection, ErrDirectConnectionRequired
	}
	if expectedConnection != "" && connection.ID != expectedConnection {
		return connection, ErrConflict
	}
	if write && connection.ReadOnly {
		return connection, ErrDirectConnectionRequired
	}
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM direct_external_campaigns WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3) AND NOT EXISTS(SELECT 1 FROM direct_campaigns WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3)`, workspace, connection.ID, campaign).Scan(&exists)
	if err != nil {
		return connection, err
	}
	if !exists {
		return connection, ErrNotFound
	}
	return connection, nil
}

func (s *Store) GetDirectExternalEditContext(ctx context.Context, actor, workspace, expectedConnection string, campaign int64, write bool) (DirectConnection, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectConnection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	connection, err := externalEditContextTx(ctx, tx, actor, workspace, expectedConnection, campaign, write)
	if err != nil {
		return connection, err
	}
	return connection, tx.Commit()
}

// A completed matching readback resolves uncertainty. A different readback
// cannot prove that an ambiguous provider request will never commit later.
func (s *Store) ObserveDirectExternalEdit(ctx context.Context, actor, workspace, connection string, campaign int64, hash string, now time.Time) (DirectExternalEditControl, error) {
	if !directOAuthStateHashPattern.MatchString(hash) {
		return DirectExternalEditControl{}, ErrDirectValidation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectExternalEditControl{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = externalEditContextTx(ctx, tx, actor, workspace, connection, campaign, false); err != nil {
		return DirectExternalEditControl{}, err
	}
	revision := newStoreID("dxr_")
	_, err = tx.ExecContext(ctx, `INSERT INTO direct_external_edit_controls (workspace_id,connection_id,provider_campaign_id,observed_hash,revision_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, workspace, connection, campaign, hash, revision)
	if err != nil {
		return DirectExternalEditControl{}, err
	}
	control, err := scanExternalControl(tx.QueryRowContext(ctx, `SELECT `+externalControlColumns+` FROM direct_external_edit_controls WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 FOR UPDATE`, workspace, connection, campaign))
	if err != nil {
		return control, err
	}
	switch {
	case control.EditState != "idle" && control.WriteStarted && control.DesiredHash == hash:
		control.EditState = "idle"
	case control.EditState == "updating" && control.ClaimedAt != nil && !control.ClaimedAt.Add(directExternalEditLease).After(now):
		if control.WriteStarted {
			control.EditState = "uncertain"
		} else {
			control.EditState = "idle"
		}
	}
	if control.EditState == "idle" {
		if control.ObservedHash != hash || control.OperationID != "" {
			control.Version++
			control.RevisionID = revision
		}
		control.ObservedHash = hash
		control.OperationID = ""
		control.DesiredHash = ""
		control.WriteStarted = false
		control.ClaimedAt = nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE direct_external_edit_controls SET version=$4,observed_hash=$5,revision_id=$6,edit_state=$7,operation_id=$8,write_started=$9,claimed_at=$10,desired_hash=$11 WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3`, workspace, connection, campaign, control.Version, control.ObservedHash, control.RevisionID, control.EditState, control.OperationID, control.WriteStarted, control.ClaimedAt, control.DesiredHash)
	if err != nil {
		return control, err
	}
	return control, tx.Commit()
}

func (s *Store) ClaimDirectExternalEdit(ctx context.Context, actor, workspace string, campaign int64, fence DirectExternalEditFence, desired string, now time.Time) (string, error) {
	if fence.ConnectionID == "" || fence.Version <= 0 || fence.RevisionID == "" || !directOAuthStateHashPattern.MatchString(fence.ObservedHash) || !directOAuthStateHashPattern.MatchString(desired) {
		return "", fmt.Errorf("%w: current revision is required", ErrDirectValidation)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = externalEditContextTx(ctx, tx, actor, workspace, fence.ConnectionID, campaign, true); err != nil {
		return "", err
	}
	control, err := scanExternalControl(tx.QueryRowContext(ctx, `SELECT `+externalControlColumns+` FROM direct_external_edit_controls WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 FOR UPDATE`, workspace, fence.ConnectionID, campaign))
	if err != nil {
		return "", err
	}
	if control.EditState != "idle" {
		return "", ErrDirectProviderOperationBusy
	}
	if control.Version != fence.Version || control.ObservedHash != fence.ObservedHash || control.RevisionID != fence.RevisionID {
		return "", ErrConflict
	}
	operation := newStoreID("dxe_")
	_, err = tx.ExecContext(ctx, `UPDATE direct_external_edit_controls SET edit_state='updating',operation_id=$4,desired_hash=$5,claimed_at=$6,version=version+1 WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3`, workspace, fence.ConnectionID, campaign, operation, desired, now.UTC())
	if err != nil {
		return "", err
	}
	if err = appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspace, ActorUserID: actor, Action: "direct.external.edit_claimed",
		EntityType: "direct_external_campaign", EntityID: strconv.FormatInt(campaign, 10),
		Metadata: mustJSON(map[string]any{"connection_id": fence.ConnectionID, "operation_id": operation,
			"expected_version": fence.Version, "observed_hash": fence.ObservedHash, "desired_hash": desired}),
		CreatedAt: now.UTC(),
	}); err != nil {
		return "", err
	}
	return operation, tx.Commit()
}

func (s *Store) BeginDirectExternalWrite(ctx context.Context, actor, workspace, connection string, campaign int64, operation string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = externalEditContextTx(ctx, tx, actor, workspace, connection, campaign, true); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE direct_external_edit_controls SET write_started=TRUE WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 AND operation_id=$4 AND edit_state='updating' AND write_started=FALSE AND claimed_at>$5`, workspace, connection, campaign, operation, now.Add(-directExternalEditLease))
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

// Finalization is fenced by the private operation ID, independent of an HTTP
// cancellation or a later member removal. It never issues a provider request.
func (s *Store) FinishDirectExternalEdit(ctx context.Context, workspace, connection string, campaign int64, operation string, uncertain bool) error {
	state := "idle"
	if uncertain {
		state = "uncertain"
	}
	result, err := s.db.ExecContext(ctx, `UPDATE direct_external_edit_controls SET edit_state=$5,operation_id=CASE WHEN $5='idle' THEN '' ELSE operation_id END,desired_hash=CASE WHEN $5='idle' THEN '' ELSE desired_hash END,claimed_at=CASE WHEN $5='idle' THEN NULL ELSE claimed_at END,write_started=CASE WHEN $5='idle' THEN FALSE ELSE write_started END WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3 AND operation_id=$4 AND edit_state<>'idle'`, workspace, connection, campaign, operation, state)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) BookmarkDirectExternalCampaign(ctx context.Context, actor, workspace, connection string, campaign int64, bookmarked bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Bookmarking changes only local organization and works for read-only accounts.
	if _, err = externalEditContextTx(ctx, tx, actor, workspace, connection, campaign, false); err != nil {
		return err
	}
	if err = requireWorkspaceRole(ctx, tx, actor, workspace, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE direct_external_edit_controls SET bookmarked=$4 WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=$3`, workspace, connection, campaign, bookmarked)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return ErrNotFound
	}
	if err = appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspace, ActorUserID: actor, Action: "direct.external.bookmark_changed",
		EntityType: "direct_external_campaign", EntityID: strconv.FormatInt(campaign, 10),
		Metadata:  mustJSON(map[string]any{"connection_id": connection, "bookmarked": bookmarked}),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RequireDirectCreativeRole(ctx context.Context, actor, workspace string) error {
	return requireWorkspaceRole(ctx, s.db, actor, workspace, WorkspaceRoleOwner, WorkspaceRoleEditor)
}
