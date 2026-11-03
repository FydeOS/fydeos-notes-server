// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const openAppCodeTTL = 5 * time.Minute

type OpenAppHTTPHandler struct {
	db            PasswordLoginDatabase
	cfg           config.Config
	authenticator RequestAuthenticator
	limiter       PasswordLoginLimiter
}

func NewOpenAppHTTPHandler(db PasswordLoginDatabase, cfg config.Config, authenticator RequestAuthenticator, limiter PasswordLoginLimiter) *OpenAppHTTPHandler {
	if limiter == nil {
		limiter = newMemoryLimiter()
	}
	return &OpenAppHTTPHandler{db: db, cfg: cfg, authenticator: authenticator, limiter: limiter}
}

type openAppPayload struct {
	UserID    string `json:"userId"`
	AuthEpoch int    `json:"authEpoch"`
}

func (h *OpenAppHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setPragma(w)
	issueCode := strings.HasSuffix(r.URL.Path, "/open-app/sign-in-code")
	if !issueCode && !strings.HasSuffix(r.URL.Path, "/open-app/sign-in") {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeTokenError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed.")
		return
	}
	var body []byte
	var err error
	if issueCode {
		_, err = readBody(w, r, 100<<10)
		if errors.Is(err, errBodyTooLarge) {
			err = nil
		}
	} else {
		body, err = readBody(w, r, signInBodyLimit)
	}
	if err != nil {
		writeInvalidAuthState(w)
		return
	}
	if utf16Len(r.Header.Get("x-affine-version")) > 128 {
		writeTokenError(w, http.StatusBadRequest, "UNSUPPORTED_CLIENT_VERSION", "Unsupported client version.")
		return
	}
	if !h.limiter.AllowStrict(r.Context(), requestClientIP(r, h.cfg), time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeTokenError(w, http.StatusTooManyRequests, "TOO_MANY_REQUESTS", "Too many requests.")
		return
	}
	if issueCode {
		h.issueCode(w, r)
		return
	}
	h.signIn(w, r, body)
}

func (h *OpenAppHTTPHandler) issueCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principal, err := AuthenticateRequest(ctx, w, r, h.authenticator)
	if err != nil && !errors.Is(err, ErrNoSession) {
		if !WriteAccessTokenError(w, err) {
			writeUnavailable(w)
		}
		return
	}
	if principal == nil {
		writeForbidden(w)
		return
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		writeUnavailable(w)
		return
	}
	defer rollback(tx)
	var disabled bool
	var epoch int
	var now time.Time
	err = tx.QueryRow(ctx, `SELECT disabled, auth_epoch, clock_timestamp() FROM users WHERE id=$1 FOR SHARE`, principal.User.ID).Scan(&disabled, &epoch, &now)
	if err != nil || disabled {
		writeInvalidAuthState(w)
		return
	}
	code := uuid.NewString()
	if err := insertState(ctx, tx, purposeOpenApp, sha256hex(code), nil, openAppPayload{UserID: principal.User.ID, AuthEpoch: epoch}, now.Add(openAppCodeTTL)); err != nil || tx.Commit(ctx) != nil {
		writeUnavailable(w)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"code": code})
}

func (h *OpenAppHTTPHandler) signIn(w http.ResponseWriter, r *http.Request, body []byte) {
	ctx := r.Context()
	if !isJSONContentType(r) {
		writeInvalidAuthState(w)
		return
	}
	fields, err := parseStringObject(body, map[string]bool{"code": true}, nil)
	code := fields["code"]
	if err != nil || code == "" || len(code) > 512 {
		writeInvalidAuthState(w)
		return
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		writeInvalidAuthState(w)
		return
	}
	defer rollback(tx)
	var raw []byte
	err = tx.QueryRow(ctx, `UPDATE runtime_states SET consumed_at=clock_timestamp(), updated_at=clock_timestamp()
		WHERE purpose=$1 AND token_hash=$2 AND consumed_at IS NULL AND expires_at > clock_timestamp() RETURNING payload`,
		purposeOpenApp, sha256hex(code)).Scan(&raw)
	if err != nil {
		writeInvalidAuthState(w)
		return
	}
	var payload map[string]json.RawMessage
	var parsed openAppPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if json.Unmarshal(raw, &payload) != nil || len(payload) != 2 || dec.Decode(&parsed) != nil || parsed.UserID == "" {
		writeInvalidAuthState(w)
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeInvalidAuthState(w)
		return
	}
	var disabled bool
	var epoch int
	err = tx.QueryRow(ctx, `SELECT disabled, auth_epoch FROM users WHERE id=$1 FOR UPDATE`, parsed.UserID).Scan(&disabled, &epoch)
	if err != nil || disabled || epoch != parsed.AuthEpoch {
		if errors.Is(err, pgx.ErrNoRows) || err == nil {
			_ = tx.Commit(ctx)
		}
		writeInvalidAuthState(w)
		return
	}
	issued, err := issueSession(ctx, tx, r, h.cfg, parsed.UserID, epoch)
	if err != nil || tx.Commit(ctx) != nil {
		writeInvalidAuthState(w)
		return
	}
	out := map[string]any{"id": parsed.UserID}
	issued.apply(w, h.cfg, parsed.UserID, out)
	writeJSON(w, http.StatusOK, out)
}
