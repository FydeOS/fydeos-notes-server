// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package commentattachments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type DocumentAuthorizer interface {
	AuthorizeDocument(context.Context, string, string, string) (workspaces.DocumentAuthorization, error)
}

type AttachmentReader interface {
	GetCommentAttachment(context.Context, string, string, string) (io.ReadCloser, storage.ObjectMetadata, error)
}

type Handler struct {
	authenticator auth.RequestAuthenticator
	authorizer    DocumentAuthorizer
	attachments   AttachmentReader
}

func NewHandler(authenticator auth.RequestAuthenticator, authorizer DocumentAuthorizer, attachments AttachmentReader) http.Handler {
	return &Handler{authenticator: authenticator, authorizer: authorizer, attachments: attachments}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.authenticator == nil || h.authorizer == nil || h.attachments == nil {
		writeUserError(w, http.StatusInternalServerError, "internal_server_error", "internal_server_error", "An internal error occurred.", nil)
		return
	}
	principal, err := auth.AuthenticateRequest(r.Context(), w, r, h.authenticator)
	if err != nil && !errors.Is(err, auth.ErrNoSession) {
		if auth.WriteAccessTokenError(w, err) {
			return
		}
		writeUserError(w, http.StatusInternalServerError, "internal_server_error", "internal_server_error", "An internal error occurred.", nil)
		return
	}
	if principal == nil {
		writeUserError(w, http.StatusUnauthorized, "authentication_required", "authentication_required", "You must sign in first to access this resource.", nil)
		return
	}

	workspaceID, docID, key := r.PathValue("id"), r.PathValue("docId"), r.PathValue("key")
	access, err := h.authorizer.AuthorizeDocument(r.Context(), principal.User.ID, workspaceID, docID)
	if errors.Is(err, workspaces.ErrNotFound) {
		writeUserError(w, http.StatusNotFound, "space_not_found", "resource_not_found", "Space "+workspaceID+" not found.", map[string]string{"spaceId": workspaceID})
		return
	}
	if err != nil {
		writeUserError(w, http.StatusInternalServerError, "internal_server_error", "internal_server_error", "An internal error occurred.", nil)
		return
	}
	if !access.Read {
		writeUserError(w, http.StatusForbidden, "doc_action_denied", "no_permission", "You do not have permission to perform Doc.Read action on doc "+docID+".", map[string]string{"spaceId": workspaceID, "docId": docID, "action": "Doc.Read"})
		return
	}

	body, metadata, err := h.attachments.GetCommentAttachment(r.Context(), workspaceID, docID, key)
	if errors.Is(err, fs.ErrNotExist) {
		writeUserError(w, http.StatusNotFound, "comment_attachment_not_found", "resource_not_found", "Comment attachment not found.", nil)
		return
	}
	if err != nil {
		writeUserError(w, http.StatusInternalServerError, "internal_server_error", "internal_server_error", "An internal error occurred.", nil)
		return
	}
	defer body.Close()

	contentType := metadata.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconvFormatInt(metadata.ContentLength))
	if !metadata.LastModified.IsZero() {
		w.Header().Set("Last-Modified", metadata.LastModified.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	applyAttachmentHeaders(w.Header(), contentType, key)
	_, _ = io.Copy(w, body)
}

func applyAttachmentHeaders(headers http.Header, contentType, filename string) {
	headers.Set("X-Content-Type-Options", "nosniff")
	lower := strings.ToLower(contentType)
	for _, prefix := range []string{"text/html", "application/xhtml+xml", "image/svg+xml", "application/xml", "text/xml", "text/javascript"} {
		if strings.HasPrefix(lower, prefix) {
			safeName := safeFilename(filename)
			headers.Set("Content-Disposition", "attachment; filename=\""+safeName+"\"; filename*=UTF-8''"+url.PathEscape(safeName))
			return
		}
	}
}

func safeFilename(filename string) string {
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

func writeUserError(w http.ResponseWriter, status int, name, errorType, message string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(userErrorResponse{
		Status:  status,
		Code:    http.StatusText(status),
		Type:    strings.ToUpper(errorType),
		Name:    strings.ToUpper(name),
		Message: message,
		Data:    data,
	})
}

type userErrorResponse struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func strconvFormatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
