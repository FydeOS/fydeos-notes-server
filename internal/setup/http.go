// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

// Package setup creates the first administrator of a new self-hosted server.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/fydeos/fydeos-notes-server/internal/password"
)

const maxRequestBytes = 64 << 10

// ErrAlreadyInitialized means the server already has at least one user.
var ErrAlreadyInitialized = errors.New("server is already initialized")

// DB is the database access the handler needs.
type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}

// PasswordLimits returns the configured minimum and maximum password length.
type PasswordLimits func(context.Context) (int, int, error)

// Admin is the account created by the setup request.
type Admin struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type handler struct {
	db     DB
	limits PasswordLimits
}

// NewHTTPHandler serves POST /api/setup/create-admin-user.
//
// The request only succeeds while the database has no users, so it can be
// used once, by whoever reaches a new server first. Deploy a new server on a
// private network, or finish setup before exposing it.
//
// No session exists yet, so there is no CSRF token to check. Requiring a JSON
// content type makes browsers send a CORS preflight for cross-site requests,
// which the CORS policy rejects.
func NewHTTPHandler(db DB, limits PasswordLimits) http.Handler {
	return &handler{db: db, limits: limits}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed.")
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "The request body must be JSON.")
		return
	}
	var body struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "The request body is not valid JSON.")
		return
	}

	email := strings.TrimSpace(body.Email)
	if !validEmail(email) {
		writeError(w, http.StatusBadRequest, "INVALID_EMAIL", "Please enter a valid email address.")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	if utf8.RuneCountInString(name) > 64 {
		writeError(w, http.StatusBadRequest, "INVALID_NAME", "The name must be at most 64 characters.")
		return
	}
	minLength, maxLength, err := h.limits(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "The server configuration is unavailable.")
		return
	}
	if length := utf8.RuneCountInString(body.Password); length < minLength || length > maxLength {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD_LENGTH", "The password does not meet the length requirement.")
		return
	}

	admin, err := createFirstAdmin(r.Context(), h.db, name, email, body.Password)
	switch {
	case errors.Is(err, ErrAlreadyInitialized):
		writeError(w, http.StatusForbidden, "ACTION_FORBIDDEN", "The first administrator has already been created.")
		return
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "The administrator could not be created.")
		return
	}
	writeJSON(w, http.StatusCreated, admin)
}

// createFirstAdmin inserts a user with the administrator feature, but only if
// no user exists yet. An advisory lock serializes concurrent setup requests.
func createFirstAdmin(ctx context.Context, db DB, name, email, plainPassword string) (*Admin, error) {
	hash, err := password.Hash(plainPassword)
	if err != nil {
		return nil, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('notes-first-admin-setup',0))`); err != nil {
		return nil, err
	}
	var hasUsers bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users)`).Scan(&hasUsers); err != nil {
		return nil, err
	}
	if hasUsers {
		return nil, ErrAlreadyInitialized
	}
	admin := &Admin{ID: uuid.NewString(), Name: name, Email: email}
	if _, err := tx.Exec(ctx, `INSERT INTO users(id,name,email,password,registered,disabled,created_at)
		VALUES($1,$2,$3,$4,true,false,clock_timestamp())`, admin.ID, admin.Name, admin.Email, hash); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_features(user_id,name,type,reason,activated)
		VALUES($1,'administrator',0,'initial setup',true)`, admin.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return admin, nil
}

func validEmail(value string) bool {
	if value == "" || len(value) > 320 {
		return false
	}
	parsed, err := mail.ParseAddress(value)
	return err == nil && parsed.Address == value && strings.Contains(value, "@")
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"status": status, "code": code, "message": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
