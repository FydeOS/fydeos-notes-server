// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type copilotWorkspaceAccessReader struct {
	permissions map[string]bool
}

type copilotWorkspaceArtifactManagerStub struct {
	embeddingAvailable bool
	items              []copilot.WorkspaceArtifact
	added              []byte
	removedID          string
	removeCalls        int
}

func (s *copilotWorkspaceArtifactManagerStub) EmbeddingAvailable(context.Context) bool {
	return s.embeddingAvailable
}

func (s *copilotWorkspaceArtifactManagerStub) Add(_ context.Context, workspaceID, fileName, mediaType string, source io.Reader) (copilot.WorkspaceArtifact, error) {
	body, err := io.ReadAll(source)
	if err != nil {
		return copilot.WorkspaceArtifact{}, err
	}
	s.added = body
	return copilot.WorkspaceArtifact{WorkspaceID: workspaceID, ArtifactID: "artifact-1", ContentHash: "hash", FileName: fileName, EmbeddingStatus: "processing", MediaType: mediaType, Size: int64(len(body)), CreatedAt: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}, nil
}

func (s *copilotWorkspaceArtifactManagerStub) List(context.Context, string, int, int) ([]copilot.WorkspaceArtifact, int, error) {
	return s.items, len(s.items), nil
}

func (s *copilotWorkspaceArtifactManagerStub) Remove(_ context.Context, _, artifactID string) (bool, error) {
	s.removeCalls++
	s.removedID = artifactID
	return true, nil
}

func TestCopilotWorkspaceArtifactsGraphQLLifecycleAndAuthorization(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	manager := &copilotWorkspaceArtifactManagerStub{embeddingAvailable: true, items: []copilot.WorkspaceArtifact{{WorkspaceID: fixture.workspaceID, ArtifactID: "existing", ContentHash: "hash-2", FileName: "existing.pdf", EmbeddingStatus: "processing", MediaType: "application/pdf", Size: 12, CreatedAt: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)}}}
	permissions := copilotWorkspaceAccessReader{permissions: map[string]bool{"Workspace_Read": true, "Workspace_Settings_Update": true}}
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{WorkspacePermissionReader: permissions, CopilotWorkspaceArtifacts: manager})
	query := graphRequest(t, handler, `query($id:String!) { workspace(id:$id) { embedding { artifacts(pagination:{first:10}) { totalCount edges { cursor node { workspaceId artifactId contentHash fileName embeddingStatus mediaType size createdAt } } pageInfo { hasNextPage hasPreviousPage } } } } }`, map[string]any{"id": fixture.workspaceID}, fixture.sessionID, fixture.userID)
	if errs, ok := query["errors"].([]any); ok && len(errs) > 0 {
		t.Fatalf("artifact list query failed: %#v", errs)
	}
	page := query["data"].(map[string]any)["workspace"].(map[string]any)["embedding"].(map[string]any)["artifacts"].(map[string]any)
	if page["totalCount"] != float64(1) || page["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)["artifactId"] != "existing" {
		t.Fatalf("artifact page = %#v", page)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	operations, err := json.Marshal(map[string]any{"query": `mutation($workspaceId:String!,$blob:Upload!) { addWorkspaceArtifact(workspaceId:$workspaceId,blob:$blob) { artifactId fileName embeddingStatus size } }`, "variables": map[string]any{"workspaceId": fixture.workspaceID, "blob": nil}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("operations", string(operations)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("map", `{"0":["variables.blob"]}`); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("0", "upload.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("artifact bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	disabledManager := &copilotWorkspaceArtifactManagerStub{}
	disabledHandler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{WorkspacePermissionReader: permissions, CopilotWorkspaceArtifacts: disabledManager})
	disabledRequest := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body.Bytes()))
	disabledRequest.Header.Set("Content-Type", writer.FormDataContentType())
	disabledRequest.Header.Set("x-operation-name", "AddWorkspaceArtifact")
	disabledRequest.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fixture.sessionID})
	disabledRequest.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: fixture.userID})
	disabledResponse := newDeadlineTestResponseWriter()
	disabledHandler.ServeHTTP(disabledResponse, disabledRequest)
	var disabledResult map[string]any
	if err := json.Unmarshal(disabledResponse.Body.Bytes(), &disabledResult); err != nil {
		t.Fatal(err)
	}
	if graphErrorName(disabledResult) != "COPILOT_EMBEDDING_UNAVAILABLE" || disabledManager.added != nil {
		t.Fatalf("disabled embedding upload result=%#v, manager=%#v", disabledResult, disabledManager)
	}

	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("x-operation-name", "AddWorkspaceArtifact")
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fixture.sessionID})
	request.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: fixture.userID})
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("artifact upload GraphQL status=%d body=%s", response.Code, response.Body.String())
	}
	var uploadResult map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &uploadResult); err != nil {
		t.Fatal(err)
	}
	if errs, ok := uploadResult["errors"].([]any); ok && len(errs) > 0 {
		t.Fatalf("artifact upload failed: %#v", errs)
	}
	if string(manager.added) != "artifact bytes" {
		t.Fatalf("stored artifact body = %q", manager.added)
	}

	removed := graphRequest(t, handler, `mutation($workspaceId:String!,$artifactId:String!) { removeWorkspaceArtifact(workspaceId:$workspaceId,artifactId:$artifactId) }`, map[string]any{"workspaceId": fixture.workspaceID, "artifactId": "artifact-1"}, fixture.sessionID, fixture.userID)
	if errs, ok := removed["errors"].([]any); ok && len(errs) > 0 {
		t.Fatalf("artifact removal failed: %#v", errs)
	}
	if removed["data"].(map[string]any)["removeWorkspaceArtifact"] != true || manager.removedID != "artifact-1" {
		t.Fatalf("artifact removal result=%#v manager=%#v", removed, manager)
	}

	deniedHandler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{WorkspacePermissionReader: copilotWorkspaceAccessReader{permissions: map[string]bool{"Workspace_Read": true}}, CopilotWorkspaceArtifacts: manager})
	denied := graphRequest(t, deniedHandler, `mutation($workspaceId:String!,$artifactId:String!) { removeWorkspaceArtifact(workspaceId:$workspaceId,artifactId:$artifactId) }`, map[string]any{"workspaceId": fixture.workspaceID, "artifactId": "artifact-2"}, fixture.sessionID, fixture.userID)
	if errs, ok := denied["errors"].([]any); !ok || len(errs) == 0 || manager.removeCalls != 1 {
		t.Fatalf("artifact mutation without settings permission was not rejected: result=%#v manager=%#v", denied, manager)
	}
}

func (r copilotWorkspaceAccessReader) GetWorkspaceRolePermissions(context.Context, string, string) (workspaces.WorkspaceAccess, error) {
	return workspaces.WorkspaceAccess{Role: "Owner", Permissions: r.permissions}, nil
}

func TestCopilotWorkspaceIgnoredDocsGraphQLLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	for _, statement := range []string{
		`CREATE TABLE ai_workspace_ignored_docs (workspace_id varchar NOT NULL, doc_id varchar NOT NULL, created_at timestamptz(3) NOT NULL DEFAULT current_timestamp, PRIMARY KEY(workspace_id,doc_id))`,
		`CREATE TABLE workspace_pages (workspace_id text NOT NULL, page_id text NOT NULL, title text, published_at timestamptz, PRIMARY KEY(workspace_id,page_id))`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("prepare ignored document test schema: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_pages(workspace_id,page_id,title) VALUES($1,'doc-a','Alpha'),($1,'doc-b','Beta')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,created_at,updated_at,created_by,updated_by) VALUES
		($1,'doc-a','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z',$2,$2),
		($1,'doc-b','2026-02-01T00:00:00Z','2026-02-02T00:00:00Z',$2,$2)`, fixture.workspaceID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	permissions := workspaces.NewAuthorizationService(repository)
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{
		WorkspacePermissionReader: permissions,
	})
	checkNoErrors := func(result map[string]any) {
		t.Helper()
		if failures, ok := result["errors"].([]any); ok && len(failures) > 0 {
			t.Fatalf("GraphQL errors: %#v", failures)
		}
	}
	mutation := `mutation($workspaceId:String!,$add:[String!],$remove:[String!]) { updateWorkspaceEmbeddingIgnoredDocs(workspaceId:$workspaceId,add:$add,remove:$remove) }`
	mutate := func(add, remove []string) float64 {
		t.Helper()
		result := graphRequest(t, handler, mutation, map[string]any{"workspaceId": fixture.workspaceID, "add": add, "remove": remove}, fixture.sessionID, fixture.userID)
		checkNoErrors(result)
		return result["data"].(map[string]any)["updateWorkspaceEmbeddingIgnoredDocs"].(float64)
	}
	if got := mutate([]string{"doc-a", "doc-b"}, nil); got != 2 {
		t.Fatalf("added count = %v, want 2", got)
	}
	if got := mutate([]string{"doc-a"}, nil); got != 0 {
		t.Fatalf("duplicate add count = %v, want 0", got)
	}
	if got := mutate([]string{"doc-a"}, []string{"doc-a"}); got != 1 {
		t.Fatalf("add/remove same id count = %v, want 1 removal", got)
	}
	if got := mutate(nil, []string{"doc-a"}); got != 0 {
		t.Fatalf("repeated remove count = %v, want 0", got)
	}
	if got := mutate([]string{"doc-a"}, nil); got != 1 {
		t.Fatalf("re-add count = %v, want 1", got)
	}

	query := `query($workspaceId:String!,$pagination:PaginationInput!) { workspace(id:$workspaceId) { embedding { ignoredDocs(pagination:$pagination) { totalCount pageInfo { hasNextPage hasPreviousPage endCursor } edges { node { docId createdAt docCreatedAt docUpdatedAt title createdBy createdByAvatar updatedBy } } } allIgnoredDocs { docId title } } } }`
	result := graphRequest(t, handler, query, map[string]any{"workspaceId": fixture.workspaceID, "pagination": map[string]any{"first": 1}}, fixture.sessionID, fixture.userID)
	checkNoErrors(result)
	workspace := result["data"].(map[string]any)["workspace"].(map[string]any)
	embedding := workspace["embedding"].(map[string]any)
	page := embedding["ignoredDocs"].(map[string]any)
	if page["totalCount"] != float64(2) || page["pageInfo"].(map[string]any)["hasNextPage"] != true {
		t.Fatalf("ignored doc page metadata = %#v", page)
	}
	node := page["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)
	if node["docId"] != "doc-a" || node["title"] != "Alpha" || node["createdBy"] != "Web User" {
		t.Fatalf("ignored doc metadata = %#v", node)
	}
	if _, err := time.Parse(time.RFC3339Nano, node["docCreatedAt"].(string)); err != nil {
		t.Fatalf("docCreatedAt is not an RFC3339 date: %v", err)
	}
	all := embedding["allIgnoredDocs"].([]any)
	if len(all) != 2 || all[0].(map[string]any)["docId"] != "doc-a" {
		t.Fatalf("all ignored docs = %#v", all)
	}

	deniedHandler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{
		WorkspacePermissionReader: copilotWorkspaceAccessReader{permissions: map[string]bool{"Workspace_Read": true}},
	})
	denied := graphRequest(t, deniedHandler, mutation, map[string]any{"workspaceId": fixture.workspaceID, "add": []string{"denied"}}, fixture.sessionID, fixture.userID)
	if len(denied["errors"].([]any)) == 0 {
		t.Fatalf("mutation without Workspace.Settings.Update succeeded: %#v", denied)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ai_workspace_ignored_docs WHERE workspace_id=$1`, fixture.workspaceID).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("ignored doc rows after denied mutation = %d, err=%v", remaining, err)
	}
}
