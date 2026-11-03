// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/database/migrations"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresUserUpdateJobsRecoverLeaseAndFenceNewGenerations(t *testing.T) {
	pool := emptyRealtimeTestPool(t)
	ctx := context.Background()
	if err := applyUserRefreshFixture(ctx, pool); err != nil {
		t.Fatalf("apply Go backend migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email) VALUES ('user-1','Initial','user-1@example.test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET name='Changed once' WHERE id='user-1'`); err != nil {
		t.Fatal(err)
	}
	firstStore := NewPostgresUserUpdateRefreshJobStore(pool)
	secondStore := NewPostgresUserUpdateRefreshJobStore(pool)
	first, ok, err := firstStore.Claim(ctx, 150*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("first claim = %#v, %v, want job", first, err)
	}
	owned, err := firstStore.Renew(ctx, first, time.Second)
	if err != nil || !owned {
		t.Fatalf("first lease renewal = %v, %v; want owned", owned, err)
	}
	if _, ok, err := secondStore.Claim(ctx, time.Second); err != nil || ok {
		t.Fatalf("second instance claimed a leased job: ok=%v err=%v", ok, err)
	}
	grantCursor := workspaces.DocGrantRef{WorkspaceID: "workspace-a", DocID: "doc-a"}
	if err := firstStore.Advance(ctx, first, UserUpdateJobState{Grants: true, AfterGrant: &grantCursor}); err != nil {
		t.Fatalf("advance first page: %v", err)
	}
	second, ok, err := secondStore.Claim(ctx, time.Second)
	if err != nil || !ok || !second.State.Grants || second.State.AfterGrant == nil || *second.State.AfterGrant != grantCursor {
		t.Fatalf("reclaimed cursor = %#v, ok=%v err=%v", second, ok, err)
	}
	if err := secondStore.Retry(ctx, second, 0); err != nil {
		t.Fatalf("release job for retry: %v", err)
	}
	retried, ok, err := firstStore.Claim(ctx, time.Second)
	if err != nil || !ok || retried.Attempt != 1 || retried.State.AfterGrant == nil || *retried.State.AfterGrant != grantCursor {
		t.Fatalf("retried job = %#v, ok=%v err=%v", retried, ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET name='Changed twice' WHERE id='user-1'`); err != nil {
		t.Fatal(err)
	}
	owned, err = firstStore.Renew(ctx, retried, time.Second)
	if err != nil || owned {
		t.Fatalf("stale lease renewal = %v, %v; want not owned", owned, err)
	}
	if err := firstStore.Complete(ctx, retried); err != nil {
		t.Fatalf("stale completion should be harmless: %v", err)
	}
	latest, ok, err := secondStore.Claim(ctx, time.Second)
	if err != nil || !ok || latest.Generation != first.Generation+1 || latest.State.Grants || latest.State.AfterWorkspaceID != "" || latest.State.AfterGrant != nil || latest.Attempt != 0 {
		t.Fatalf("latest generation did not reset stale progress: %#v, ok=%v err=%v", latest, ok, err)
	}
	if err := secondStore.Complete(ctx, latest); err != nil {
		t.Fatalf("complete latest job: %v", err)
	}
	if _, ok, err := firstStore.Claim(ctx, time.Second); err != nil || ok {
		t.Fatalf("completed refresh job was not deleted: ok=%v err=%v", ok, err)
	}
}

func TestPostgresUserUpdateJobsRecoverExpiredLeaseAfterProcessExit(t *testing.T) {
	pool := emptyRealtimeTestPool(t)
	ctx := context.Background()
	if err := applyUserRefreshFixture(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email) VALUES ('user-1','Initial','user-1@example.test'); UPDATE users SET avatar_url='https://example.test/a.png' WHERE id='user-1'`); err != nil {
		t.Fatal(err)
	}
	firstProcess := NewPostgresUserUpdateRefreshJobStore(pool)
	secondProcess := NewPostgresUserUpdateRefreshJobStore(pool)
	_, ok, err := firstProcess.Claim(ctx, 50*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("first process claim: ok=%v err=%v", ok, err)
	}
	time.Sleep(75 * time.Millisecond)
	recovered, ok, err := secondProcess.Claim(ctx, time.Second)
	if err != nil || !ok || recovered.UserID != "user-1" || recovered.Generation != 1 {
		t.Fatalf("expired lease was not recovered: %#v ok=%v err=%v", recovered, ok, err)
	}
	if err := secondProcess.Complete(ctx, recovered); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM go_user_update_refresh_jobs`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("remaining jobs = %d, %v; want 0", remaining, err)
	}
}

func TestPostgresUserUpdateJobsMarkDirtyFencesInFlightRefresh(t *testing.T) {
	pool := emptyRealtimeTestPool(t)
	ctx := context.Background()
	if err := applyUserRefreshFixture(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email) VALUES ('user-1','Initial','user-1@example.test')`); err != nil {
		t.Fatal(err)
	}
	store := NewPostgresUserUpdateRefreshJobStore(pool)
	if err := store.MarkDirty(ctx, "user-1"); err != nil {
		t.Fatalf("mark user update refresh job dirty: %v", err)
	}
	job, ok, err := store.Claim(ctx, time.Second)
	if err != nil || !ok {
		t.Fatalf("claim marked job: ok=%v err=%v", ok, err)
	}
	cursor := workspaces.DocGrantRef{WorkspaceID: "workspace-a", DocID: "doc-a"}
	if err := store.Advance(ctx, job, UserUpdateJobState{Grants: true, AfterWorkspaceID: "workspace-z", AfterGrant: &cursor}); err != nil {
		t.Fatalf("advance marked job: %v", err)
	}
	if err := store.MarkDirty(ctx, "user-1"); err != nil {
		t.Fatalf("mark in-flight job dirty: %v", err)
	}
	if err := store.Complete(ctx, job); err != nil {
		t.Fatalf("stale completion should be harmless: %v", err)
	}
	latest, ok, err := store.Claim(ctx, time.Second)
	if err != nil || !ok || latest.Generation != job.Generation+1 || latest.State.Grants || latest.State.AfterWorkspaceID != "" || latest.State.AfterGrant != nil {
		t.Fatalf("new dirty generation did not reset stale progress: %#v ok=%v err=%v", latest, ok, err)
	}
	if err := store.Complete(ctx, latest); err != nil {
		t.Fatalf("complete latest refresh: %v", err)
	}
	if err := store.MarkDirty(ctx, ""); err == nil {
		t.Fatal("MarkDirty accepted an empty user ID")
	}
}

func TestPostgresUserUpdateJobsClaimRowsAcrossWorkersWithoutOverlap(t *testing.T) {
	pool := emptyRealtimeTestPool(t)
	ctx := context.Background()
	if err := applyUserRefreshFixture(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users(id,name,email) VALUES ('user-a','Initial','user-a@example.test'), ('user-b','Initial','user-b@example.test');
		UPDATE users SET name='Changed'`); err != nil {
		t.Fatal(err)
	}
	store := NewPostgresUserUpdateRefreshJobStore(pool)
	claimed := make(chan string, 8)
	errorsFound := make(chan error, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			job, ok, err := store.Claim(ctx, time.Second)
			if err != nil {
				errorsFound <- err
				return
			}
			if ok {
				claimed <- job.UserID
			}
		}()
	}
	workers.Wait()
	close(claimed)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	ids := make(map[string]bool)
	for id := range claimed {
		if ids[id] {
			t.Fatalf("multiple workers claimed %q concurrently", id)
		}
		ids[id] = true
	}
	if len(ids) != 2 || !ids["user-a"] || !ids["user-b"] {
		t.Fatalf("claimed users = %#v, want both pending jobs once", ids)
	}
}

func TestDurableUserUpdateQueueProcessesProducerEnqueuedJob(t *testing.T) {
	pool := emptyRealtimeTestPool(t)
	ctx := context.Background()
	if err := applyUserRefreshFixture(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email) VALUES ('user-1','Initial','user-1@example.test')`); err != nil {
		t.Fatal(err)
	}
	reader := userUpdateReaderFake{workspaceIDs: []string{"workspace-1"}}
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{}, WorkspaceDependencies{
		Members:     workspaceMembersReaderFake{members: []workspaces.Member{{ID: "user-1", InviteID: "user-1", Role: "Owner", Status: "Accepted"}}, count: 1},
		UserUpdates: reader, UserUpdateJobs: NewPostgresUserUpdateRefreshJobStore(pool),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()
	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO connection")
	}
	subscribeRealtimeTopic(t, client, 1, "workspace.members.changed", map[string]any{"workspaceId": "workspace-1"})
	backend.PublishUserUpdated(ctx, "user-1")
	assertRealtimeEvent(t, client.poll(), `"topic":"workspace.members.changed"`, `"reason":"user-updated"`)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var remaining int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM go_user_update_refresh_jobs`).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remaining durable refresh jobs = %d after worker completion deadline; want 0", remaining)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func applyUserRefreshFixture(ctx context.Context, pool *pgxpool.Pool) error {
	return migrations.Apply(ctx, pool)
}
