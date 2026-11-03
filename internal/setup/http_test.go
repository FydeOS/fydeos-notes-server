// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fydeos/fydeos-notes-server/internal/database/migrations"
	"github.com/fydeos/fydeos-notes-server/internal/password"
)

func fixedLimits(context.Context) (int, int, error) { return 8, 32, nil }

func serve(t *testing.T, handler http.Handler, method, contentType, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, "/api/setup/create-admin-user", strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var decoded map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response, decoded
}

func TestHandlerRejectsInvalidRequestsBeforeTouchingTheDatabase(t *testing.T) {
	handler := NewHTTPHandler(nil, fixedLimits)
	for _, test := range []struct {
		name, method, contentType, body string
		status                          int
		code                            string
	}{
		{"wrong method", http.MethodGet, "", "", http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED"},
		{"form body", http.MethodPost, "application/x-www-form-urlencoded", "email=a", http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"},
		{"text body", http.MethodPost, "text/plain", "{}", http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"},
		{"malformed JSON", http.MethodPost, "application/json", "{", http.StatusBadRequest, "BAD_REQUEST"},
		{"invalid email", http.MethodPost, "application/json", `{"email":"not-an-email","password":"long-enough"}`, http.StatusBadRequest, "INVALID_EMAIL"},
		{"display-name email", http.MethodPost, "application/json", `{"email":"Admin <a@example.test>","password":"long-enough"}`, http.StatusBadRequest, "INVALID_EMAIL"},
		{"short password", http.MethodPost, "application/json", `{"email":"a@example.test","password":"short"}`, http.StatusBadRequest, "INVALID_PASSWORD_LENGTH"},
		{"long password", http.MethodPost, "application/json", `{"email":"a@example.test","password":"` + strings.Repeat("x", 33) + `"}`, http.StatusBadRequest, "INVALID_PASSWORD_LENGTH"},
		{"long name", http.MethodPost, "application/json", `{"name":"` + strings.Repeat("n", 65) + `","email":"a@example.test","password":"long-enough"}`, http.StatusBadRequest, "INVALID_NAME"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, body := serve(t, handler, test.method, test.contentType, test.body)
			if response.Code != test.status || body["code"] != test.code || body["message"] == "" {
				t.Fatalf("response = %d %v", response.Code, body)
			}
		})
	}
}

func TestCreateFirstAdminOnlyOnce(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	handler := NewHTTPHandler(pool, fixedLimits)

	// Concurrent requests: exactly one may win.
	var wg sync.WaitGroup
	codes := make(chan int, 4)
	for index := range 4 {
		wg.Go(func() {
			response, _ := serve(t, handler, http.MethodPost, "application/json",
				`{"name":"Admin","email":"admin`+string(rune('a'+index))+`@example.test","password":"correct-horse-1"}`)
			codes <- response.Code
		})
	}
	wg.Wait()
	close(codes)
	created, forbidden := 0, 0
	for code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusForbidden:
			forbidden++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 1 || forbidden != 3 {
		t.Fatalf("created=%d forbidden=%d", created, forbidden)
	}

	var userID, hash string
	var admin bool
	if err := pool.QueryRow(ctx, `SELECT id,password,EXISTS(SELECT 1 FROM user_features f WHERE f.user_id=u.id AND f.name='administrator' AND f.activated)
		FROM users u`).Scan(&userID, &hash, &admin); err != nil {
		t.Fatal(err)
	}
	if !admin || !password.Verify(hash, "correct-horse-1") {
		t.Fatalf("first user admin=%t password verified=%t", admin, password.Verify(hash, "correct-horse-1"))
	}
}

func TestDefaultNameComesFromEmail(t *testing.T) {
	pool := setupTestPool(t)
	response, body := serve(t, NewHTTPHandler(pool, fixedLimits), http.MethodPost, "application/json; charset=utf-8",
		`{"email":"owner@example.test","password":"correct-horse-1"}`)
	if response.Code != http.StatusCreated || body["name"] != "owner" || body["email"] != "owner@example.test" || body["id"] == "" {
		t.Fatalf("response = %d %v", response.Code, body)
	}
}

func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if host := parsed.Hostname(); host != "localhost" && host != "127.0.0.1" && host != "::1" {
		t.Fatal("integration tests require a loopback PostgreSQL host")
	}
	if strings.Trim(parsed.Path, "/") != "notes_test" {
		t.Fatal("integration tests require a disposable database named notes_test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "notes_setup_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}
