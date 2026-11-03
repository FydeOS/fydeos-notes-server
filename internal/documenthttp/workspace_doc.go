// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documenthttp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/crdt/yjs"
	"github.com/fydeos/fydeos-notes-server/internal/documentidentity"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type DocumentAuthorizer interface {
	AuthorizeDocument(context.Context, string, string, string) (workspaces.DocumentAuthorization, error)
}

type WorkspaceDocumentReader interface {
	DiffWorkspace(context.Context, string, string, []byte) ([]byte, []byte, int64, error)
}

type DocumentModeReader interface {
	ReadDocumentMode(context.Context, string, string) (int, error)
}

type WorkspaceReadAuthorizer interface {
	ReadWorkspace(context.Context, string, string) (workspaces.Workspace, error)
}

type PublicDocumentsReader interface {
	ReadPublicDocuments(context.Context, string) ([]workspaces.DocumentInfo, error)
}

type WorkspaceDocumentReadService interface {
	WorkspaceReadAuthorizer
	PublicDocumentsReader
}

type DocumentViewRecorder interface {
	RecordDocView(context.Context, string, string, string, bool, string, time.Time) error
}

type Handler struct {
	authenticator auth.RequestAuthenticator
	authorizer    DocumentAuthorizer
	documents     WorkspaceDocumentReader
	modes         DocumentModeReader
	workspaces    WorkspaceDocumentReadService
	analytics     DocumentViewRecorder
	viewSlots     chan struct{}
}

func NewHandler(authenticator auth.RequestAuthenticator, authorizer DocumentAuthorizer, reader WorkspaceDocumentReader, modes DocumentModeReader, workspaceReader WorkspaceDocumentReadService, analytics DocumentViewRecorder) *Handler {
	return &Handler{
		authenticator: authenticator, authorizer: authorizer, documents: reader, modes: modes,
		workspaces: workspaceReader, analytics: analytics, viewSlots: make(chan struct{}, 32),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.authorizer == nil || h.documents == nil || h.modes == nil || h.workspaces == nil {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return
	}
	principal, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	identity, err := documentidentity.Canonicalize(r.PathValue("docId"), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return
	}
	workspaceID, docID := identity.WorkspaceID, identity.DocumentID
	actor := "anonymous"
	userID := ""
	if principal != nil {
		actor = principal.User.ID
		userID = principal.User.ID
	}
	if docID == workspaceID {
		if _, err := h.workspaces.ReadWorkspace(r.Context(), actor, workspaceID); err != nil {
			writeWorkspaceReadError(w, workspaceID, err)
			return
		}
	} else {
		access, err := h.authorizer.AuthorizeDocument(r.Context(), actor, workspaceID, docID)
		if errors.Is(err, workspaces.ErrNotFound) {
			writeError(w, http.StatusNotFound, "space_not_found", "Space "+workspaceID+" not found.", map[string]string{"spaceId": workspaceID})
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
			return
		}
		if !access.Read {
			writeError(w, http.StatusForbidden, "doc_action_denied", "You do not have permission to perform Doc.Read action on doc "+docID+".", map[string]string{"spaceId": workspaceID, "docId": docID, "action": "Doc.Read"})
			return
		}
	}
	if docID != workspaceID {
		mode, err := h.modes.ReadDocumentMode(r.Context(), workspaceID, docID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
			return
		}
		w.Header().Set("publish-mode", publishMode(mode))
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	content, _, _, err := h.documents.DiffWorkspace(r.Context(), workspaceID, docID, nil)
	if errors.Is(err, documents.ErrWorkspaceDocNotFound) {
		writeError(w, http.StatusNotFound, "doc_not_found", "Doc "+docID+" under Space "+workspaceID+" not found.", map[string]string{"spaceId": workspaceID, "docId": docID})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return
	}
	if h.analytics != nil && docID != workspaceID {
		job := docViewJob{
			workspace: workspaceID, doc: docID,
			visitor: docVisitorID(r, principal, workspaceID, docID),
			guest:   principal == nil, user: userID, viewedAt: time.Now().UTC(),
		}
		select {
		case h.viewSlots <- struct{}{}:
			go h.recordDocView(job)
		default:
			slog.Warn("dropping document view analytics because recorder is saturated", "workspace_id", workspaceID, "doc_id", docID)
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (h *Handler) PublicRootDocHandler() http.Handler {
	return http.HandlerFunc(h.ServePublicRootDoc)
}

func (h *Handler) ServePublicRootDoc(w http.ResponseWriter, r *http.Request) {
	if h.authorizer == nil || h.documents == nil {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return
	}
	if h.workspaces == nil {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return
	}
	principal, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	workspaceID, requestedDocID := r.PathValue("id"), r.PathValue("docId")
	actor := "anonymous"
	if principal != nil {
		actor = principal.User.ID
	}
	access, err := h.authorizer.AuthorizeDocument(r.Context(), actor, workspaceID, requestedDocID)
	if errors.Is(err, workspaces.ErrNotFound) {
		writeError(w, http.StatusNotFound, "space_not_found", "Space "+workspaceID+" not found.", map[string]string{"spaceId": workspaceID})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return
	}
	if !access.Read {
		writeError(w, http.StatusForbidden, "doc_action_denied", "You do not have permission to perform Doc.Read action on doc "+requestedDocID+".", map[string]string{"spaceId": workspaceID, "docId": requestedDocID, "action": "Doc.Read"})
		return
	}
	rootUpdate, _, _, err := h.documents.DiffWorkspace(r.Context(), workspaceID, workspaceID, nil)
	if errors.Is(err, documents.ErrWorkspaceDocNotFound) {
		writeError(w, http.StatusNotFound, "doc_not_found", "Doc "+workspaceID+" under Space "+workspaceID+" not found.", map[string]string{"spaceId": workspaceID, "docId": workspaceID})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return
	}
	publicDocs, err := h.workspaces.ReadPublicDocuments(r.Context(), workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return
	}
	metas := make([]yjs.PublicRootDocMeta, 0, len(publicDocs))
	for _, doc := range publicDocs {
		metas = append(metas, yjs.PublicRootDocMeta{ID: doc.DocID, Title: doc.Title})
	}
	content, err := yjs.BuildPublicRootDoc(rootUpdate, metas)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	if h.authenticator == nil {
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

type docViewJob struct {
	workspace string
	doc       string
	visitor   string
	guest     bool
	user      string
	viewedAt  time.Time
}

func (h *Handler) recordDocView(job docViewJob) {
	defer func() { <-h.viewSlots }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.analytics.RecordDocView(ctx, job.workspace, job.doc, job.visitor, job.guest, job.user, job.viewedAt); err != nil {
		slog.WarnContext(ctx, "failed to record document view", "workspace_id", job.workspace, "doc_id", job.doc)
	}
}

func publishMode(mode int) string {
	if mode == 1 {
		return "edgeless"
	}
	return "page"
}

func docVisitorID(r *http.Request, principal *auth.Principal, workspace, doc string) string {
	tracker := ""
	if principal != nil {
		for _, candidate := range []string{principal.SessionID, principal.UserSessionID, principal.AuthSessionID} {
			if candidate != "" {
				tracker = candidate
				break
			}
		}
	}
	if tracker == "" {
		for _, candidate := range []string{r.Header.Get("CF-Connecting-IP"), firstForwardedIP(r.Header.Get("X-Forwarded-For")), r.Header.Get("X-Real-IP"), r.Header.Get("CF-Ray"), remoteIP(r.RemoteAddr)} {
			if value := strings.TrimSpace(candidate); value != "" {
				tracker = value
				break
			}
		}
	}
	digest := sha256.Sum256([]byte(workspace + ":" + doc + ":" + tracker))
	return hex.EncodeToString(digest[:])
}

func firstForwardedIP(value string) string {
	first, _, _ := strings.Cut(value, ",")
	return strings.TrimSpace(first)
}

func remoteIP(value string) string {
	host, _, err := net.SplitHostPort(value)
	if err == nil {
		return host
	}
	return value
}

func writeError(w http.ResponseWriter, status int, name, message string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(userErrorResponse{
		Status: status, Code: http.StatusText(status), Type: strings.ToUpper(errorType(status)),
		Name: strings.ToUpper(name), Message: message, Data: data,
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

func writeWorkspaceReadError(w http.ResponseWriter, workspace string, err error) {
	switch {
	case errors.Is(err, workspaces.ErrNotFound):
		writeError(w, http.StatusNotFound, "space_not_found", "Space "+workspace+" not found.", map[string]string{"spaceId": workspace})
	case errors.Is(err, workspaces.ErrAccessDenied):
		writeError(w, http.StatusForbidden, "space_access_denied", "You do not have permission to access Space "+workspace+".", map[string]string{"spaceId": workspace})
	default:
		writeError(w, http.StatusInternalServerError, "internal_server_error", "An internal error occurred.", nil)
	}
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
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	default:
		return "internal_server_error"
	}
}
