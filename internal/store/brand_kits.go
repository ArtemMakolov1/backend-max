package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	maxBrandAudienceRunes       = 500
	maxBrandToneRunes           = 100
	maxBrandCTARunes            = 500
	maxBrandVisualStyleRunes    = 1000
	maxBrandForbiddenWords      = 50
	maxBrandForbiddenWordRunes  = 100
	maxBrandExamplePosts        = 10
	maxBrandExamplePostRunes    = 4000
	maxChannelTemplateNameRunes = 120
)

const brandProfileColumns = `audience,tone,cta,forbidden_words,example_posts,visual_style`

const channelTemplateColumns = `id,workspace_id,channel_id,name,` + brandProfileColumns +
	`,is_default,version,created_at,updated_at`

// BrandProfile is reusable editorial guidance. It is deliberately data-only:
// callers must keep treating it as untrusted editorial context when building
// model prompts.
type BrandProfile struct {
	Audience       string   `json:"audience"`
	Tone           string   `json:"tone"`
	CTA            string   `json:"cta"`
	ForbiddenWords []string `json:"forbidden_words"`
	ExamplePosts   []string `json:"example_posts"`
	VisualStyle    string   `json:"visual_style"`
}

type WorkspaceBrandKit struct {
	WorkspaceID string `json:"workspace_id"`
	BrandProfile
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type WorkspaceBrandKitUpdate struct {
	BrandProfile
	ExpectedVersion int64
}

type ChannelTemplate struct {
	ID          int64  `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ChannelID   *int64 `json:"channel_id,omitempty"`
	Name        string `json:"name"`
	BrandProfile
	IsDefault bool      `json:"is_default"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ChannelTemplateCreate struct {
	ChannelID *int64
	Name      string
	BrandProfile
	IsDefault bool
}

type ChannelTemplateUpdate struct {
	ChannelID *int64
	Name      string
	BrandProfile
	IsDefault       bool
	ExpectedVersion int64
}

// WorkspaceBrandContext contains the workspace baseline and the selected
// channel override. Template is nil when the workspace has no matching/default
// template.
type WorkspaceBrandContext struct {
	BrandKit WorkspaceBrandKit
	Template *ChannelTemplate
}

func (s *Store) GetWorkspaceBrandKit(ctx context.Context, actorUserID, workspaceID string) (WorkspaceBrandKit, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return WorkspaceBrandKit{}, err
	}
	return getWorkspaceBrandKit(ctx, s.db, workspaceID)
}

func getWorkspaceBrandKit(ctx context.Context, q workspaceQueryer, workspaceID string) (WorkspaceBrandKit, error) {
	return scanWorkspaceBrandKit(q.QueryRowContext(ctx, `SELECT workspace_id,`+brandProfileColumns+
		`,version,created_at,updated_at FROM workspace_brand_kits WHERE workspace_id=$1`, workspaceID))
}

func (s *Store) UpdateWorkspaceBrandKit(
	ctx context.Context,
	actorUserID, workspaceID string,
	update WorkspaceBrandKitUpdate,
) (WorkspaceBrandKit, error) {
	profile, err := normalizeBrandProfile(update.BrandProfile)
	if err != nil {
		return WorkspaceBrandKit{}, err
	}
	if update.ExpectedVersion <= 0 {
		return WorkspaceBrandKit{}, errors.New("brand kit version must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceBrandKit{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return WorkspaceBrandKit{}, err
	}
	now := time.Now().UTC()
	kit, err := scanWorkspaceBrandKit(tx.QueryRowContext(ctx, `UPDATE workspace_brand_kits SET
audience=$1,tone=$2,cta=$3,forbidden_words=$4,example_posts=$5,visual_style=$6,
version=version+1,updated_at=$7
WHERE workspace_id=$8 AND version=$9
RETURNING workspace_id,`+brandProfileColumns+`,version,created_at,updated_at`,
		profile.Audience, profile.Tone, profile.CTA, profile.ForbiddenWords, profile.ExamplePosts,
		profile.VisualStyle, now, workspaceID, update.ExpectedVersion))
	if errors.Is(err, ErrNotFound) {
		return WorkspaceBrandKit{}, fmt.Errorf("%w: brand kit changed in another session", ErrConflict)
	}
	if err != nil {
		return WorkspaceBrandKit{}, mapWorkspaceWriteError("update workspace brand kit", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "brand_kit.updated",
		EntityType: "brand_kit", EntityID: workspaceID,
		Metadata: mustJSON(map[string]any{"version": kit.Version}), CreatedAt: now,
	}); err != nil {
		return WorkspaceBrandKit{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceBrandKit{}, fmt.Errorf("commit brand kit update: %w", err)
	}
	return kit, nil
}

func (s *Store) ListChannelTemplates(ctx context.Context, actorUserID, workspaceID string) ([]ChannelTemplate, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelTemplateColumns+`
FROM workspace_channel_templates WHERE workspace_id=$1
ORDER BY is_default DESC,lower(name),id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list channel templates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]ChannelTemplate, 0)
	for rows.Next() {
		template, err := scanChannelTemplate(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, template)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channel templates: %w", err)
	}
	return result, nil
}

func (s *Store) GetChannelTemplate(
	ctx context.Context, actorUserID, workspaceID string, templateID int64,
) (ChannelTemplate, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return ChannelTemplate{}, err
	}
	return getChannelTemplate(ctx, s.db, workspaceID, templateID)
}

func getChannelTemplate(
	ctx context.Context, q workspaceQueryer, workspaceID string, templateID int64,
) (ChannelTemplate, error) {
	if templateID <= 0 {
		return ChannelTemplate{}, ErrNotFound
	}
	return scanChannelTemplate(q.QueryRowContext(ctx, `SELECT `+channelTemplateColumns+
		` FROM workspace_channel_templates WHERE workspace_id=$1 AND id=$2`, workspaceID, templateID))
}

// ResolveWorkspaceBrandContext resolves an explicit template, or applies
// channel-specific -> global-default precedence when channelID is supplied.
// With neither selector it deliberately returns the workspace kit only.
// Supplying an unknown/foreign selector is a not-found error so tenant
// membership cannot be inferred.
func (s *Store) ResolveWorkspaceBrandContext(
	ctx context.Context, actorUserID, workspaceID string, templateID, channelID *int64,
) (WorkspaceBrandContext, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return WorkspaceBrandContext{}, err
	}
	kit, err := getWorkspaceBrandKit(ctx, s.db, workspaceID)
	if err != nil {
		return WorkspaceBrandContext{}, err
	}
	var template ChannelTemplate
	if templateID != nil {
		template, err = getChannelTemplate(ctx, s.db, workspaceID, *templateID)
		if err != nil {
			return WorkspaceBrandContext{}, err
		}
	} else if channelID != nil {
		if *channelID <= 0 {
			return WorkspaceBrandContext{}, ErrNotFound
		}
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM channels WHERE workspace_id=$1 AND id=$2`,
			workspaceID, *channelID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return WorkspaceBrandContext{}, ErrNotFound
		} else if err != nil {
			return WorkspaceBrandContext{}, fmt.Errorf("resolve brand context channel: %w", err)
		}
		template, err = scanChannelTemplate(s.db.QueryRowContext(ctx, `SELECT `+channelTemplateColumns+
			` FROM workspace_channel_templates WHERE workspace_id=$1 AND channel_id=$2
ORDER BY is_default DESC,updated_at DESC,id DESC LIMIT 1`, workspaceID, *channelID))
		if errors.Is(err, ErrNotFound) {
			template, err = scanChannelTemplate(s.db.QueryRowContext(ctx, `SELECT `+channelTemplateColumns+
				` FROM workspace_channel_templates WHERE workspace_id=$1 AND channel_id IS NULL AND is_default=TRUE`, workspaceID))
		}
	} else {
		return WorkspaceBrandContext{BrandKit: kit}, nil
	}
	if errors.Is(err, ErrNotFound) {
		return WorkspaceBrandContext{BrandKit: kit}, nil
	}
	if err != nil {
		return WorkspaceBrandContext{}, err
	}
	return WorkspaceBrandContext{BrandKit: kit, Template: &template}, nil
}

func (s *Store) CreateChannelTemplate(
	ctx context.Context, actorUserID, workspaceID string, input ChannelTemplateCreate,
) (ChannelTemplate, error) {
	name, profile, err := normalizeChannelTemplateInput(input.Name, input.BrandProfile)
	if err != nil {
		return ChannelTemplate{}, err
	}
	if input.ChannelID != nil && *input.ChannelID <= 0 {
		return ChannelTemplate{}, errors.New("channel id must be positive")
	}
	if input.IsDefault && input.ChannelID != nil {
		return ChannelTemplate{}, errors.New("default channel template must not target a channel")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChannelTemplate{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return ChannelTemplate{}, err
	}
	now := time.Now().UTC()
	if input.IsDefault {
		if err := clearChannelTemplateDefaults(ctx, tx, actorUserID, workspaceID, 0, now); err != nil {
			return ChannelTemplate{}, err
		}
	}
	template, err := scanChannelTemplate(tx.QueryRowContext(ctx, `INSERT INTO workspace_channel_templates(
workspace_id,channel_id,name,audience,tone,cta,forbidden_words,example_posts,visual_style,is_default,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)
RETURNING `+channelTemplateColumns, workspaceID, input.ChannelID, name, profile.Audience, profile.Tone,
		profile.CTA, profile.ForbiddenWords, profile.ExamplePosts, profile.VisualStyle, input.IsDefault, now))
	if err != nil {
		return ChannelTemplate{}, mapWorkspaceWriteError("create channel template", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "channel_template.created",
		EntityType: "channel_template", EntityID: fmt.Sprint(template.ID),
		Metadata: mustJSON(channelTemplateAuditMetadata(template)), CreatedAt: now,
	}); err != nil {
		return ChannelTemplate{}, err
	}
	if err := tx.Commit(); err != nil {
		return ChannelTemplate{}, fmt.Errorf("commit channel template create: %w", err)
	}
	return template, nil
}

func (s *Store) UpdateChannelTemplate(
	ctx context.Context, actorUserID, workspaceID string, templateID int64, input ChannelTemplateUpdate,
) (ChannelTemplate, error) {
	if templateID <= 0 {
		return ChannelTemplate{}, ErrNotFound
	}
	if input.ExpectedVersion <= 0 {
		return ChannelTemplate{}, errors.New("channel template version must be positive")
	}
	if input.ChannelID != nil && *input.ChannelID <= 0 {
		return ChannelTemplate{}, errors.New("channel id must be positive")
	}
	if input.IsDefault && input.ChannelID != nil {
		return ChannelTemplate{}, errors.New("default channel template must not target a channel")
	}
	name, profile, err := normalizeChannelTemplateInput(input.Name, input.BrandProfile)
	if err != nil {
		return ChannelTemplate{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChannelTemplate{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return ChannelTemplate{}, err
	}
	now := time.Now().UTC()
	if input.IsDefault {
		if err := clearChannelTemplateDefaults(ctx, tx, actorUserID, workspaceID, templateID, now); err != nil {
			return ChannelTemplate{}, err
		}
	}
	template, err := scanChannelTemplate(tx.QueryRowContext(ctx, `UPDATE workspace_channel_templates SET
channel_id=$1,name=$2,audience=$3,tone=$4,cta=$5,forbidden_words=$6,example_posts=$7,
visual_style=$8,is_default=$9,version=version+1,updated_at=$10
WHERE workspace_id=$11 AND id=$12 AND version=$13
RETURNING `+channelTemplateColumns, input.ChannelID, name, profile.Audience, profile.Tone, profile.CTA,
		profile.ForbiddenWords, profile.ExamplePosts, profile.VisualStyle, input.IsDefault, now,
		workspaceID, templateID, input.ExpectedVersion))
	if errors.Is(err, ErrNotFound) {
		return ChannelTemplate{}, fmt.Errorf("%w: channel template changed in another session", ErrConflict)
	}
	if err != nil {
		return ChannelTemplate{}, mapWorkspaceWriteError("update channel template", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "channel_template.updated",
		EntityType: "channel_template", EntityID: fmt.Sprint(template.ID),
		Metadata: mustJSON(channelTemplateAuditMetadata(template)), CreatedAt: now,
	}); err != nil {
		return ChannelTemplate{}, err
	}
	if err := tx.Commit(); err != nil {
		return ChannelTemplate{}, fmt.Errorf("commit channel template update: %w", err)
	}
	return template, nil
}

func (s *Store) DeleteChannelTemplate(
	ctx context.Context, actorUserID, workspaceID string, templateID, expectedVersion int64,
) error {
	if templateID <= 0 {
		return ErrNotFound
	}
	if expectedVersion <= 0 {
		return errors.New("channel template version must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return err
	}
	var name string
	var version int64
	var channelID *int64
	if err := tx.QueryRowContext(ctx, `DELETE FROM workspace_channel_templates
WHERE workspace_id=$1 AND id=$2 AND version=$3 RETURNING name,channel_id,version`, workspaceID, templateID, expectedVersion).
		Scan(&name, &channelID, &version); errors.Is(err, sql.ErrNoRows) {
		var currentVersion int64
		if lookupErr := tx.QueryRowContext(ctx, `SELECT version FROM workspace_channel_templates
WHERE workspace_id=$1 AND id=$2`, workspaceID, templateID).Scan(&currentVersion); errors.Is(lookupErr, sql.ErrNoRows) {
			return ErrNotFound
		} else if lookupErr != nil {
			return fmt.Errorf("resolve channel template delete conflict: %w", lookupErr)
		}
		return fmt.Errorf("%w: channel template changed in another session", ErrConflict)
	} else if err != nil {
		return mapWorkspaceWriteError("delete channel template", err)
	}
	now := time.Now().UTC()
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "channel_template.deleted",
		EntityType: "channel_template", EntityID: fmt.Sprint(templateID),
		Metadata: mustJSON(map[string]any{"name": name, "channel_id": channelID, "version": version}), CreatedAt: now,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit channel template delete: %w", err)
	}
	return nil
}

func clearChannelTemplateDefaults(
	ctx context.Context, tx *sql.Tx, actorUserID, workspaceID string, exceptID int64, now time.Time,
) error {
	rows, err := tx.QueryContext(ctx, `UPDATE workspace_channel_templates SET
is_default=FALSE,version=version+1,updated_at=$1
WHERE workspace_id=$2 AND is_default=TRUE AND ($3::bigint=0 OR id<>$3)
RETURNING id,name,channel_id,version`, now, workspaceID, exceptID)
	if err != nil {
		return mapWorkspaceWriteError("clear channel template default", err)
	}
	type clearedTemplate struct {
		id, version int64
		name        string
		channelID   *int64
	}
	cleared := make([]clearedTemplate, 0, 1)
	for rows.Next() {
		var template clearedTemplate
		if err := rows.Scan(&template.id, &template.name, &template.channelID, &template.version); err != nil {
			_ = rows.Close()
			return err
		}
		cleared = append(cleared, template)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, template := range cleared {
		if err := appendAuditEventTx(ctx, tx, AuditEvent{
			WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "channel_template.default_cleared",
			EntityType: "channel_template", EntityID: fmt.Sprint(template.id),
			Metadata: mustJSON(map[string]any{
				"name": template.name, "channel_id": template.channelID, "version": template.version,
			}), CreatedAt: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func scanWorkspaceBrandKit(row scanner) (WorkspaceBrandKit, error) {
	var kit WorkspaceBrandKit
	typeMap := pgtype.NewMap()
	if err := row.Scan(&kit.WorkspaceID, &kit.Audience, &kit.Tone, &kit.CTA,
		typeMap.SQLScanner(&kit.ForbiddenWords), typeMap.SQLScanner(&kit.ExamplePosts),
		&kit.VisualStyle, &kit.Version, &kit.CreatedAt, &kit.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkspaceBrandKit{}, ErrNotFound
		}
		return WorkspaceBrandKit{}, fmt.Errorf("scan workspace brand kit: %w", err)
	}
	normalizeBrandProfileOutput(&kit.BrandProfile)
	kit.CreatedAt = kit.CreatedAt.UTC()
	kit.UpdatedAt = kit.UpdatedAt.UTC()
	return kit, nil
}

func scanChannelTemplate(row scanner) (ChannelTemplate, error) {
	var template ChannelTemplate
	typeMap := pgtype.NewMap()
	if err := row.Scan(&template.ID, &template.WorkspaceID, &template.ChannelID, &template.Name,
		&template.Audience, &template.Tone, &template.CTA,
		typeMap.SQLScanner(&template.ForbiddenWords), typeMap.SQLScanner(&template.ExamplePosts),
		&template.VisualStyle, &template.IsDefault, &template.Version, &template.CreatedAt, &template.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChannelTemplate{}, ErrNotFound
		}
		return ChannelTemplate{}, fmt.Errorf("scan channel template: %w", err)
	}
	normalizeBrandProfileOutput(&template.BrandProfile)
	template.CreatedAt = template.CreatedAt.UTC()
	template.UpdatedAt = template.UpdatedAt.UTC()
	return template, nil
}

func normalizeChannelTemplateInput(name string, profile BrandProfile) (string, BrandProfile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", BrandProfile{}, errors.New("channel template name is required")
	}
	if utf8.RuneCountInString(name) > maxChannelTemplateNameRunes {
		return "", BrandProfile{}, fmt.Errorf("channel template name must not exceed %d characters", maxChannelTemplateNameRunes)
	}
	profile, err := normalizeBrandProfile(profile)
	return name, profile, err
}

func normalizeBrandProfile(profile BrandProfile) (BrandProfile, error) {
	profile.Audience = strings.TrimSpace(profile.Audience)
	profile.Tone = strings.TrimSpace(profile.Tone)
	profile.CTA = strings.TrimSpace(profile.CTA)
	profile.VisualStyle = strings.TrimSpace(profile.VisualStyle)
	for _, field := range []struct {
		value string
		limit int
		name  string
	}{
		{profile.Audience, maxBrandAudienceRunes, "audience"},
		{profile.Tone, maxBrandToneRunes, "tone"},
		{profile.CTA, maxBrandCTARunes, "cta"},
		{profile.VisualStyle, maxBrandVisualStyleRunes, "visual style"},
	} {
		if utf8.RuneCountInString(field.value) > field.limit {
			return BrandProfile{}, fmt.Errorf("%s must not exceed %d characters", field.name, field.limit)
		}
	}
	var err error
	profile.ForbiddenWords, err = normalizeBrandStrings(
		profile.ForbiddenWords, maxBrandForbiddenWords, maxBrandForbiddenWordRunes, true, "forbidden words")
	if err != nil {
		return BrandProfile{}, err
	}
	profile.ExamplePosts, err = normalizeBrandStrings(
		profile.ExamplePosts, maxBrandExamplePosts, maxBrandExamplePostRunes, false, "example posts")
	if err != nil {
		return BrandProfile{}, err
	}
	return profile, nil
}

func normalizeBrandStrings(values []string, maxItems, maxRunes int, foldCase bool, field string) ([]string, error) {
	if len(values) > maxItems {
		return nil, fmt.Errorf("%s must not exceed %d items", field, maxItems)
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, fmt.Errorf("%s must not contain empty items", field)
		}
		if utf8.RuneCountInString(value) > maxRunes {
			return nil, fmt.Errorf("%s item must not exceed %d characters", field, maxRunes)
		}
		key := value
		if foldCase {
			key = strings.ToLower(value)
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func normalizeBrandProfileOutput(profile *BrandProfile) {
	if profile.ForbiddenWords == nil {
		profile.ForbiddenWords = []string{}
	}
	if profile.ExamplePosts == nil {
		profile.ExamplePosts = []string{}
	}
}

func channelTemplateAuditMetadata(template ChannelTemplate) map[string]any {
	return map[string]any{
		"name": template.Name, "channel_id": template.ChannelID,
		"is_default": template.IsDefault, "version": template.Version,
	}
}
