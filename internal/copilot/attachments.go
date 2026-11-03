// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
)

const maxSessionAttachmentBytes int64 = 20 << 20

type SessionAttachmentObjects interface {
	PutObject(context.Context, string, string, io.Reader, storage.PutOptions) (storage.ObjectMetadata, error)
	GetObject(context.Context, string, string) (io.ReadCloser, storage.ObjectMetadata, error)
}

type SessionAttachmentSessions interface {
	GetScope(context.Context, string, string) (string, *string, error)
	WorkspaceExists(context.Context, string) (bool, error)
}

type SessionAttachmentAuthorizer func(context.Context, string, string, *string) error

type SessionAttachments struct {
	objects   SessionAttachmentObjects
	sessions  SessionAttachmentSessions
	authorize SessionAttachmentAuthorizer
	password  ChatAuthenticator
	feature   ChatFeatureReader
}

type MessageArtifactAttachment struct {
	ContentHash        string
	FileName           string
	MediaType          string
	SizeBytes          int64
	ExtractedTextRunes *int64
}

func NewSessionAttachments(objects SessionAttachmentObjects, sessions SessionAttachmentSessions, authorize SessionAttachmentAuthorizer, authenticator ChatAuthenticator, feature ChatFeatureReader) *SessionAttachments {
	return &SessionAttachments{objects: objects, sessions: sessions, authorize: authorize, password: authenticator, feature: feature}
}

func (s *SessionAttachments) Upload(ctx context.Context, userID, sessionID, fileName, declaredMIME string, source io.Reader) (string, error) {
	attachmentURL, _, err := s.UploadForMessage(ctx, userID, sessionID, fileName, declaredMIME, source)
	return attachmentURL, err
}

func (s *SessionAttachments) UploadForMessage(ctx context.Context, userID, sessionID, fileName, declaredMIME string, source io.Reader) (string, MessageArtifactAttachment, error) {
	return s.upload(ctx, userID, sessionID, "", fileName, declaredMIME, source)
}

func (s *SessionAttachments) upload(ctx context.Context, userID, sessionID, expectedKey, fileName, declaredMIME string, source io.Reader) (string, MessageArtifactAttachment, error) {
	if s == nil || s.objects == nil {
		return "", MessageArtifactAttachment{}, errors.New("Copilot attachments are unavailable")
	}
	workspaceID, _, err := s.authorizeSession(ctx, userID, sessionID, "")
	if err != nil {
		return "", MessageArtifactAttachment{}, err
	}
	if source == nil {
		return "", MessageArtifactAttachment{}, errors.New("Copilot attachment body is required")
	}
	name := safeAttachmentFilename(fileName)
	if strings.TrimSpace(fileName) != "" && name == "" {
		return "", MessageArtifactAttachment{}, errors.New("invalid Copilot attachment filename")
	}
	body, err := io.ReadAll(io.LimitReader(source, maxSessionAttachmentBytes+1))
	if err != nil {
		return "", MessageArtifactAttachment{}, err
	}
	if len(body) == 0 {
		return "", MessageArtifactAttachment{}, errors.New("Copilot attachment body is required")
	}
	if int64(len(body)) > maxSessionAttachmentBytes {
		return "", MessageArtifactAttachment{}, errors.New("Copilot attachment exceeds 20 MiB")
	}
	keyBytes := sha256.Sum256(body)
	key := base64.RawURLEncoding.EncodeToString(keyBytes[:])
	if expectedKey != "" && expectedKey != key {
		return "", MessageArtifactAttachment{}, errors.New("Copilot attachment checksum mismatch")
	}
	contentType, _, sniffErr := mime.ParseMediaType(http.DetectContentType(body))
	if sniffErr != nil || contentType == "" {
		contentType = "application/octet-stream"
	}
	length := int64(len(body))
	objectKey := userID + "/" + workspaceID + "/" + key
	if _, err := s.objects.PutObject(ctx, storage.CopilotScope, objectKey, bytes.NewReader(body), storage.PutOptions{ContentType: contentType, ContentLength: &length}); err != nil {
		return "", MessageArtifactAttachment{}, err
	}
	if _, _, err := s.authorizeSession(ctx, userID, sessionID, workspaceID); err != nil {
		return "", MessageArtifactAttachment{}, err
	}
	values := url.Values{"workspaceId": {workspaceID}}
	if name != "" {
		values.Set("fileName", name)
	}
	attachmentURL := fmt.Sprintf("/api/copilot/chat/%s/attachments/%s?%s", url.PathEscape(sessionID), url.PathEscape(key), values.Encode())
	return attachmentURL, MessageArtifactAttachment{
		ContentHash: key, FileName: name, MediaType: contentType, SizeBytes: length,
		ExtractedTextRunes: extractedArtifactTextRunes(body, name, contentType),
	}, nil
}

func (s *SessionAttachments) LoadForProvider(ctx context.Context, userID, sessionID, workspaceID, attachmentURL string, maxBytes ...int64) ([]byte, storage.ObjectMetadata, error) {
	actualWorkspace, _, err := s.authorizeSession(ctx, userID, sessionID, workspaceID)
	if err != nil {
		return nil, storage.ObjectMetadata{}, err
	}
	parsed, err := url.Parse(attachmentURL)
	if err != nil || parsed.RawQuery == "" || parsed.Query().Get("workspaceId") != actualWorkspace {
		return nil, storage.ObjectMetadata{}, errors.New("invalid Copilot attachment locator")
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) != 6 || segments[0] != "api" || segments[1] != "copilot" || segments[2] != "chat" || segments[3] != sessionID || segments[4] != "attachments" || !validAttachmentKey(segments[5]) {
		return nil, storage.ObjectMetadata{}, errors.New("invalid Copilot attachment locator")
	}
	object, metadata, err := s.objects.GetObject(ctx, storage.CopilotScope, userID+"/"+actualWorkspace+"/"+segments[5])
	if err != nil {
		return nil, storage.ObjectMetadata{}, err
	}
	defer object.Close()
	limit := maxSessionAttachmentBytes
	if len(maxBytes) > 0 && maxBytes[0] > 0 && maxBytes[0] < limit {
		limit = maxBytes[0]
	}
	data, err := io.ReadAll(io.LimitReader(object, limit+1))
	if err != nil {
		return nil, storage.ObjectMetadata{}, err
	}
	if int64(len(data)) > limit {
		return nil, storage.ObjectMetadata{}, errors.New("Copilot attachment exceeds 20 MiB")
	}
	hash := sha256.Sum256(data)
	if base64.RawURLEncoding.EncodeToString(hash[:]) != segments[5] {
		return nil, storage.ObjectMetadata{}, errors.New("Copilot attachment checksum mismatch")
	}
	return data, metadata, nil
}

func (s *SessionAttachments) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.objects == nil || s.sessions == nil || s.password == nil {
		http.Error(w, "Copilot attachments are unavailable", http.StatusServiceUnavailable)
		return
	}
	if s.feature != nil {
		enabled, err := s.feature.CopilotEnabled(r.Context())
		if err != nil || !enabled {
			http.Error(w, "Copilot is disabled", http.StatusServiceUnavailable)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, err := s.password.FromRequest(r.Context(), r)
	if err != nil || principal == nil {
		http.Error(w, "Authentication is required", http.StatusUnauthorized)
		return
	}
	sessionID, key := r.PathValue("sessionId"), r.PathValue("key")
	if sessionID == "" || !validAttachmentKey(key) {
		http.NotFound(w, r)
		return
	}
	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		http.NotFound(w, r)
		return
	}
	actualWorkspace, _, err := s.authorizeSession(r.Context(), principal.User.ID, sessionID, workspaceID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPut {
		attachmentURL, _, err := s.upload(r.Context(), principal.User.ID, sessionID, key, r.URL.Query().Get("fileName"), r.URL.Query().Get("mimeType"), r.Body)
		if err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "20 MiB") {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": attachmentURL})
		return
	}
	attachmentURL := fmt.Sprintf("/api/copilot/chat/%s/attachments/%s?%s", url.PathEscape(sessionID), url.PathEscape(key), r.URL.RawQuery)
	body, metadata, err := s.LoadForProvider(r.Context(), principal.User.ID, sessionID, actualWorkspace, attachmentURL)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", metadata.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	if fileName := safeAttachmentFilename(r.URL.Query().Get("fileName")); fileName != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fileName}))
	}
	_, _ = w.Write(body)
}

func (s *SessionAttachments) authorizeSession(ctx context.Context, userID, sessionID, expectedWorkspaceID string) (string, *string, error) {
	if s == nil || s.sessions == nil || s.authorize == nil || userID == "" || sessionID == "" {
		return "", nil, errors.New("Copilot session not found")
	}
	workspaceID, docID, err := s.sessions.GetScope(ctx, userID, sessionID)
	if err != nil || workspaceID == "" || (expectedWorkspaceID != "" && workspaceID != expectedWorkspaceID) {
		return "", nil, errors.New("Copilot session not found")
	}
	canonical, err := s.sessions.WorkspaceExists(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	if !canonical {
		return "", nil, errors.New("Local workspaces don't support attachments or references.")
	}
	if err := s.authorize(ctx, userID, workspaceID, docID); err != nil {
		return "", nil, err
	}
	return workspaceID, docID, nil
}

func validAttachmentKey(key string) bool {
	if len(key) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(key)
	return err == nil
}

func safeAttachmentFilename(value string) string {
	name := filepath.Base(strings.TrimSpace(value))
	if name == "." || name == "" || len(name) > 255 || strings.ContainsAny(name, "\r\n\x00") {
		return ""
	}
	return name
}
