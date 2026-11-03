// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
)

func fetchRemoteAttachment(ctx context.Context, client *http.Client, locator string, maxBytes ...int64) ([]byte, string, error) {
	limit := maxSessionAttachmentBytes
	if len(maxBytes) > 0 && maxBytes[0] > 0 && maxBytes[0] < limit {
		limit = maxBytes[0]
	}
	target, err := remoteAttachmentURL(locator)
	if err != nil {
		return nil, "", err
	}
	if client == nil {
		return nil, "", errors.New("remote Copilot attachments are unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", errors.New("invalid remote Copilot attachment")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("fetch remote Copilot attachment: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("remote Copilot attachment returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		if limit < maxSessionAttachmentBytes {
			return nil, "", errors.New("Copilot attachments exceed the request byte budget")
		}
		return nil, "", errors.New("Copilot attachment exceeds 20 MiB")
	}
	contentType, _, err := mime.ParseMediaType(http.DetectContentType(data))
	if err != nil || contentType == "" {
		contentType = "application/octet-stream"
	}
	return data, contentType, nil
}

func remoteAttachmentURL(locator string) (string, error) {
	parsed, err := url.Parse(locator)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" {
		return "", errors.New("invalid remote Copilot attachment URL")
	}
	if parsed.Scheme == "gs" {
		if parsed.Fragment != "" || parsed.Hostname() == "" {
			return "", errors.New("invalid gs Copilot attachment URL")
		}
		return "https://storage.googleapis.com/" + parsed.Hostname() + parsed.EscapedPath() + querySuffix(parsed.RawQuery), nil
	}
	if parsed.Scheme != "https" || parsed.Fragment != "" {
		return "", errors.New("remote Copilot attachments must use HTTPS or gs://")
	}
	return parsed.String(), nil
}

func querySuffix(query string) string {
	if query == "" {
		return ""
	}
	return "?" + query
}

func remoteAttachmentFilename(locator string) string {
	parsed, err := url.Parse(locator)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "gs") {
		return ""
	}
	return safeAttachmentFilename(path.Base(strings.TrimSuffix(parsed.Path, "/")))
}
