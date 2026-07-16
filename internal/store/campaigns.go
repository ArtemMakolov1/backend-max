package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const campaignColumns = `id,workspace_id,name,description,status,created_by,created_at,updated_at,archived_at`

var ErrCampaignApprovalRequired = errors.New("campaign post revision approval required")

type Campaign struct {
	ID          string            `json:"id"`
	WorkspaceID string            `json:"workspace_id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Status      string            `json:"status"`
	CreatedBy   string            `json:"created_by"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ArchivedAt  *time.Time        `json:"archived_at,omitempty"`
	Variants    []CampaignVariant `json:"variants"`
}

type CampaignVariant struct {
	ID            string     `json:"id"`
	WorkspaceID   string     `json:"workspace_id"`
	CampaignID    string     `json:"campaign_id"`
	ChannelID     int64      `json:"channel_id"`
	ChannelTitle  string     `json:"channel_title,omitempty"`
	PostID        *int64     `json:"post_id,omitempty"`
	Title         string     `json:"title"`
	Content       string     `json:"content"`
	Format        string     `json:"format"`
	PlannedAt     time.Time  `json:"planned_at"`
	Status        string     `json:"status"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	PostStatus    string     `json:"post_status,omitempty"`
	ReviewStatus  string     `json:"review_status,omitempty"`
	PostUpdatedAt *time.Time `json:"post_updated_at,omitempty"`
}

type CampaignChanges struct {
	Name              *string
	Description       *string
	ExpectedUpdatedAt time.Time
}

type CampaignVariantChanges struct {
	ChannelID         *int64
	Title             *string
	Content           *string
	Format            *string
	PlannedAt         *time.Time
	ExpectedUpdatedAt time.Time
}

type CalendarItem struct {
	ID               string     `json:"id"`
	Kind             string     `json:"kind"`
	CampaignID       string     `json:"campaign_id,omitempty"`
	VariantID        string     `json:"variant_id,omitempty"`
	PostID           *int64     `json:"post_id,omitempty"`
	ChannelID        int64      `json:"channel_id"`
	ChannelTitle     string     `json:"channel_title"`
	Title            string     `json:"title"`
	OccursAt         time.Time  `json:"occurs_at"`
	Status           string     `json:"status"`
	ReviewStatus     string     `json:"review_status,omitempty"`
	PostUpdatedAt    *time.Time `json:"post_updated_at,omitempty"`
	VariantUpdatedAt *time.Time `json:"variant_updated_at,omitempty"`
}

type CampaignScheduleItem struct {
	VariantID         string
	ScheduledAt       time.Time
	ExpectedUpdatedAt time.Time
}

type CampaignItemConflict struct {
	VariantID string `json:"variant_id"`
	PostID    *int64 `json:"post_id,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type CampaignScheduleError struct {
	Items []CampaignItemConflict `json:"items"`
}

func (e *CampaignScheduleError) Error() string { return "campaign schedule contains conflicts" }
func (e *CampaignScheduleError) Unwrap() error { return ErrConflict }

func (s *Store) CreateCampaign(
	ctx context.Context,
	actorUserID, workspaceID string,
	campaign Campaign,
	variants []CampaignVariant,
) (Campaign, error) {
	campaign.Name = strings.TrimSpace(campaign.Name)
	if campaign.Name == "" || utf8.RuneCountInString(campaign.Name) > 160 {
		return Campaign{}, errors.New("campaign name must contain 1 to 160 characters")
	}
	if utf8.RuneCountInString(campaign.Description) > 4000 {
		return Campaign{}, errors.New("campaign description must not exceed 4000 characters")
	}
	if len(variants) == 0 || len(variants) > 200 {
		return Campaign{}, errors.New("campaign must contain 1 to 200 variants")
	}
	if campaign.ID == "" {
		campaign.ID = newStoreID("cmp_")
	}
	now := campaign.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Campaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Campaign{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO campaigns(
id,workspace_id,name,description,status,created_by,created_at,updated_at)
VALUES($1,$2,$3,$4,'planned',$5,$6,$6)`, campaign.ID, workspaceID,
		campaign.Name, campaign.Description, actorUserID, now)
	if err != nil {
		return Campaign{}, mapWorkspaceWriteError("create campaign", err)
	}
	seen := make(map[string]struct{}, len(variants))
	for index := range variants {
		variant := variants[index]
		if variant.ID == "" {
			variant.ID = newStoreID("cv_")
		}
		variant.CampaignID, variant.WorkspaceID, variant.CreatedBy = campaign.ID, workspaceID, actorUserID
		if variant.Format == "" {
			variant.Format = FormatMarkdown
		}
		if err := validateCampaignVariant(variant); err != nil {
			return Campaign{}, fmt.Errorf("variant %d: %w", index+1, err)
		}
		key := fmt.Sprintf("%d/%s", variant.ChannelID, variant.PlannedAt.UTC().Format(time.RFC3339Nano))
		if _, duplicate := seen[key]; duplicate {
			return Campaign{}, fmt.Errorf("variant %d duplicates a channel and planned time", index+1)
		}
		seen[key] = struct{}{}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT active FROM channels
WHERE workspace_id=$1 AND id=$2`, workspaceID, variant.ChannelID).Scan(&active); errors.Is(err, sql.ErrNoRows) {
			return Campaign{}, ErrNotFound
		} else if err != nil {
			return Campaign{}, err
		} else if !active {
			return Campaign{}, errors.New("campaign channel is inactive")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO campaign_variants(
id,workspace_id,campaign_id,channel_id,title,content,format,planned_at,status,created_by,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'planned',$9,$10,$10)`, variant.ID, workspaceID,
			campaign.ID, variant.ChannelID, strings.TrimSpace(variant.Title), variant.Content,
			variant.Format, variant.PlannedAt.UTC(), actorUserID, now)
		if err != nil {
			return Campaign{}, mapWorkspaceWriteError("create campaign variant", err)
		}
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "campaign.created",
		EntityType: "campaign", EntityID: campaign.ID,
		Metadata: mustJSON(map[string]any{"variant_count": len(variants)}), CreatedAt: now,
	}); err != nil {
		return Campaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return Campaign{}, err
	}
	return s.GetCampaign(ctx, actorUserID, workspaceID, campaign.ID)
}

// CreateCampaignFromExistingDraft persists a single planned variant around a
// draft that was created by another feature (for example analytics repeat).
// It never schedules the post and therefore cannot bypass revision approval.
func (s *Store) CreateCampaignFromExistingDraft(
	ctx context.Context,
	actorUserID, workspaceID string,
	campaign Campaign,
	postID int64,
	plannedAt time.Time,
) (Campaign, error) {
	campaign.Name = strings.TrimSpace(campaign.Name)
	if campaign.Name == "" || utf8.RuneCountInString(campaign.Name) > 160 {
		return Campaign{}, errors.New("campaign name must contain 1 to 160 characters")
	}
	if utf8.RuneCountInString(campaign.Description) > 4000 {
		return Campaign{}, errors.New("campaign description must not exceed 4000 characters")
	}
	plannedAt = plannedAt.UTC()
	if plannedAt.IsZero() || !plannedAt.After(time.Now().UTC()) {
		return Campaign{}, errors.New("planned_at must be in the future")
	}
	if campaign.ID == "" {
		campaign.ID = newStoreID("cmp_")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Campaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Campaign{}, err
	}
	var title, content, format, status string
	var channelID sql.NullInt64
	var currentRevisionID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT title,content,format,status,channel_id,current_revision_id
FROM posts WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, postID).Scan(
		&title, &content, &format, &status, &channelID, &currentRevisionID); errors.Is(err, sql.ErrNoRows) {
		return Campaign{}, ErrNotFound
	} else if err != nil {
		return Campaign{}, err
	}
	if status != PostStatusDraft || !channelID.Valid || !currentRevisionID.Valid {
		return Campaign{}, fmt.Errorf("%w: campaign source must be a revisioned workspace draft with a channel", ErrConflict)
	}
	variant := CampaignVariant{
		ChannelID: channelID.Int64, Title: title, Content: content,
		Format: format, PlannedAt: plannedAt,
	}
	if err := validateCampaignVariant(variant); err != nil {
		return Campaign{}, fmt.Errorf("campaign source draft: %w", err)
	}
	var channelActive bool
	if err := tx.QueryRowContext(ctx, `SELECT active FROM channels
WHERE workspace_id=$1 AND id=$2`, workspaceID, channelID.Int64).Scan(&channelActive); err != nil {
		return Campaign{}, err
	}
	if !channelActive {
		return Campaign{}, errors.New("campaign channel is inactive")
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO campaigns(
id,workspace_id,name,description,status,created_by,created_at,updated_at)
VALUES($1,$2,$3,$4,'active',$5,$6,$6)`, campaign.ID, workspaceID,
		campaign.Name, campaign.Description, actorUserID, now)
	if err != nil {
		return Campaign{}, mapWorkspaceWriteError("create campaign from draft", err)
	}
	variantID := newStoreID("cv_")
	_, err = tx.ExecContext(ctx, `INSERT INTO campaign_variants(
id,workspace_id,campaign_id,channel_id,post_id,title,content,format,planned_at,status,created_by,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'materialized',$10,$11,$11)`, variantID,
		workspaceID, campaign.ID, channelID.Int64, postID, title, content, format,
		plannedAt, actorUserID, now)
	if err != nil {
		return Campaign{}, mapWorkspaceWriteError("link campaign source draft", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "campaign.created_from_draft",
		EntityType: "campaign", EntityID: campaign.ID,
		Metadata: mustJSON(map[string]any{"post_id": postID, "variant_id": variantID, "planned_at": plannedAt}), CreatedAt: now,
	}); err != nil {
		return Campaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return Campaign{}, err
	}
	return s.GetCampaign(ctx, actorUserID, workspaceID, campaign.ID)
}

func (s *Store) ListCampaigns(ctx context.Context, actorUserID, workspaceID string, includeArchived bool) ([]Campaign, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	where := "workspace_id=$1 AND archived_at IS NULL"
	if includeArchived {
		where = "workspace_id=$1"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+campaignColumns+` FROM campaigns WHERE `+where+`
ORDER BY updated_at DESC,id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]Campaign, 0)
	for rows.Next() {
		campaign, err := scanCampaign(rows)
		if err != nil {
			return nil, err
		}
		campaign.Variants, err = s.listCampaignVariants(ctx, workspaceID, campaign.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, campaign)
	}
	return result, rows.Err()
}

func (s *Store) GetCampaign(ctx context.Context, actorUserID, workspaceID, campaignID string) (Campaign, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return Campaign{}, err
	}
	campaign, err := scanCampaign(s.db.QueryRowContext(ctx, `SELECT `+campaignColumns+`
FROM campaigns WHERE workspace_id=$1 AND id=$2`, workspaceID, campaignID))
	if err != nil {
		return Campaign{}, err
	}
	campaign.Variants, err = s.listCampaignVariants(ctx, workspaceID, campaignID)
	return campaign, err
}

func (s *Store) UpdateCampaign(
	ctx context.Context,
	actorUserID, workspaceID, campaignID string,
	changes CampaignChanges,
) (Campaign, error) {
	if changes.ExpectedUpdatedAt.IsZero() {
		return Campaign{}, errors.New("expected_updated_at is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Campaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Campaign{}, err
	}
	var name, description string
	var currentUpdated time.Time
	if err := tx.QueryRowContext(ctx, `SELECT name,description,updated_at FROM campaigns
WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL FOR UPDATE`, workspaceID, campaignID).Scan(
		&name, &description, &currentUpdated); errors.Is(err, sql.ErrNoRows) {
		return Campaign{}, ErrNotFound
	} else if err != nil {
		return Campaign{}, err
	}
	if !currentUpdated.Equal(changes.ExpectedUpdatedAt.UTC()) {
		return Campaign{}, fmt.Errorf("%w: campaign changed in another session", ErrConflict)
	}
	if changes.Name != nil {
		name = strings.TrimSpace(*changes.Name)
	}
	if changes.Description != nil {
		description = *changes.Description
	}
	if name == "" || utf8.RuneCountInString(name) > 160 || utf8.RuneCountInString(description) > 4000 {
		return Campaign{}, errors.New("campaign fields are invalid")
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE campaigns SET name=$1,description=$2,updated_at=$3
WHERE workspace_id=$4 AND id=$5`, name, description, now, workspaceID, campaignID); err != nil {
		return Campaign{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "campaign.updated",
		EntityType: "campaign", EntityID: campaignID, Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return Campaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return Campaign{}, err
	}
	return s.GetCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func (s *Store) ArchiveCampaign(
	ctx context.Context,
	actorUserID, workspaceID, campaignID string,
	expectedUpdatedAt time.Time,
) error {
	if expectedUpdatedAt.IsZero() {
		return errors.New("expected_updated_at is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE campaigns
SET status='archived',archived_at=$1,updated_at=$1
WHERE workspace_id=$2 AND id=$3 AND archived_at IS NULL AND updated_at=$4`, now,
		workspaceID, campaignID, expectedUpdatedAt.UTC())
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM campaigns WHERE workspace_id=$1 AND id=$2`, workspaceID, campaignID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("%w: campaign changed in another session", ErrConflict)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "campaign.archived",
		EntityType: "campaign", EntityID: campaignID, Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AddCampaignVariants(
	ctx context.Context,
	actorUserID, workspaceID, campaignID string,
	variants []CampaignVariant,
) (Campaign, error) {
	if len(variants) == 0 || len(variants) > 200 {
		return Campaign{}, errors.New("1 to 200 variants are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Campaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Campaign{}, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM campaigns
WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL FOR UPDATE`, workspaceID, campaignID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return Campaign{}, ErrNotFound
	} else if err != nil {
		return Campaign{}, err
	}
	now := time.Now().UTC()
	for index := range variants {
		variant := variants[index]
		variant.ID = newStoreID("cv_")
		variant.WorkspaceID, variant.CampaignID, variant.CreatedBy = workspaceID, campaignID, actorUserID
		if variant.Format == "" {
			variant.Format = FormatMarkdown
		}
		if err := validateCampaignVariant(variant); err != nil {
			return Campaign{}, fmt.Errorf("variant %d: %w", index+1, err)
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT active FROM channels WHERE workspace_id=$1 AND id=$2`,
			workspaceID, variant.ChannelID).Scan(&active); errors.Is(err, sql.ErrNoRows) {
			return Campaign{}, ErrNotFound
		} else if err != nil {
			return Campaign{}, err
		} else if !active {
			return Campaign{}, errors.New("campaign channel is inactive")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO campaign_variants(
id,workspace_id,campaign_id,channel_id,title,content,format,planned_at,status,created_by,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'planned',$9,$10,$10)`, variant.ID, workspaceID,
			campaignID, variant.ChannelID, strings.TrimSpace(variant.Title), variant.Content,
			variant.Format, variant.PlannedAt.UTC(), actorUserID, now)
		if err != nil {
			return Campaign{}, mapWorkspaceWriteError("add campaign variant", err)
		}
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "campaign.variants_added",
		EntityType: "campaign", EntityID: campaignID,
		Metadata: mustJSON(map[string]any{"variant_count": len(variants)}), CreatedAt: now,
	}); err != nil {
		return Campaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return Campaign{}, err
	}
	return s.GetCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func (s *Store) UpdateCampaignVariant(
	ctx context.Context,
	actorUserID, workspaceID, campaignID, variantID string,
	changes CampaignVariantChanges,
) (CampaignVariant, error) {
	if changes.ExpectedUpdatedAt.IsZero() {
		return CampaignVariant{}, errors.New("expected_updated_at is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CampaignVariant{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return CampaignVariant{}, err
	}
	var activeCampaign int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM campaigns
WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL FOR UPDATE`, workspaceID, campaignID).Scan(&activeCampaign); errors.Is(err, sql.ErrNoRows) {
		return CampaignVariant{}, ErrNotFound
	} else if err != nil {
		return CampaignVariant{}, err
	}
	variant, err := scanCampaignVariant(tx.QueryRowContext(ctx, campaignVariantSelect+`
WHERE cv.workspace_id=$1 AND cv.campaign_id=$2 AND cv.id=$3 FOR UPDATE OF cv`, workspaceID, campaignID, variantID))
	if err != nil {
		return CampaignVariant{}, err
	}
	if variant.PostID != nil {
		return CampaignVariant{}, fmt.Errorf("%w: edit the materialized post instead", ErrConflict)
	}
	if !variant.UpdatedAt.Equal(changes.ExpectedUpdatedAt.UTC()) {
		return CampaignVariant{}, fmt.Errorf("%w: campaign variant changed in another session", ErrConflict)
	}
	if changes.ChannelID != nil {
		variant.ChannelID = *changes.ChannelID
	}
	if changes.Title != nil {
		variant.Title = strings.TrimSpace(*changes.Title)
	}
	if changes.Content != nil {
		variant.Content = *changes.Content
	}
	if changes.Format != nil {
		variant.Format = *changes.Format
	}
	if changes.PlannedAt != nil {
		variant.PlannedAt = changes.PlannedAt.UTC()
	}
	if err := validateCampaignVariant(variant); err != nil {
		return CampaignVariant{}, err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT active FROM channels WHERE workspace_id=$1 AND id=$2`,
		workspaceID, variant.ChannelID).Scan(&active); err != nil {
		return CampaignVariant{}, err
	}
	if !active {
		return CampaignVariant{}, errors.New("campaign channel is inactive")
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `UPDATE campaign_variants SET channel_id=$1,title=$2,content=$3,
format=$4,planned_at=$5,updated_at=$6 WHERE workspace_id=$7 AND campaign_id=$8 AND id=$9`,
		variant.ChannelID, variant.Title, variant.Content, variant.Format, variant.PlannedAt, now,
		workspaceID, campaignID, variantID)
	if err != nil {
		return CampaignVariant{}, mapWorkspaceWriteError("update campaign variant", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "campaign.variant_updated",
		EntityType: "campaign_variant", EntityID: variantID,
		Metadata: mustJSON(map[string]any{"campaign_id": campaignID}), CreatedAt: now,
	}); err != nil {
		return CampaignVariant{}, err
	}
	if err := tx.Commit(); err != nil {
		return CampaignVariant{}, err
	}
	return s.getCampaignVariant(ctx, workspaceID, campaignID, variantID)
}

func (s *Store) DeleteCampaignVariant(
	ctx context.Context,
	actorUserID, workspaceID, campaignID, variantID string,
	expectedUpdatedAt time.Time,
) error {
	if expectedUpdatedAt.IsZero() {
		return errors.New("expected_updated_at is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return err
	}
	var activeCampaign int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM campaigns
WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL FOR UPDATE`, workspaceID, campaignID).Scan(&activeCampaign); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM campaign_variants
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3 AND updated_at=$4 AND post_id IS NULL
  AND EXISTS (
      SELECT 1 FROM campaign_variants other
      WHERE other.workspace_id=$1 AND other.campaign_id=$2 AND other.id<>$3
  )`,
		workspaceID, campaignID, variantID, expectedUpdatedAt.UTC())
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		var postID *int64
		var updatedAt time.Time
		if err := tx.QueryRowContext(ctx, `SELECT post_id,updated_at FROM campaign_variants
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3`, workspaceID, campaignID, variantID).Scan(&postID, &updatedAt); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if postID != nil {
			return fmt.Errorf("%w: materialized campaign variants cannot be deleted", ErrConflict)
		}
		if updatedAt.Equal(expectedUpdatedAt.UTC()) {
			var variantCount int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM campaign_variants
WHERE workspace_id=$1 AND campaign_id=$2`, workspaceID, campaignID).Scan(&variantCount); err != nil {
				return err
			}
			if variantCount <= 1 {
				return fmt.Errorf("%w: a campaign must keep one variant; archive the campaign instead", ErrConflict)
			}
		}
		return fmt.Errorf("%w: campaign variant changed; reload and try again", ErrConflict)
	}
	now := time.Now().UTC()
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "campaign.variant_deleted",
		EntityType: "campaign_variant", EntityID: variantID,
		Metadata: mustJSON(map[string]any{"campaign_id": campaignID}), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MaterializeCampaign(
	ctx context.Context,
	actorUserID, workspaceID, campaignID string,
	variantIDs []string,
) (Campaign, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Campaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Campaign{}, err
	}
	var compatOwner string
	if err := tx.QueryRowContext(ctx, `SELECT compat_owner_user_id FROM workspaces
WHERE id=$1 AND archived_at IS NULL FOR SHARE`, workspaceID).Scan(&compatOwner); errors.Is(err, sql.ErrNoRows) {
		return Campaign{}, ErrNotFound
	} else if err != nil {
		return Campaign{}, err
	}
	var activeCampaign int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM campaigns
WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL FOR UPDATE`, workspaceID, campaignID).Scan(&activeCampaign); errors.Is(err, sql.ErrNoRows) {
		return Campaign{}, ErrNotFound
	} else if err != nil {
		return Campaign{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,channel_id,title,content,format,planned_at,post_id
FROM campaign_variants WHERE workspace_id=$1 AND campaign_id=$2 ORDER BY id`, workspaceID, campaignID)
	if err != nil {
		return Campaign{}, err
	}
	defer func() { _ = rows.Close() }()
	type materializeRow struct {
		id, title, content, format string
		channelID                  int64
		plannedAt                  time.Time
		postID                     *int64
	}
	all := make([]materializeRow, 0)
	for rows.Next() {
		var row materializeRow
		if err := rows.Scan(&row.id, &row.channelID, &row.title, &row.content, &row.format, &row.plannedAt, &row.postID); err != nil {
			if closeErr := rows.Close(); closeErr != nil {
				return Campaign{}, errors.Join(err, closeErr)
			}
			return Campaign{}, err
		}
		all = append(all, row)
	}
	if err := rows.Err(); err != nil {
		if closeErr := rows.Close(); closeErr != nil {
			return Campaign{}, errors.Join(err, closeErr)
		}
		return Campaign{}, err
	}
	if err := rows.Close(); err != nil {
		return Campaign{}, err
	}
	selected := make(map[string]struct{}, len(variantIDs))
	for _, id := range variantIDs {
		selected[strings.TrimSpace(id)] = struct{}{}
	}
	if len(selected) != 0 {
		for id := range selected {
			found := false
			for _, row := range all {
				if row.id == id {
					found = true
					break
				}
			}
			if !found {
				return Campaign{}, ErrNotFound
			}
		}
	}
	now := time.Now().UTC()
	materialized := 0
	for _, row := range all {
		if len(selected) != 0 {
			if _, ok := selected[row.id]; !ok {
				continue
			}
		}
		if row.postID != nil {
			continue
		}
		var postID int64
		err := tx.QueryRowContext(ctx, `INSERT INTO posts(
owner_id,workspace_id,title,content,format,status,channel_id,notify,disable_link_preview,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,'draft',$6,TRUE,FALSE,$7,$7) RETURNING id`, compatOwner, workspaceID,
			row.title, row.content, row.format, row.channelID, now).Scan(&postID)
		if err != nil {
			return Campaign{}, fmt.Errorf("materialize campaign post: %w", err)
		}
		snapshot := mustJSON(map[string]any{
			"title": row.title, "content": row.content, "format": row.format,
			"channel_id": row.channelID, "image_url": "", "image_path": "", "image_prompt": "",
			"link_buttons": []any{}, "notify": true, "disable_link_preview": false,
			"attachments": []any{},
		})
		var revisionID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO post_revisions(
workspace_id,post_id,revision_number,author_user_id,snapshot,created_at)
VALUES($1,$2,1,$3,$4,$5) RETURNING id`, workspaceID, postID, actorUserID, snapshot, now).Scan(&revisionID); err != nil {
			return Campaign{}, fmt.Errorf("materialize campaign revision: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE posts SET current_revision_id=$1 WHERE workspace_id=$2 AND id=$3`,
			revisionID, workspaceID, postID); err != nil {
			return Campaign{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE campaign_variants
SET post_id=$1,status='materialized',updated_at=$2 WHERE workspace_id=$3 AND id=$4`,
			postID, now, workspaceID, row.id); err != nil {
			return Campaign{}, err
		}
		materialized++
	}
	if materialized > 0 {
		if err := appendAuditEventTx(ctx, tx, AuditEvent{
			WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "campaign.materialized",
			EntityType: "campaign", EntityID: campaignID,
			Metadata: mustJSON(map[string]any{"post_count": materialized}), CreatedAt: now,
		}); err != nil {
			return Campaign{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Campaign{}, err
	}
	return s.GetCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func validateCampaignVariant(variant CampaignVariant) error {
	variant.Title = strings.TrimSpace(variant.Title)
	if variant.ChannelID <= 0 {
		return errors.New("channel_id must be positive")
	}
	if variant.Title == "" || utf8.RuneCountInString(variant.Title) > 200 {
		return errors.New("title must contain 1 to 200 characters")
	}
	if strings.TrimSpace(variant.Content) == "" || utf8.RuneCountInString(variant.Content) > 4000 {
		return errors.New("content must contain 1 to 4000 characters")
	}
	if !ValidFormat(variant.Format) {
		return errors.New("format must be markdown or html")
	}
	if variant.PlannedAt.IsZero() {
		return errors.New("planned_at is required")
	}
	return nil
}

const campaignVariantSelect = `SELECT cv.id,cv.workspace_id,cv.campaign_id,cv.channel_id,c.title,
cv.post_id,cv.title,cv.content,cv.format,cv.planned_at,cv.status,cv.created_by,cv.created_at,cv.updated_at,
COALESCE(p.status,''),COALESCE(p.review_status,''),p.updated_at
FROM campaign_variants cv
JOIN channels c ON c.workspace_id=cv.workspace_id AND c.id=cv.channel_id
LEFT JOIN posts p ON p.workspace_id=cv.workspace_id AND p.id=cv.post_id `

func (s *Store) listCampaignVariants(ctx context.Context, workspaceID, campaignID string) ([]CampaignVariant, error) {
	rows, err := s.db.QueryContext(ctx, campaignVariantSelect+`
WHERE cv.workspace_id=$1 AND cv.campaign_id=$2 ORDER BY cv.planned_at,cv.id`, workspaceID, campaignID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]CampaignVariant, 0)
	for rows.Next() {
		variant, err := scanCampaignVariant(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, variant)
	}
	return result, rows.Err()
}

func (s *Store) getCampaignVariant(ctx context.Context, workspaceID, campaignID, variantID string) (CampaignVariant, error) {
	return scanCampaignVariant(s.db.QueryRowContext(ctx, campaignVariantSelect+`
WHERE cv.workspace_id=$1 AND cv.campaign_id=$2 AND cv.id=$3`, workspaceID, campaignID, variantID))
}

func scanCampaign(row scanner) (Campaign, error) {
	var campaign Campaign
	if err := row.Scan(&campaign.ID, &campaign.WorkspaceID, &campaign.Name, &campaign.Description,
		&campaign.Status, &campaign.CreatedBy, &campaign.CreatedAt, &campaign.UpdatedAt, &campaign.ArchivedAt); errors.Is(err, sql.ErrNoRows) {
		return Campaign{}, ErrNotFound
	} else if err != nil {
		return Campaign{}, err
	}
	campaign.CreatedAt, campaign.UpdatedAt = campaign.CreatedAt.UTC(), campaign.UpdatedAt.UTC()
	if campaign.ArchivedAt != nil {
		value := campaign.ArchivedAt.UTC()
		campaign.ArchivedAt = &value
	}
	return campaign, nil
}

func scanCampaignVariant(row scanner) (CampaignVariant, error) {
	var variant CampaignVariant
	if err := row.Scan(&variant.ID, &variant.WorkspaceID, &variant.CampaignID, &variant.ChannelID,
		&variant.ChannelTitle, &variant.PostID, &variant.Title, &variant.Content, &variant.Format,
		&variant.PlannedAt, &variant.Status, &variant.CreatedBy, &variant.CreatedAt, &variant.UpdatedAt,
		&variant.PostStatus, &variant.ReviewStatus, &variant.PostUpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return CampaignVariant{}, ErrNotFound
	} else if err != nil {
		return CampaignVariant{}, err
	}
	variant.PlannedAt = variant.PlannedAt.UTC()
	variant.CreatedAt, variant.UpdatedAt = variant.CreatedAt.UTC(), variant.UpdatedAt.UTC()
	if variant.PostUpdatedAt != nil {
		value := variant.PostUpdatedAt.UTC()
		variant.PostUpdatedAt = &value
	}
	return variant, nil
}

func sortCampaignScheduleItems(items []CampaignScheduleItem) {
	sort.Slice(items, func(i, j int) bool { return items[i].VariantID < items[j].VariantID })
}
