// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package users

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (r *Repository) GetAccountRecord(ctx context.Context, userID string) (*AccountRecord, error) {
	account, err := scanAccountRecord(r.db.QueryRow(ctx, accountRecordSelect+` WHERE id=$1`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return account, err
}

func (r *Repository) RevokeSessionsForDeletion(ctx context.Context, userID string) error {
	db, ok := r.db.(TransactionalRowQuerier)
	if !ok {
		return ErrAccountLifecycleUnavailable
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockAccount(ctx, tx, userID); err != nil {
		return err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET auth_epoch=auth_epoch+1 WHERE id=$1`, userID); err != nil {
		return err
	}
	if err := invalidateUserSessions(ctx, tx, userID, "user_deleted_or_disabled", now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) DeleteAccountForAdmin(ctx context.Context, userID string) (*AccountRecord, error) {
	db, ok := r.db.(TransactionalRowQuerier)
	if !ok {
		return nil, ErrAccountLifecycleUnavailable
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockAccount(ctx, tx, userID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_invitations WHERE invitee_user_id=$1`, userID); err != nil {
		return nil, err
	}
	account, err := scanAccountRecord(tx.QueryRow(ctx, `DELETE FROM users WHERE id=$1 RETURNING `+accountRecordColumns, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return account, nil
}

func (r *Repository) BanAccountForAdmin(ctx context.Context, userID string) (*AccountRecord, *User, error) {
	db, ok := r.db.(TransactionalRowQuerier)
	if !ok {
		return nil, nil, ErrAccountLifecycleUnavailable
	}
	for attempt := 0; attempt < 3; attempt++ {
		before, err := r.GetAccountRecord(ctx, userID)
		if err != nil {
			return nil, nil, err
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			return nil, nil, err
		}
		account, bannedUser, err := banAccountInTransaction(ctx, tx, before)
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(context.Background())
		}
		if errors.Is(err, errAccountEmailChanged) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		user, err := r.Get(ctx, bannedUser.ID)
		if err != nil {
			return nil, nil, err
		}
		return account, user, nil
	}
	return nil, nil, ErrAccountLifecycleUnavailable
}

var errAccountEmailChanged = errors.New("account email changed during ban")

func banAccountInTransaction(ctx context.Context, tx pgx.Tx, before *AccountRecord) (*AccountRecord, *User, error) {
	if before == nil {
		return nil, nil, ErrAccountLifecycleUnavailable
	}
	if err := identity.LockEmail(ctx, tx, before.Email); err != nil {
		return nil, nil, err
	}
	current, err := scanAccountRecord(tx.QueryRow(ctx, accountRecordSelect+` WHERE id=$1 FOR UPDATE`, before.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if !strings.EqualFold(current.Email, before.Email) {
		return nil, nil, errAccountEmailChanged
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_invitations WHERE invitee_user_id=$1`, current.ID); err != nil {
		return nil, nil, err
	}
	deleted, err := scanAccountRecord(tx.QueryRow(ctx, `DELETE FROM users WHERE id=$1 RETURNING `+accountRecordColumns, current.ID))
	if err != nil {
		return nil, nil, err
	}
	newID := uuid.NewString()
	_, err = tx.Exec(ctx, `INSERT INTO users(
		id,name,email,email_verified,avatar_url,created_at,password,registered,disabled,auth_epoch
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,true,$9)`,
		newID, deleted.Name, deleted.Email, deleted.EmailVerifiedAt, deleted.AvatarURL,
		deleted.CreatedAt, deleted.Password, deleted.Registered, deleted.AuthEpoch+1)
	if err != nil {
		return nil, nil, err
	}
	return deleted, &User{ID: newID}, nil
}

func lockAccount(ctx context.Context, tx pgx.Tx, userID string) error {
	var lockedID string
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

const accountRecordColumns = `id,name,email,email_verified,avatar_url,created_at,password,registered,disabled,auth_epoch`
const accountRecordSelect = `SELECT ` + accountRecordColumns + ` FROM users`

func scanAccountRecord(row pgx.Row) (*AccountRecord, error) {
	var account AccountRecord
	var emailVerified pgtype.Timestamptz
	var avatarURL, password pgtype.Text
	if err := row.Scan(&account.ID, &account.Name, &account.Email, &emailVerified, &avatarURL,
		&account.CreatedAt, &password, &account.Registered, &account.Disabled, &account.AuthEpoch); err != nil {
		return nil, err
	}
	if emailVerified.Valid {
		value := emailVerified.Time
		account.EmailVerifiedAt = &value
	}
	if avatarURL.Valid {
		account.AvatarURL = &avatarURL.String
	}
	if password.Valid {
		account.Password = &password.String
	}
	return &account, nil
}
