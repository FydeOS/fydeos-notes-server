// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gqlgraphql "github.com/99designs/gqlgen/graphql"
	gqlhandler "github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/blobs"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/google/uuid"
)

type fakeBlobManager struct {
	deleteArgs       []any
	deleteCalls      int
	releaseCalls     int
	releaseResponses []releaseBatchResponse
	listed           []blobs.Blob
}

type releaseBatchResponse struct {
	released int
	hasMore  bool
}

func (f *fakeBlobManager) ListManaged(context.Context, string, string) ([]blobs.Blob, error) {
	return f.listed, nil
}

func (f *fakeBlobManager) Delete(_ context.Context, actorID, workspaceID, key string, permanently bool) (bool, error) {
	f.deleteCalls++
	f.deleteArgs = []any{actorID, workspaceID, key, permanently}
	return true, nil
}

func (f *fakeBlobManager) ReleaseDeletedBatch(context.Context, string, string, int) (int, bool, error) {
	index := f.releaseCalls
	f.releaseCalls++
	response := f.releaseResponses[index]
	return response.released, response.hasMore, nil
}

type fakeBlobUploader struct {
	setActorID       string
	setWorkspace     string
	setKey           string
	setMIME          string
	setBody          []byte
	setResponse      string
	setErr           error
	createActorID    string
	createWorkspace  string
	createKey        string
	createSize       int64
	createMIME       string
	createResponse   blobs.UploadInit
	partActorID      string
	partWorkspace    string
	partKey          string
	partUploadID     string
	partNumber       int32
	partURLResponse  blobs.UploadPart
	commentActor     string
	commentWorkspace string
	commentDoc       string
	commentKey       string
	commentName      string
	commentBody      []byte
	commentURL       string
	commentErr       error
}

func (f *fakeBlobUploader) CreateBlobUpload(_ context.Context, actorID, workspaceID, key string, size int64, mime string) (blobs.UploadInit, error) {
	f.createActorID, f.createWorkspace, f.createKey, f.createSize, f.createMIME = actorID, workspaceID, key, size, mime
	return f.createResponse, nil
}

func (f *fakeBlobUploader) BlobUploadPartURL(_ context.Context, actorID, workspaceID, key, uploadID string, partNumber int32) (blobs.UploadPart, error) {
	f.partActorID, f.partWorkspace, f.partKey, f.partUploadID, f.partNumber = actorID, workspaceID, key, uploadID, partNumber
	return f.partURLResponse, nil
}

func (f *fakeBlobUploader) SetBlob(_ context.Context, actorID, workspaceID, key, mime string, body io.Reader) (string, error) {
	f.setActorID, f.setWorkspace, f.setKey, f.setMIME = actorID, workspaceID, key, mime
	f.setBody, _ = io.ReadAll(body)
	if f.setResponse == "" {
		f.setResponse = key
	}
	return f.setResponse, f.setErr
}

func (f *fakeBlobUploader) UploadCommentAttachment(_ context.Context, actorID, workspaceID, docID, key, name string, body io.Reader) (string, error) {
	f.commentActor, f.commentWorkspace, f.commentDoc = actorID, workspaceID, docID
	f.commentKey, f.commentName = key, name
	f.commentBody, _ = io.ReadAll(body)
	return f.commentURL, f.commentErr
}

func (*fakeBlobUploader) CompleteBlobUpload(context.Context, string, string, string, *string, []storage.CompletedPart) (string, error) {
	return "", nil
}

func (*fakeBlobUploader) AbortBlobUpload(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

func TestDeleteBlobUsesPreferredKeyAndCallerIdentity(t *testing.T) {
	manager := &fakeBlobManager{}
	resolver := &mutationResolver{Resolver: &Resolver{BlobManager: manager}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "actor-1"}})
	hash := "legacy-hash"
	key := "canonical-key"
	permanently := true
	if _, err := resolver.DeleteBlob(ctx, "workspace-1", &hash, &key, permanently); err != nil {
		t.Fatal(err)
	}
	want := []any{"actor-1", "workspace-1", "canonical-key", true}
	for i := range want {
		if manager.deleteArgs[i] != want[i] {
			t.Fatalf("delete args = %#v, want %#v", manager.deleteArgs, want)
		}
	}
}

func TestDeleteBlobLegacyGraphQLArguments(t *testing.T) {
	manager := &fakeBlobManager{}
	server := gqlhandler.New(NewExecutableSchema(Config{Resolvers: &Resolver{BlobManager: manager}}))
	server.AddTransport(transport.POST{})
	principal := &auth.Principal{User: auth.User{ID: "actor-1"}}

	clientMutation := `mutation deleteBlob($workspaceId: String!, $key: String!, $permanently: Boolean) {
		deleteBlob(workspaceId: $workspaceId, key: $key, permanently: $permanently)
	}`
	clientResult := serveBlobResolverGraphQL(t, server, clientMutation, map[string]any{
		"workspaceId": "workspace-1", "key": "client-key",
	}, principal)
	if clientResult["errors"] != nil || clientResult["data"].(map[string]any)["deleteBlob"] != true {
		t.Fatalf("client deleteBlob result = %#v", clientResult)
	}
	if manager.deleteCalls != 1 || len(manager.deleteArgs) != 4 || manager.deleteArgs[2] != "client-key" || manager.deleteArgs[3] != false {
		t.Fatalf("client delete arguments = %#v, want default soft delete", manager.deleteArgs)
	}

	hashMutation := `mutation deleteBlobByHash($workspaceId: String!, $hash: String) {
		deleteBlob(workspaceId: $workspaceId, hash: $hash)
	}`
	hashResult := serveBlobResolverGraphQL(t, server, hashMutation, map[string]any{
		"workspaceId": "workspace-1", "hash": "legacy-hash",
	}, principal)
	if hashResult["errors"] != nil || hashResult["data"].(map[string]any)["deleteBlob"] != true {
		t.Fatalf("legacy hash deleteBlob result = %#v", hashResult)
	}
	if manager.deleteCalls != 2 || len(manager.deleteArgs) != 4 || manager.deleteArgs[2] != "legacy-hash" || manager.deleteArgs[3] != false {
		t.Fatalf("legacy hash arguments = %#v, want hash fallback and default soft delete", manager.deleteArgs)
	}

	nullResult := serveBlobResolverGraphQL(t, server, clientMutation, map[string]any{
		"workspaceId": "workspace-1", "key": "client-key", "permanently": nil,
	}, principal)
	if nullResult["errors"] == nil || manager.deleteCalls != 2 {
		t.Fatalf("explicit null permanently was not rejected before deleting: result %#v, args %#v", nullResult, manager.deleteArgs)
	}

	literalNullResult := serveBlobResolverGraphQL(t, server, `mutation {
		deleteBlob(workspaceId: "workspace-1", key: "client-key", permanently: null)
	}`, nil, principal)
	if literalNullResult["errors"] == nil || manager.deleteCalls != 2 {
		t.Fatalf("literal null permanently was not rejected before deleting: result %#v, args %#v", literalNullResult, manager.deleteArgs)
	}
}

func serveBlobResolverGraphQL(t *testing.T, server *gqlhandler.Server, query string, variables map[string]any, principal *auth.Principal) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode GraphQL response: %v: %s", err, response.Body.String())
	}
	return result
}

func TestReleaseDeletedBlobsDrainsBatches(t *testing.T) {
	manager := &fakeBlobManager{releaseResponses: []releaseBatchResponse{{released: 999, hasMore: true}, {released: 0, hasMore: false}}}
	resolver := &mutationResolver{Resolver: &Resolver{BlobManager: manager}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "actor-1"}})
	result, err := resolver.ReleaseDeletedBlobs(ctx, "workspace-1")
	if err != nil || !result || manager.releaseCalls != 2 {
		t.Fatalf("release result=%t calls=%d err=%v", result, manager.releaseCalls, err)
	}
}

func TestWorkspaceBlobsMapsManagedInventory(t *testing.T) {
	created := time.Date(2026, 10, 17, 2, 3, 4, 0, time.UTC)
	manager := &fakeBlobManager{listed: []blobs.Blob{{Key: "blob-1", MIME: "image/png", Size: 42, CreatedAt: created}}}
	resolver := &workspaceTypeResolver{Resolver: &Resolver{BlobManager: manager}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "actor-1"}})
	result, err := resolver.Blobs(ctx, &model.WorkspaceType{ID: "workspace-1"})
	if err != nil || len(result) != 1 {
		t.Fatalf("blob inventory = %#v, %v", result, err)
	}
	if result[0].Key != "blob-1" || result[0].Mime != "image/png" || result[0].Size != 42 || result[0].CreatedAt != created.Format(time.RFC3339Nano) {
		t.Fatalf("mapped blob = %#v", result[0])
	}
}

func TestCreateBlobUploadMapsClientContract(t *testing.T) {
	expires := time.Date(2026, 10, 17, 2, 3, 4, 0, time.UTC)
	uploader := &fakeBlobUploader{createResponse: blobs.UploadInit{
		Method: blobs.UploadMethodMultipart, BlobKey: "asset-1", UploadID: stringPointer("upload-1"),
		PartSize: int32Pointer(5 << 20), ExpiresAt: &expires,
		Headers:       map[string]string{"x-amz-content-sha256": "UNSIGNED-PAYLOAD"},
		UploadedParts: []storage.MultipartPart{{PartNumber: 2, ETag: "etag-2"}},
	}}
	resolver := &mutationResolver{Resolver: &Resolver{BlobUploader: uploader}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "actor-1"}})
	result, err := resolver.CreateBlobUpload(ctx, "workspace-1", "asset-1", 11<<20, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if uploader.createActorID != "actor-1" || uploader.createWorkspace != "workspace-1" || uploader.createKey != "asset-1" || uploader.createSize != 11<<20 || uploader.createMIME != "application/octet-stream" {
		t.Fatalf("create arguments = %+v", uploader)
	}
	if result.Method != model.BlobUploadMethodMultipart || result.BlobKey != "asset-1" || result.UploadID == nil || *result.UploadID != "upload-1" || result.PartSize == nil || *result.PartSize != 5<<20 {
		t.Fatalf("upload init = %+v", result)
	}
	if result.Headers["x-amz-content-sha256"] != "UNSIGNED-PAYLOAD" || len(result.UploadedParts) != 1 || result.UploadedParts[0].PartNumber != 2 || result.UploadedParts[0].Etag != "etag-2" {
		t.Fatalf("mapped headers/parts = %#v / %#v", result.Headers, result.UploadedParts)
	}
	if result.ExpiresAt == nil || *result.ExpiresAt != expires.Format(time.RFC3339Nano) {
		t.Fatalf("upload expiry = %v", result.ExpiresAt)
	}
}

func TestBlobUploadPartURLUsesWorkspaceAndAuthenticatedUser(t *testing.T) {
	expires := time.Date(2026, 10, 17, 2, 3, 4, 0, time.UTC)
	uploader := &fakeBlobUploader{partURLResponse: blobs.UploadPart{
		UploadURL: "https://uploads.example.test/part", Headers: map[string]string{"x-test": "value"}, ExpiresAt: expires,
	}}
	resolver := &workspaceTypeResolver{Resolver: &Resolver{BlobUploader: uploader}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "actor-1"}})
	result, err := resolver.BlobUploadPartURL(ctx, &model.WorkspaceType{ID: "workspace-1"}, "asset-1", "upload-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if uploader.partActorID != "actor-1" || uploader.partWorkspace != "workspace-1" || uploader.partKey != "asset-1" || uploader.partUploadID != "upload-1" || uploader.partNumber != 3 {
		t.Fatalf("part URL arguments = %+v", uploader)
	}
	if result.UploadURL != "https://uploads.example.test/part" || result.Headers["x-test"] != "value" || result.ExpiresAt == nil || *result.ExpiresAt != expires.Format(time.RFC3339Nano) {
		t.Fatalf("part URL response = %+v", result)
	}
}

func stringPointer(value string) *string { return &value }

func int32Pointer(value int32) *int32 { return &value }

func TestUploadCommentAttachmentUsesAuthenticatedCallerAndPreservesContract(t *testing.T) {
	uploader := &fakeBlobUploader{commentURL: "https://notes.example.test/api/workspaces/workspace-1/docs/doc-1/comment-attachments/key"}
	resolver := &mutationResolver{Resolver: &Resolver{BlobUploader: uploader}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "commenter-1"}})
	attachment := gqlgraphql.Upload{Filename: "note.txt", Size: 5, File: bytes.NewReader([]byte("hello")), ContentType: "application/x-ignored"}
	got, err := resolver.UploadCommentAttachment(ctx, attachment, "doc-1", "workspace-1")
	if err != nil || got != uploader.commentURL {
		t.Fatalf("upload URL = %q, %v", got, err)
	}
	if uploader.commentActor != "commenter-1" || uploader.commentWorkspace != "workspace-1" || uploader.commentDoc != "doc-1" || uploader.commentName != "note.txt" || string(uploader.commentBody) != "hello" {
		t.Fatalf("comment attachment arguments = %+v", uploader)
	}
	if _, err := uuid.Parse(uploader.commentKey); err != nil {
		t.Fatalf("attachment key %q is not a UUID: %v", uploader.commentKey, err)
	}
}

func TestUploadCommentAttachmentRejectsAnonymousAndOversizedRequests(t *testing.T) {
	uploader := &fakeBlobUploader{}
	resolver := &mutationResolver{Resolver: &Resolver{BlobUploader: uploader}}
	attachment := gqlgraphql.Upload{Filename: "large.bin", Size: blobs.MaxCommentAttachmentSize + 1, File: bytes.NewReader(nil)}
	if _, err := resolver.UploadCommentAttachment(context.Background(), attachment, "doc-1", "workspace-1"); err != ErrAuthenticationRequired {
		t.Fatalf("anonymous upload error = %v", err)
	}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "commenter-1"}})
	if _, err := resolver.UploadCommentAttachment(ctx, attachment, "doc-1", "workspace-1"); err != blobs.ErrCommentAttachmentQuotaExceeded {
		t.Fatalf("oversized upload error = %v", err)
	}
	if uploader.commentKey != "" {
		t.Fatalf("oversized upload reached storage service: %+v", uploader)
	}
}
