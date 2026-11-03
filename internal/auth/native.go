// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	refreshTokenPrefix  = "aff_rt_v1."
	refreshRateLimit    = 30
	refreshRateWindow   = 60 * time.Second
	refreshRateBucket   = 10 * time.Second
	refreshRateRowTTL   = 70 * time.Second
	revokeUserDisabled  = "user_disabled"
	revokeTokenReused   = "refresh_token_reused"
	revokeTokenRevoke   = "refresh_token_revoke"
	revokeUserAction    = "user_action"
	revokeSecurityEvent = "security_action"
)

type NativeSessionHTTPHandler struct {
	db         NativeSessionDB
	cfg        config.Config
	privateKey string
}

func NewNativeSessionHTTPHandler(db NativeSessionDB, cfg config.Config, privateKey string) *NativeSessionHTTPHandler {
	return &NativeSessionHTTPHandler{db: db, cfg: cfg, privateKey: privateKey}
}

type tokenPair struct {
	accessToken    string
	refreshToken   string
	refreshExpires time.Time
	sessionID      string
	absolute       time.Time
}

func (h *NativeSessionHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setPragma(w)
	var op string
	switch {
	case strings.HasSuffix(r.URL.Path, "/session/exchange"):
		op = "exchange"
	case strings.HasSuffix(r.URL.Path, "/session/refresh"):
		op = "refresh"
	case strings.HasSuffix(r.URL.Path, "/session/revoke"):
		op = "revoke"
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeTokenError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed.")
		return
	}
	native := IsNativeClientRequest(r)
	if op == "revoke" && !native {
		writeTokenError(w, http.StatusForbidden, "ACTION_FORBIDDEN", "You are not allowed to perform this action.")
		return
	}
	allowed := map[string]bool{"refreshToken": true}
	if op == "exchange" {
		allowed = map[string]bool{"code": true, "installationId": true, "platform": true, "deviceName": true}
	}
	var fields map[string]string
	body, err := readBody(w, r, nativeBodyLimit)
	if err == nil && isJSONContentType(r) {
		fields, err = parseStringObject(body, allowed, map[string]bool{"deviceName": true})
	} else if err == nil {
		err = errNativeValidation
	}
	if err != nil {
		h.writeError(w, errNativeValidation)
		return
	}
	if !native {
		writeTokenError(w, http.StatusForbidden, "ACTION_FORBIDDEN", "You are not allowed to perform this action.")
		return
	}
	if utf16Len(r.Header.Get("x-affine-version")) > 128 {
		writeTokenError(w, http.StatusBadRequest, "INVALID_AUTH_STATE", "Invalid auth state.")
		return
	}
	ctx := r.Context()
	var pair tokenPair
	var expiresIn time.Duration
	switch op {
	case "revoke":
		if err := h.revoke(ctx, fields["refreshToken"]); err != nil {
			h.writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{})
		return
	case "exchange":
		pair, expiresIn, err = h.exchange(ctx, r, fields)
	case "refresh":
		pair, expiresIn, err = h.refresh(ctx, r, fields["refreshToken"])
	}
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"tokenType":        "Bearer",
		"accessToken":      pair.accessToken,
		"expiresIn":        int64(expiresIn / time.Second),
		"refreshToken":     pair.refreshToken,
		"refreshExpiresAt": pair.refreshExpires.UTC().Format(time.RFC3339),
		"session":          map[string]any{"id": pair.sessionID, "absoluteExpiresAt": pair.absolute.UTC().Format(time.RFC3339)},
	})
}

func (h *NativeSessionHTTPHandler) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNativeValidation):
		writeTokenError(w, 400, "VALIDATION_ERROR", "Invalid request body.")
	case errors.Is(err, ErrInvalidAuthState):
		writeTokenError(w, 400, "INVALID_AUTH_STATE", "Invalid auth state. You might start the auth progress from another device.")
	case errors.Is(err, errRefreshTokenInvalid):
		writeTokenError(w, 401, "REFRESH_TOKEN_INVALID", "The refresh token is invalid.")
	case errors.Is(err, errRefreshTokenReused):
		writeTokenError(w, 401, "REFRESH_TOKEN_REUSED", "The refresh token has already been used.")
	case errors.Is(err, ErrAuthSessionExpired):
		writeTokenError(w, 401, "AUTH_SESSION_EXPIRED", "The auth session has expired.")
	case errors.Is(err, ErrAuthSessionRevoked):
		writeTokenError(w, 401, "AUTH_SESSION_REVOKED", "The auth session has been revoked.")
	case errors.Is(err, errRefreshRateLimited):
		writeTokenError(w, 429, "AUTH_REFRESH_RATE_LIMITED", "Too many refresh requests.")
	default:
		writeTokenError(w, 503, "AUTH_SESSION_TEMPORARILY_UNAVAILABLE", "Auth session service is temporarily unavailable.")
	}
}

func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (h *NativeSessionHTTPHandler) signPair(keys keyring, userID, sessionID string, now time.Time, policy tokenPolicy, pair tokenPair) (tokenPair, error) {
	access, err := signAccessToken(keys.active(), userID, sessionID, now, policy.access)
	pair.accessToken = access
	return pair, err
}

func (h *NativeSessionHTTPHandler) exchange(ctx context.Context, r *http.Request, fields map[string]string) (tokenPair, time.Duration, error) {
	code := fields["code"]
	installation := fields["installationId"]
	platform := fields["platform"]
	var deviceName *string
	if raw, ok := fields["deviceName"]; ok {
		trimmed := strings.TrimSpace(raw)
		if n := utf16Len(trimmed); n < 1 || n > 200 {
			return tokenPair{}, 0, errNativeValidation
		}
		deviceName = &trimmed
	}
	if len(code) < 1 || len(code) > 512 || !canonicalUUID(installation) || (platform != "ios" && platform != "android" && platform != "electron") {
		return tokenPair{}, 0, errNativeValidation
	}
	policy, err := loadTokenPolicy(ctx, h.db, h.cfg)
	if err != nil {
		return tokenPair{}, 0, err
	}
	keys, err := loadKeyring(ctx, h.db)
	if err != nil {
		return tokenPair{}, 0, err
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return tokenPair{}, 0, err
	}
	defer rollback(tx)
	codeHash := sha256hex(code)
	const stateQuery = `SELECT payload FROM runtime_states WHERE purpose=$1 AND token_hash=$2 AND consumed_at IS NULL AND expires_at > clock_timestamp()`
	var raw []byte
	if err := tx.QueryRow(ctx, stateQuery, purposeExchange, codeHash).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tokenPair{}, 0, ErrInvalidAuthState
		}
		return tokenPair{}, 0, err
	}
	var payload exchangePayload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.UserID == "" {
		return tokenPair{}, 0, ErrInvalidAuthState
	}
	var disabled bool
	var epoch int
	err = tx.QueryRow(ctx, `SELECT disabled, auth_epoch FROM users WHERE id=$1 FOR UPDATE`, payload.UserID).Scan(&disabled, &epoch)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (disabled || epoch != payload.AuthEpoch)) {
		return tokenPair{}, 0, ErrInvalidAuthState
	}
	if err != nil {
		return tokenPair{}, 0, err
	}
	var confirm []byte
	if err := tx.QueryRow(ctx, stateQuery+` FOR UPDATE`, purposeExchange, codeHash).Scan(&confirm); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tokenPair{}, 0, ErrInvalidAuthState
		}
		return tokenPair{}, 0, err
	}
	var confirmed exchangePayload
	if json.Unmarshal(confirm, &confirmed) != nil || confirmed.UserID != payload.UserID || confirmed.AuthEpoch != payload.AuthEpoch {
		return tokenPair{}, 0, ErrInvalidAuthState
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return tokenPair{}, 0, err
	}
	appVersion := clientVersionHeader(r)
	if appVersion == nil {
		appVersion = payload.ClientVersion
	}
	absolute := now.Add(policy.absolute)
	idle := minTime(now.Add(policy.idle), absolute)
	cookieSessionID, userSessionID, authSessionID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	refresh, refreshID, secretHash := newRefreshToken()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO multiple_users_sessions(id) VALUES($1)`, []any{cookieSessionID}},
		{`INSERT INTO user_sessions(id,session_id,user_id,expires_at,sign_in_client_version,refresh_client_version,created_at) VALUES($1,$2,$3,$4,$5,$5,$6)`,
			[]any{userSessionID, cookieSessionID, payload.UserID, idle, appVersion, now}},
		{`INSERT INTO auth_sessions(id,user_session_id,installation_id,platform,device_name,app_version,created_at,last_seen_at,idle_expires_at,absolute_expires_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$7,$8,$9)`, []any{authSessionID, userSessionID, installation, platform, deviceName, appVersion, now, idle, absolute}},
		{`INSERT INTO auth_refresh_tokens(id,auth_session_id,generation,secret_hash,created_at,expires_at) VALUES($1,$2,0,$3,$4,$5)`,
			[]any{refreshID, authSessionID, secretHash, now, idle}},
		{`UPDATE runtime_states SET consumed_at=$3, updated_at=$3 WHERE purpose=$1 AND token_hash=$2`, []any{purposeExchange, codeHash, now}},
	}
	for _, s := range statements {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return tokenPair{}, 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return tokenPair{}, 0, err
	}
	pair, err := h.signPair(keys, payload.UserID, authSessionID, now, policy, tokenPair{refreshToken: refresh, refreshExpires: idle, sessionID: authSessionID, absolute: absolute})
	return pair, policy.access, err
}

func newRefreshToken() (token, id, secretHash string) {
	id = randomB64(18)
	secretRaw := mustRandom(32)
	secret := b64.EncodeToString(secretRaw)
	return refreshTokenPrefix + id + "." + secret, id, sha256hexBytes(secretRaw)
}

func parseRefreshToken(token string) (id string, secret []byte, ok bool) {
	rest, found := strings.CutPrefix(token, refreshTokenPrefix)
	if !found {
		return "", nil, false
	}
	id, secretText, found := strings.Cut(rest, ".")
	if !found {
		return "", nil, false
	}
	idRaw, err1 := b64.DecodeString(id)
	secret, err2 := b64.DecodeString(secretText)
	if err1 != nil || err2 != nil || len(idRaw) != 18 || len(secret) != 32 || b64.EncodeToString(idRaw) != id || b64.EncodeToString(secret) != secretText {
		return "", nil, false
	}
	return id, secret, true
}

func hashMatches(storedHex string, secret []byte) bool {
	stored, err := hex.DecodeString(storedHex)
	if err != nil {
		return false
	}
	got, _ := hex.DecodeString(sha256hexBytes(secret))
	return subtle.ConstantTimeCompare(stored, got) == 1
}

type refreshRow struct {
	id, secretHash               string
	generation                   int
	expires                      time.Time
	usedAt, graceUsedAt, revoked *time.Time
	replacedBy, successor        *string
	successorExpires             *time.Time
}

type lockedSession struct {
	userID, userSessionID, authSessionID string
	userDisabled                         bool
	userSessionExpires                   *time.Time
	revokedAt                            *time.Time
	idle, absolute                       time.Time
	tokens                               []refreshRow
	now                                  time.Time
}

func (s *lockedSession) token(id string) *refreshRow {
	for i := range s.tokens {
		if s.tokens[i].id == id {
			return &s.tokens[i]
		}
	}
	return nil
}

// refreshOwner finds the session chain of a refresh token without locking.
func refreshOwner(ctx context.Context, db RowQuerier, tokenID string) (authSessionID, userSessionID, userID string, err error) {
	err = db.QueryRow(ctx, `SELECT a.id, us.id, us.user_id FROM auth_refresh_tokens t
		JOIN auth_sessions a ON a.id = t.auth_session_id JOIN user_sessions us ON us.id = a.user_session_id WHERE t.id=$1`, tokenID).
		Scan(&authSessionID, &userSessionID, &userID)
	return
}

// lockSessionChain locks user → user_session → auth_session → refresh rows.
func lockSessionChain(ctx context.Context, tx pgx.Tx, userID, userSessionID, authSessionID string) (*lockedSession, error) {
	s := &lockedSession{authSessionID: authSessionID}
	if err := tx.QueryRow(ctx, `SELECT id, disabled FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&s.userID, &s.userDisabled); err != nil {
		return nil, err
	}
	var usUser string
	if err := tx.QueryRow(ctx, `SELECT id, user_id, expires_at FROM user_sessions WHERE id=$1 FOR UPDATE`, userSessionID).Scan(&s.userSessionID, &usUser, &s.userSessionExpires); err != nil {
		return nil, err
	}
	var aUS string
	if err := tx.QueryRow(ctx, `SELECT user_session_id, revoked_at, idle_expires_at, absolute_expires_at, clock_timestamp() FROM auth_sessions WHERE id=$1 FOR UPDATE`, authSessionID).
		Scan(&aUS, &s.revokedAt, &s.idle, &s.absolute, &s.now); err != nil {
		return nil, err
	}
	if usUser != userID || aUS != userSessionID {
		return nil, pgx.ErrNoRows
	}
	rows, err := tx.Query(ctx, `SELECT id, secret_hash, generation, expires_at, used_at, grace_used_at, revoked_at, replaced_by_id, successor_ciphertext, successor_expires_at
		FROM auth_refresh_tokens WHERE auth_session_id=$1 ORDER BY id FOR UPDATE`, authSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t refreshRow
		if err := rows.Scan(&t.id, &t.secretHash, &t.generation, &t.expires, &t.usedAt, &t.graceUsedAt, &t.revoked, &t.replacedBy, &t.successor, &t.successorExpires); err != nil {
			return nil, err
		}
		s.tokens = append(s.tokens, t)
	}
	return s, rows.Err()
}

func revokeAuthSession(ctx context.Context, tx pgx.Tx, authSessionID, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=clock_timestamp(), revoke_reason=$2 WHERE id=$1 AND revoked_at IS NULL`, authSessionID, reason); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE auth_refresh_tokens SET revoked_at=clock_timestamp() WHERE auth_session_id=$1 AND revoked_at IS NULL`, authSessionID)
	return err
}

// consumeRefreshQuota counts one refresh in the rolling per-token window.
func consumeRefreshQuota(ctx context.Context, db beginner, tokenID string) (bool, error) {
	scope := "auth:session_refresh:" + tokenID
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, scope); err != nil {
		return false, err
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return false, err
	}
	var used int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(count),0)::bigint FROM runtime_rolling_quota_counters
		WHERE scope_key=$1 AND window_seconds=$2 AND bucket_start > $3`, scope, int(refreshRateWindow/time.Second), now.Add(-refreshRateWindow)).Scan(&used); err != nil {
		return false, err
	}
	if used >= refreshRateLimit {
		return false, nil
	}
	bucket := now.Truncate(refreshRateBucket)
	if _, err := tx.Exec(ctx, `INSERT INTO runtime_rolling_quota_counters(scope_key,window_seconds,bucket_start,count,expires_at,updated_at)
		VALUES($1,$2,$3,1,$4,$5) ON CONFLICT (scope_key,window_seconds,bucket_start)
		DO UPDATE SET count=runtime_rolling_quota_counters.count+1, expires_at=EXCLUDED.expires_at, updated_at=EXCLUDED.updated_at`,
		scope, int(refreshRateWindow/time.Second), bucket, now.Add(refreshRateRowTTL), now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (h *NativeSessionHTTPHandler) refresh(ctx context.Context, r *http.Request, token string) (tokenPair, time.Duration, error) {
	tokenID, secret, ok := parseRefreshToken(token)
	if !ok {
		return tokenPair{}, 0, errRefreshTokenInvalid
	}
	authSessionID, userSessionID, userID, err := refreshOwner(ctx, h.db, tokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tokenPair{}, 0, errRefreshTokenInvalid
	}
	if err != nil {
		return tokenPair{}, 0, err
	}
	if allowed, err := consumeRefreshQuota(ctx, h.db, tokenID); err != nil {
		return tokenPair{}, 0, err
	} else if !allowed {
		return tokenPair{}, 0, errRefreshRateLimited
	}
	policy, err := loadTokenPolicy(ctx, h.db, h.cfg)
	if err != nil {
		return tokenPair{}, 0, err
	}
	keys, err := loadKeyring(ctx, h.db)
	if err != nil {
		return tokenPair{}, 0, err
	}
	privateKey, err := LoadAppPrivateKey(ctx, h.db, h.cfg, h.privateKey)
	if err != nil || strings.TrimSpace(privateKey) == "" {
		return tokenPair{}, 0, errors.New("auth: app private key unavailable")
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return tokenPair{}, 0, err
	}
	defer rollback(tx)
	s, err := lockSessionChain(ctx, tx, userID, userSessionID, authSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tokenPair{}, 0, errRefreshTokenInvalid
	}
	if err != nil {
		return tokenPair{}, 0, err
	}
	current := s.token(tokenID)
	if current == nil || !hashMatches(current.secretHash, secret) {
		return tokenPair{}, 0, errRefreshTokenInvalid
	}
	now := s.now
	if s.userDisabled {
		if err := revokeAuthSession(ctx, tx, authSessionID, revokeUserDisabled); err != nil {
			return tokenPair{}, 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return tokenPair{}, 0, err
		}
		return tokenPair{}, 0, ErrAuthSessionRevoked
	}
	if s.revokedAt != nil || current.revoked != nil {
		return tokenPair{}, 0, ErrAuthSessionRevoked
	}
	if !current.expires.After(now) || !s.idle.After(now) || !s.absolute.After(now) || (s.userSessionExpires != nil && !s.userSessionExpires.After(now)) {
		return tokenPair{}, 0, ErrAuthSessionExpired
	}
	version := clientVersionHeader(r)
	if current.usedAt == nil {
		next, nextID, nextHash := newRefreshToken()
		expires := minTime(now.Add(policy.idle), s.absolute)
		sealed, err := sealSuccessor(privateKey, next, tokenID, authSessionID)
		if err != nil {
			return tokenPair{}, 0, err
		}
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO auth_refresh_tokens(id,auth_session_id,generation,secret_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6)`,
				[]any{nextID, authSessionID, current.generation + 1, nextHash, now, expires}},
			{`UPDATE auth_refresh_tokens SET used_at=$2, replaced_by_id=$3, successor_ciphertext=$4, successor_expires_at=$5 WHERE id=$1`,
				[]any{tokenID, now, nextID, sealed, now.Add(policy.grace)}},
			{`UPDATE auth_sessions SET last_seen_at=$2, idle_expires_at=$3, app_version=$4 WHERE id=$1`, []any{authSessionID, now, expires, version}},
			{`UPDATE user_sessions SET expires_at=$2, refresh_client_version=$3 WHERE id=$1`, []any{userSessionID, expires, version}},
		}
		for _, st := range statements {
			if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
				return tokenPair{}, 0, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return tokenPair{}, 0, err
		}
		pair, err := h.signPair(keys, userID, authSessionID, now, policy, tokenPair{refreshToken: next, refreshExpires: expires, sessionID: authSessionID, absolute: s.absolute})
		return pair, policy.access, err
	}
	var replacement *refreshRow
	if current.replacedBy != nil {
		replacement = s.token(*current.replacedBy)
	}
	if current.graceUsedAt == nil && current.successor != nil && current.successorExpires != nil && current.successorExpires.After(now) &&
		replacement != nil && replacement.usedAt == nil && replacement.revoked == nil && replacement.expires.After(now) {
		successor, err := openSuccessor(privateKey, *current.successor, tokenID, authSessionID)
		if err != nil {
			return tokenPair{}, 0, err
		}
		sid, ssecret, ok := parseRefreshToken(successor)
		if !ok || sid != replacement.id || !hashMatches(replacement.secretHash, ssecret) {
			return tokenPair{}, 0, errors.New("auth: successor mismatch")
		}
		if _, err := tx.Exec(ctx, `UPDATE auth_refresh_tokens SET grace_used_at=$2 WHERE id=$1`, tokenID, now); err != nil {
			return tokenPair{}, 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return tokenPair{}, 0, err
		}
		pair, err := h.signPair(keys, userID, authSessionID, now, policy, tokenPair{refreshToken: successor, refreshExpires: replacement.expires, sessionID: authSessionID, absolute: s.absolute})
		return pair, policy.access, err
	}
	if err := revokeAuthSession(ctx, tx, authSessionID, revokeTokenReused); err != nil {
		return tokenPair{}, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return tokenPair{}, 0, err
	}
	return tokenPair{}, 0, errRefreshTokenReused
}

func (h *NativeSessionHTTPHandler) revoke(ctx context.Context, token string) error {
	tokenID, secret, ok := parseRefreshToken(token)
	if !ok {
		return nil
	}
	authSessionID, userSessionID, userID, err := refreshOwner(ctx, h.db, tokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	s, err := lockSessionChain(ctx, tx, userID, userSessionID, authSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	current := s.token(tokenID)
	if current == nil || !hashMatches(current.secretHash, secret) {
		return nil
	}
	if err := revokeAuthSession(ctx, tx, authSessionID, revokeTokenRevoke); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
