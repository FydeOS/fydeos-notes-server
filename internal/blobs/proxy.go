// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
)

const maxSafeInteger = 1<<53 - 1

// proxyError is a client-visible 400 error.
type proxyError string

func (e proxyError) Error() string { return string(e) }

// ProxyUploadHandler serves server-proxied signed uploads.
func (s *Service) ProxyUploadHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT "+proxyUploadPath, func(w http.ResponseWriter, r *http.Request) {
		writeProxyResult(w, "", s.proxyUpload(r))
	})
	mux.HandleFunc("PUT "+proxyMultipartPath, func(w http.ResponseWriter, r *http.Request) {
		etag, err := s.proxyMultipart(r)
		writeProxyResult(w, etag, err)
	})
	return mux
}

func writeProxyResult(w http.ResponseWriter, etag string, err error) {
	if err == nil {
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	status, message := http.StatusInternalServerError, "Upload failed"
	var clientErr proxyError
	if errors.As(err, &clientErr) {
		status, message = http.StatusBadRequest, string(clientErr)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

func queryString(r *http.Request, name string) (string, error) {
	values, ok := r.URL.Query()[name]
	if !ok || len(values) != 1 || values[0] == "" {
		return "", proxyError("Missing " + name + ".")
	}
	return values[0], nil
}

func queryInt(r *http.Request, name string) (int64, error) {
	raw, err := queryString(r, name)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || math.Abs(value) > maxSafeInteger {
		return 0, proxyError("Invalid " + name + ".")
	}
	return int64(value), nil
}

func (s *Service) proxyUpload(r *http.Request) error {
	var workspaceID, key, contentType string
	var length, expiresAt int64
	var err error
	for _, step := range []func() error{
		func() error { workspaceID, err = queryString(r, "workspaceId"); return err },
		func() error { key, err = queryString(r, "key"); return err },
		func() error { contentType, err = queryString(r, "contentType"); return err },
		func() error { length, err = queryInt(r, "contentLength"); return err },
		func() error { expiresAt, err = queryInt(r, "expiresAt"); return err },
		func() error { _, err = queryString(r, "token"); return err },
	} {
		if err := step(); err != nil {
			return err
		}
	}
	token := r.URL.Query().Get("token")
	if length < 0 {
		return proxyError("Invalid content length")
	}
	if values := r.Header.Values("Content-Length"); len(values) > 0 {
		if len(values) > 1 {
			return proxyError("Invalid Content-Length header")
		}
		header, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || header < 0 {
			return proxyError("Invalid Content-Length header")
		}
		if header != length {
			return proxyError("Content length mismatch")
		}
	}
	if err := s.checkProxyAccess(proxyUploadPath, []any{workspaceID, key, contentType, length}, expiresAt, token); err != nil {
		return err
	}
	row, err := loadBlobRow(r.Context(), s.pool, workspaceID, key, false)
	if err != nil {
		return err
	}
	if row == nil {
		return proxyError("Blob upload is not initialized")
	}
	if row.status == "completed" || row.deleted {
		return proxyError("Blob upload is already completed")
	}
	if row.size != length {
		return proxyError("Content length does not match upload metadata")
	}
	if row.mime != "" && contentType != "" && row.mime != contentType {
		return proxyError("Mime type mismatch")
	}
	data, err := readExact(r.Body, length)
	if err != nil {
		return err
	}
	if _, err := s.objects.PutObject(r.Context(), storage.BlobScope, blobStagingKey(workspaceID, row.reservationID, key),
		bytes.NewReader(data), storage.PutOptions{ContentType: row.mime, ContentLength: &length}); err != nil {
		return proxyError("Upload failed")
	}
	return nil
}

func (s *Service) proxyMultipart(r *http.Request) (string, error) {
	var workspaceID, key, uploadID string
	var partNumber, length, expiresAt int64
	var err error
	for _, step := range []func() error{
		func() error { workspaceID, err = queryString(r, "workspaceId"); return err },
		func() error { key, err = queryString(r, "key"); return err },
		func() error { uploadID, err = queryString(r, "uploadId"); return err },
		func() error { partNumber, err = queryInt(r, "partNumber"); return err },
		func() error { length, err = queryInt(r, "contentLength"); return err },
		func() error { expiresAt, err = queryInt(r, "expiresAt"); return err },
		func() error { _, err = queryString(r, "token"); return err },
	} {
		if err := step(); err != nil {
			return "", err
		}
	}
	token := r.URL.Query().Get("token")
	if length < 1 {
		return "", proxyError("Invalid content length")
	}
	if partNumber < 1 || partNumber > maxPartNumber {
		return "", proxyError("Invalid part number")
	}
	values := r.Header.Values("Content-Length")
	if len(values) == 0 && r.ContentLength > 0 {
		values = []string{strconv.FormatInt(r.ContentLength, 10)}
	}
	if len(values) != 1 {
		return "", proxyError("Missing Content-Length header")
	}
	header, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || header <= 0 {
		return "", proxyError("Missing Content-Length header")
	}
	if header != length {
		return "", proxyError("Content length mismatch")
	}
	if err := s.checkProxyAccess(proxyMultipartPath, []any{workspaceID, key, uploadID, partNumber, length}, expiresAt, token); err != nil {
		return "", err
	}
	row, err := loadBlobRow(r.Context(), s.pool, workspaceID, key, false)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", proxyError("Multipart upload is not initialized")
	}
	if row.status == "completed" || row.deleted {
		return "", proxyError("Blob upload is already completed")
	}
	if row.uploadID == nil || *row.uploadID != uploadID {
		return "", proxyError("Upload id mismatch")
	}
	offset := (partNumber - 1) * multipartPart
	if length > multipartPart || offset+length > row.size {
		return "", proxyError("Part size exceeds upload metadata")
	}
	data, err := readExact(r.Body, length)
	if err != nil {
		return "", err
	}
	etag, err := s.objects.UploadPart(r.Context(), storage.BlobScope, blobStagingKey(workspaceID, row.reservationID, key),
		uploadID, int32(partNumber), bytes.NewReader(data), length)
	if err != nil {
		return "", proxyError("Upload failed")
	}
	return etag, nil
}

func (s *Service) checkProxyAccess(path string, fields []any, expiresAt int64, token string) error {
	settings, ok := s.objects.UploadURLSettings(storage.BlobScope)
	if !ok || !settings.ProxyUpload {
		return proxyError("Invalid endpoint")
	}
	if expiresAt < time.Now().Unix() {
		return proxyError("Upload URL expired")
	}
	if !s.objects.VerifyUploadToken(storage.BlobScope, path, fields, expiresAt, token) {
		return proxyError("Invalid upload token")
	}
	return nil
}

// readExact reads exactly length bytes, reading at most one extra byte.
func readExact(body io.Reader, length int64) ([]byte, error) {
	if body == nil {
		body = http.NoBody
	}
	data, err := io.ReadAll(io.LimitReader(body, length+1))
	if err != nil || int64(len(data)) != length {
		return nil, proxyError("Content length mismatch")
	}
	return data, nil
}
