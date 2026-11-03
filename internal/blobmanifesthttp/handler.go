// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobmanifesthttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/blobs"
	"github.com/fydeos/fydeos-notes-server/internal/documentidentity"
)

type ManifestReader interface {
	DocManifest(context.Context, string, blobs.BlobSource) (blobs.BlobManifest, error)
	ReadableWorkspaceManifest(context.Context, string, string, *string, *int) (blobs.BlobManifest, error)
}

type BlobOpener interface {
	OpenBlobV1(context.Context, string, blobs.BlobSource, string) (io.ReadCloser, blobs.BlobReadMetadata, error)
}

type Handler struct {
	authenticator auth.RequestAuthenticator
	reader        ManifestReader
}

func NewHandler(authenticator auth.RequestAuthenticator, reader ManifestReader) *Handler {
	return &Handler{authenticator: authenticator, reader: reader}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("name") != "" {
		h.serveBlobRead(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/readable-blob-manifest/v1") {
		h.serveReadableWorkspaceManifest(w, r)
		return
	}
	h.serveDocManifest(w, r)
}

func (h *Handler) serveDocManifest(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	workspaceID := r.PathValue("id")
	identity, source, ok := parseBlobSource(w, r, workspaceID)
	if !ok {
		return
	}
	actorID := ""
	if principal != nil {
		actorID = principal.User.ID
	}
	manifest, err := h.reader.DocManifest(r.Context(), actorID, source)
	if err != nil {
		writeManifestError(w, err, identity.WorkspaceID, identity.DocumentID, source.Type)
		return
	}
	writeManifest(w, manifest)
}

func (h *Handler) serveBlobRead(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	opener, ok := h.reader.(BlobOpener)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return
	}
	workspaceID := r.PathValue("id")
	identity, source, ok := parseBlobSource(w, r, workspaceID)
	if !ok {
		return
	}
	actorID := ""
	if principal != nil {
		actorID = principal.User.ID
	}
	body, metadata, err := opener.OpenBlobV1(r.Context(), actorID, source, r.PathValue("name"))
	if err != nil {
		writeBlobReadError(w, err, identity.WorkspaceID, identity.DocumentID, source.Type, r.PathValue("name"))
		return
	}
	defer body.Close()
	contentType := metadata.MIME
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(metadata.Size, 10))
	if !metadata.LastModified.IsZero() {
		w.Header().Set("Last-Modified", metadata.LastModified.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	applyAttachHeaders(w.Header(), contentType, r.PathValue("name"))
	_, _ = io.Copy(w, body)
}

func parseBlobSource(w http.ResponseWriter, r *http.Request, workspaceID string) (documentidentity.Identity, blobs.BlobSource, bool) {
	docID := r.URL.Query().Get("docId")
	if docID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid blob source.", nil)
		return documentidentity.Identity{}, blobs.BlobSource{}, false
	}
	identity, err := documentidentity.Canonicalize(docID, workspaceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid blob source.", nil)
		return documentidentity.Identity{}, blobs.BlobSource{}, false
	}
	source := blobs.BlobSource{Type: r.URL.Query().Get("sourceType"), WorkspaceID: identity.WorkspaceID, DocID: identity.DocumentID}
	if source.Type == "history" {
		rawTimestamp := r.URL.Query().Get("timestampMs")
		_, hasTimestamp := r.URL.Query()["timestampMs"]
		timestamp, valid := parseSafeInteger(rawTimestamp)
		if !hasTimestamp {
			valid = false
		}
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid_history_timestamp", "Invalid history timestamp.", map[string]string{"timestamp": rawTimestamp})
			return documentidentity.Identity{}, blobs.BlobSource{}, false
		}
		source.TimestampMS = &timestamp
	}
	return identity, source, true
}

func (h *Handler) serveReadableWorkspaceManifest(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if principal == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required.", nil)
		return
	}
	query := r.URL.Query()
	var cursor *string
	if values, exists := query["cursor"]; exists && len(values) > 0 {
		value := values[0]
		cursor = &value
	}
	var limit *int
	if raw := query.Get("limit"); hasQueryValue(query, "limit") {
		if parsed, valid := parseIntegerQuery(raw); valid {
			limit = &parsed
		}
	}
	workspaceID := r.PathValue("id")
	manifest, err := h.reader.ReadableWorkspaceManifest(r.Context(), principal.User.ID, workspaceID, cursor, limit)
	if err != nil {
		writeManifestError(w, err, workspaceID, "", "workspace")
		return
	}
	writeManifest(w, manifest)
}

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	if h.authenticator == nil || h.reader == nil {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return nil, false
	}
	principal, err := auth.AuthenticateRequest(r.Context(), w, r, h.authenticator)
	if err == nil || errors.Is(err, auth.ErrNoSession) {
		return principal, true
	}
	if auth.WriteAccessTokenError(w, err) {
		return nil, false
	}
	writeError(w, http.StatusServiceUnavailable, "internal_server_error", "Authentication is unavailable.", nil)
	return nil, false
}

func writeManifest(w http.ResponseWriter, manifest blobs.BlobManifest) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(manifest)
}

func writeManifestError(w http.ResponseWriter, err error, workspaceID, docID, sourceType string) {
	switch {
	case errors.Is(err, blobs.ErrManifestInvalidInput), errors.Is(err, blobs.ErrManifestCursor):
		name := "bad_request"
		if errors.Is(err, blobs.ErrManifestCursor) {
			name = "blob_manifest_cursor_invalid"
		}
		writeError(w, http.StatusBadRequest, name, "Invalid blob manifest request.", nil)
	case errors.Is(err, blobs.ErrManifestNotFound):
		writeError(w, http.StatusNotFound, "blob_source_not_found", "Blob source not found.", map[string]string{"spaceId": workspaceID, "docId": docID})
	case errors.Is(err, blobs.ErrManifestAccessDenied):
		if docID == workspaceID {
			writeError(w, http.StatusForbidden, "space_access_denied", "You do not have permission to access Space "+workspaceID+".", map[string]string{"spaceId": workspaceID})
			return
		}
		action := "Doc.Read"
		if sourceType == "history" {
			action = "Doc.History.Read"
		}
		writeError(w, http.StatusForbidden, "doc_action_denied", "You do not have permission to perform "+action+" action on doc "+docID+".", map[string]string{"spaceId": workspaceID, "docId": docID, "action": action})
	case errors.Is(err, blobs.ErrManifestWorkspaceSyncDenied):
		writeError(w, http.StatusForbidden, "workspace_sync_denied", "You do not have permission to sync Space "+workspaceID+".", map[string]string{"spaceId": workspaceID, "action": "Workspace.Sync"})
	default:
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
	}
}

func writeBlobReadError(w http.ResponseWriter, err error, workspaceID, docID, sourceType, key string) {
	switch {
	case errors.Is(err, blobs.ErrManifestInvalidInput), errors.Is(err, blobs.ErrManifestBlobNotReferenced), errors.Is(err, blobs.ErrManifestBlobNotCompleted):
		writeError(w, http.StatusBadRequest, "blob_invalid", "Blob cannot be read from this source.", nil)
	case errors.Is(err, blobs.ErrManifestNotFound):
		writeError(w, http.StatusNotFound, "blob_source_not_found", "Blob source not found.", map[string]string{"spaceId": workspaceID, "docId": docID})
	case errors.Is(err, blobs.ErrManifestBlobObjectNotFound), errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, "blob_not_found", "Blob not found.", map[string]string{"spaceId": workspaceID, "blobId": key})
	case errors.Is(err, blobs.ErrManifestAccessDenied):
		if docID == workspaceID {
			writeError(w, http.StatusForbidden, "space_access_denied", "You do not have permission to access Space "+workspaceID+".", map[string]string{"spaceId": workspaceID})
			return
		}
		action := "Doc.Read"
		if sourceType == "history" {
			action = "Doc.History.Read"
		}
		writeError(w, http.StatusForbidden, "doc_action_denied", "You do not have permission to perform "+action+" action on doc "+docID+".", map[string]string{"spaceId": workspaceID, "docId": docID, "action": action})
	default:
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
	}
}

func applyAttachHeaders(headers http.Header, contentType, filename string) {
	headers.Set("X-Content-Type-Options", "nosniff")
	lower := strings.ToLower(contentType)
	for _, prefix := range []string{"text/html", "application/xhtml+xml", "image/svg+xml", "application/xml", "text/xml", "text/javascript"} {
		if strings.HasPrefix(lower, prefix) {
			safeName := safeBlobFilename(filename)
			headers.Set("Content-Disposition", "attachment; filename=\""+safeName+"\"; filename*=UTF-8''"+url.PathEscape(safeName))
			return
		}
	}
}

func safeBlobFilename(filename string) string {
	var safe strings.Builder
	for _, character := range filename {
		if character == '\r' || character == '\n' {
			continue
		}
		allowed := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' || character == ' '
		if allowed {
			safe.WriteRune(character)
		} else {
			safe.WriteByte('_')
		}
	}
	if safe.Len() == 0 {
		return "download"
	}
	return safe.String()
}

type userErrorResponse struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func writeError(w http.ResponseWriter, status int, name, message string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(userErrorResponse{Status: status, Code: http.StatusText(status), Type: strings.ToUpper(errorType(status)), Name: strings.ToUpper(name), Message: message, Data: data})
}

func errorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusForbidden:
		return "no_permission"
	case http.StatusNotFound:
		return "resource_not_found"
	case http.StatusUnauthorized:
		return "authentication_required"
	default:
		return "internal_server_error"
	}
}

func parseSafeInteger(raw string) (int64, bool) {
	value, valid := parseNumber(raw)
	if !valid || math.Trunc(value) != value || math.Abs(value) > 9_007_199_254_740_991 {
		return 0, false
	}
	return int64(value), true
}

func parseIntegerQuery(raw string) (int, bool) {
	value, valid := parseNumber(raw)
	if !valid || math.Trunc(value) != value {
		return 0, false
	}
	if value > 100 {
		return 101, true
	}
	if value < 1 {
		return -1, true
	}
	return int(value), true
}

func parseNumber(raw string) (float64, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, true
	}
	if len(value) > 2 && value[0] == '0' {
		base := 0
		switch value[1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
		if base != 0 {
			parsed, err := strconv.ParseUint(value[2:], base, 64)
			if err != nil {
				return 0, false
			}
			return float64(parsed), true
		}
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

func hasQueryValue(query map[string][]string, key string) bool {
	_, exists := query[key]
	return exists
}
