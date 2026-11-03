// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	gqlupload "github.com/99designs/gqlgen/graphql"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type enabledCopilotCredentials struct{ MCPCredentialManager }

func (enabledCopilotCredentials) CopilotEnabled(context.Context) (bool, error) { return true, nil }

type copilotWorkspacePermissionStub struct{}

func (copilotWorkspacePermissionStub) GetWorkspaceRolePermissions(context.Context, string, string) (workspaces.WorkspaceAccess, error) {
	return workspaces.WorkspaceAccess{Permissions: map[string]bool{"Workspace_Copilot": true}}, nil
}

type copilotAttachmentObjectStore struct{ objects map[string][]byte }

func (s *copilotAttachmentObjectStore) PutObject(_ context.Context, _, key string, body io.Reader, _ storage.PutOptions) (storage.ObjectMetadata, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectMetadata{}, err
	}
	if s.objects == nil {
		s.objects = make(map[string][]byte)
	}
	s.objects[key] = data
	return storage.ObjectMetadata{ContentLength: int64(len(data))}, nil
}

func (s *copilotAttachmentObjectStore) GetObject(context.Context, string, string) (io.ReadCloser, storage.ObjectMetadata, error) {
	return nil, storage.ObjectMetadata{}, errors.New("not used")
}

func (s *copilotAttachmentObjectStore) DeleteObject(_ context.Context, _, key string) error {
	delete(s.objects, key)
	return nil
}

func TestCopilotSessionGraphQLLifecycleUsesLegacyTables(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE workspaces (id text PRIMARY KEY)`,
		`CREATE TABLE workspace_artifacts (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),workspace_id text NOT NULL,content_hash text NOT NULL,
			display_name text,file_name text,canonical_media_type text NOT NULL,size_bytes bigint NOT NULL,
			storage_scope text NOT NULL,storage_key text NOT NULL,status text NOT NULL,library_owned boolean NOT NULL DEFAULT false,
			extracted_text_runes bigint,ready_at timestamptz,reservation_expires_at timestamptz,created_at timestamptz DEFAULT now(),updated_at timestamptz DEFAULT now(),
			UNIQUE(workspace_id,content_hash)
		)`,
		`CREATE TABLE ai_message_artifacts(message_id text NOT NULL,workspace_id text NOT NULL,artifact_id uuid NOT NULL,role text NOT NULL,display_name text,metadata jsonb,created_at timestamptz DEFAULT now(),PRIMARY KEY(message_id,artifact_id,role))`,
		`CREATE TYPE "AiPromptRole" AS ENUM ('system','assistant','user')`,
		`CREATE TABLE ai_sessions_metadata (
			id varchar(36) PRIMARY KEY,user_id varchar(36) NOT NULL,workspace_id varchar(36) NOT NULL,
			doc_id varchar(36),prompt_name varchar(32) NOT NULL,prompt_action varchar(32) DEFAULT '',
			pinned boolean NOT NULL DEFAULT false,title varchar,parent_session_id varchar(36),
			created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),
			"messageCost" integer NOT NULL DEFAULT 0,
			deleted_at timestamptz,focus jsonb
		)`,
		`CREATE TABLE ai_action_runs(id text PRIMARY KEY,user_id text,session_id text,action_id text,status text)`,
		`CREATE TABLE ai_transcript_tasks(id text PRIMARY KEY,user_id text,status text)`,
		`CREATE TABLE ai_usage_events(user_id text,provider_source text,feature_kind text,billing_unit_id text)`,
		`CREATE UNIQUE INDEX ai_session_unique_pinned_idx ON ai_sessions_metadata(user_id,workspace_id) WHERE pinned=true AND deleted_at IS NULL`,
		`CREATE TABLE ai_sessions_messages (
			id varchar(36) PRIMARY KEY,session_id varchar(36) NOT NULL,
			compat_submission_id varchar,role "AiPromptRole" NOT NULL,content text NOT NULL,
			"streamObjects" json,attachments json,params json,scope_snapshot jsonb,
			created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now()
		)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("set up Copilot schema: %v", err)
		}
	}
	store := copilot.NewStore(pool)
	attachmentObjects := &copilotAttachmentObjectStore{}
	resolver := &Resolver{CopilotSessions: store, MCPCredentials: enabledCopilotCredentials{}, WorkspacePermissionReader: copilotWorkspacePermissionStub{}}
	userID, workspaceID := "copilot-user", "personal-workspace"
	requestContext := auth.WithPrincipal(ctx, &auth.Principal{User: auth.User{ID: userID}})
	mutations := &mutationResolver{Resolver: resolver}

	firstID, err := mutations.CreateCopilotSession(requestContext, model.CreateChatSessionInput{WorkspaceID: workspaceID, PromptName: "Chat With AFFiNE AI"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	reusedID, err := mutations.CreateCopilotSession(requestContext, model.CreateChatSessionInput{WorkspaceID: workspaceID, PromptName: "Chat With AFFiNE AI"})
	if err != nil || reusedID != firstID {
		t.Fatalf("default session reuse = %q, %v; want %q", reusedID, err, firstID)
	}
	history, err := mutations.CreateCopilotSessionWithHistory(requestContext, model.CreateChatSessionInput{WorkspaceID: workspaceID, PromptName: "Chat With AFFiNE AI", ReuseLatestChat: boolPtr(true)})
	if err != nil || history.SessionID != firstID {
		t.Fatalf("create-with-history reused session = %#v, %v", history, err)
	}
	newSession := model.CreateChatSessionInput{WorkspaceID: workspaceID, PromptName: "Chat With AFFiNE AI", ReuseLatestChat: boolPtr(false)}
	secondID, err := mutations.CreateCopilotSession(requestContext, newSession)
	if err != nil || secondID == firstID {
		t.Fatalf("create distinct session = %q, %v", secondID, err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_messages(id,session_id,role,content,attachments,"streamObjects",scope_snapshot) VALUES($1,$2,'user',$3,'["blob-key"]','[{"type":"text-delta","textDelta":"hello"}]','{"version":1}')`, "message-1", secondID, "hello"); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES('canonical-workspace')`); err != nil {
		t.Fatalf("seed canonical workspace: %v", err)
	}
	docID := "document-1"
	actionSession, err := store.Create(ctx, copilot.CreateSessionInput{
		UserID: userID, WorkspaceID: "canonical-workspace", DocID: &docID,
		PromptName: "Generate a caption", ReuseLatestChat: true,
	}, false)
	if err != nil || actionSession.Action == nil || *actionSession.Action != "Generate a caption" {
		t.Fatalf("action session = %#v, %v", actionSession, err)
	}
	var persistedAction string
	if err := pool.QueryRow(ctx, `SELECT prompt_action FROM ai_sessions_metadata WHERE id=$1`, actionSession.ID).Scan(&persistedAction); err != nil || persistedAction != "Generate a caption" {
		t.Fatalf("persisted prompt_action = %q, %v", persistedAction, err)
	}
	if _, err := store.Create(ctx, copilot.CreateSessionInput{
		UserID: userID, WorkspaceID: "canonical-workspace", PromptName: "Generate a caption", Pinned: true,
	}, false); !errors.Is(err, copilot.ErrCopilotActionPromptInvalid) {
		t.Fatalf("pinned action session error = %v", err)
	}
	if _, err := store.Update(ctx, copilot.UpdateSessionInput{
		UserID: userID, WorkspaceID: "canonical-workspace", SessionID: actionSession.ID,
		PromptName: stringPtr("Generate a caption"), SetDocID: true, Personal: false,
	}); !errors.Is(err, copilot.ErrCopilotActionPromptInvalid) {
		t.Fatalf("action prompt after clearing document error = %v", err)
	}
	chatPrompt := "Chat With AFFiNE AI"
	if _, err := store.Update(ctx, copilot.UpdateSessionInput{
		UserID: userID, WorkspaceID: "canonical-workspace", SessionID: actionSession.ID,
		PromptName: &chatPrompt, Personal: false,
	}); err != nil {
		t.Fatalf("switch action session to chat prompt: %v", err)
	}
	updatedActionSession, err := store.Get(ctx, userID, "canonical-workspace", actionSession.ID, false)
	if err != nil || updatedActionSession.Action != nil {
		t.Fatalf("updated session action = %#v, %v", updatedActionSession.Action, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_metadata(id,user_id,workspace_id,prompt_name) VALUES('canonical-session',$1,'canonical-workspace','ask')`, userID); err != nil {
		t.Fatalf("seed canonical Copilot session: %v", err)
	}
	resolver.CopilotAttachments = copilot.NewSessionAttachments(attachmentObjects, store, func(context.Context, string, string, *string) error { return nil }, nil, nil)
	parent, err := (&userTypeResolver{resolver}).Copilot(requestContext, &model.UserType{ID: userID}, &workspaceID)
	if err != nil {
		t.Fatalf("resolve copilot: %v", err)
	}
	page, err := (&copilotResolver{resolver}).Chats(requestContext, parent, model.PaginationInput{First: intPtr(10)}, nil, &model.QueryChatHistoriesInput{WithMessages: boolPtr(true)})
	if err != nil {
		t.Fatalf("list chat histories: %v", err)
	}
	if page.TotalCount != 2 || len(page.Edges) != 2 {
		t.Fatalf("session page = %#v", page)
	}
	var foundMessage bool
	for _, edge := range page.Edges {
		for _, message := range edge.Node.Messages {
			if message.ID != nil && *message.ID == "message-1" {
				foundMessage = message.Content == "hello" && len(message.Attachments) == 1 && message.StreamObjects[0].TextDelta != nil && *message.StreamObjects[0].TextDelta == "hello"
			}
		}
	}
	if !foundMessage {
		t.Fatalf("message projection missing persisted fields: %#v", page.Edges)
	}
	userContent := "Explain this document"
	createdMessageID, err := mutations.CreateCopilotMessage(requestContext, model.CreateChatMessageInput{
		SessionID: secondID, Content: &userContent,
		Params: &model.JSON{Value: map[string]any{"locale": "en"}},
	})
	if err != nil {
		t.Fatalf("create user message: %v", err)
	}
	attachmentBytes := []byte("copilot file attachment")
	attachmentMessageID, err := mutations.CreateCopilotMessage(requestContext, model.CreateChatMessageInput{
		SessionID:   "canonical-session",
		Content:     &userContent,
		Attachments: []string{"data:text/plain,legacy%20reference"},
		Blob:        &gqlupload.Upload{File: bytes.NewReader(attachmentBytes), Filename: "notes.txt", ContentType: "text/plain", Size: int64(len(attachmentBytes))},
		Blobs:       []*gqlupload.Upload{{File: bytes.NewReader(attachmentBytes), Filename: "notes-copy.txt", ContentType: "text/plain", Size: int64(len(attachmentBytes))}},
	})
	if err != nil {
		t.Fatalf("create message with uploaded file: %v", err)
	}
	canonicalSession, err := store.Get(ctx, userID, "canonical-workspace", "canonical-session", false)
	if err != nil || len(canonicalSession.Messages) != 1 || canonicalSession.Messages[0].ID != attachmentMessageID || len(canonicalSession.Messages[0].Attachments) == 0 {
		t.Fatalf("uploaded attachment message = %#v, %v", canonicalSession.Messages, err)
	}
	var storedAttachments []string
	if err := pool.QueryRow(ctx, `SELECT attachments FROM ai_sessions_messages WHERE id=$1`, attachmentMessageID).Scan(&storedAttachments); err != nil || len(storedAttachments) != 3 || storedAttachments[0] != "data:text/plain,legacy%20reference" {
		t.Fatalf("persisted attachment URL = %#v, %v", storedAttachments, err)
	}
	var artifactID, scopeSnapshot string
	if err := pool.QueryRow(ctx, `SELECT artifact.id::text,message.scope_snapshot::text
		FROM workspace_artifacts artifact
		JOIN ai_message_artifacts link ON link.artifact_id=artifact.id AND link.workspace_id=artifact.workspace_id
		JOIN ai_sessions_messages message ON message.id=link.message_id
		WHERE message.id=$1 AND link.role='attachment'`, attachmentMessageID).Scan(&artifactID, &scopeSnapshot); err != nil {
		t.Fatalf("uploaded message artifact scope was not persisted: %v", err)
	}
	var persistedScope struct {
		Retrieval struct {
			Mode                string   `json:"mode"`
			RequiredArtifactIDs []string `json:"requiredArtifactIds"`
		} `json:"retrieval"`
	}
	if err := json.Unmarshal([]byte(scopeSnapshot), &persistedScope); err != nil {
		t.Fatalf("decode uploaded message scope snapshot: %v", err)
	}
	if persistedScope.Retrieval.Mode != "required" || len(persistedScope.Retrieval.RequiredArtifactIDs) != 1 || persistedScope.Retrieval.RequiredArtifactIDs[0] != artifactID {
		t.Fatalf("uploaded message scope snapshot = %s", scopeSnapshot)
	}
	var artifactLinks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ai_message_artifacts WHERE message_id=$1 AND artifact_id=$2`, attachmentMessageID, artifactID).Scan(&artifactLinks); err != nil || artifactLinks != 1 {
		t.Fatalf("duplicate attachment artifact links = %d, %v", artifactLinks, err)
	}
	if !bytes.Equal(attachmentObjects.objects[userID+"/canonical-workspace/"+attachmentKeyFromURL(t, storedAttachments[1])], attachmentBytes) {
		t.Fatal("uploaded Copilot attachment object does not match input bytes")
	}
	createdSession, err := store.Get(ctx, userID, workspaceID, secondID, true)
	if err != nil || len(createdSession.Messages) != 2 || createdSession.Messages[1].ID != createdMessageID || createdSession.Messages[1].Role != "user" || createdSession.Messages[1].Content != userContent {
		t.Fatalf("created Copilot message=%#v err=%v", createdSession.Messages, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_messages(id,session_id,role,content,created_at) VALUES
		('fork-user-message',$1,'user','question',now()-interval '2 seconds'),
		('fork-assistant-message',$1,'assistant','answer',now()-interval '1 second'),
		('fork-tail-message',$1,'user','later question',now())`, firstID); err != nil {
		t.Fatalf("seed fork history: %v", err)
	}
	forkID, err := store.Fork(ctx, copilot.ForkSessionInput{UserID: userID, WorkspaceID: workspaceID, DocID: "fork-doc", SessionID: firstID, LatestMessageID: stringPtr("fork-assistant-message"), Personal: true})
	if err != nil {
		t.Fatalf("fork session through assistant message: %v", err)
	}
	forked, err := store.Get(ctx, userID, workspaceID, forkID, true)
	if err != nil || len(forked.Messages) != 2 || forked.ParentSessionID == nil || *forked.ParentSessionID != firstID {
		t.Fatalf("fork history = %#v, %v", forked, err)
	}
	var forkCost int
	if err := pool.QueryRow(ctx, `SELECT "messageCost" FROM ai_sessions_metadata WHERE id=$1`, forkID).Scan(&forkCost); err != nil || forkCost != 1 {
		t.Fatalf("fork user-message cost = %d, %v", forkCost, err)
	}
	quota, err := (&copilotResolver{resolver}).Quota(requestContext, parent)
	if err != nil || quota.Used != 3 || quota.Limit != nil {
		t.Fatalf("Copilot quota = %#v, %v; want three used messages with no subscription limit", quota, err)
	}
	if _, err := store.Fork(ctx, copilot.ForkSessionInput{UserID: userID, WorkspaceID: workspaceID, DocID: "fork-doc", SessionID: firstID, LatestMessageID: stringPtr("missing-message"), Personal: true}); !errors.Is(err, copilot.ErrMessageNotFound) {
		t.Fatalf("fork with missing cutoff error = %v", err)
	}

	updatedID, err := store.Update(ctx, copilot.UpdateSessionInput{UserID: userID, WorkspaceID: workspaceID, SessionID: secondID, Personal: true, Pinned: boolPtr(true)})
	if err != nil || updatedID != secondID {
		t.Fatalf("pin session = %q, %v", updatedID, err)
	}
	var pinnedCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ai_sessions_metadata WHERE user_id=$1 AND workspace_id=$2 AND pinned=true AND deleted_at IS NULL`, userID, workspaceID).Scan(&pinnedCount); err != nil || pinnedCount != 1 {
		t.Fatalf("pinned session count = %d, %v", pinnedCount, err)
	}

	deleted, err := mutations.CleanupCopilotSession(requestContext, model.DeleteSessionInput{WorkspaceID: workspaceID, SessionIds: []string{secondID}})
	if err != nil || len(deleted) != 1 || deleted[0] != secondID {
		t.Fatalf("cleanup session = %#v, %v", deleted, err)
	}
	if _, err := store.Get(ctx, userID, workspaceID, secondID, true); !errors.Is(err, copilot.ErrSessionNotFound) {
		t.Fatalf("deleted session get error = %v", err)
	}
	if _, err := store.Create(ctx, copilot.CreateSessionInput{UserID: "other-user", WorkspaceID: workspaceID, PromptName: "ask"}, true); !errors.Is(err, copilot.ErrPersonalScope) {
		t.Fatalf("cross-user personal scope error = %v", err)
	}
}

func TestCopilotTranscriptQueryAndSettlementGraphQL(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE workspaces(id text PRIMARY KEY)`,
		`CREATE TABLE ai_transcript_tasks(
			id varchar PRIMARY KEY,user_id varchar NOT NULL,workspace_id varchar NOT NULL,blob_id varchar NOT NULL,
			status varchar NOT NULL,protected_result json,settled_at timestamptz,created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now())`,
		`INSERT INTO ai_transcript_tasks(id,user_id,workspace_id,blob_id,status,protected_result) VALUES
			('ready-task','transcript-user','personal-workspace','blob-1','ready',
			 '{"sourceAudio":{"blobId":"blob-1"},"normalizedSegments":[{"speaker":"A","startSec":0,"endSec":1,"start":"00:00:00","end":"00:00:01","text":"hello"}],"normalizedTranscript":"hello","summaryJson":{"title":"Meeting","durationMinutes":1,"attendees":[],"keyPoints":[],"actionItems":[],"decisions":[],"openQuestions":[],"blockers":[]}}'),
			('pending-task','transcript-user','personal-workspace','blob-2','pending','{}'),
			('other-user-task','another-user','personal-workspace','blob-3','ready','{}')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("set up transcript schema: %v", err)
		}
	}
	workspaceID := "personal-workspace"
	userID := "transcript-user"
	requestContext := auth.WithPrincipal(ctx, &auth.Principal{User: auth.User{ID: userID}})
	resolver := &Resolver{
		CopilotSessions:        copilot.NewStore(pool),
		CopilotTranscriptTasks: copilot.NewTranscriptTasks(pool),
		MCPCredentials:         enabledCopilotCredentials{},
	}
	parent := &model.Copilot{WorkspaceID: &workspaceID}
	copilotField := &copilotResolver{Resolver: resolver}
	readyID := "ready-task"
	ready, err := copilotField.TranscriptTask(requestContext, parent, nil, &readyID)
	if err != nil || ready == nil || ready.Status != model.AiJobStatusFinished || ready.NormalizedTranscript != nil || ready.SourceAudio == nil {
		t.Fatalf("ready transcript query = %#v, err=%v", ready, err)
	}
	pendingID := "pending-task"
	pending, err := copilotField.TranscriptTask(requestContext, parent, nil, &pendingID)
	if err != nil || pending == nil || pending.Status != model.AiJobStatusPending || pending.SourceAudio != nil {
		t.Fatalf("pending transcript query = %#v, err=%v", pending, err)
	}
	otherUserID := "other-user-task"
	otherUser, err := copilotField.TranscriptTask(requestContext, parent, nil, &otherUserID)
	if err == nil || otherUser != nil {
		t.Fatalf("cross-user transcript query = %#v, err=%v; want canonical permission fallback", otherUser, err)
	}
	mutation := &mutationResolver{Resolver: resolver}
	settled, err := mutation.SettleTranscriptTask(requestContext, readyID, workspaceID)
	if err != nil || settled == nil || settled.Status != model.AiJobStatusFinished || settled.NormalizedTranscript == nil || *settled.NormalizedTranscript != "hello" || settled.Title == nil || *settled.Title != "Meeting" {
		t.Fatalf("settle transcript = %#v, err=%v", settled, err)
	}
	again, err := mutation.SettleTranscriptTask(requestContext, readyID, workspaceID)
	if err != nil || again == nil || again.NormalizedTranscript == nil || *again.NormalizedTranscript != "hello" {
		t.Fatalf("repeat settle transcript = %#v, err=%v", again, err)
	}
	if _, err := mutation.SettleTranscriptTask(requestContext, otherUserID, workspaceID); err == nil || errors.Is(err, ErrTranscriptTaskNotFound) {
		t.Fatalf("cross-user settlement error = %v; want canonical permission fallback", err)
	}
}

type transcriptTaskPublisherRecorder struct {
	statuses []string
}

func (p *transcriptTaskPublisherRecorder) PublishTranscriptTaskChanged(_, _, status, _ string) {
	p.statuses = append(p.statuses, status)
}

func TestCopilotTranscriptSubmitAndRetryGraphQL(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE workspaces(id text PRIMARY KEY)`,
		`CREATE TABLE ai_transcript_tasks(
			id varchar PRIMARY KEY,user_id varchar NOT NULL,workspace_id varchar NOT NULL,blob_id varchar NOT NULL,
			status varchar NOT NULL,recipe_id varchar,recipe_version varchar,action_run_id varchar,dispatch_generation varchar,
			input_snapshot jsonb,public_meta jsonb,protected_result jsonb,error_code varchar,settled_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("set up transcript mutation schema: %v", err)
		}
	}
	userID, workspaceID := "transcript-submit-user", "personal-transcript-workspace"
	requestContext := auth.WithPrincipal(ctx, &auth.Principal{User: auth.User{ID: userID}})
	objects := &copilotAttachmentObjectStore{objects: make(map[string][]byte)}
	publisher := &transcriptTaskPublisherRecorder{}
	resolver := &Resolver{
		CopilotSessions:         copilot.NewStore(pool),
		CopilotTranscriptTasks:  copilot.NewTranscriptTasks(pool, objects),
		TranscriptTaskPublisher: publisher,
		MCPCredentials:          enabledCopilotCredentials{},
	}
	mutation := &mutationResolver{Resolver: resolver}
	blob := &gqlupload.Upload{Filename: "meeting.mp3", ContentType: "audio/mpeg", File: bytes.NewReader([]byte("audio contents"))}
	input := &model.SubmitAudioTranscriptionInput{
		SourceAudio:   &model.TranscriptionSourceAudioInput{DurationMs: intPtr(1200), MimeType: stringPtr("audio/mpeg")},
		SliceManifest: []*model.AudioSliceManifestItemInput{{Index: 0, FileName: "meeting.mp3", MimeType: "audio/mpeg", StartSec: 0, DurationSec: 1.2}},
	}
	task, err := mutation.SubmitTranscriptTask(requestContext, blob, nil, "blob-1", input, workspaceID)
	if err != nil || task == nil || task.Status != model.AiJobStatusPending {
		t.Fatalf("submit transcript task = %#v, err=%v", task, err)
	}
	if len(objects.objects) != 1 || len(publisher.statuses) != 1 || publisher.statuses[0] != "pending" {
		t.Fatalf("stored audio/event = %d/%v", len(objects.objects), publisher.statuses)
	}
	duplicateBlob := &gqlupload.Upload{Filename: "meeting.mp3", ContentType: "audio/mpeg", File: bytes.NewReader([]byte("audio contents"))}
	if _, err := mutation.SubmitTranscriptTask(requestContext, duplicateBlob, nil, "blob-1", input, workspaceID); !errors.Is(err, copilot.ErrTranscriptJobExists) {
		t.Fatalf("duplicate transcript submit error = %v", err)
	}
	if len(objects.objects) != 1 {
		t.Fatalf("duplicate submission leaked objects: %d", len(objects.objects))
	}
	if _, err := mutation.SubmitTranscriptTask(requestContext, nil, nil, "blob-empty", nil, workspaceID); !errors.Is(err, copilot.ErrTranscriptAudioNotProvided) {
		t.Fatalf("empty transcript submit error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ai_transcript_tasks SET status='failed',error_code='provider_error' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	retried, err := mutation.RetryTranscriptTask(requestContext, task.ID, workspaceID)
	if err != nil || retried == nil || retried.Status != model.AiJobStatusPending {
		t.Fatalf("retry transcript task = %#v, err=%v", retried, err)
	}
	if len(publisher.statuses) != 2 || publisher.statuses[1] != "pending" {
		t.Fatalf("retry events = %v", publisher.statuses)
	}
	if _, err := mutation.RetryTranscriptTask(requestContext, task.ID, workspaceID); !errors.Is(err, copilot.ErrTranscriptRetryNotAllowed) {
		t.Fatalf("retry pending transcript error = %v", err)
	}
}

func attachmentKeyFromURL(t *testing.T, attachmentURL string) string {
	t.Helper()
	parsed, err := url.Parse(attachmentURL)
	if err != nil {
		t.Fatal(err)
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) != 6 {
		t.Fatalf("unexpected attachment URL path %q", parsed.Path)
	}
	return segments[5]
}

func TestCopilotQuotaCountsManagedRequestsByUTCDayAndExemptsBYOK(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE ai_sessions_metadata (
			id varchar(36) PRIMARY KEY,user_id varchar(36) NOT NULL,workspace_id varchar(36) NOT NULL,
			prompt_action varchar(32),created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE ai_sessions_messages (
			id varchar(36) PRIMARY KEY,session_id varchar(36) NOT NULL,role text NOT NULL,created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE ai_action_runs (
			id varchar(36) PRIMARY KEY,user_id varchar(36) NOT NULL,session_id varchar(36),user_message_id varchar(36),
			action_id varchar NOT NULL,status varchar NOT NULL,created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE ai_usage_events (
			id varchar(36) PRIMARY KEY,user_id varchar(36),provider_source varchar NOT NULL,feature_kind varchar NOT NULL,
			task_id varchar,billing_unit_id varchar,session_id varchar,created_at timestamptz NOT NULL DEFAULT now()
		)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("set up Copilot quota schema: %v", err)
		}
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_metadata(id,user_id,workspace_id,prompt_action,created_at) VALUES
		('chat-session','quota-user','workspace','', $1),
		('action-session','quota-user','workspace','summarize', $1),
		('old-session','quota-user','workspace','', $1 - interval '1 day')`, day.Add(10*time.Hour)); err != nil {
		t.Fatalf("seed quota sessions: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_messages(id,session_id,role,created_at) VALUES
		('legacy-chat','chat-session','user',$1),
		('byok-chat','chat-session','user',$1),
		('yesterday-chat','old-session','user',$1 - interval '1 day')`, day.Add(11*time.Hour)); err != nil {
		t.Fatalf("seed quota messages: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_usage_events(id,user_id,provider_source,feature_kind,task_id,billing_unit_id,session_id,created_at) VALUES
		('managed-event','quota-user','fydeos_managed_quota','chat','attempt-1',NULL,'chat-session',$1),
		('byok-event','quota-user','fydeos_byok_selection','chat',NULL,'byok-chat','chat-session',$1)`, day.Add(12*time.Hour)); err != nil {
		t.Fatalf("seed quota usage: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_action_runs(id,user_id,session_id,user_message_id,action_id,status,created_at) VALUES
		('session-run','quota-user','action-session',NULL,'summarize','succeeded',$1),
		('standalone-run','quota-user',NULL,NULL,'rewrite','succeeded',$1)`, day.Add(13*time.Hour)); err != nil {
		t.Fatalf("seed quota action runs: %v", err)
	}
	store := copilot.NewStore(pool)
	used, err := store.CountFydeosManagedRequests(ctx, "quota-user", day.Add(14*time.Hour))
	if err != nil || used != 4 {
		t.Fatalf("managed Copilot requests = %d, %v; want four UTC-day units", used, err)
	}

	resolver := &copilotResolver{Resolver: &Resolver{
		Config:          config.Config{FydeOSManagedCopilot: true},
		CopilotSessions: store,
		MCPCredentials:  enabledCopilotCredentials{},
	}}
	requestContext := auth.WithPrincipal(ctx, &auth.Principal{User: auth.User{ID: "quota-user"}})
	quota, err := resolver.Quota(requestContext, &model.Copilot{})
	if err != nil || quota.Limit == nil || *quota.Limit != copilot.FydeosManagedQuotaLimit || quota.Used != 4 {
		t.Fatalf("managed Copilot quota = %#v, %v", quota, err)
	}
}

func boolPtr(value bool) *bool       { return &value }
func intPtr(value int) *int          { return &value }
func stringPtr(value string) *string { return &value }
