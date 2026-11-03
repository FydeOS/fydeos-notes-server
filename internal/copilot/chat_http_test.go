// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestLoadMessageAttachmentChecksDataSizeBeforeDecode(t *testing.T) {
	handler := &ChatHTTPHandler{}
	session := Session{}

	if _, _, err := handler.loadMessageAttachment(context.Background(), session, "data:text/plain;base64,YQ==", 1); err != nil {
		t.Fatalf("load one-byte base64 data URL: %v", err)
	}
	if _, _, err := handler.loadMessageAttachment(context.Background(), session, "data:text/plain;base64,YWI=", 1); err == nil {
		t.Fatal("expected oversized base64 data URL to be rejected")
	}
	if _, _, err := handler.loadMessageAttachment(context.Background(), session, "data:text/plain,"+strings.Repeat("a", 4), 1); err == nil {
		t.Fatal("expected oversized escaped data URL to be rejected")
	}
}

func TestBase64PayloadDecodedLength(t *testing.T) {
	for payload, want := range map[string]int64{
		base64.StdEncoding.EncodeToString([]byte("a")):   1,
		base64.StdEncoding.EncodeToString([]byte("ab")):  2,
		base64.StdEncoding.EncodeToString([]byte("abc")): 3,
		"abc": int64(maxSessionAttachmentBytes) + 1,
	} {
		if got := base64PayloadDecodedLength(payload); got != want {
			t.Errorf("base64PayloadDecodedLength(%q) = %d, want %d", payload, got, want)
		}
	}
}

func TestParseCopilotToolsConfig(t *testing.T) {
	for _, test := range []struct {
		name    string
		input   string
		want    chatToolsConfig
		wantErr bool
	}{
		{name: "omitted", input: "", want: chatToolsConfig{searchWorkspace: true, readingDocs: true, searchArtifacts: true, readingArtifacts: true}},
		{name: "empty", input: "{}", want: chatToolsConfig{searchWorkspace: true, readingDocs: true, searchArtifacts: true, readingArtifacts: true}},
		{name: "disabled UI tools", input: `{"searchWorkspace":false,"readingDocs":false}`, want: chatToolsConfig{searchArtifacts: true, readingArtifacts: true}},
		{name: "artifact tools can be disabled", input: `{"searchArtifacts":false,"readingArtifacts":false}`, want: chatToolsConfig{searchWorkspace: true, readingDocs: true}},
		{name: "enabled UI tool", input: `{"searchWorkspace":true}`, want: chatToolsConfig{searchWorkspace: true, readingDocs: true, searchArtifacts: true, readingArtifacts: true}},
		{name: "invalid JSON", input: `{`, wantErr: true},
		{name: "non-boolean UI tool", input: `{"searchWorkspace":null}`, wantErr: true},
		{name: "unknown tool", input: `{"unknown":false}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, err := parseCopilotToolsConfig(test.input)
			if (err != nil) != test.wantErr || actual != test.want {
				t.Fatalf("parseCopilotToolsConfig(%q) = (%+v, %v), want (%+v, error=%t)", test.input, actual, err, test.want, test.wantErr)
			}
		})
	}
}

func TestArtifactScopeForMessageUsesPersistedRequiredScope(t *testing.T) {
	session := Session{Messages: []Message{
		{ID: "other", ScopeSnapshot: json.RawMessage(`{"retrieval":{"mode":"workspace"}}`)},
		{ID: "message-1", ScopeSnapshot: json.RawMessage(`{"retrieval":{"mode":"required","requiredArtifactIds":["6ba7b810-9dad-11d1-80b4-00c04fd430c8"]}}`)},
	}}
	scope := artifactScopeForMessage(session, "message-1")
	if scope.Mode != "required" || len(scope.RequiredArtifactIDs) != 1 || scope.RequiredArtifactIDs[0] != "6ba7b810-9dad-11d1-80b4-00c04fd430c8" {
		t.Fatalf("artifact retrieval scope = %#v", scope)
	}
	if fallback := artifactScopeForMessage(session, "missing"); fallback.Mode != "workspace" {
		t.Fatalf("missing message scope = %#v", fallback)
	}
}

func TestLoadMessageAttachmentReadsAuthorizedDocumentBlob(t *testing.T) {
	docID := "doc-1"
	handler := &ChatHTTPHandler{}
	var gotActor, gotWorkspace, gotDoc, gotKey string
	handler.SetDocumentBlobReader(func(_ context.Context, actorID, workspaceID, requestedDocID, key string) (io.ReadCloser, string, int64, error) {
		gotActor, gotWorkspace, gotDoc, gotKey = actorID, workspaceID, requestedDocID, key
		return io.NopCloser(strings.NewReader("image bytes")), "image/png", int64(len("image bytes")), nil
	})
	session := Session{UserID: "user-1", WorkspaceID: "workspace-1", DocID: &docID}

	data, metadata, err := handler.loadMessageAttachment(context.Background(), session, "blob-id", 100)
	if err != nil {
		t.Fatalf("load document blob: %v", err)
	}
	if string(data) != "image bytes" || metadata.ContentType != "image/png" || metadata.ContentLength != int64(len(data)) {
		t.Fatalf("loaded blob = %q, metadata = %#v", data, metadata)
	}
	if gotActor != "user-1" || gotWorkspace != "workspace-1" || gotDoc != docID || gotKey != "blob-id" {
		t.Fatalf("document blob request = (%q, %q, %q, %q)", gotActor, gotWorkspace, gotDoc, gotKey)
	}
}

func TestLoadMessageAttachmentRejectsOversizedDocumentBlob(t *testing.T) {
	docID := "doc-1"
	handler := &ChatHTTPHandler{}
	handler.SetDocumentBlobReader(func(context.Context, string, string, string, string) (io.ReadCloser, string, int64, error) {
		return io.NopCloser(strings.NewReader("too large")), "text/plain", 9, nil
	})
	session := Session{UserID: "user-1", WorkspaceID: "workspace-1", DocID: &docID}
	if _, _, err := handler.loadMessageAttachment(context.Background(), session, "blob-id", 4); err == nil {
		t.Fatal("expected document blob larger than remaining request budget to be rejected")
	}
}
