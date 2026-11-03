// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	entitlementspg "github.com/fydeos/fydeos-notes-server/internal/entitlements/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ProfileStore persists workspace BYOK profiles and resolves call targets.
type ProfileStore struct {
	db          Database
	runtimePath string
	deployment  string
	keyLoader   KeyLoader

	mu       sync.RWMutex
	resolver EndpointResolver
	runner   ProbeRunner
}

// NewProfileStore creates a store backed by db.
func NewProfileStore(db Database, runtimePath, deployment string, keyLoader KeyLoader) *ProfileStore {
	return &ProfileStore{db: db, runtimePath: runtimePath, deployment: deployment, keyLoader: keyLoader}
}

// SetEndpointResolver overrides DNS resolution for custom endpoints.
func (s *ProfileStore) SetEndpointResolver(resolver EndpointResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolver = resolver
}

// SetProbeRunner overrides the connectivity probe runner.
func (s *ProfileStore) SetProbeRunner(runner ProbeRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runner = runner
}

func (s *ProfileStore) endpointResolver() EndpointResolver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolver
}

func (s *ProfileStore) probeRunner() ProbeRunner {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.runner == nil {
		return HTTPProbeRunner{}
	}
	return s.runner
}

// Policy loads the current runtime policy.
func (s *ProfileStore) Policy(ctx context.Context) (Policy, error) {
	return LoadPolicy(ctx, s.db, s.runtimePath, s.deployment)
}

// rootSecret loads the root secret; any failure is ErrCredentialUnavailable.
func (s *ProfileStore) rootSecret(ctx context.Context) ([]byte, error) {
	if s.keyLoader == nil {
		return nil, ErrCredentialUnavailable
	}
	secret, err := s.keyLoader(ctx)
	if err != nil || strings.TrimSpace(secret) == "" {
		return nil, ErrCredentialUnavailable
	}
	return []byte(secret), nil
}

func (s *ProfileStore) envelopeKey(ctx context.Context) (*CredentialEnvelopeKey, error) {
	secret, err := s.rootSecret(ctx)
	if err != nil {
		return nil, err
	}
	defer clear(secret)
	return NewCredentialEnvelopeKey(secret)
}

// --- Entitlement -----------------------------------------------------------

// Entitlement evaluates BYOK rights for a workspace and actor.
func (s *ProfileStore) Entitlement(ctx context.Context, workspaceID, actorID string) (Entitlement, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Entitlement{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
		return Entitlement{}, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $1)`, workspaceID).Scan(&exists); err != nil {
		return Entitlement{}, err
	}
	if !exists {
		return Entitlement{}, nil
	}
	var ownerID string
	var now time.Time
	err = tx.QueryRow(ctx, `
		SELECT user_id, now() FROM workspace_members
		WHERE workspace_id = $1 AND role = 'owner' AND state = 'active'
		ORDER BY created_at, user_id LIMIT 1`, workspaceID).Scan(&ownerID, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entitlement{}, ErrWorkspaceOwnerNotFound
	}
	if err != nil {
		return Entitlement{}, err
	}
	deployment := entitlements.ParseDeployment(s.deployment)
	if deployment == entitlements.SelfHosted {
		return Entitlement{Server: true, Local: true}, nil
	}
	workspaceFacts, err := entitlementspg.LoadFacts(ctx, tx, "workspace", []string{workspaceID})
	if err != nil {
		return Entitlement{}, err
	}
	users := []string{ownerID}
	if actorID != "" && actorID != ownerID {
		users = append(users, actorID)
	}
	userFacts, err := entitlementspg.LoadFacts(ctx, tx, "user", users)
	if err != nil {
		return Entitlement{}, err
	}
	grant := func(targetType, id string, facts []entitlements.Fact) bool {
		ctx := entitlements.Context{Deployment: deployment, TargetType: targetType, Now: now}
		if targetType == "workspace" {
			ctx.WorkspaceID = id
		}
		return entitlements.Resolve(ctx, facts).Rights.CopilotBYOK
	}
	workspaceGrant := grant("workspace", workspaceID, workspaceFacts[workspaceID])
	ownerGrant := grant("user", ownerID, userFacts[ownerID])
	actorGrant := actorID != "" && grant("user", actorID, userFacts[actorID])
	return Entitlement{Server: workspaceGrant || ownerGrant, Local: workspaceGrant || ownerGrant || actorGrant}, nil
}

// --- Usage -----------------------------------------------------------------

// AggregateUsageByDay sums BYOK token usage per UTC day and feature in [from, to).
func (s *ProfileStore) AggregateUsageByDay(ctx context.Context, workspaceID string, from, to time.Time) ([]UsagePoint, error) {
	rows, err := s.db.Query(ctx, `
		SELECT date_trunc('day', created_at AT TIME ZONE 'UTC') AS day, feature_kind, coalesce(sum(total_tokens), 0)::bigint
		FROM ai_usage_events
		WHERE workspace_id = $1 AND provider_source IN ('byok_server', 'byok_local')
		  AND created_at >= $2 AND created_at < $3
		GROUP BY 1, 2
		ORDER BY 1, 2`, workspaceID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []UsagePoint{}
	for rows.Next() {
		var day time.Time
		var point UsagePoint
		if err := rows.Scan(&day, &point.FeatureKind, &point.TotalTokens); err != nil {
			return nil, err
		}
		point.Date = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		points = append(points, point)
	}
	return points, rows.Err()
}

// --- Profile rows ----------------------------------------------------------

const profileColumns = `id, workspace_id, provider, name, description, encrypted_api_key, definition,
	sort_order, enabled, revision, credential_generation, validation, created_at`

func scanProfile(row pgx.Row) (Profile, error) {
	var p Profile
	var definition, validation []byte
	var sortOrder, revision, generation int32
	if err := row.Scan(&p.ProfileID, &p.WorkspaceID, &p.Provider, &p.Name, &p.Description, &p.EncryptedCredential,
		&definition, &sortOrder, &p.Enabled, &revision, &generation, &validation, &p.CreatedAt); err != nil {
		return Profile{}, err
	}
	p.SortOrder, p.Revision, p.CredentialGeneration = int64(sortOrder), int64(revision), int64(generation)
	if err := json.Unmarshal(definition, &p.Definition); err != nil {
		return Profile{}, fmt.Errorf("decode BYOK profile definition: %w", err)
	}
	if len(validation) > 0 && string(validation) != "null" {
		var v Validation
		if err := json.Unmarshal(validation, &v); err != nil {
			return Profile{}, fmt.Errorf("decode BYOK profile validation: %w", err)
		}
		p.Validation = &v
	}
	return p, nil
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func listProfiles(ctx context.Context, q queryer, workspaceID string) ([]Profile, error) {
	rows, err := q.Query(ctx, `SELECT `+profileColumns+` FROM ai_workspace_byok_configs
		WHERE workspace_id = $1 ORDER BY sort_order, created_at, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := []Profile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

func getProfile(ctx context.Context, q queryer, workspaceID, profileID string, forUpdate bool) (Profile, error) {
	query := `SELECT ` + profileColumns + ` FROM ai_workspace_byok_configs WHERE workspace_id = $1 AND id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	p, err := scanProfile(q.QueryRow(ctx, query, workspaceID, profileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrProfileNotFound
	}
	return p, err
}

func lockWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('byok-profiles:' || $1, 0))`, workspaceID)
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func marshalValidation(v *Validation) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func requireField(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalidf("%s is required", name)
	}
	return nil
}

// List returns all workspace profiles in display order.
func (s *ProfileStore) List(ctx context.Context, workspaceID string) ([]Profile, error) {
	return listProfiles(ctx, s.db, workspaceID)
}

// admitDefinition validates a definition and admits it under current policy.
func (s *ProfileStore) admitDefinition(ctx context.Context, provider string, input ProfileDefinitionInput) (ProfileDefinition, error) {
	definition, err := ValidateDefinition(provider, input)
	if err != nil {
		return ProfileDefinition{}, err
	}
	policy, err := s.Policy(ctx)
	if err != nil {
		return ProfileDefinition{}, err
	}
	if err := policy.Admit(ctx, provider, definition.Endpoint, s.endpointResolver()); err != nil {
		return ProfileDefinition{}, err
	}
	return definition, nil
}

func (s *ProfileStore) encryptServer(ctx context.Context, credential []byte, workspaceID, profileID, provider string, endpoint Endpoint) (string, error) {
	key, err := s.envelopeKey(ctx)
	if err != nil {
		return "", err
	}
	defer key.Destroy()
	return key.Encrypt(credential, ServerAAD(workspaceID, profileID, provider, endpointIdentity(endpoint)))
}

func (s *ProfileStore) decryptServer(ctx context.Context, p Profile) (*SensitiveCredential, error) {
	key, err := s.envelopeKey(ctx)
	if err != nil {
		return nil, err
	}
	defer key.Destroy()
	return key.Decrypt(p.EncryptedCredential, ServerAAD(p.WorkspaceID, p.ProfileID, p.Provider, endpointIdentity(p.Definition.Endpoint)))
}

// Create stores a new server profile.
func (s *ProfileStore) Create(ctx context.Context, input CreateProfileInput) (Profile, error) {
	for _, f := range [][2]string{{"workspaceId", input.WorkspaceID}, {"name", input.Name}, {"credential", input.Credential}, {"actorUserId", input.ActorUserID}} {
		if err := requireField(f[0], f[1]); err != nil {
			return Profile{}, err
		}
	}
	definition, err := s.admitDefinition(ctx, input.Provider, input.Definition)
	if err != nil {
		return Profile{}, err
	}
	profileID := uuid.NewString()
	encrypted, err := s.encryptServer(ctx, []byte(input.Credential), input.WorkspaceID, profileID, input.Provider, definition.Endpoint)
	if err != nil {
		return Profile{}, err
	}
	definitionJSON, err := json.Marshal(definition)
	if err != nil {
		return Profile{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Profile{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockWorkspace(ctx, tx, input.WorkspaceID); err != nil {
		return Profile{}, err
	}
	var sortOrder int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(sort_order) + 1, 0) FROM ai_workspace_byok_configs WHERE workspace_id = $1`,
		input.WorkspaceID).Scan(&sortOrder); err != nil {
		return Profile{}, err
	}
	profile, err := scanProfile(tx.QueryRow(ctx, `
		INSERT INTO ai_workspace_byok_configs (id, workspace_id, provider, name, description, encrypted_api_key, definition,
			sort_order, enabled, revision, credential_generation, validation, created_by, updated_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, 1, NULL, $10, $10, now(), now())
		RETURNING `+profileColumns,
		profileID, input.WorkspaceID, input.Provider, input.Name, input.Description, encrypted, definitionJSON,
		sortOrder, input.Enabled, input.ActorUserID))
	if isUniqueViolation(err) {
		return Profile{}, ErrProfileNameConflict
	}
	if err != nil {
		return Profile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return Profile{}, ErrProfileNameConflict
		}
		return Profile{}, err
	}
	return profile, nil
}

// reconcileValidation keeps evidence that still applies to the new definition.
func reconcileValidation(old *Validation, oldDef, newDef ProfileDefinition, generation int64) *Validation {
	if old == nil || oldDef.Endpoint != newDef.Endpoint {
		return nil
	}
	kept := &Validation{
		DefinitionFingerprint: definitionFingerprint(newDef),
		CredentialGeneration:  generation,
		Connection:            old.Connection,
		Models:                []ModelProbe{},
	}
	for _, probe := range old.Models {
		before, okBefore := findModel(oldDef, probe.ModelID)
		after, okAfter := findModel(newDef, probe.ModelID)
		if okBefore && okAfter && reflect.DeepEqual(before, after) {
			kept.Models = append(kept.Models, probe)
		}
	}
	return kept
}

// Replace updates a profile's definition, metadata and optionally credential.
func (s *ProfileStore) Replace(ctx context.Context, input ReplaceProfileInput) (Profile, error) {
	for _, f := range [][2]string{{"workspaceId", input.WorkspaceID}, {"profileId", input.ProfileID}, {"name", input.Name}, {"actorUserId", input.ActorUserID}} {
		if err := requireField(f[0], f[1]); err != nil {
			return Profile{}, err
		}
	}
	if input.ExpectedRevision < 1 {
		return Profile{}, invalidf("expectedRevision must be positive")
	}
	if input.Credential != nil && strings.TrimSpace(*input.Credential) == "" {
		return Profile{}, invalidf("credential is required")
	}
	current, err := getProfile(ctx, s.db, input.WorkspaceID, input.ProfileID, false)
	if err != nil {
		return Profile{}, err
	}
	if current.Revision != input.ExpectedRevision {
		return Profile{}, ErrRevisionConflict
	}
	definition, err := s.admitDefinition(ctx, current.Provider, input.Definition)
	if err != nil {
		return Profile{}, err
	}
	var plaintext *SensitiveCredential
	if input.Credential != nil {
		plaintext = NewSensitiveCredential([]byte(*input.Credential))
	} else if plaintext, err = s.decryptServer(ctx, current); err != nil {
		return Profile{}, err
	}
	defer plaintext.Destroy()
	encrypted, err := s.encryptServer(ctx, plaintext.Bytes(), current.WorkspaceID, current.ProfileID, current.Provider, definition.Endpoint)
	if err != nil {
		return Profile{}, err
	}
	definitionJSON, err := json.Marshal(definition)
	if err != nil {
		return Profile{}, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Profile{}, err
	}
	defer tx.Rollback(ctx)
	locked, err := getProfile(ctx, tx, input.WorkspaceID, input.ProfileID, true)
	if err != nil {
		return Profile{}, err
	}
	if locked.Revision != input.ExpectedRevision {
		return Profile{}, ErrRevisionConflict
	}
	generation := locked.CredentialGeneration
	var validation *Validation
	if input.Credential != nil {
		generation++
	} else {
		validation = reconcileValidation(locked.Validation, locked.Definition, definition, generation)
	}
	validationJSON, err := marshalValidation(validation)
	if err != nil {
		return Profile{}, err
	}
	profile, err := scanProfile(tx.QueryRow(ctx, `
		UPDATE ai_workspace_byok_configs
		SET name = $3, description = $4, definition = $5, encrypted_api_key = $6, enabled = $7,
			credential_generation = $8, validation = $9, revision = revision + 1, updated_by = $10, updated_at = now()
		WHERE workspace_id = $1 AND id = $2
		RETURNING `+profileColumns,
		input.WorkspaceID, input.ProfileID, input.Name, input.Description, definitionJSON, encrypted, input.Enabled,
		generation, validationJSON, input.ActorUserID))
	if isUniqueViolation(err) {
		return Profile{}, ErrProfileNameConflict
	}
	if err != nil {
		return Profile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

// Rotate replaces a profile's credential.
func (s *ProfileStore) Rotate(ctx context.Context, input RotateCredentialInput) (Profile, error) {
	for _, f := range [][2]string{{"workspaceId", input.WorkspaceID}, {"profileId", input.ProfileID}, {"credential", input.Credential}, {"actorUserId", input.ActorUserID}} {
		if err := requireField(f[0], f[1]); err != nil {
			return Profile{}, err
		}
	}
	if input.ExpectedRevision < 1 {
		return Profile{}, invalidf("expectedRevision must be positive")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Profile{}, err
	}
	defer tx.Rollback(ctx)
	current, err := getProfile(ctx, tx, input.WorkspaceID, input.ProfileID, true)
	if err != nil {
		return Profile{}, err
	}
	if current.Revision != input.ExpectedRevision {
		return Profile{}, ErrRevisionConflict
	}
	encrypted, err := s.encryptServer(ctx, []byte(input.Credential), current.WorkspaceID, current.ProfileID, current.Provider, current.Definition.Endpoint)
	if err != nil {
		return Profile{}, err
	}
	profile, err := scanProfile(tx.QueryRow(ctx, `
		UPDATE ai_workspace_byok_configs
		SET encrypted_api_key = $3, credential_generation = credential_generation + 1, revision = revision + 1,
			validation = NULL, updated_by = $4, updated_at = now()
		WHERE workspace_id = $1 AND id = $2
		RETURNING `+profileColumns, input.WorkspaceID, input.ProfileID, encrypted, input.ActorUserID))
	if err != nil {
		return Profile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

// Delete removes a profile; it reports whether a row was removed.
func (s *ProfileStore) Delete(ctx context.Context, workspaceID, profileID string) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM ai_workspace_byok_configs WHERE workspace_id = $1 AND id = $2`, workspaceID, profileID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Reorder assigns sort orders from the list position, all or nothing.
func (s *ProfileStore) Reorder(ctx context.Context, workspaceID, actorUserID string, profiles []ProfileOrderInput) ([]Profile, error) {
	if err := requireField("workspaceId", workspaceID); err != nil {
		return nil, err
	}
	if err := requireField("actorUserId", actorUserID); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, item := range profiles {
		if strings.TrimSpace(item.ProfileID) == "" || item.ExpectedRevision < 1 {
			return nil, invalidf("profileId and expectedRevision are required")
		}
		if seen[item.ProfileID] {
			return nil, invalidf("duplicate profileId")
		}
		seen[item.ProfileID] = true
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockWorkspace(ctx, tx, workspaceID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id, revision FROM ai_workspace_byok_configs WHERE workspace_id = $1 FOR UPDATE`, workspaceID)
	if err != nil {
		return nil, err
	}
	revisions := map[string]int64{}
	for rows.Next() {
		var id string
		var revision int32
		if err := rows.Scan(&id, &revision); err != nil {
			rows.Close()
			return nil, err
		}
		revisions[id] = int64(revision)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(revisions) != len(profiles) {
		return nil, invalidf("BYOK profile order must contain every server profile")
	}
	for _, item := range profiles {
		revision, ok := revisions[item.ProfileID]
		if !ok || revision != item.ExpectedRevision {
			return nil, ErrRevisionConflict
		}
	}
	for index, item := range profiles {
		if _, err := tx.Exec(ctx, `
			UPDATE ai_workspace_byok_configs
			SET sort_order = $3, revision = revision + 1, updated_by = $4, updated_at = now()
			WHERE workspace_id = $1 AND id = $2`, workspaceID, item.ProfileID, index, actorUserID); err != nil {
			return nil, err
		}
	}
	result, err := listProfiles(ctx, tx, workspaceID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}
