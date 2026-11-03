// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type attachmentRoundTripper func(*http.Request) (*http.Response, error)

func (f attachmentRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRemoteAttachmentURLAllowsOnlyHTTPSAndGoogleStorage(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "https://cdn.example.test/files/report.pdf?signature=opaque", want: "https://cdn.example.test/files/report.pdf?signature=opaque", ok: true},
		{input: "gs://notes-bucket/files/report.pdf?generation=2", want: "https://storage.googleapis.com/notes-bucket/files/report.pdf?generation=2", ok: true},
		{input: "http://127.0.0.1/private", ok: false},
		{input: "https://user:password@example.test/file", ok: false},
		{input: "https://example.test/file#fragment", ok: false},
		{input: "file:///etc/passwd", ok: false},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := remoteAttachmentURL(test.input)
			if (err == nil) != test.ok || test.ok && got != test.want {
				t.Fatalf("remoteAttachmentURL(%q) = %q, %v; want %q, allowed=%t", test.input, got, err, test.want, test.ok)
			}
		})
	}
}

func TestFetchRemoteAttachmentBoundsBodyAndSniffsMIME(t *testing.T) {
	client := &http.Client{Transport: attachmentRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://cdn.example.test/image.png" {
			t.Fatalf("fetch target = %q", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: io.NopCloser(strings.NewReader("plain text")), Request: request}, nil
	})}
	body, contentType, err := fetchRemoteAttachment(context.Background(), client, "https://cdn.example.test/image.png")
	if err != nil || string(body) != "plain text" || contentType != "text/plain" {
		t.Fatalf("fetchRemoteAttachment = %q, %q, %v", body, contentType, err)
	}
	if _, _, err := fetchRemoteAttachment(context.Background(), client, "https://cdn.example.test/image.png", 4); err == nil || !strings.Contains(err.Error(), "request byte budget") {
		t.Fatalf("per-request attachment budget error = %v", err)
	}

	oversized := &http.Client{Transport: attachmentRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(io.LimitReader(zeroReader{}, maxSessionAttachmentBytes+1)), Request: request}, nil
	})}
	if _, _, err := fetchRemoteAttachment(context.Background(), oversized, "https://cdn.example.test/large.bin"); err == nil || !strings.Contains(err.Error(), "20 MiB") {
		t.Fatalf("oversized remote attachment error = %v", err)
	}
}
