// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// authenticateLimited acquires a read slot and resolves the caller. It
// returns ok=false after writing an error response.
func authenticateLimited(w http.ResponseWriter, r *http.Request, limiter *ReadLimiter, authenticator RequestAuthenticator) (*Principal, func(), bool) {
	release, ok := limiter.TryAcquire()
	if !ok {
		writeOverloaded(w)
		return nil, nil, false
	}
	principal, err := AuthenticateRequest(r.Context(), w, r, authenticator)
	if err != nil && !errors.Is(err, ErrNoSession) {
		if !WriteAccessTokenError(w, err) {
			writeUnavailable(w)
		}
		release()
		return nil, nil, false
	}
	return principal, release, true
}

type SessionHTTPHandler struct {
	authenticator RequestAuthenticator
	limiter       *ReadLimiter
}

func NewSessionHTTPHandler(authenticator RequestAuthenticator, limiters ...*ReadLimiter) *SessionHTTPHandler {
	return &SessionHTTPHandler{authenticator: authenticator, limiter: pickReadLimiter(limiters)}
}

func (h *SessionHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setNoStore(w, false)
	if r.Method != http.MethodGet {
		writeSimpleMethodNotAllowed(w, http.MethodGet)
		return
	}
	principal, release, ok := authenticateLimited(w, r, h.limiter, h.authenticator)
	if !ok {
		return
	}
	defer release()
	if principal == nil {
		writeJSON(w, http.StatusOK, map[string]any{"user": nil})
		return
	}
	u := principal.User
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]any{
		"id": u.ID, "name": u.Name, "email": u.Email, "avatarUrl": u.AvatarURL,
		"emailVerified": u.EmailVerified, "hasPassword": u.HasPassword, "disabled": u.Disabled,
	}})
}

// Bound methods.

type BoundMethodsQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type MethodsRepository interface {
	GetBoundMethods(ctx context.Context, userID string) (BoundMethods, error)
}

type BoundMethods struct {
	Password BoundMethod    `json:"password"`
	OAuth    OAuthMethods   `json:"oauth"`
	Passkey  PasskeyMethods `json:"passkey"`
}

type BoundMethod struct {
	Bound bool `json:"bound"`
}

type OAuthMethods struct {
	Bound     bool     `json:"bound"`
	Providers []string `json:"providers"`
}

type PasskeyMethods struct {
	Bound bool   `json:"bound"`
	Count uint32 `json:"count"`
}

type BoundMethodsRepository struct {
	db BoundMethodsQuerier
}

func NewBoundMethodsRepository(db BoundMethodsQuerier) *BoundMethodsRepository {
	return &BoundMethodsRepository{db: db}
}

func (r *BoundMethodsRepository) GetBoundMethods(ctx context.Context, userID string) (BoundMethods, error) {
	methods := BoundMethods{OAuth: OAuthMethods{Providers: []string{}}}
	if err := r.db.QueryRow(ctx, `SELECT password IS NOT NULL FROM users WHERE id=$1`, userID).Scan(&methods.Password.Bound); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return BoundMethods{}, err
	}
	rows, err := r.db.Query(ctx, `SELECT DISTINCT provider FROM user_connected_accounts WHERE user_id=$1 ORDER BY provider`, userID)
	if err != nil {
		return BoundMethods{}, err
	}
	providers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return BoundMethods{}, err
	}
	methods.OAuth.Providers = append(methods.OAuth.Providers, providers...)
	methods.OAuth.Bound = len(providers) > 0
	return methods, nil
}

type BoundMethodsHTTPHandler struct {
	authenticator RequestAuthenticator
	repository    MethodsRepository
	limiter       *ReadLimiter
}

func NewBoundMethodsHTTPHandler(authenticator RequestAuthenticator, repository MethodsRepository, limiters ...*ReadLimiter) *BoundMethodsHTTPHandler {
	return &BoundMethodsHTTPHandler{authenticator: authenticator, repository: repository, limiter: pickReadLimiter(limiters)}
}

func (h *BoundMethodsHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setNoStore(w, false)
	if r.Method != http.MethodGet {
		writeSimpleMethodNotAllowed(w, http.MethodGet)
		return
	}
	principal, release, ok := authenticateLimited(w, r, h.limiter, h.authenticator)
	if !ok {
		return
	}
	defer release()
	if principal == nil {
		writeAuthRequired(w)
		return
	}
	methods, err := h.repository.GetBoundMethods(r.Context(), principal.User.ID)
	if err != nil {
		writeUnavailable(w)
		return
	}
	if methods.OAuth.Providers == nil {
		methods.OAuth.Providers = []string{}
	}
	methods.Passkey = PasskeyMethods{}
	writeJSON(w, http.StatusOK, methods)
}

// Session management.

type SessionManagementDB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Begin(context.Context) (pgx.Tx, error)
}

type SessionManagementRepository interface {
	List(ctx context.Context, userID string) ([]AuthSession, error)
	Revoke(ctx context.Context, authSessionID, userID string) error
	RevokeUser(ctx context.Context, userID string) error
}

type AuthSession struct {
	ID                string     `json:"id"`
	InstallationID    string     `json:"installationId"`
	Platform          string     `json:"platform"`
	DeviceName        *string    `json:"deviceName"`
	AppVersion        *string    `json:"appVersion"`
	CreatedAt         time.Time  `json:"createdAt"`
	LastSeenAt        time.Time  `json:"lastSeenAt"`
	IdleExpiresAt     time.Time  `json:"idleExpiresAt"`
	AbsoluteExpiresAt time.Time  `json:"absoluteExpiresAt"`
	RevokedAt         *time.Time `json:"revokedAt"`
	RevokeReason      *string    `json:"revokeReason"`
	Current           bool       `json:"current"`
}

type PostgresSessionManagementRepository struct {
	db SessionManagementDB
}

func NewPostgresSessionManagementRepository(db SessionManagementDB) *PostgresSessionManagementRepository {
	return &PostgresSessionManagementRepository{db: db}
}

func (p *PostgresSessionManagementRepository) List(ctx context.Context, userID string) ([]AuthSession, error) {
	rows, err := p.db.Query(ctx, `SELECT a.id, a.installation_id, a.platform, a.device_name, a.app_version, a.created_at, a.last_seen_at,
		a.idle_expires_at, a.absolute_expires_at, a.revoked_at, a.revoke_reason
		FROM auth_sessions a JOIN user_sessions us ON us.id = a.user_session_id
		WHERE us.user_id=$1 AND (us.expires_at IS NULL OR us.expires_at > clock_timestamp())
		  AND a.revoked_at IS NULL AND a.idle_expires_at > clock_timestamp() AND a.absolute_expires_at > clock_timestamp()
		ORDER BY a.last_seen_at DESC, a.id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AuthSession, error) {
		var s AuthSession
		err := row.Scan(&s.ID, &s.InstallationID, &s.Platform, &s.DeviceName, &s.AppVersion, &s.CreatedAt, &s.LastSeenAt,
			&s.IdleExpiresAt, &s.AbsoluteExpiresAt, &s.RevokedAt, &s.RevokeReason)
		return s, err
	})
}

func (p *PostgresSessionManagementRepository) Revoke(ctx context.Context, authSessionID, userID string) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	var userSessionID, owner string
	err = tx.QueryRow(ctx, `SELECT us.id, us.user_id FROM auth_sessions a JOIN user_sessions us ON us.id = a.user_session_id WHERE a.id=$1`, authSessionID).Scan(&userSessionID, &owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != userID) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, q := range []string{
		`SELECT id FROM user_sessions WHERE id=$1 FOR UPDATE`,
		`SELECT id FROM auth_sessions WHERE user_session_id=$1 FOR UPDATE`,
		`SELECT id FROM auth_refresh_tokens WHERE auth_session_id IN (SELECT id FROM auth_sessions WHERE user_session_id=$1) ORDER BY id FOR UPDATE`,
	} {
		if _, err := tx.Exec(ctx, q, userSessionID); err != nil {
			return err
		}
	}
	if err := revokeAuthSession(ctx, tx, authSessionID, revokeUserAction); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// revokeAllUserSessions locks and revokes every session of a user whose row is
// already locked, then deletes the user's cookie sessions.
func revokeAllUserSessions(ctx context.Context, tx pgx.Tx, userID, reason string) error {
	for _, q := range []string{
		`SELECT id FROM user_sessions WHERE user_id=$1 ORDER BY id FOR UPDATE`,
		`SELECT id FROM auth_sessions WHERE user_session_id IN (SELECT id FROM user_sessions WHERE user_id=$1) ORDER BY id FOR UPDATE`,
		`SELECT id FROM auth_refresh_tokens WHERE auth_session_id IN (SELECT a.id FROM auth_sessions a JOIN user_sessions us ON us.id=a.user_session_id WHERE us.user_id=$1) ORDER BY id FOR UPDATE`,
		`UPDATE auth_sessions SET revoked_at=clock_timestamp(), revoke_reason=$2 WHERE revoked_at IS NULL AND user_session_id IN (SELECT id FROM user_sessions WHERE user_id=$1)`,
		`UPDATE auth_refresh_tokens SET revoked_at=clock_timestamp() WHERE revoked_at IS NULL AND auth_session_id IN (SELECT a.id FROM auth_sessions a JOIN user_sessions us ON us.id=a.user_session_id WHERE us.user_id=$1)`,
		`DELETE FROM user_sessions WHERE user_id=$1`,
	} {
		args := []any{userID}
		if strings.Contains(q, "$2") {
			args = append(args, reason)
		}
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

func (p *PostgresSessionManagementRepository) RevokeUser(ctx context.Context, userID string) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID); err != nil {
		return err
	}
	if err := revokeAllUserSessions(ctx, tx, userID, revokeSecurityEvent); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type SessionManagementHTTPHandler struct {
	authenticator RequestAuthenticator
	repository    SessionManagementRepository
	limiter       *ReadLimiter
}

func NewSessionManagementHTTPHandler(authenticator RequestAuthenticator, repository SessionManagementRepository, limiters ...*ReadLimiter) *SessionManagementHTTPHandler {
	return &SessionManagementHTTPHandler{authenticator: authenticator, repository: repository, limiter: pickReadLimiter(limiters)}
}

func (h *SessionManagementHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setPragma(w)
	revokeAll := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sessions/revoke-all")
	if r.Method != http.MethodGet && r.Method != http.MethodDelete && !revokeAll {
		writeSimpleMethodNotAllowed(w, "GET, DELETE")
		return
	}
	principal, release, ok := authenticateLimited(w, r, h.limiter, h.authenticator)
	if !ok {
		return
	}
	defer release()
	if principal == nil {
		writeAuthRequired(w)
		return
	}
	ctx := r.Context()
	switch {
	case r.Method == http.MethodGet:
		sessions, err := h.repository.List(ctx, principal.User.ID)
		if err != nil {
			writeUnavailable(w)
			return
		}
		if sessions == nil {
			sessions = []AuthSession{}
		}
		for i := range sessions {
			sessions[i].Current = principal.AuthSessionID != "" && sessions[i].ID == principal.AuthSessionID
		}
		writeJSON(w, http.StatusOK, sessions)
	case r.Method == http.MethodDelete:
		if !checkCSRF(r) {
			writeForbidden(w)
			return
		}
		id := r.PathValue("id")
		if id == "" {
			id = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		}
		if _, err := uuid.Parse(id); err != nil {
			writeInvalidAuthState(w)
			return
		}
		if err := h.repository.Revoke(ctx, id, principal.User.ID); err != nil {
			writeUnavailable(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
	default:
		if !checkCSRF(r) {
			writeForbidden(w)
			return
		}
		if err := h.repository.RevokeUser(ctx, principal.User.ID); err != nil {
			writeUnavailable(w)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{})
	}
}

// Sign-out.

type CookieSessionRepository interface {
	SignOut(ctx context.Context, sessionID string, userID *string) error
	ListUsers(ctx context.Context, sessionID string) ([]string, error)
}

type PostgresCookieSessionRepository struct {
	db SessionManagementDB
}

func NewPostgresCookieSessionRepository(db SessionManagementDB) *PostgresCookieSessionRepository {
	return &PostgresCookieSessionRepository{db: db}
}

func (p *PostgresCookieSessionRepository) SignOut(ctx context.Context, sessionID string, userID *string) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	type stmt struct {
		sql  string
		args []any
	}
	var statements []stmt
	if userID != nil {
		statements = []stmt{
			{`SELECT id FROM users WHERE id=$1 FOR UPDATE`, []any{*userID}},
			{`SELECT id FROM user_sessions WHERE session_id=$1 ORDER BY id FOR UPDATE`, []any{sessionID}},
			{`DELETE FROM user_sessions WHERE session_id=$1 AND user_id=$2`, []any{sessionID, *userID}},
		}
	} else {
		statements = []stmt{
			{`SELECT id FROM users WHERE id IN (SELECT user_id FROM user_sessions WHERE session_id=$1) ORDER BY id FOR UPDATE`, []any{sessionID}},
			{`SELECT id FROM user_sessions WHERE session_id=$1 ORDER BY id FOR UPDATE`, []any{sessionID}},
			{`DELETE FROM multiple_users_sessions WHERE id=$1`, []any{sessionID}},
		}
	}
	for _, st := range statements {
		if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (p *PostgresCookieSessionRepository) ListUsers(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := p.db.Query(ctx, `SELECT user_id FROM user_sessions WHERE session_id=$1
		AND (expires_at IS NULL OR expires_at > clock_timestamp()) ORDER BY created_at DESC, id DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

type SignOutHTTPHandler struct {
	authenticator RequestAuthenticator
	repository    CookieSessionRepository
	limiter       *ReadLimiter
}

func NewSignOutHTTPHandler(authenticator RequestAuthenticator, repository CookieSessionRepository, limiters ...*ReadLimiter) *SignOutHTTPHandler {
	return &SignOutHTTPHandler{authenticator: authenticator, repository: repository, limiter: pickReadLimiter(limiters)}
}

func (h *SignOutHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setNoStore(w, false)
	if r.Method != http.MethodPost {
		writeSimpleMethodNotAllowed(w, http.MethodPost)
		return
	}
	principal, release, ok := authenticateLimited(w, r, h.limiter, h.authenticator)
	if !ok {
		return
	}
	defer release()
	if principal == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if !checkCSRF(r) {
		writeForbidden(w)
		return
	}
	ctx := r.Context()
	var target *string
	if query := r.URL.Query(); query.Has("user_id") {
		v := query.Get("user_id")
		target = &v
	}
	if err := h.repository.SignOut(ctx, principal.SessionID, target); err != nil {
		writeUnavailable(w)
		return
	}
	remaining, err := h.repository.ListUsers(ctx, principal.SessionID)
	if err != nil {
		writeUnavailable(w)
		return
	}
	if len(remaining) == 0 {
		ClearSessionCookies(w)
	} else {
		http.SetCookie(w, &http.Cookie{Name: UserCookieName, Value: remaining[0], Path: "/", SameSite: http.SameSiteLaxMode})
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}
