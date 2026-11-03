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
	"github.com/fydeos/fydeos-notes-server/internal/identity"
	"github.com/google/uuid"
)

const magicLinkMaxAttempts = 10

type magicLinkHandler struct {
	db         PasswordLoginDatabase
	cfg        config.Config
	limiter    PasswordLoginLimiter
	privateKey string
}

func NewMagicLinkHTTPHandler(db PasswordLoginDatabase, cfg config.Config, limiter PasswordLoginLimiter, privateKey string) http.Handler {
	if limiter == nil {
		limiter = newMemoryLimiter()
	}
	return &magicLinkHandler{db: db, cfg: cfg, limiter: limiter, privateKey: privateKey}
}

var magicLinkFields = map[string]bool{"email": true, "token": true, "client_nonce": true}

func (h *magicLinkHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setPragma(w)
	body, ok := signInPrelude(w, r, h.cfg, h.limiter)
	if !ok {
		return
	}
	fields, err := parseStringObject(body, magicLinkFields, nil)
	if err != nil || !isJSONContentType(r) {
		writeAPIError(w, apiTokenNotFound)
		return
	}
	email := strings.TrimSpace(fields["email"])
	if utf16Len(email) > 320 || !optionalLength(fields, "token", 1, 512) || fields["token"] == "" || !optionalLength(fields, "client_nonce", 1, 512) {
		writeAPIError(w, apiTokenNotFound)
		return
	}
	if !strictEmail(email) {
		writeAPIError(w, apiInvalidEmailShort(email))
		return
	}
	var nonce *string
	if v, ok := fields["client_nonce"]; ok {
		nonce = &v
	}
	userID, issued, apiErr := h.complete(r.Context(), r, email, fields["token"], nonce)
	if apiErr != nil {
		writeAPIError(w, *apiErr)
		return
	}
	out := map[string]any{"id": userID}
	issued.apply(w, h.cfg, userID, out)
	writeJSON(w, http.StatusOK, out)
}

func (h *magicLinkHandler) complete(ctx context.Context, r *http.Request, email, otp string, nonce *string) (string, issuedSession, *apiError) {
	invalid := func() (string, issuedSession, *apiError) { e := apiInvalidToken; return "", issuedSession{}, &e }
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return invalid()
	}
	defer rollback(tx)
	if err := identity.LockEmail(ctx, tx, email); err != nil {
		return invalid()
	}
	users, err := lockUsersByEmail(ctx, tx, email)
	if err != nil || len(users) > 1 {
		return invalid()
	}
	lower := strings.ToLower(email)
	tokenHash := sha256hex(lower)
	var rawPayload []byte
	var attempts int
	var expires, now time.Time
	err = tx.QueryRow(ctx, `SELECT payload, attempts, expires_at, clock_timestamp() FROM runtime_states
		WHERE purpose=$1 AND token_hash=$2 AND consumed_at IS NULL FOR UPDATE`, purposeMagicLink, tokenHash).Scan(&rawPayload, &attempts, &expires, &now)
	if err != nil {
		return invalid()
	}
	consume := func() error {
		_, err := tx.Exec(ctx, `UPDATE runtime_states SET consumed_at=clock_timestamp(), updated_at=clock_timestamp() WHERE purpose=$1 AND token_hash=$2`, purposeMagicLink, tokenHash)
		return err
	}
	if !expires.After(now) || attempts >= magicLinkMaxAttempts {
		if consume() == nil {
			_ = tx.Commit(ctx)
		}
		return invalid()
	}
	var payload magicLinkPayload
	if err := json.Unmarshal(rawPayload, &payload); err != nil || payload.OTPHash == nil {
		return invalid()
	}
	if payload.ClientNonce != nil && (nonce == nil || subtle.ConstantTimeCompare([]byte(*nonce), []byte(*payload.ClientNonce)) != 1) {
		e := apiInvalidAuthState
		return "", issuedSession{}, &e
	}
	want, err1 := hex.DecodeString(*payload.OTPHash)
	got, err2 := hex.DecodeString(sha256hex(otp))
	if err1 != nil || err2 != nil || subtle.ConstantTimeCompare(want, got) != 1 {
		if _, err := tx.Exec(ctx, `UPDATE runtime_states SET attempts=attempts+1,
			consumed_at=CASE WHEN attempts+1 >= $3 THEN clock_timestamp() ELSE consumed_at END, updated_at=clock_timestamp()
			WHERE purpose=$1 AND token_hash=$2`, purposeMagicLink, tokenHash, magicLinkMaxAttempts); err == nil {
			_ = tx.Commit(ctx)
		}
		return invalid()
	}
	var userID string
	var epoch int
	if len(users) == 1 {
		u := users[0]
		if u.Disabled || payload.UserID == nil || payload.AuthEpoch == nil || *payload.UserID != u.ID || *payload.AuthEpoch != u.AuthEpoch {
			return invalid()
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET registered=true, email_verified=COALESCE(email_verified, clock_timestamp()) WHERE id=$1`, u.ID); err != nil {
			return invalid()
		}
		userID, epoch = u.ID, u.AuthEpoch
	} else {
		if payload.UserID != nil {
			return invalid()
		}
		allowed, err := signUpAllowed(ctx, tx, h.cfg, email, "auth.allowSignup")
		if errors.Is(err, errDomainCheckNetwork) {
			e := apiNetworkError
			return "", issuedSession{}, &e
		}
		if err != nil || !allowed {
			return invalid()
		}
		userID = uuid.NewString()
		name := email[:strings.LastIndex(email, "@")]
		if _, err := tx.Exec(ctx, `INSERT INTO users(id,name,email,email_verified,registered,auth_epoch,created_at)
			VALUES($1,$2,$3,clock_timestamp(),true,0,clock_timestamp())`, userID, name, email); err != nil {
			return invalid()
		}
	}
	if err := consume(); err != nil {
		return invalid()
	}
	issued, err := issueSession(ctx, tx, r, h.cfg, userID, epoch)
	if err != nil || tx.Commit(ctx) != nil {
		return invalid()
	}
	return userID, issued, nil
}
