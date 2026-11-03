// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package users

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrNotFound                   = errors.New("user not found")
	ErrSettingsUnavailable        = errors.New("user settings are unavailable")
	ErrProfileUpdateUnavailable   = errors.New("user profile update is unavailable")
	ErrUserManagementUnavailable  = errors.New("user management is unavailable")
	ErrInvalidUserListPagination  = errors.New("invalid user list pagination")
	ErrUserFeatureNotConfigurable = errors.New("user feature is not configurable")
	ErrUserStatusUnavailable      = errors.New("user status management is unavailable")
	ErrEmailAlreadyUsed           = errors.New("email is already registered")
	ErrInvalidEmail               = errors.New("invalid email")
)

type RowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type TransactionalRowQuerier interface {
	RowQuerier
	Begin(context.Context) (pgx.Tx, error)
}

type Repository struct {
	db RowQuerier
}

type User struct {
	ID            string
	Name          string
	Email         string
	AvatarURL     *string
	EmailVerified bool
	HasPassword   bool
	Disabled      bool
	CreatedAt     *time.Time
	Features      []string
}

type UserListFilter struct {
	Skip     int
	First    int
	Keyword  string
	Features []string
}

func NewRepository(db RowQuerier) *Repository {
	return &Repository{db: db}
}

func (r *Repository) Get(ctx context.Context, id string) (*User, error) {
	var user User
	var avatarURL pgtype.Text
	var createdAt pgtype.Timestamptz
	err := r.db.QueryRow(ctx, `
		SELECT id, name, email, avatar_url, email_verified IS NOT NULL,
		       password IS NOT NULL, disabled, created_at
		FROM users
		WHERE id = $1`, id).Scan(
		&user.ID,
		&user.Name,
		&user.Email,
		&avatarURL,
		&user.EmailVerified,
		&user.HasPassword,
		&user.Disabled,
		&createdAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if avatarURL.Valid {
		user.AvatarURL = &avatarURL.String
	}
	if createdAt.Valid {
		created := createdAt.Time
		user.CreatedAt = &created
	}
	features, err := r.features(ctx, id)
	if err != nil {
		return nil, err
	}
	user.Features = features
	return &user, nil
}

func (r *Repository) GetPublic(ctx context.Context, id string) (*User, error) {
	var user User
	var avatarURL pgtype.Text
	err := r.db.QueryRow(ctx, `
		SELECT id, name, avatar_url
		FROM users
		WHERE id = $1 AND disabled = false`, id).Scan(&user.ID, &user.Name, &avatarURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if avatarURL.Valid {
		user.AvatarURL = &avatarURL.String
	}
	return &user, nil
}

func (r *Repository) GetByEmail(ctx context.Context, email string) (*User, error) {
	var id string
	err := r.db.QueryRow(ctx, `
		SELECT target.id
		FROM users target
		WHERE lower(target.email) = lower($1)
		  AND target.disabled = false
		LIMIT 1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *Repository) GetByEmailIncludingDisabled(ctx context.Context, email string) (*User, error) {
	var id string
	err := r.db.QueryRow(ctx, `
		SELECT id
		FROM users
		WHERE lower(email) = lower($1)
		LIMIT 1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *Repository) List(ctx context.Context, filter UserListFilter) ([]*User, error) {
	if filter.Skip < 0 || filter.First < 0 {
		return nil, ErrInvalidUserListPagination
	}
	rows, err := r.db.Query(ctx, `
		SELECT user_row.id, user_row.name, user_row.email, user_row.avatar_url,
		       user_row.email_verified IS NOT NULL, user_row.password IS NOT NULL,
		       user_row.disabled, user_row.created_at, feature_rows.features
		FROM users user_row
		LEFT JOIN LATERAL (
			SELECT COALESCE(array_agg(DISTINCT feature.name ORDER BY feature.name), ARRAY[]::text[]) AS features
			FROM user_features feature
			WHERE feature.user_id = user_row.id
			  AND feature.name = 'administrator'
			  AND feature.activated = true
			  AND (feature.expired_at IS NULL OR feature.expired_at > now())
		) feature_rows ON true
		WHERE (NULLIF(btrim($1), '') IS NULL
		       OR lower(user_row.email) LIKE '%' || lower(btrim($1)) || '%'
		       OR user_row.id LIKE '%' || btrim($1) || '%')
		  AND (COALESCE(cardinality($2::text[]), 0) = 0 OR EXISTS (
			SELECT 1 FROM user_features filter_feature
			WHERE filter_feature.user_id = user_row.id
			  AND filter_feature.name = ANY($2::text[])
			  AND filter_feature.activated = true
		  ))
		ORDER BY user_row.created_at ASC
		OFFSET $3 LIMIT $4`, filter.Keyword, filter.Features, filter.Skip, filter.First)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]*User, 0)
	for rows.Next() {
		var user User
		var avatarURL pgtype.Text
		var createdAt pgtype.Timestamptz
		if err := rows.Scan(&user.ID, &user.Name, &user.Email, &avatarURL, &user.EmailVerified, &user.HasPassword, &user.Disabled, &createdAt, &user.Features); err != nil {
			return nil, err
		}
		if avatarURL.Valid {
			user.AvatarURL = &avatarURL.String
		}
		if createdAt.Valid {
			created := createdAt.Time
			user.CreatedAt = &created
		}
		users = append(users, &user)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return users, nil
}

func (r *Repository) Count(ctx context.Context, filter UserListFilter) (int, error) {
	var count int
	err := r.db.QueryRow(ctx, `
		SELECT count(*)
		FROM users user_row
		WHERE (NULLIF(btrim($1), '') IS NULL
		       OR lower(user_row.email) LIKE '%' || lower(btrim($1)) || '%'
		       OR user_row.id LIKE '%' || btrim($1) || '%')
		  AND (COALESCE(cardinality($2::text[]), 0) = 0 OR EXISTS (
			SELECT 1 FROM user_features filter_feature
			WHERE filter_feature.user_id = user_row.id
			  AND filter_feature.name = ANY($2::text[])
			  AND filter_feature.activated = true
		  ))`, filter.Keyword, filter.Features).Scan(&count)
	return count, err
}

func (r *Repository) Create(ctx context.Context, email, name string, passwordHash *string) (*User, error) {
	db, ok := r.db.(TransactionalRowQuerier)
	if !ok {
		return nil, ErrUserManagementUnavailable
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := identity.LockEmail(ctx, tx, email); err != nil {
		return nil, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(email)=lower($1))`, email).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrEmailAlreadyUsed
	}

	userID := uuid.NewString()
	_, err = tx.Exec(ctx, `INSERT INTO users(id,name,email,password,registered,disabled,created_at)
		VALUES($1,$2,$3,$4,true,false,clock_timestamp())`, userID, name, email, passwordHash)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "email") {
			return nil, ErrEmailAlreadyUsed
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Get(ctx, userID)
}

func (r *Repository) UpdateEmail(ctx context.Context, userID, email string) (*User, error) {
	db, ok := r.db.(TransactionalRowQuerier)
	if !ok {
		return nil, ErrUserManagementUnavailable
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := identity.LockEmail(ctx, tx, email); err != nil {
		return nil, err
	}
	var lockedUserID string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedUserID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var usedID string
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE lower(email)=lower($1) AND id<>$2 FOR UPDATE`, email, userID).Scan(&usedID)
	if err == nil {
		return nil, ErrEmailAlreadyUsed
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET auth_epoch=auth_epoch+1,email=$2,email_verified=$3 WHERE id=$1`, userID, email, now); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "email") {
			return nil, ErrEmailAlreadyUsed
		}
		return nil, err
	}
	if err := invalidateUserSessions(ctx, tx, userID, "administrator_changed_email", now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Get(ctx, userID)
}

func (r *Repository) UpdateProfile(ctx context.Context, userID, name string) (*User, error) {
	result, err := r.db.Exec(ctx, `UPDATE users SET name = $2 WHERE id = $1`, userID, name)
	if err != nil {
		return nil, err
	}
	if result.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return r.Get(ctx, userID)
}

func (r *Repository) UpdateAvatar(ctx context.Context, userID string, avatarURL *string) (*User, error) {
	result, err := r.db.Exec(ctx, `UPDATE users SET avatar_url = $2 WHERE id = $1`, userID, avatarURL)
	if err != nil {
		return nil, err
	}
	if result.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return r.Get(ctx, userID)
}

func (r *Repository) GetSettings(ctx context.Context, userID string) (UserSettings, error) {
	var payload []byte
	err := r.db.QueryRow(ctx, `SELECT payload FROM user_settings WHERE user_id = $1`, userID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultUserSettings(), nil
	}
	if err != nil {
		return UserSettings{}, err
	}
	return decodeUserSettings(payload)
}

func (r *Repository) UpdateSettings(ctx context.Context, userID string, update UserSettingsUpdate) (UserSettings, error) {
	patch := make(map[string]bool, 3)
	if update.ReceiveInvitationEmail != nil {
		patch["receiveInvitationEmail"] = *update.ReceiveInvitationEmail
	}
	if update.ReceiveMentionEmail != nil {
		patch["receiveMentionEmail"] = *update.ReceiveMentionEmail
	}
	if update.ReceiveCommentEmail != nil {
		patch["receiveCommentEmail"] = *update.ReceiveCommentEmail
	}
	payload, err := json.Marshal(patch)
	if err != nil {
		return UserSettings{}, err
	}
	var result []byte
	err = r.db.QueryRow(ctx, `
		INSERT INTO user_settings (user_id, created_at, updated_at, payload)
		VALUES ($1, now(), now(), $2::jsonb)
		ON CONFLICT (user_id) DO UPDATE SET
			updated_at = now(),
			payload = jsonb_build_object(
				'receiveInvitationEmail', COALESCE((EXCLUDED.payload->>'receiveInvitationEmail')::boolean, (user_settings.payload->>'receiveInvitationEmail')::boolean, true),
				'receiveMentionEmail', COALESCE((EXCLUDED.payload->>'receiveMentionEmail')::boolean, (user_settings.payload->>'receiveMentionEmail')::boolean, true),
				'receiveCommentEmail', COALESCE((EXCLUDED.payload->>'receiveCommentEmail')::boolean, (user_settings.payload->>'receiveCommentEmail')::boolean, true)
			)
		RETURNING payload`, userID, payload).Scan(&result)
	if err != nil {
		return UserSettings{}, err
	}
	return decodeUserSettings(result)
}

func (r *Repository) UpdateFeatures(ctx context.Context, userID string, features []string) (bool, error) {
	grantAdministrator := false
	for _, feature := range features {
		if feature != "administrator" {
			return false, ErrUserFeatureNotConfigurable
		}
		grantAdministrator = true
	}

	if grantAdministrator {
		var active bool
		if err := r.db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM user_features
				WHERE user_id = $1 AND name = 'administrator' AND activated = true
			)`, userID).Scan(&active); err != nil {
			return false, err
		}
		if !active {
			if _, err := r.db.Exec(ctx, `
				INSERT INTO user_features (user_id, name, type, reason, activated)
				VALUES ($1, 'administrator', 0, 'admin panel', true)`, userID); err != nil {
				return false, err
			}
		}
	} else {
		if _, err := r.db.Exec(ctx, `
			UPDATE user_features
			SET activated = false
			WHERE user_id = $1 AND name = 'administrator'`, userID); err != nil {
			return false, err
		}
	}

	var publishUpdate bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND disabled = false)`, userID).Scan(&publishUpdate)
	return publishUpdate, err
}

func (r *Repository) Enable(ctx context.Context, userID string) (*User, error) {
	db, ok := r.db.(TransactionalRowQuerier)
	if !ok {
		return nil, ErrUserStatusUnavailable
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var lockedUserID string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedUserID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET auth_epoch=auth_epoch+1, disabled=false WHERE id=$1`, userID); err != nil {
		return nil, err
	}
	if err := invalidateUserSessions(ctx, tx, userID, "administrator_enabled_user", now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Get(ctx, userID)
}

func collectLockedIDs(ctx context.Context, queryer RowQuerier, query string, args ...any) ([]string, error) {
	rows, err := queryer.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func invalidateUserSessions(ctx context.Context, tx pgx.Tx, userID, reason string, now time.Time) error {
	userSessionIDs, err := collectLockedIDs(ctx, tx, `SELECT id FROM user_sessions WHERE user_id=$1 ORDER BY id FOR UPDATE`, userID)
	if err != nil {
		return err
	}
	authSessionIDs, err := collectLockedIDs(ctx, tx, `SELECT id FROM auth_sessions WHERE user_session_id=ANY($1) ORDER BY id FOR UPDATE`, userSessionIDs)
	if err != nil {
		return err
	}
	if _, err := collectLockedIDs(ctx, tx, `SELECT id FROM auth_refresh_tokens WHERE auth_session_id=ANY($1) ORDER BY id FOR UPDATE`, authSessionIDs); err != nil {
		return err
	}
	if len(authSessionIDs) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=$2,revoke_reason=$3 WHERE id=ANY($1) AND revoked_at IS NULL`, authSessionIDs, now, reason); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID)
	return err
}

func (r *Repository) features(ctx context.Context, userID string) ([]string, error) {
	var features []string
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(DISTINCT name ORDER BY name), ARRAY[]::text[])
		FROM user_features
		WHERE user_id = $1
		  AND name = 'administrator'
		  AND activated = true
		  AND (expired_at IS NULL OR expired_at > now())`, userID).Scan(&features)
	return features, err
}
