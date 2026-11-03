// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestManagedQuotaReservationSerializesConcurrentRequestsAndReleasesFailures(t *testing.T) {
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsedURL, err := url.Parse(databaseURL)
	if err != nil || (parsedURL.Hostname() != "localhost" && parsedURL.Hostname() != "127.0.0.1" && parsedURL.Hostname() != "::1") {
		t.Fatal("managed quota integration test requires a loopback PostgreSQL URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "copilot_quota_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	parsed, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	parsed.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET search_path TO `+schema)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, parsed)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop quota test schema: %v", err)
		}
		admin.Close()
	})
	for _, statement := range []string{
		`CREATE TABLE ai_sessions_metadata(id text PRIMARY KEY,user_id text NOT NULL,workspace_id text NOT NULL,doc_id text,prompt_name varchar(32) NOT NULL,prompt_action text,pinned boolean NOT NULL DEFAULT false,title text,parent_session_id text,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),deleted_at timestamptz,"messageCost" integer NOT NULL DEFAULT 0,focus jsonb)`,
		`CREATE TABLE ai_sessions_messages(id text PRIMARY KEY,session_id text NOT NULL,role text NOT NULL,created_at timestamptz NOT NULL)`,
		`CREATE TABLE ai_action_runs(id text PRIMARY KEY,user_id text NOT NULL,session_id text,user_message_id text,action_id text,status text,created_at timestamptz NOT NULL)`,
		`CREATE TABLE ai_transcript_tasks(id text PRIMARY KEY,user_id text NOT NULL,status text NOT NULL)`,
		`CREATE TABLE ai_usage_events(id text PRIMARY KEY,workspace_id text,local_workspace_id text,user_id text,provider text NOT NULL,provider_source text NOT NULL,feature_kind text NOT NULL,model text,session_id text,task_id text,billing_unit_id text,prompt_tokens integer NOT NULL DEFAULT 0,completion_tokens integer NOT NULL DEFAULT 0,total_tokens integer NOT NULL DEFAULT 0,cached_tokens integer NOT NULL DEFAULT 0,created_at timestamptz NOT NULL)`,
		`CREATE TABLE entitlements(target_type text NOT NULL,target_id text NOT NULL,source text NOT NULL,status text NOT NULL,plan text NOT NULL,quantity integer,starts_at timestamptz,expires_at timestamptz,grace_until timestamptz,signed_payload bytea,updated_at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_usage_events(id,user_id,provider,provider_source,feature_kind,billing_unit_id,created_at)
		VALUES ('cloud-image-1','quota-user','openai','affine_cloud','image','assistant-image-1',now()),
		       ('cloud-image-duplicate','quota-user','openai','affine_cloud','image','assistant-image-1',now())`); err != nil {
		t.Fatal(err)
	}
	if used, err := countUserQuotaCost(ctx, pool, "quota-user"); err != nil || used != 1 {
		t.Fatalf("cloud image quota cost=%d err=%v; want one unit per billing unit", used, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_action_runs(id,user_id,session_id,action_id,status,created_at)
		SELECT 'limit-action-'||n,'limit-user','session-1','action','succeeded',now() FROM generate_series(1,9) AS n`); err != nil {
		t.Fatal(err)
	}
	routes := NewTranscriptRoutes(pool, config.Config{}, nil)
	reservations := make(chan struct {
		id  string
		err error
	}, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			id, err := routes.ReserveCloudImageQuota(ctx, "limit-user", "workspace-1", "session-1", fmt.Sprintf("image-turn-%d", i), "openai", "gpt-image-1")
			reservations <- struct {
				id  string
				err error
			}{id: id, err: err}
		}(i)
	}
	var reservationID string
	var rejected int
	for i := 0; i < 2; i++ {
		result := <-reservations
		if result.err == nil {
			reservationID = result.id
		} else if errors.Is(result.err, ErrManagedQuotaExceeded) {
			rejected++
		} else {
			t.Fatalf("reserve concurrent cloud image: %v", result.err)
		}
	}
	if reservationID == "" || rejected != 1 {
		t.Fatalf("image reservation=%q rejected=%d", reservationID, rejected)
	}
	if used, err := countUserQuotaCost(ctx, pool, "limit-user"); err != nil || used != 10 {
		t.Fatalf("quota with image reservation=%d err=%v; want 10", used, err)
	}
	if err := routes.ReleaseCloudImageQuota(ctx, reservationID); err != nil {
		t.Fatal(err)
	}
	if used, err := countUserQuotaCost(ctx, pool, "limit-user"); err != nil || used != 9 {
		t.Fatalf("quota after image reservation release=%d err=%v; want 9", used, err)
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_metadata(id,user_id,workspace_id,prompt_name,prompt_action,created_at)
		SELECT 'session-'||n,'quota-user','workspace-1','chat','',$1 FROM generate_series(1,99) AS n`, day.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_messages(id,session_id,role,created_at)
		SELECT 'message-'||n,'session-'||n,'user',$1 FROM generate_series(1,99) AS n`, day.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	var wg sync.WaitGroup
	results := make(chan struct {
		messageID string
		id        string
		err       error
	}, 2)
	for _, messageID := range []string{"message-100", "message-101"} {
		wg.Add(1)
		go func(messageID string) {
			defer wg.Done()
			id, err := store.ReserveFydeosManagedRequest(ctx, "quota-user", "workspace-1", "session-1", messageID, "openai", "model-1", false, day.Add(3*time.Hour))
			results <- struct {
				messageID string
				id        string
				err       error
			}{messageID: messageID, id: id, err: err}
		}(messageID)
	}
	wg.Wait()
	close(results)
	var acceptedID, acceptedMessage string
	var denied int
	for result := range results {
		if result.err == nil {
			acceptedID, acceptedMessage = result.id, result.messageID
		} else if errors.Is(result.err, ErrManagedQuotaExceeded) {
			denied++
		} else {
			t.Fatalf("reserve concurrent request %s: %v", result.messageID, result.err)
		}
	}
	if acceptedID == "" || denied != 1 {
		t.Fatalf("accepted reservation=%q denied=%d", acceptedID, denied)
	}
	if used, err := store.CountFydeosManagedRequests(ctx, "quota-user", day.Add(4*time.Hour)); err != nil || used != FydeosManagedQuotaLimit {
		t.Fatalf("quota after concurrent reservations = %d, %v; want %d", used, err, FydeosManagedQuotaLimit)
	}
	var workspaceID, localWorkspaceID *string
	if err := pool.QueryRow(ctx, `SELECT workspace_id,local_workspace_id FROM ai_usage_events WHERE id=$1`, acceptedID).Scan(&workspaceID, &localWorkspaceID); err != nil || workspaceID == nil || *workspaceID != "workspace-1" || localWorkspaceID != nil {
		t.Fatalf("canonical quota scope = %v/%v, err=%v", workspaceID, localWorkspaceID, err)
	}
	if _, err := store.ReserveFydeosManagedRequest(ctx, "quota-user", "workspace-1", "session-1", "message-102", "openai", "model-1", false, day.Add(4*time.Hour)); !errors.Is(err, ErrManagedQuotaExceeded) {
		t.Fatalf("101st managed request error = %v", err)
	}
	if err := store.ReleaseFydeosManagedRequest(ctx, acceptedID); err != nil {
		t.Fatal(err)
	}
	if used, err := store.CountFydeosManagedRequests(ctx, "quota-user", day.Add(4*time.Hour)); err != nil || used != FydeosManagedQuotaLimit-1 {
		t.Fatalf("quota after failed request release = %d, %v", used, err)
	}
	retryID, err := store.ReserveFydeosManagedRequest(ctx, "quota-user", "workspace-1", "session-1", acceptedMessage, "openai", "model-1", false, day.Add(4*time.Hour))
	if err != nil || retryID != acceptedID {
		t.Fatalf("retry reservation = %q, %v; want original %q", retryID, err, acceptedID)
	}
	if used, err := store.CountFydeosManagedRequests(ctx, "quota-user", day.Add(4*time.Hour)); err != nil || used != FydeosManagedQuotaLimit {
		t.Fatalf("quota after retry reservation = %d, %v", used, err)
	}
	if err := store.ReleaseFydeosManagedRequest(ctx, retryID); err != nil {
		t.Fatal(err)
	}
	docID := "document-1"
	actionSession, err := store.Create(ctx, CreateSessionInput{
		UserID: "quota-user", WorkspaceID: "workspace-1", DocID: &docID,
		PromptName: "Generate a caption", ManagedQuota: true,
	}, false)
	if err != nil || actionSession.Action == nil || *actionSession.Action != "Generate a caption" {
		t.Fatalf("managed action session = %#v, %v", actionSession, err)
	}
	if used, err := store.CountFydeosManagedRequests(ctx, "quota-user", day.Add(4*time.Hour)); err != nil || used != FydeosManagedQuotaLimit {
		t.Fatalf("quota after action session creation = %d, %v; want %d", used, err, FydeosManagedQuotaLimit)
	}
	if _, err := store.Create(ctx, CreateSessionInput{
		UserID: "quota-user", WorkspaceID: "workspace-1", DocID: &docID,
		PromptName: "Generate a caption", ManagedQuota: true,
	}, false); !errors.Is(err, ErrManagedQuotaExceeded) {
		t.Fatalf("101st action session error = %v", err)
	}

	personalID, err := store.ReserveFydeosManagedRequest(ctx, "personal-user", "personal-workspace", "personal-session", "personal-message", "openai", "model-1", true, day.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT workspace_id,local_workspace_id FROM ai_usage_events WHERE id=$1`, personalID).Scan(&workspaceID, &localWorkspaceID); err != nil || workspaceID != nil || localWorkspaceID == nil || *localWorkspaceID != "personal-workspace" {
		t.Fatalf("personal quota scope = %v/%v, err=%v", workspaceID, localWorkspaceID, err)
	}
}
