// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package mcpcredentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	TokenPrefix           = "aff_mcp_v1"
	rotationGrace         = 24 * time.Hour
	lastUsedWriteInterval = 5 * time.Minute
)

var (
	ErrCopilotDisabled       = errors.New("Copilot is disabled.")
	ErrCredentialNotFound    = errors.New("MCP credential not found")
	ErrInvalidExpiration     = errors.New("Unsupported MCP credential expiration")
	ErrInvalidName           = errors.New("MCP credential name is required")
	ErrInvalidAccessMode     = errors.New("Unsupported MCP credential access mode")
	ErrWriteToolsUnavailable = errors.New("MCP write tools are not available")
	ErrUnauthorized          = errors.New("unauthorized MCP credential")
	ErrUnavailable           = errors.New("MCP credential service is unavailable")
)

type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type DB interface {
	Querier
	Begin(context.Context) (pgx.Tx, error)
}

type Service struct {
	db  DB
	cfg config.Config
	now func() time.Time
}

type CreateInput struct {
	UserID         string
	WorkspaceID    string
	Name           string
	AccessMode     string
	ExpirationDays int
}

type Credential struct {
	ID          string
	Name        string
	WorkspaceID string
	AccessMode  string
	Fingerprint string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
	GraceEndsAt *time.Time
	Status      string
}

type Issued struct {
	Credential Credential
	Token      string
}

type AuthenticatedCredential struct {
	ID          string
	UserID      string
	WorkspaceID string
	AccessMode  string
	Name        string
}

type storedCredential struct {
	Credential
	UserID       string
	FamilyID     string
	Generation   int
	SecretHash   string
	ReplacedByID *string
}

func NewService(db DB, cfg config.Config) *Service {
	return &Service{db: db, cfg: cfg, now: time.Now}
}

func (s *Service) CopilotEnabled(ctx context.Context) (bool, error) {
	if s.db == nil {
		return false, ErrUnavailable
	}
	raw, found, err := config.ReadRuntimeValue(ctx, s.db, s.cfg.RuntimeConfigPath, "copilot.enabled")
	if err != nil {
		return false, fmt.Errorf("%w: read Copilot availability", ErrUnavailable)
	}
	if !found {
		return false, nil
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		return false, fmt.Errorf("%w: invalid runtime configuration copilot.enabled", ErrUnavailable)
	}
	return enabled, nil
}

func (s *Service) ReadWriteAvailable() bool {
	return s.cfg.NodeEnv == "development" || s.cfg.Namespace == "dev"
}

func (s *Service) List(ctx context.Context, userID, workspaceID string) ([]Credential, error) {
	if s.db == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, family_id, generation, name, fingerprint, user_id, workspace_id,
		       access_mode::text, created_at, expires_at, last_used_at, revoked_at,
		       grace_ends_at, secret_hash, replaced_by_id
		FROM mcp_credentials
		WHERE user_id=$1 AND workspace_id=$2
		ORDER BY family_id ASC, generation DESC`, userID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list MCP credentials: %w", err)
	}
	defer rows.Close()

	latest := make(map[string]Credential)
	for rows.Next() {
		stored, err := scanCredential(rows)
		if err != nil {
			return nil, fmt.Errorf("scan MCP credential: %w", err)
		}
		if _, exists := latest[stored.FamilyID]; exists {
			continue
		}
		stored.Credential.Status = status(stored.Credential, s.now())
		if stored.Credential.Status != "ROTATING" {
			stored.Credential.GraceEndsAt = nil
		}
		latest[stored.FamilyID] = stored.Credential
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list MCP credentials: %w", err)
	}
	result := make([]Credential, 0, len(latest))
	for _, credential := range latest {
		result = append(result, credential)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}

func (s *Service) Create(ctx context.Context, input CreateInput) (Issued, error) {
	if err := validate(input); err != nil {
		return Issued{}, err
	}
	if input.AccessMode == "READ_WRITE" && !s.ReadWriteAvailable() {
		return Issued{}, ErrWriteToolsUnavailable
	}
	return s.issue(ctx, s.db, input, "", 0, nil)
}

func (s *Service) Rotate(ctx context.Context, id, userID, workspaceID string, expirationDays int) (Issued, error) {
	if !validExpiration(expirationDays) {
		return Issued{}, ErrInvalidExpiration
	}
	if s.db == nil {
		return Issued{}, ErrUnavailable
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Issued{}, fmt.Errorf("begin MCP credential rotation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	current, err := scanCredential(tx.QueryRow(ctx, `
		SELECT id, family_id, generation, name, fingerprint, user_id, workspace_id,
		       access_mode::text, created_at, expires_at, last_used_at, revoked_at,
		       grace_ends_at, secret_hash, replaced_by_id
		FROM mcp_credentials
		WHERE id=$1 AND user_id=$2 AND workspace_id=$3
		FOR UPDATE`, id, userID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Issued{}, ErrCredentialNotFound
	}
	if err != nil {
		return Issued{}, fmt.Errorf("read MCP credential for rotation: %w", err)
	}
	now := s.now()
	if current.RevokedAt != nil || current.ReplacedByID != nil || !current.ExpiresAt.After(now) {
		return Issued{}, ErrCredentialNotFound
	}
	graceEnd := now.Add(rotationGrace)
	if current.ExpiresAt.Before(graceEnd) {
		graceEnd = current.ExpiresAt
	}
	issued, err := s.issue(ctx, tx, CreateInput{
		UserID: userID, WorkspaceID: workspaceID, Name: current.Name,
		AccessMode: current.AccessMode, ExpirationDays: expirationDays,
	}, current.FamilyID, current.Generation+1, &graceEnd)
	if err != nil {
		return Issued{}, err
	}
	updated, err := tx.Exec(ctx, `
		UPDATE mcp_credentials
		SET replaced_by_id=$4, expires_at=$5
		WHERE id=$1 AND user_id=$2 AND workspace_id=$3 AND revoked_at IS NULL AND replaced_by_id IS NULL`,
		id, userID, workspaceID, issued.Credential.ID, graceEnd)
	if err != nil {
		return Issued{}, fmt.Errorf("mark MCP credential as replaced: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return Issued{}, ErrCredentialNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return Issued{}, fmt.Errorf("commit MCP credential rotation: %w", err)
	}
	return issued, nil
}

func (s *Service) Revoke(ctx context.Context, id, userID, workspaceID string) (bool, error) {
	if s.db == nil {
		return false, ErrUnavailable
	}
	var familyID string
	err := s.db.QueryRow(ctx, `SELECT family_id FROM mcp_credentials WHERE id=$1 AND user_id=$2 AND workspace_id=$3`, id, userID, workspaceID).Scan(&familyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read MCP credential family for revocation: %w", err)
	}
	result, err := s.db.Exec(ctx, `
		UPDATE mcp_credentials SET revoked_at=$4
		WHERE family_id=$1 AND user_id=$2 AND workspace_id=$3 AND revoked_at IS NULL`, familyID, userID, workspaceID, s.now())
	if err != nil {
		return false, fmt.Errorf("revoke MCP credential family: %w", err)
	}
	return result.RowsAffected() > 0, nil
}

func (s *Service) Authenticate(ctx context.Context, token, workspaceID string) (*AuthenticatedCredential, error) {
	if s.db == nil {
		return nil, ErrUnavailable
	}
	enabled, err := s.CopilotEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrCopilotDisabled
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != TokenPrefix || parts[1] == "" || parts[2] == "" {
		return nil, ErrUnauthorized
	}
	now := s.now()
	var credential AuthenticatedCredential
	var expectedHash string
	err = s.db.QueryRow(ctx, `
		SELECT credential.id, credential.user_id, credential.workspace_id,
		       credential.access_mode::text, credential.name, credential.secret_hash
		FROM mcp_credentials credential
	JOIN users user_record ON user_record.id=credential.user_id
		WHERE credential.id=$1 AND credential.workspace_id=$2
	  AND credential.revoked_at IS NULL AND credential.expires_at>$3
	  AND user_record.disabled=false`, parts[1], workspaceID, now).Scan(
		&credential.ID, &credential.UserID, &credential.WorkspaceID,
		&credential.AccessMode, &credential.Name, &expectedHash,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("authenticate MCP credential: %w", err)
	}
	actual := sha256.Sum256([]byte(parts[2]))
	actualHash := hex.EncodeToString(actual[:])
	if len(expectedHash) != len(actualHash) || subtle.ConstantTimeCompare([]byte(expectedHash), []byte(actualHash)) != 1 {
		return nil, ErrUnauthorized
	}
	_, err = s.db.Exec(ctx, `
		UPDATE mcp_credentials SET last_used_at=$3
		WHERE id=$1 AND (last_used_at IS NULL OR last_used_at<$2)`, credential.ID, now.Add(-lastUsedWriteInterval), now)
	if err != nil {
		return nil, fmt.Errorf("update MCP credential last-used timestamp: %w", err)
	}
	return &credential, nil
}

func (s *Service) issue(ctx context.Context, db Querier, input CreateInput, familyID string, generation int, graceEnd *time.Time) (Issued, error) {
	if s.db == nil || db == nil {
		return Issued{}, ErrUnavailable
	}
	if err := validate(input); err != nil {
		return Issued{}, err
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return Issued{}, fmt.Errorf("generate MCP credential ID: %w", err)
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return Issued{}, fmt.Errorf("generate MCP credential secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	secretDigest := sha256.Sum256([]byte(secret))
	secretHash := hex.EncodeToString(secretDigest[:])
	if familyID == "" {
		familyID = id.String()
	}
	expiresAt := s.now().Add(time.Duration(input.ExpirationDays) * 24 * time.Hour)
	created, err := scanCredential(db.QueryRow(ctx, `
		INSERT INTO mcp_credentials (
			id,family_id,generation,name,secret_hash,fingerprint,user_id,workspace_id,
			access_mode,expires_at,grace_ends_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::"McpAccessMode",$10,$11)
		RETURNING id,family_id,generation,name,fingerprint,user_id,workspace_id,
		          access_mode::text,created_at,expires_at,last_used_at,revoked_at,
		          grace_ends_at,secret_hash,replaced_by_id`,
		id.String(), familyID, generation, strings.TrimSpace(input.Name), secretHash,
		secretHash[:12], input.UserID, input.WorkspaceID, input.AccessMode, expiresAt, graceEnd))
	if err != nil {
		return Issued{}, fmt.Errorf("create MCP credential: %w", err)
	}
	created.Credential.Status = status(created.Credential, s.now())
	return Issued{Credential: created.Credential, Token: TokenPrefix + "." + id.String() + "." + secret}, nil
}

func validate(input CreateInput) error {
	if !validExpiration(input.ExpirationDays) {
		return ErrInvalidExpiration
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(utf16.Encode([]rune(name))) > 64 {
		return ErrInvalidName
	}
	if input.AccessMode != "READ_ONLY" && input.AccessMode != "READ_WRITE" {
		return ErrInvalidAccessMode
	}
	return nil
}

func validExpiration(days int) bool {
	return days == 30 || days == 90 || days == 365
}

func status(credential Credential, now time.Time) string {
	switch {
	case credential.RevokedAt != nil:
		return "REVOKED"
	case !credential.ExpiresAt.After(now):
		return "EXPIRED"
	case credential.GraceEndsAt != nil && credential.GraceEndsAt.After(now):
		return "ROTATING"
	case credential.ExpiresAt.Sub(now) <= 7*24*time.Hour:
		return "EXPIRING"
	default:
		return "ACTIVE"
	}
}

func scanCredential(row pgx.Row) (storedCredential, error) {
	var stored storedCredential
	var lastUsed, revoked, graceEnd pgtype.Timestamptz
	var replacedBy pgtype.Text
	err := row.Scan(
		&stored.ID, &stored.FamilyID, &stored.Generation, &stored.Name,
		&stored.Fingerprint, &stored.UserID, &stored.WorkspaceID, &stored.AccessMode,
		&stored.CreatedAt, &stored.ExpiresAt, &lastUsed, &revoked, &graceEnd,
		&stored.SecretHash, &replacedBy,
	)
	if err != nil {
		return storedCredential{}, err
	}
	if lastUsed.Valid {
		value := lastUsed.Time
		stored.LastUsedAt = &value
	}
	if revoked.Valid {
		value := revoked.Time
		stored.RevokedAt = &value
	}
	if graceEnd.Valid {
		value := graceEnd.Time
		stored.GraceEndsAt = &value
	}
	if replacedBy.Valid {
		value := replacedBy.String
		stored.ReplacedByID = &value
	}
	return stored, nil
}
