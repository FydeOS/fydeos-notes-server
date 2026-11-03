// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	purposeMagicLink      = "magic_link_otp"
	purposeExchange       = "auth_challenge:auth_session_exchange"
	purposeOpenApp        = "auth_challenge:open_app_sign_in"
	purposeCaptcha        = "auth_challenge:captcha"
	purposeChallengPrefix = "auth_challenge:"
)

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func dbNow(ctx context.Context, q RowQuerier) (time.Time, error) {
	var now time.Time
	err := q.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}

func insertState(ctx context.Context, tx pgx.Tx, purpose, tokenHash string, lookupKey *string, payload any, expires time.Time) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO runtime_states(purpose,token_hash,lookup_key,payload,attempts,expires_at,created_at,updated_at)
		VALUES($1,$2,$3,$4::jsonb,0,$5,clock_timestamp(),clock_timestamp())`, purpose, tokenHash, lookupKey, string(encoded), expires)
	return err
}

type exchangePayload struct {
	UserID        string  `json:"userId"`
	AuthEpoch     int     `json:"authEpoch"`
	ClientVersion *string `json:"clientVersion"`
}

// IssueNativeExchangeCode stores a one-minute exchange code in tx.
func IssueNativeExchangeCode(ctx context.Context, tx pgx.Tx, userID string, authEpoch int, clientVersion *string, now time.Time) (string, error) {
	if tx == nil || userID == "" {
		return "", errors.New("auth: exchange code needs a transaction and user")
	}
	code := uuid.NewString()
	err := insertState(ctx, tx, purposeExchange, sha256hex(code), nil, exchangePayload{UserID: userID, AuthEpoch: authEpoch, ClientVersion: clientVersion}, now.Add(NativeExchangeCodeTTL))
	if err != nil {
		return "", err
	}
	return code, nil
}

type issuedSession struct {
	exchangeCode string
	sessionID    string
	expires      time.Time
}

// issueSession creates either a native exchange code or a cookie session.
func issueSession(ctx context.Context, tx pgx.Tx, r *http.Request, cfg config.Config, userID string, authEpoch int) (issuedSession, error) {
	now, err := dbNow(ctx, tx)
	if err != nil {
		return issuedSession{}, err
	}
	if IsNativeClientRequest(r) {
		code, err := IssueNativeExchangeCode(ctx, tx, userID, authEpoch, clientVersionHeader(r), now)
		return issuedSession{exchangeCode: code}, err
	}
	ttl, err := readConfigInt(ctx, tx, cfg, "auth.session.ttl", 15*86400)
	if err != nil {
		return issuedSession{}, err
	}
	if ttl <= 0 {
		return issuedSession{}, errors.New("invalid auth.session.ttl")
	}
	sessionID := ""
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if _, perr := uuid.Parse(c.Value); perr == nil {
			err := tx.QueryRow(ctx, `SELECT id FROM multiple_users_sessions WHERE id=$1 FOR UPDATE`, c.Value).Scan(&sessionID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return issuedSession{}, err
			}
		}
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, sessionID); err != nil {
			return issuedSession{}, err
		}
	}
	expires := now.Add(time.Duration(ttl) * time.Second)
	if _, err := tx.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,expires_at,sign_in_client_version,created_at)
		VALUES($1,$2,$3,$4,$5,clock_timestamp())
		ON CONFLICT (session_id,user_id) DO UPDATE SET expires_at=EXCLUDED.expires_at,
		  sign_in_client_version=COALESCE(EXCLUDED.sign_in_client_version, user_sessions.sign_in_client_version)`,
		uuid.NewString(), sessionID, userID, expires, clientVersionHeader(r)); err != nil {
		return issuedSession{}, err
	}
	return issuedSession{sessionID: sessionID, expires: expires}, nil
}

// apply writes cookies (web) or adds the exchange code (native) to body.
func (s issuedSession) apply(w http.ResponseWriter, cfg config.Config, userID string, body map[string]any) {
	if s.exchangeCode != "" {
		ClearSessionCookies(w)
		body["exchangeCode"] = s.exchangeCode
		return
	}
	setSignInCookies(w, s.sessionID, userID, s.expires, cfg.HTTPS)
}

func mustRandom(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return buf
}

func sha256hexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
