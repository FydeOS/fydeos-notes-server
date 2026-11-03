// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/identity"
	"github.com/fydeos/fydeos-notes-server/internal/password"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const securityTokenTTL = 30 * time.Minute

type securityKind struct {
	purpose         string
	mail            string
	addUserID       bool
	requireVerified bool
}

var securityKinds = map[string]securityKind{
	"change_password": {"change_password", "ChangePassword", true, true},
	"set_password":    {"change_password", "SetPassword", true, false},
	"change_email":    {"change_email", "ChangeEmail", false, true},
	"verify_email":    {"verify_email", "VerifyEmail", false, false},
}

type SecurityChallengeService struct {
	db           PasswordLoginDatabase
	cfg          config.Config
	bootstrapKey string
}

func NewSecurityChallengeService(db PasswordLoginDatabase, cfg config.Config, bootstrapKey string) *SecurityChallengeService {
	return &SecurityChallengeService{db: db, cfg: cfg, bootstrapKey: bootstrapKey}
}

type challengePayload struct {
	UserID    string  `json:"userId"`
	AuthEpoch int     `json:"authEpoch"`
	Email     *string `json:"email"`
}

type securityUser struct {
	email    string
	verified bool
	disabled bool
	epoch    int
}

func lockSecurityUser(ctx context.Context, tx pgx.Tx, userID string) (securityUser, error) {
	var u securityUser
	err := tx.QueryRow(ctx, `SELECT email, email_verified IS NOT NULL, disabled, auth_epoch FROM users WHERE id=$1 FOR UPDATE`, userID).
		Scan(&u.email, &u.verified, &u.disabled, &u.epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrSecurityLinkExpired
	}
	if err == nil && u.disabled {
		return u, ErrWrongSignInCredentials
	}
	return u, err
}

// createChallenge stores a security token and returns it.
func createChallenge(ctx context.Context, tx pgx.Tx, purpose, userID string, epoch int, email string) (string, error) {
	now, err := dbNow(ctx, tx)
	if err != nil {
		return "", err
	}
	token := uuid.NewString()
	err = insertState(ctx, tx, purposeChallengPrefix+purpose, sha256hex(token), nil, challengePayload{UserID: userID, AuthEpoch: epoch, Email: &email}, now.Add(securityTokenTTL))
	return token, err
}

// consumeChallenge consumes a token bound to the user, epoch and (optionally)
// e-mail; any mismatch leaves it untouched.
func consumeChallenge(ctx context.Context, tx pgx.Tx, purpose, token, userID string, epoch int, email *string) error {
	if token == "" {
		return ErrInvalidEmailToken
	}
	hash := sha256hex(token)
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM runtime_states WHERE purpose=$1 AND token_hash=$2 AND consumed_at IS NULL
		AND expires_at > clock_timestamp() FOR UPDATE`, purposeChallengPrefix+purpose, hash).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidEmailToken
	}
	if err != nil {
		return err
	}
	var payload challengePayload
	if json.Unmarshal(raw, &payload) != nil || payload.UserID != userID || payload.AuthEpoch != epoch {
		return ErrInvalidEmailToken
	}
	if email != nil && (payload.Email == nil || *payload.Email != *email) {
		return ErrInvalidEmailToken
	}
	_, err = tx.Exec(ctx, `UPDATE runtime_states SET consumed_at=clock_timestamp(), updated_at=clock_timestamp() WHERE purpose=$1 AND token_hash=$2`, purposeChallengPrefix+purpose, hash)
	return err
}

func (s *SecurityChallengeService) prepareCommon(ctx context.Context, r *http.Request, callbackURL string) (string, error) {
	configured, err := mailConfigured(ctx, s.db, s.cfg)
	if err != nil {
		return "", err
	}
	if !configured {
		return "", ErrEmailServiceNotConfigured
	}
	return resolveCallbackURL(r, s.cfg, callbackURL, "")
}

func (s *SecurityChallengeService) privateKey(ctx context.Context, tx pgx.Tx) (string, error) {
	key, err := LoadAppPrivateKey(ctx, tx, s.cfg, s.bootstrapKey)
	if err == nil && strings.TrimSpace(key) == "" {
		err = errors.New("auth: app private key is not configured")
	}
	return key, err
}

func (s *SecurityChallengeService) PrepareSecurityChallenge(ctx context.Context, r *http.Request, kind, userID, callbackURL string) (bool, error) {
	spec, ok := securityKinds[kind]
	if !ok {
		return false, fmt.Errorf("auth: unknown security challenge kind %q", kind)
	}
	callback, err := s.prepareCommon(ctx, r, callbackURL)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	user, err := lockSecurityUser(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	if spec.requireVerified && !user.verified {
		return false, ErrSecurityEmailVerificationNeeded
	}
	token, err := createChallenge(ctx, tx, spec.purpose, userID, user.epoch, user.email)
	if err != nil {
		return false, err
	}
	var pairs [][2]string
	if spec.addUserID {
		pairs = append(pairs, [2]string{"userId", userID})
	}
	pairs = append(pairs, [2]string{"token", token})
	key, err := s.privateKey(ctx, tx)
	if err != nil {
		return false, err
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return false, err
	}
	err = queueMail(ctx, tx, mailRequest{
		name: spec.mail, to: user.email, userID: &userID,
		props:     map[string]any{"url": appendQuery(callback, pairs...)},
		dedupeKey: "auth:auth_challenge:" + spec.purpose + ":" + sha256hex(token),
		expires:   now.Add(securityTokenTTL), sourcePrefix: sourcePrefix(requestClientIP(r, s.cfg)), privateKey: key,
	})
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func validChangeEmail(email string) bool {
	if utf16Len(email) > 320 || strings.Count(email, "@") != 1 {
		return false
	}
	local, domain, _ := strings.Cut(email, "@")
	if local == "" || domain == "" {
		return false
	}
	return !strings.ContainsFunc(local, unicode.IsSpace)
}

func (s *SecurityChallengeService) PrepareVerifyChangeEmail(ctx context.Context, r *http.Request, userID, sourceToken, email, callbackURL string) (bool, error) {
	if sourceToken == "" {
		return false, ErrInvalidEmailToken
	}
	email = strings.TrimSpace(email)
	if !validChangeEmail(email) {
		return false, ErrInvalidEmail
	}
	callback, err := s.prepareCommon(ctx, r, callbackURL)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	if err := identity.LockEmail(ctx, tx, email); err != nil {
		return false, err
	}
	user, err := lockSecurityUser(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(user.email, email) {
		return false, ErrSecuritySameEmail
	}
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(email)=lower($1))`, email).Scan(&taken); err != nil {
		return false, err
	}
	if taken {
		return false, ErrSecurityEmailAlreadyUsed
	}
	if err := consumeChallenge(ctx, tx, "change_email", sourceToken, userID, user.epoch, nil); err != nil {
		return false, err
	}
	token, err := createChallenge(ctx, tx, "verify_change_email", userID, user.epoch, email)
	if err != nil {
		return false, err
	}
	key, err := s.privateKey(ctx, tx)
	if err != nil {
		return false, err
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return false, err
	}
	err = queueMail(ctx, tx, mailRequest{
		name: "VerifyChangeEmail", to: email, userID: &userID,
		props:     map[string]any{"url": appendQuery(callback, [2]string{"token", token}, [2]string{"email", email})},
		dedupeKey: "auth:verify-change-email:" + sha256hex(token),
		expires:   now.Add(securityTokenTTL), sourcePrefix: sourcePrefix(requestClientIP(r, s.cfg)), privateKey: key,
	})
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *SecurityChallengeService) CreateSecurityURL(ctx context.Context, r *http.Request, userID, callbackURL string) (string, error) {
	callback, err := resolveCallbackURL(r, s.cfg, callbackURL, "")
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer rollback(tx)
	user, err := lockSecurityUser(ctx, tx, userID)
	if err != nil {
		return "", err
	}
	token, err := createChallenge(ctx, tx, "change_password", userID, user.epoch, user.email)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return appendQuery(callback, [2]string{"userId", userID}, [2]string{"token", token}), nil
}

func (s *SecurityChallengeService) CompletePasswordChallenge(ctx context.Context, userID, token, newPassword string) (bool, error) {
	if newPassword == "" {
		return false, ErrSecurityPasswordRequired
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	user, err := lockSecurityUser(ctx, tx, userID)
	if errors.Is(err, ErrSecurityLinkExpired) {
		return false, ErrInvalidEmailToken
	}
	if err != nil {
		return false, err
	}
	if err := consumeChallenge(ctx, tx, "change_password", token, userID, user.epoch, nil); err != nil {
		return false, err
	}
	hash, err := password.Hash(newPassword)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET password=$2, auth_epoch=auth_epoch+1 WHERE id=$1`, userID, hash); err != nil {
		return false, err
	}
	if err := revokeAllUserSessions(ctx, tx, userID, revokeSecurityEvent); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *SecurityChallengeService) CompleteEmailChallenge(ctx context.Context, userID, token, email string) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	if err := identity.LockEmail(ctx, tx, email); err != nil {
		return false, err
	}
	user, err := lockSecurityUser(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(email)=lower($1) AND id<>$2)`, email, userID).Scan(&taken); err != nil {
		return false, err
	}
	if taken {
		return false, ErrSecurityEmailAlreadyUsed
	}
	if err := consumeChallenge(ctx, tx, "verify_change_email", token, userID, user.epoch, &email); err != nil {
		return false, err
	}
	var epoch int
	if err := tx.QueryRow(ctx, `UPDATE users SET email=$2, email_verified=clock_timestamp(), auth_epoch=auth_epoch+1 WHERE id=$1 RETURNING auth_epoch`, userID, email).Scan(&epoch); err != nil {
		return false, err
	}
	if err := revokeAllUserSessions(ctx, tx, userID, revokeSecurityEvent); err != nil {
		return false, err
	}
	key, err := s.privateKey(ctx, tx)
	if err != nil {
		return false, err
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return false, err
	}
	if err := queueMail(ctx, tx, mailRequest{
		name: "EmailChanged", to: email, userID: &userID, props: map[string]any{"to": email},
		dedupeKey: fmt.Sprintf("auth:email-changed:%s:%d", userID, epoch), expires: now.Add(24 * time.Hour), privateKey: key,
	}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *SecurityChallengeService) CompleteVerifyEmailChallenge(ctx context.Context, userID, token string) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	user, err := lockSecurityUser(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	if err := consumeChallenge(ctx, tx, "verify_email", token, userID, user.epoch, nil); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET email_verified=COALESCE(email_verified, clock_timestamp()) WHERE id=$1`, userID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
