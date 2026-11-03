// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package avatars

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
)

type PublicHandler struct {
	service *Service
}

func NewPublicHandler(service *Service) http.Handler {
	return &PublicHandler{service: service}
}

func (h *PublicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.service == nil || !h.service.PublicStorage() {
		writeAvatarError(w, http.StatusForbidden, "ACTION_FORBIDDEN", "ACTION_FORBIDDEN", "Only available when avatar storage provider is fs or assetpack.")
		return
	}
	key := r.PathValue("id")
	body, metadata, err := h.service.Get(r.Context(), key)
	if errors.Is(err, fs.ErrNotExist) {
		writeAvatarError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "USER_AVATAR_NOT_FOUND", "User avatar not found.")
		return
	}
	if err != nil {
		writeAvatarError(w, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "INTERNAL_SERVER_ERROR", "An internal error occurred.")
		return
	}
	defer body.Close()
	contentType := metadata.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", metadata.ContentLength))
	if !metadata.LastModified.IsZero() {
		w.Header().Set("Last-Modified", metadata.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, body)
}

func writeAvatarError(w http.ResponseWriter, status int, errorType, name, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Status  int    `json:"status"`
		Code    string `json:"code"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Message string `json:"message"`
	}{status, http.StatusText(status), errorType, name, message})
}
