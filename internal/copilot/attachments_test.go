// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
)

type attachmentObject struct {
	body     []byte
	metadata storage.ObjectMetadata
}

type attachmentObjects struct{ values map[string]attachmentObject }

func (s *attachmentObjects) PutObject(_ context.Context, _, key string, body io.Reader, options storage.PutOptions) (storage.ObjectMetadata, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectMetadata{}, err
	}
	metadata := storage.ObjectMetadata{ContentType: options.ContentType, ContentLength: int64(len(data))}
	if s.values == nil {
		s.values = make(map[string]attachmentObject)
	}
	s.values[key] = attachmentObject{body: data, metadata: metadata}
	return metadata, nil
}

func (s *attachmentObjects) GetObject(_ context.Context, _, key string) (io.ReadCloser, storage.ObjectMetadata, error) {
	value, ok := s.values[key]
	if !ok {
		return nil, storage.ObjectMetadata{}, errors.New("missing object")
	}
	return io.NopCloser(bytes.NewReader(value.body)), value.metadata, nil
}

func (s *attachmentObjects) DeleteObject(_ context.Context, _, key string) error {
	delete(s.values, key)
	return nil
}

type attachmentSessions struct {
	userID      string
	workspaceID string
	docID       *string
	canonical   bool
}

func (s attachmentSessions) GetScope(_ context.Context, userID, _ string) (string, *string, error) {
	if userID != s.userID {
		return "", nil, ErrSessionNotFound
	}
	return s.workspaceID, s.docID, nil
}

func (s attachmentSessions) WorkspaceExists(context.Context, string) (bool, error) {
	return s.canonical, nil
}

type attachmentAuthenticator struct{ userID string }

func (a attachmentAuthenticator) FromRequest(context.Context, *http.Request) (*auth.Principal, error) {
	if a.userID == "" {
		return nil, auth.ErrNoSession
	}
	return &auth.Principal{User: auth.User{ID: a.userID}}, nil
}

type copilotAttachmentFeature bool

func (f copilotAttachmentFeature) CopilotEnabled(context.Context) (bool, error) { return bool(f), nil }

func TestSessionAttachmentUploadAndPrivateDownload(t *testing.T) {
	objects := &attachmentObjects{}
	sessions := attachmentSessions{userID: "user-1", workspaceID: "workspace-1", canonical: true}
	authorize := func(_ context.Context, userID, workspaceID string, _ *string) error {
		if userID != "user-1" || workspaceID != "workspace-1" {
			return errors.New("denied")
		}
		return nil
	}
	service := NewSessionAttachments(objects, sessions, authorize, attachmentAuthenticator{userID: "user-1"}, nil)
	body := []byte("plain attachment bytes")
	attachmentURL, err := service.Upload(context.Background(), "user-1", "session-1", "notes.txt", "text/plain", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("upload attachment: %v", err)
	}
	parsed, err := url.Parse(attachmentURL)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Split(strings.Trim(parsed.Path, "/"), "/")[5]
	expectedHash := sha256.Sum256(body)
	if key != base64.RawURLEncoding.EncodeToString(expectedHash[:]) {
		t.Fatalf("content key = %q, want hash key", key)
	}
	if !bytes.Equal(objects.values["user-1/workspace-1/"+key].body, body) {
		t.Fatal("stored object bytes differ from upload")
	}

	request := httptest.NewRequest(http.MethodGet, attachmentURL, nil)
	request.SetPathValue("sessionId", "session-1")
	request.SetPathValue("key", key)
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), body) {
		t.Fatalf("download = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "private, no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Content-Disposition") == "" || response.Header().Get("Content-Length") != "22" {
		t.Fatalf("unsafe attachment response headers: %v", response.Header())
	}
}

func TestSessionAttachmentDoesNotTrustDeclaredMIMEForUnknownBytes(t *testing.T) {
	objects := &attachmentObjects{}
	sessions := attachmentSessions{userID: "user-1", workspaceID: "workspace-1", canonical: true}
	service := NewSessionAttachments(objects, sessions, func(context.Context, string, string, *string) error { return nil }, nil, nil)
	if _, err := service.Upload(context.Background(), "user-1", "session-1", "fake.png", "image/png", bytes.NewReader([]byte{1, 2, 3, 4})); err != nil {
		t.Fatal(err)
	}
	for _, object := range objects.values {
		if object.metadata.ContentType != "application/octet-stream" {
			t.Fatalf("unrecognized bytes trusted declared MIME type: %q", object.metadata.ContentType)
		}
	}
}

func TestSessionAttachmentRejectsWrongScopeAndOversizedUpload(t *testing.T) {
	objects := &attachmentObjects{}
	sessions := attachmentSessions{userID: "user-1", workspaceID: "workspace-1", canonical: true}
	authorize := func(_ context.Context, userID, workspaceID string, _ *string) error {
		if userID != "user-1" || workspaceID != "workspace-1" {
			return errors.New("denied")
		}
		return nil
	}
	service := NewSessionAttachments(objects, sessions, authorize, attachmentAuthenticator{userID: "user-2"}, nil)
	if _, err := service.Upload(context.Background(), "user-1", "session-1", "a.txt", "text/plain", strings.NewReader("x")); err != nil {
		t.Fatalf("upload: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/copilot/chat/session-1/attachments/invalid?workspaceId=workspace-1", nil)
	request.SetPathValue("sessionId", "session-1")
	request.SetPathValue("key", "invalid")
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("invalid key status = %d, want 404", response.Code)
	}

	service.password = attachmentAuthenticator{userID: "user-1"}
	request = httptest.NewRequest(http.MethodGet, "/api/copilot/chat/session-1/attachments/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA?workspaceId=other", nil)
	request.SetPathValue("sessionId", "session-1")
	request.SetPathValue("key", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	response = httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace status = %d, want 404", response.Code)
	}

	_, err := service.Upload(context.Background(), "user-1", "session-1", "large.bin", "application/octet-stream", io.LimitReader(zeroReader{}, maxSessionAttachmentBytes+1))
	if err == nil || !strings.Contains(err.Error(), "20 MiB") {
		t.Fatalf("oversized upload error = %v", err)
	}
	if len(objects.values) != 1 {
		t.Fatalf("oversized upload wrote an object; stored objects=%d", len(objects.values))
	}
}

func TestSessionAttachmentPutRequiresMatchingContentHashAndFeature(t *testing.T) {
	objects := &attachmentObjects{}
	sessions := attachmentSessions{userID: "user-1", workspaceID: "workspace-1", canonical: true}
	authorize := func(context.Context, string, string, *string) error { return nil }
	service := NewSessionAttachments(objects, sessions, authorize, attachmentAuthenticator{userID: "user-1"}, copilotAttachmentFeature(true))
	body := []byte("uploaded over the legacy attachment route")
	hash := sha256.Sum256(body)
	key := base64.RawURLEncoding.EncodeToString(hash[:])
	request := httptest.NewRequest(http.MethodPut, "/api/copilot/chat/session-1/attachments/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA?workspaceId=workspace-1", bytes.NewReader(body))
	request.SetPathValue("sessionId", "session-1")
	request.SetPathValue("key", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || len(objects.values) != 0 {
		t.Fatalf("checksum mismatch response=%d stored=%d", response.Code, len(objects.values))
	}

	request = httptest.NewRequest(http.MethodPut, "/api/copilot/chat/session-1/attachments/"+key+"?workspaceId=workspace-1", bytes.NewReader(body))
	request.SetPathValue("sessionId", "session-1")
	request.SetPathValue("key", key)
	response = httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(objects.values) != 1 {
		t.Fatalf("valid upload response=%d stored=%d body=%q", response.Code, len(objects.values), response.Body.String())
	}

	service.feature = copilotAttachmentFeature(false)
	request = httptest.NewRequest(http.MethodGet, "/api/copilot/chat/session-1/attachments/"+key+"?workspaceId=workspace-1", nil)
	request.SetPathValue("sessionId", "session-1")
	request.SetPathValue("key", key)
	response = httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled Copilot attachment status = %d, want 503", response.Code)
	}
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	for i := range buffer {
		buffer[i] = 0
	}
	return len(buffer), nil
}
