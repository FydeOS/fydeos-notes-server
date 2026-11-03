// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/fydeos/fydeos-notes-server/internal/identity"
)

// providerNamespace is persisted; the formula must not change.
func providerNamespace(p OAuthProviderConfig) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(providerIssuer(p), "/") + "\n" + p.ClientID))
	return "oauth:" + p.Name + ":" + hex.EncodeToString(sum[:16])
}

func validEmail(email string) bool {
	if len(email) > 320 || strings.Count(email, "@") != 1 {
		return false
	}
	local, domain, _ := strings.Cut(email, "@")
	return local != "" && domain != "" && strings.IndexFunc(local, unicode.IsSpace) < 0
}

func validSubject(subject string) bool {
	return strings.TrimSpace(subject) != "" && strings.TrimSpace(subject) == subject && len(subject) <= 512 && strings.IndexFunc(subject, isControl) < 0
}

type localUser struct {
	id          string
	email       string
	registered  bool
	disabled    bool
	hasPassword bool
}

const userColumns = `id, email, registered, disabled, password IS NOT NULL`

func scanUser(row pgx.Row) (localUser, error) {
	var u localUser
	err := row.Scan(&u.id, &u.email, &u.registered, &u.disabled, &u.hasPassword)
	return u, err
}

type linkedAccount struct {
	id        string
	userID    string
	namespace *string
}

// resolveAccount finds or creates the local user for a provider identity.
func resolveAccount(ctx context.Context, tx pgx.Tx, p OAuthProviderConfig, profile providerProfile, now time.Time, allowSignup bool) (localUser, error) {
	email := strings.TrimSpace(profile.Email)
	subject := profile.Subject
	if !validEmail(email) || !validSubject(subject) {
		return localUser{}, ErrInvalidOAuthResponse
	}
	namespace := providerNamespace(p)
	isFydeOS := p.Name == providerFydeOS
	lockKey := "auth:oauth:" + namespace + ":" + subject
	if isFydeOS {
		lockKey = "auth:oauth:fydeos:" + subject
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return localUser{}, err
	}
	if err := identity.LockEmail(ctx, tx, email); err != nil {
		return localUser{}, err
	}

	var rows pgx.Rows
	var err error
	if isFydeOS {
		rows, err = tx.Query(ctx, `SELECT id, user_id, provider_namespace FROM user_connected_accounts
			WHERE provider = $1 AND provider_account_id = $2 FOR UPDATE`, p.Name, subject)
	} else {
		rows, err = tx.Query(ctx, `SELECT a.id, a.user_id, a.provider_namespace FROM user_connected_accounts a
			WHERE a.provider = $1 AND a.provider_account_id = $2
			  AND (a.provider_namespace = $3 OR (a.provider_namespace IS NULL AND NOT EXISTS (
			    SELECT 1 FROM user_connected_accounts b WHERE b.provider_namespace = $3 AND b.provider_account_id = $2)))
			FOR UPDATE OF a`, p.Name, subject, namespace)
	}
	if err != nil {
		return localUser{}, err
	}
	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (linkedAccount, error) {
		var l linkedAccount
		return l, row.Scan(&l.id, &l.userID, &l.namespace)
	})
	if err != nil {
		return localUser{}, err
	}
	if len(links) > 1 {
		return localUser{}, ErrOAuthAccountAlreadyLinked
	}
	if len(links) == 1 {
		link := links[0]
		if isFydeOS && link.namespace != nil && *link.namespace != namespace {
			return localUser{}, ErrOAuthAccountAlreadyLinked
		}
		if link.namespace == nil {
			if _, err := tx.Exec(ctx, `UPDATE user_connected_accounts SET provider_namespace = $2, updated_at = $3 WHERE id = $1`, link.id, namespace, now); err != nil {
				return localUser{}, err
			}
		}
		user, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1 FOR UPDATE`, link.userID))
		if err != nil {
			return localUser{}, err
		}
		if user.disabled {
			return localUser{}, ErrWrongSignInCredentials
		}
		if profile.EmailVerified && strings.EqualFold(user.email, email) {
			if _, err := tx.Exec(ctx, `UPDATE users SET email_verified = $2 WHERE id = $1 AND email_verified IS NULL`, user.id, now); err != nil {
				return localUser{}, err
			}
		}
		return user, nil
	}

	rows, err = tx.Query(ctx, `SELECT `+userColumns+` FROM users WHERE lower(email) = lower($1) LIMIT 2 FOR UPDATE`, email)
	if err != nil {
		return localUser{}, err
	}
	matches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (localUser, error) { return scanUser(row) })
	if err != nil {
		return localUser{}, err
	}
	var user localUser
	switch len(matches) {
	case 0:
		if !allowSignup {
			return localUser{}, ErrOAuthSignupForbidden
		}
		name := strings.TrimSpace(profile.Name)
		if name == "" {
			name, _, _ = strings.Cut(email, "@")
		}
		var verifiedAt *time.Time
		if profile.EmailVerified {
			verifiedAt = &now
		}
		user = localUser{id: uuid.NewString(), email: email, registered: true}
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, name, email, email_verified, avatar_url, registered, disabled, auth_epoch, created_at)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), true, false, 0, $6)`, user.id, name, email, verifiedAt, profile.AvatarURL, now); err != nil {
			return localUser{}, err
		}
	case 1:
		user = matches[0]
		if user.disabled {
			return localUser{}, ErrWrongSignInCredentials
		}
		if isFydeOS {
			var hasLinks, hasSessions bool
			if err := tx.QueryRow(ctx, `SELECT
				EXISTS (SELECT 1 FROM user_connected_accounts WHERE user_id = $1),
				EXISTS (SELECT 1 FROM user_sessions WHERE user_id = $1)`, user.id).Scan(&hasLinks, &hasSessions); err != nil {
				return localUser{}, err
			}
			if !profile.EmailVerified || user.registered || user.hasPassword || hasLinks || hasSessions {
				return localUser{}, ErrOAuthAccountAlreadyLinked
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET registered = true,
				email_verified = CASE WHEN $2 THEN COALESCE(email_verified, $3) ELSE email_verified END,
				name = COALESCE(NULLIF($4, ''), name),
				avatar_url = COALESCE(NULLIF($5, ''), avatar_url)
			WHERE id = $1`, user.id, profile.EmailVerified, now, strings.TrimSpace(profile.Name), profile.AvatarURL); err != nil {
			return localUser{}, err
		}
	default:
		if isFydeOS {
			return localUser{}, ErrOAuthAccountAlreadyLinked
		}
		return localUser{}, ErrWrongSignInCredentials
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_connected_accounts (id, user_id, provider, provider_namespace, provider_account_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, uuid.NewString(), user.id, p.Name, namespace, subject, now); err != nil {
		return localUser{}, err
	}
	return user, nil
}
