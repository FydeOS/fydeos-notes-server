// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package storage

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

func signUploadToken(signKey, path string, fields []any, expiresAt int64) (string, error) {
	payload, err := uploadTokenPayload(path, fields, expiresAt)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(signKey))
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func verifyUploadToken(signKey, path string, fields []any, expiresAt int64, token string) bool {
	signature, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	want, err := signUploadToken(signKey, path, fields, expiresAt)
	if err != nil {
		return false
	}
	wantBytes, err := base64.RawURLEncoding.DecodeString(want)
	return err == nil && hmac.Equal(signature, wantBytes)
}

func uploadTokenPayload(path string, fields []any, expiresAt int64) ([]byte, error) {
	canonical := make([]any, 0, len(fields)+5)
	canonical = append(canonical, "affine-storage-upload", 1, "PUT", path)
	canonical = append(canonical, fields...)
	canonical = append(canonical, expiresAt)
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(canonical); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(payload.Bytes(), []byte("\n")), nil
}

func BuildProxyUploadURL(prefix, path string, query map[string]string) (string, error) {
	base, err := url.Parse(strings.TrimRight(prefix, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.RawQuery != "" || base.Fragment != "" {
		return "", errors.New("upload URL prefix must be an HTTP(S) URL without query or fragment")
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawPath = ""
	values := base.Query()
	for key, value := range query {
		values.Set(key, value)
	}
	base.RawQuery = values.Encode()
	return base.String(), nil
}

func ApplyPresignedURLPrefix(request PresignedRequest, prefix string) (PresignedRequest, error) {
	if prefix == "" {
		return request, nil
	}
	parsedPrefix, err := url.Parse(prefix)
	if err != nil || (parsedPrefix.Scheme != "http" && parsedPrefix.Scheme != "https") || parsedPrefix.Host == "" || parsedPrefix.User != nil || parsedPrefix.Path != "" && parsedPrefix.Path != "/" || parsedPrefix.RawQuery != "" || parsedPrefix.Fragment != "" {
		return PresignedRequest{}, errors.New("upload URL prefix must contain only an HTTP(S) origin")
	}
	parsedRequest, err := url.Parse(request.URL)
	if err != nil || parsedRequest.Scheme == "" || parsedRequest.Host == "" {
		return PresignedRequest{}, errors.New("invalid presigned upload URL")
	}
	parsedRequest.Scheme = parsedPrefix.Scheme
	parsedRequest.Host = parsedPrefix.Host
	request.URL = parsedRequest.String()
	return request, nil
}
