// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestRemoveAvatarGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE users SET avatar_url = 'https://storage.example.test/old-avatar.webp' WHERE id = $1`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET avatar_url = 'https://storage.example.test/other-avatar.webp' WHERE id = $1`, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}

	sessionResolver := auth.NewSessionResolver(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	userRepository := users.NewRepository(pool)
	service := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	publisher := &profilePublisherRecorder{}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		sessionResolver,
		service,
		workspaceRepository,
		auth.NewReadLimiter(auth.DefaultReadConcurrency),
		publisher,
		nil,
		nil,
		HandlerDependencies{},
	)
	mutation := `mutation removeAvatar { removeAvatar { success } }`
	result := graphRequest(t, handler, mutation, nil, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || graphData(t, result)["removeAvatar"].(map[string]any)["success"] != true {
		t.Fatalf("removeAvatar result = %#v", result)
	}
	query := `query getCurrentUser { currentUser { id avatarUrl } }`
	current := graphRequest(t, handler, query, nil, fixture.sessionID, fixture.userID)
	currentUser := graphData(t, current)["currentUser"].(map[string]any)
	if currentUser["id"] != fixture.userID || currentUser["avatarUrl"] != nil {
		t.Fatalf("current user after removal = %#v", currentUser)
	}
	var otherAvatar string
	if err := pool.QueryRow(ctx, `SELECT avatar_url FROM users WHERE id = $1`, fixture.sharedUserID).Scan(&otherAvatar); err != nil {
		t.Fatal(err)
	}
	if otherAvatar != "https://storage.example.test/other-avatar.webp" {
		t.Fatalf("another user's avatar changed: %q", otherAvatar)
	}
	if len(publisher.userIDs) != 1 || publisher.userIDs[0] != fixture.userID {
		t.Fatalf("profile change publication = %#v", publisher.userIDs)
	}
	removedAgain := graphRequest(t, handler, mutation, nil, fixture.sessionID, fixture.userID)
	if removedAgain["errors"] != nil || graphData(t, removedAgain)["removeAvatar"].(map[string]any)["success"] != true {
		t.Fatalf("repeated removeAvatar result = %#v", removedAgain)
	}
	if len(publisher.userIDs) != 2 || publisher.userIDs[1] != fixture.userID {
		t.Fatalf("repeated profile change publication = %#v", publisher.userIDs)
	}

	unauthenticated := graphRequest(t, handler, mutation, nil, "", "")
	if !strings.Contains(graphErrors(unauthenticated), ErrAuthenticationRequired.Error()) {
		t.Fatalf("unauthenticated removeAvatar result = %#v", unauthenticated)
	}
	if len(publisher.userIDs) != 2 {
		t.Fatalf("unauthenticated request published profile change: %#v", publisher.userIDs)
	}
}

func TestUploadAvatarGraphQLMultipartCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	userRepository := users.NewRepository(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	uploader := &avatarUploaderFake{user: &users.User{ID: fixture.userID, AvatarURL: stringPointer("https://notes.example.test/api/avatars/new-avatar.webp")}}
	publisher := &avatarPublisherFake{}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool),
		userService,
		workspaceRepository,
		nil,
		publisher,
		nil,
		nil,
		HandlerDependencies{AvatarUploader: uploader},
	)
	request := avatarMultipartRequest(t, fixture.sessionID, fixture.userID)
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("uploadAvatar status = %d, body=%s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode uploadAvatar response: %v", err)
	}
	if result["errors"] != nil {
		t.Fatalf("uploadAvatar errors = %#v", result["errors"])
	}
	got := result["data"].(map[string]any)["uploadAvatar"].(map[string]any)
	if got["id"] != fixture.userID || got["avatarUrl"] != "https://notes.example.test/api/avatars/new-avatar.webp" {
		t.Fatalf("uploadAvatar result = %#v", got)
	}
	if uploader.userID != fixture.userID || uploader.contentType != "image/png" || uploader.body != "png data" {
		t.Fatalf("multipart upload = user %q, type %q, body %q", uploader.userID, uploader.contentType, uploader.body)
	}
	if len(publisher.userIDs) != 1 || publisher.userIDs[0] != fixture.userID {
		t.Fatalf("profile publications = %#v", publisher.userIDs)
	}

	unauthenticated := newDeadlineTestResponseWriter()
	unauthenticatedRequest := avatarMultipartRequest(t, "", "")
	handler.ServeHTTP(unauthenticated, unauthenticatedRequest)
	if !strings.Contains(unauthenticated.Body.String(), ErrAuthenticationRequired.Error()) || uploader.userID != fixture.userID {
		t.Fatalf("unauthenticated upload response = %s; uploader input now user %q", unauthenticated.Body.String(), uploader.userID)
	}
}

func TestSetBlobGraphQLMultipartClientCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	uploader := &fakeWorkspaceBlobService{
		fakeBlobManager:  &fakeBlobManager{},
		fakeBlobUploader: &fakeBlobUploader{},
	}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "cloud", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool),
		nil,
		workspaces.NewRepositoryWithDeployment(pool, "cloud"),
		nil,
		nil,
		nil,
		uploader,
		HandlerDependencies{},
	)
	body := []byte("client-compatible GraphQL multipart blob")
	request := clientSetBlobMultipartRequest(t, fixture.sessionID, fixture.userID, fixture.workspaceID, "blob-key", body, "text/plain")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("setBlob status = %d, body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("setBlob Content-Type = %q, want application/json; charset=utf-8", got)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode setBlob response: %v: %s", err, response.Body.String())
	}
	data, ok := result["data"].(map[string]any)
	if result["errors"] != nil || !ok || data["setBlob"] != "blob-key" {
		t.Fatalf("setBlob response = %#v", result)
	}
	if uploader.setActorID != fixture.userID || uploader.setWorkspace != fixture.workspaceID || uploader.setKey != "blob-key" || uploader.setMIME != "text/plain" || !bytes.Equal(uploader.setBody, body) {
		t.Fatalf("setBlob args actor=%q workspace=%q key=%q mime=%q body=%q", uploader.setActorID, uploader.setWorkspace, uploader.setKey, uploader.setMIME, uploader.setBody)
	}
}

func clientSetBlobMultipartRequest(t *testing.T, sessionID, userID, workspaceID, key string, body []byte, contentType string) *http.Request {
	t.Helper()
	requestBody := &bytes.Buffer{}
	writer := multipart.NewWriter(requestBody)
	operations := map[string]any{
		"name": "setBlob",
		"query": `mutation setBlob($workspaceId: String!, $blob: Upload!) {
  setBlob(workspaceId: $workspaceId, blob: $blob)
}`,
		"variables": map[string]any{"workspaceId": workspaceID, "blob": nil},
	}
	encodedOperations, err := json.Marshal(operations)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("operations", string(encodedOperations)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("map", `{"0":["variables.blob"]}`); err != nil {
		t.Fatal(err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="0"; filename="`+key+`"`)
	header.Set("Content-Type", contentType)
	file, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", requestBody)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("x-operation-name", "setBlob")
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sessionID})
	request.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: userID})
	return request
}

func avatarMultipartRequest(t *testing.T, sessionID, userID string) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	operations := `{"query":"mutation UploadAvatar($avatar: Upload!) { uploadAvatar(avatar: $avatar) { id avatarUrl } }","variables":{"avatar":null}}`
	if err := writer.WriteField("operations", operations); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("map", `{"0":["variables.avatar"]}`); err != nil {
		t.Fatal(err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="0"; filename="avatar.png"`)
	header.Set("Content-Type", "image/png")
	file, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("png data")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("x-operation-name", "UploadAvatar")
	if sessionID != "" {
		request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sessionID})
		request.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: userID})
	}
	return request
}
