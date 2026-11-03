// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SessionResolver turns a request into a Principal via Bearer token or cookie.
type SessionResolver struct {
	db  RowQuerier
	cfg config.Config
}

func NewSessionResolver(db RowQuerier) *SessionResolver {
	return &SessionResolver{db: db}
}

func NewSessionResolverWithConfig(db RowQuerier, cfg config.Config) *SessionResolver {
	return &SessionResolver{db: db, cfg: cfg}
}

func (s *SessionResolver) FromRequest(ctx context.Context, r *http.Request) (*Principal, error) {
	if token, isBearer, err := bearerToken(r); isBearer {
		if err != nil {
			return nil, err
		}
		return s.fromAccessToken(ctx, token)
	}
	return s.fromCookie(ctx, r)
}

func (s *SessionResolver) FromRequestWithResponse(ctx context.Context, w http.ResponseWriter, r *http.Request) (*Principal, error) {
	principal, err := s.FromRequest(ctx, r)
	if errors.Is(err, ErrNoSession) {
		if hasSessionCookie(r) {
			ClearSessionCookies(w)
		}
		return nil, err
	}
	if err != nil || principal.AuthSessionID != "" {
		return principal, err
	}
	if err := s.refreshCookieSession(ctx, w, r, principal); err != nil {
		if errors.Is(err, ErrNoSession) {
			ClearSessionCookies(w)
		}
		return nil, err
	}
	if c, err := r.Cookie(UserCookieName); err != nil || c.Value != principal.User.ID {
		http.SetCookie(w, &http.Cookie{Name: UserCookieName, Value: principal.User.ID, Path: "/", SameSite: http.SameSiteLaxMode})
	}
	return principal, nil
}

// bearerToken reports whether a Bearer Authorization header is present.
func bearerToken(r *http.Request) (string, bool, error) {
	header := r.Header.Get("Authorization")
	fields := strings.Fields(header)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "bearer") {
		return "", false, nil
	}
	if len(fields) != 2 {
		return "", true, ErrAccessTokenInvalid
	}
	return fields[1], true, nil
}

const userColumns = `u.id, u.name, u.email, u.avatar_url, u.email_verified IS NOT NULL, u.password IS NOT NULL, u.disabled`

func scanUser(row interface{ Scan(...any) error }, extra ...any) (User, error) {
	var u User
	dest := append([]any{&u.ID, &u.Name, &u.Email, &u.AvatarURL, &u.EmailVerified, &u.HasPassword, &u.Disabled}, extra...)
	err := row.Scan(dest...)
	return u, err
}

func loadFeatures(ctx context.Context, db RowQuerier, userID string) ([]string, error) {
	var admin bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_features WHERE user_id=$1 AND name='administrator' AND activated AND (expired_at IS NULL OR expired_at > clock_timestamp()))`, userID).Scan(&admin)
	if err != nil {
		return nil, fmt.Errorf("load user features: %w", err)
	}
	if admin {
		return []string{"administrator"}, nil
	}
	return []string{}, nil
}

func (s *SessionResolver) fromCookie(ctx context.Context, r *http.Request) (*Principal, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return nil, ErrNoSession
	}
	if _, err := uuid.Parse(cookie.Value); err != nil {
		return nil, ErrNoSession
	}
	var userFilter *string
	if c, err := r.Cookie(UserCookieName); err == nil && idPattern.MatchString(c.Value) {
		userFilter = &c.Value
	}
	p := &Principal{}
	row := s.db.QueryRow(ctx, `SELECT `+userColumns+`, us.session_id, us.id, us.expires_at
		FROM user_sessions us JOIN users u ON u.id = us.user_id
		LEFT JOIN auth_sessions a ON a.user_session_id = us.id
		WHERE us.session_id = $1 AND ($2::text IS NULL OR us.user_id = $2)
		  AND (us.expires_at IS NULL OR us.expires_at > clock_timestamp())
		  AND NOT u.disabled
		  AND (a.id IS NULL OR (a.revoked_at IS NULL AND a.idle_expires_at > clock_timestamp() AND a.absolute_expires_at > clock_timestamp()))
		ORDER BY us.created_at DESC, us.id DESC LIMIT 1`, cookie.Value, userFilter)
	user, err := scanUser(row, &p.SessionID, &p.UserSessionID, &p.SessionExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("resolve cookie session: %w", err)
	}
	if user.Features, err = loadFeatures(ctx, s.db, user.ID); err != nil {
		return nil, err
	}
	p.User = user
	return p, nil
}

func clientVersionHeader(r *http.Request) *string {
	v := r.Header.Get("x-affine-version")
	if len(v) < 1 || len(v) > 128 {
		return nil
	}
	return &v
}

func (s *SessionResolver) refreshCookieSession(ctx context.Context, w http.ResponseWriter, r *http.Request, p *Principal) error {
	b, ok := s.db.(beginner)
	if !ok {
		return errNoTransactions
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var disabled bool
	err = tx.QueryRow(ctx, `SELECT disabled FROM users WHERE id=$1 FOR UPDATE`, p.User.ID).Scan(&disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoSession
	}
	if err != nil {
		return err
	}
	var expires *time.Time
	var now time.Time
	err = tx.QueryRow(ctx, `SELECT expires_at, clock_timestamp() FROM user_sessions WHERE id=$1 AND user_id=$2 FOR UPDATE`, p.UserSessionID, p.User.ID).Scan(&expires, &now)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (disabled || (expires != nil && !expires.After(now)))) {
		return ErrNoSession
	}
	if err != nil {
		return err
	}
	ttl, err := readConfigInt(ctx, tx, s.cfg, "auth.session.ttl", 15*86400)
	if err != nil {
		return err
	}
	ttr, err := readConfigInt(ctx, tx, s.cfg, "auth.session.ttr", 7*86400)
	if err != nil {
		return err
	}
	if ttl <= 0 || ttr < 0 {
		return errors.New("invalid session ttl configuration")
	}
	if expires != nil && expires.Sub(now) > time.Duration(ttr)*time.Second {
		return tx.Commit(ctx)
	}
	next := now.Add(time.Duration(ttl) * time.Second)
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET expires_at=$2, refresh_client_version=$3 WHERE id=$1`, p.UserSessionID, next, clientVersionHeader(r)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	p.SessionExpiresAt = &next
	setSessionCookie(w, p.SessionID, next, s.cfg.HTTPS)
	setCSRFCookie(w, next, s.cfg.HTTPS)
	return nil
}

func (s *SessionResolver) fromAccessToken(ctx context.Context, token string) (*Principal, error) {
	kid, parts, err := parseAccessTokenKid(token)
	if err != nil {
		return nil, err
	}
	keys, err := loadKeyring(ctx, s.db)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	key, ok := keys.find(kid)
	if !ok || (key.Status == "retiring" && key.VerifyUntil.Before(now)) {
		return nil, ErrAccessTokenInvalid
	}
	claims, err := verifyAccessToken(parts, key, now)
	if err != nil {
		return nil, err
	}
	return resolveAuthSession(ctx, s.db, claims.Sub, claims.Sid)
}

func resolveAuthSession(ctx context.Context, db RowQuerier, userID, authSessionID string) (*Principal, error) {
	var userSessionID, owner string
	err := db.QueryRow(ctx, `SELECT a.user_session_id, us.user_id FROM auth_sessions a JOIN user_sessions us ON us.id = a.user_session_id WHERE a.id=$1`, authSessionID).Scan(&userSessionID, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAccessTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	if owner != userID {
		return nil, ErrAccessTokenInvalid
	}
	b, ok := db.(beginner)
	if !ok {
		return nil, errNoTransactions
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	p := &Principal{UserSessionID: userSessionID, AuthSessionID: authSessionID}
	user, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users u WHERE u.id=$1 FOR UPDATE`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAccessTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	var usUser string
	err = tx.QueryRow(ctx, `SELECT session_id, user_id, expires_at FROM user_sessions WHERE id=$1 FOR UPDATE`, userSessionID).Scan(&p.SessionID, &usUser, &p.SessionExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && usUser != userID) {
		return nil, ErrAccessTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	var revokedAt *time.Time
	var idle, absolute, created, now time.Time
	var aUS string
	err = tx.QueryRow(ctx, `SELECT user_session_id, revoked_at, idle_expires_at, absolute_expires_at, created_at, clock_timestamp() FROM auth_sessions WHERE id=$1 FOR UPDATE`, authSessionID).
		Scan(&aUS, &revokedAt, &idle, &absolute, &created, &now)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && aUS != userSessionID) {
		return nil, ErrAccessTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	if user.Disabled || revokedAt != nil {
		return nil, ErrAuthSessionRevoked
	}
	if !idle.After(now) || !absolute.After(now) || (p.SessionExpiresAt != nil && !p.SessionExpiresAt.After(now)) {
		return nil, ErrAuthSessionExpired
	}
	if user.Features, err = loadFeatures(ctx, tx, user.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	p.User = user
	p.AuthenticatedAt = &created
	return p, nil
}
