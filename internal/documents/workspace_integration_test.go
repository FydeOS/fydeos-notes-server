// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

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

	"github.com/Deln0r/ygo"
	"github.com/fydeos/fydeos-notes-server/internal/crdt/yjs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresWorkspaceDocumentRoundTripAndGenerationFence(t *testing.T) {
	pool := workspaceDocumentTestPool(t)
	ctx := context.Background()
	workspaceID := "workspace-" + uuid.NewString()
	docID := "doc-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES ($1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_permission_generations(workspace_id, generation) VALUES ($1, 7)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	base := fixtureUpdate(t, 8101, "base")
	first := fixtureUpdate(t, 8102, "first")
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at) VALUES ($1,$2,$3,now(),now())`, workspaceID, docID, base); err != nil {
		t.Fatal(err)
	}

	repository := NewWorkspaceRepository(pool)
	push, err := repository.PushWorkspace(ctx, "user-1", workspaceID, docID, first, 7)
	if err != nil {
		t.Fatal(err)
	}
	missing, stateVector, diffTimestamp, err := repository.DiffWorkspace(ctx, workspaceID, docID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if diffTimestamp != push.Timestamp {
		t.Fatalf("diff timestamp = %d, want push timestamp %d", diffTimestamp, push.Timestamp)
	}
	want, err := yjs.MergeUpdates([][]byte{base, first})
	if err != nil {
		t.Fatal(err)
	}
	wantDoc, err := yjs.Restore(want, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotDoc, err := yjs.Restore(missing, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotDoc.StateVector(), wantDoc.StateVector(); string(got) != string(want) {
		t.Fatalf("loaded state vector = %x, want %x", got, want)
	}
	if got, want := stateVector, wantDoc.StateVector(); string(got) != string(want) {
		t.Fatalf("reported state vector = %x, want %x", got, want)
	}
	peer := ygo.NewDocWithOptions(ygo.Options{ClientID: 9101})
	if err := ygo.ApplyUpdate(peer, missing); err != nil {
		t.Fatal(err)
	}
	peerDiff, _, _, err := repository.DiffWorkspace(ctx, workspaceID, docID, ygo.EncodeStateVector(peer))
	if err != nil {
		t.Fatal(err)
	}
	if len(peerDiff) != 0 && string(peerDiff) != string([]byte{0, 0}) {
		t.Fatalf("diff for synchronized workspace peer = %x, want empty update", peerDiff)
	}
	timestamps, err := repository.WorkspaceTimestamps(ctx, workspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if timestamps[docID] != push.Timestamp {
		t.Fatalf("workspace timestamp = %d, want %d", timestamps[docID], push.Timestamp)
	}

	if _, err := repository.PushWorkspace(ctx, "user-1", workspaceID, docID, fixtureUpdate(t, 8103, "stale"), 6); err != ErrWorkspacePermissionGenerationChanged {
		t.Fatalf("stale generation push error = %v", err)
	}
	var updateCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM updates WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&updateCount); err != nil {
		t.Fatal(err)
	}
	if updateCount != 1 {
		t.Fatalf("updates after stale generation rejection = %d, want 1", updateCount)
	}
}

func TestPostgresWorkspaceFirstDocumentPushCreatesSnapshot(t *testing.T) {
	pool := workspaceDocumentTestPool(t)
	ctx := context.Background()
	workspaceID := "workspace-" + uuid.NewString()
	docID := "doc-" + uuid.NewString()
	first := fixtureUpdate(t, 8151, "first page update")
	second := fixtureUpdate(t, 8152, "second page update")
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES ($1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_permission_generations(workspace_id, generation) VALUES ($1, 7)`, workspaceID); err != nil {
		t.Fatal(err)
	}

	repository := NewWorkspaceRepository(pool)
	firstPush, err := repository.PushWorkspace(ctx, "user-1", workspaceID, docID, first, 7)
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	var storedSize int
	var snapshotTime time.Time
	if err := pool.QueryRow(ctx, `SELECT blob,size,updated_at FROM snapshots WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&stored, &storedSize, &snapshotTime); err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(first) || storedSize != len(first) {
		t.Fatalf("first document snapshot = %x (size %d), want %x (size %d)", stored, storedSize, first, len(first))
	}
	if firstPush.Timestamp != snapshotTime.UnixMilli() {
		t.Fatalf("first push timestamp = %d, snapshot timestamp = %d", firstPush.Timestamp, snapshotTime.UnixMilli())
	}
	var updateCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM updates WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&updateCount); err != nil {
		t.Fatal(err)
	}
	if updateCount != 0 {
		t.Fatalf("updates after initial document push = %d, want snapshot-only initial state", updateCount)
	}

	secondPush, err := repository.PushWorkspace(ctx, "user-1", workspaceID, docID, second, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM updates WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&updateCount); err != nil {
		t.Fatal(err)
	}
	if updateCount != 1 {
		t.Fatalf("updates after second document push = %d, want 1", updateCount)
	}
	if secondPush.Timestamp <= firstPush.Timestamp {
		t.Fatalf("second push timestamp = %d, want greater than first timestamp %d", secondPush.Timestamp, firstPush.Timestamp)
	}
	if _, _, timestamp, err := repository.DiffWorkspace(ctx, workspaceID, docID, nil); err != nil || timestamp != secondPush.Timestamp {
		t.Fatalf("document diff timestamp = %d, err %v; want %d", timestamp, err, secondPush.Timestamp)
	}
}

func TestPostgresWorkspaceDocumentConcurrentWritesKeepAllYjsUpdates(t *testing.T) {
	pool := workspaceDocumentTestPool(t)
	ctx := context.Background()
	workspaceID := "workspace-" + uuid.NewString()
	docID := "doc-" + uuid.NewString()
	base := fixtureUpdate(t, 8201, "base")
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES ($1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_permission_generations(workspace_id, generation) VALUES ($1, 4)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at) VALUES ($1,$2,$3,now(),now())`, workspaceID, docID, base); err != nil {
		t.Fatal(err)
	}

	repository := NewWorkspaceRepository(pool)
	const writes = 8
	updates := make([][]byte, writes)
	errorsByIndex := make([]error, writes)
	var group sync.WaitGroup
	for i := range writes {
		updates[i] = fixtureUpdate(t, uint64(8300+i), "parallel")
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, errorsByIndex[index] = repository.PushWorkspace(ctx, "user-1", workspaceID, docID, updates[index], 4)
		}(i)
	}
	group.Wait()
	for i, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("concurrent push %d: %v", i, err)
		}
	}

	var count int
	var duplicateTimestamps int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(*) - count(DISTINCT created_at)
		FROM updates WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&count, &duplicateTimestamps); err != nil {
		t.Fatal(err)
	}
	if count != writes || duplicateTimestamps != 0 {
		t.Fatalf("concurrent update rows=%d duplicate timestamps=%d, want %d and 0", count, duplicateTimestamps, writes)
	}
	allUpdates := append([][]byte{base}, updates...)
	want, err := yjs.MergeUpdates(allUpdates)
	if err != nil {
		t.Fatal(err)
	}
	wantDoc, err := yjs.Restore(want, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, _, _, err := repository.DiffWorkspace(ctx, workspaceID, docID, nil)
	if err != nil {
		t.Fatal(err)
	}
	loadedDoc, err := yjs.Restore(loaded, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := loadedDoc.StateVector(), wantDoc.StateVector(); string(got) != string(want) {
		t.Fatalf("concurrent workspace state vector = %x, want %x", got, want)
	}
}

func TestPostgresWorkspaceRootAppendInitializesSnapshotAndRejectsLifecycleChanges(t *testing.T) {
	pool := workspaceDocumentTestPool(t)
	ctx := context.Background()
	workspaceID := "workspace-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES ($1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_permission_generations(workspace_id, generation) VALUES ($1, 3)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	repository := NewWorkspaceRepository(pool)
	root := rootPageUpdate(t, 8401, "doc-1")
	if _, err := repository.PushWorkspace(ctx, "user-1", workspaceID, workspaceID, root, 3); err != nil {
		t.Fatalf("initialize workspace root: %v", err)
	}
	var snapshots, updates int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM snapshots WHERE workspace_id=$1 AND guid=$1),
		       (SELECT count(*) FROM updates WHERE workspace_id=$1 AND guid=$1)`, workspaceID).Scan(&snapshots, &updates); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 || updates != 0 {
		t.Fatalf("root rows after initialization: snapshots=%d updates=%d, want 1 and 0", snapshots, updates)
	}
	profileUpdate := mutateWorkspaceProfile(t, root, 8402, "Old workspace name", "old-avatar")
	profilePush, err := repository.PushWorkspace(ctx, "user-1", workspaceID, workspaceID, profileUpdate, 3)
	if err != nil || !profilePush.WorkspaceMetadataChanged {
		t.Fatalf("initialize workspace profile: result=%+v err=%v", profilePush, err)
	}

	mergedRoot, _, _, err := repository.DiffWorkspace(ctx, workspaceID, workspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	metadataUpdate := mutateWorkspaceRoot(t, mergedRoot, 8403, func(txn *ygo.TransactionMut, _ *ygo.Array, page *ygo.Map) {
		page.Set(txn, "title", "Renamed")
	})
	metadataPush, err := repository.PushWorkspace(ctx, "user-1", workspaceID, workspaceID, metadataUpdate, 3)
	if err != nil || metadataPush.WorkspaceMetadataChanged {
		t.Fatalf("append ordinary root metadata update: result=%+v err=%v", metadataPush, err)
	}
	mergedRoot, _, _, err = repository.DiffWorkspace(ctx, workspaceID, workspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	profileUpdate = mutateWorkspaceProfile(t, mergedRoot, 8404, "New workspace name", "new-avatar")
	profilePush, err = repository.PushWorkspace(ctx, "user-1", workspaceID, workspaceID, profileUpdate, 3)
	if err != nil || !profilePush.WorkspaceMetadataChanged {
		t.Fatalf("append workspace profile update: result=%+v err=%v", profilePush, err)
	}
	var workspaceName, avatarKey string
	if err := pool.QueryRow(ctx, `SELECT name,avatar_key FROM workspaces WHERE id=$1`, workspaceID).Scan(&workspaceName, &avatarKey); err != nil || workspaceName != "New workspace name" || avatarKey != "new-avatar" {
		t.Fatalf("workspace SQL profile projection=%q/%q err=%v", workspaceName, avatarKey, err)
	}
	mergedRoot, _, _, err = repository.DiffWorkspace(ctx, workspaceID, workspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	clearNameUpdate := mutateWorkspaceProfile(t, mergedRoot, 8405, "", "new-avatar")
	profilePush, err = repository.PushWorkspace(ctx, "user-1", workspaceID, workspaceID, clearNameUpdate, 3)
	if err != nil || !profilePush.WorkspaceMetadataChanged {
		t.Fatalf("clear workspace profile name: result=%+v err=%v", profilePush, err)
	}
	if err := pool.QueryRow(ctx, `SELECT name,avatar_key FROM workspaces WHERE id=$1`, workspaceID).Scan(&workspaceName, &avatarKey); err != nil || workspaceName != "" || avatarKey != "new-avatar" {
		t.Fatalf("cleared workspace SQL profile projection=%q/%q err=%v", workspaceName, avatarKey, err)
	}
	mergedRoot, _, _, err = repository.DiffWorkspace(ctx, workspaceID, workspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	trashUpdate := mutateWorkspaceRoot(t, mergedRoot, 8406, func(txn *ygo.TransactionMut, _ *ygo.Array, page *ygo.Map) {
		page.Set(txn, "trash", true)
	})
	if _, err := repository.PushWorkspace(ctx, "user-1", workspaceID, workspaceID, trashUpdate, 3); err != ErrWorkspaceRootCommandRequired {
		t.Fatalf("ordinary root trash update error = %v, want ErrWorkspaceRootCommandRequired", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM updates WHERE workspace_id=$1 AND guid=$1`, workspaceID).Scan(&updates); err != nil {
		t.Fatal(err)
	}
	if updates != 4 {
		t.Fatalf("root updates after rejected lifecycle change = %d, want 4", updates)
	}
}

func TestPostgresWorkspaceDocLifecycleEnforcesQuotaAndDeletesRelatedRows(t *testing.T) {
	pool := workspaceDocumentTestPool(t)
	ctx := context.Background()
	workspaceID := "workspace-" + uuid.NewString()
	docID := "doc-" + uuid.NewString()
	root := rootPageUpdate(t, 8501, docID)
	doc := fixtureUpdate(t, 8502, "document")
	if _, err := pool.Exec(ctx, `CREATE TABLE users (id varchar PRIMARY KEY);
		CREATE TABLE workspace_members (workspace_id varchar NOT NULL, user_id varchar NOT NULL, role text NOT NULL, state text NOT NULL);
		CREATE TABLE workspace_invitations (workspace_id varchar NOT NULL, status text NOT NULL);
		CREATE TABLE effective_workspace_quota_states (
			workspace_id varchar PRIMARY KEY, owner_user_id varchar, uses_owner_quota boolean NOT NULL,
			seat_limit integer NOT NULL, storage_quota bigint NOT NULL, readonly boolean NOT NULL,
			known boolean NOT NULL, stale boolean NOT NULL, stale_after timestamptz
		);
		CREATE TABLE entitlements (
			target_type text NOT NULL, target_id varchar NOT NULL, source text NOT NULL, status text NOT NULL,
			plan text NOT NULL, quantity integer, starts_at timestamptz, expires_at timestamptz,
			grace_until timestamptz, signed_payload bytea, updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE blobs (workspace_id varchar NOT NULL, size bigint NOT NULL, deleted_at timestamptz, status text NOT NULL, reservation_expires_at timestamptz);
		CREATE TABLE comment_attachments (workspace_id varchar NOT NULL, size bigint NOT NULL, deleted_at timestamptz, status text NOT NULL, reservation_expires_at timestamptz);
		CREATE TABLE comments (workspace_id varchar NOT NULL, doc_id varchar NOT NULL);
		CREATE TABLE replies (workspace_id varchar NOT NULL, doc_id varchar NOT NULL);
		CREATE TABLE snapshot_histories (workspace_id varchar NOT NULL, guid varchar NOT NULL);
		CREATE TABLE doc_grants (workspace_id varchar NOT NULL, doc_id varchar NOT NULL);
		CREATE TABLE doc_access_policies (workspace_id varchar NOT NULL, doc_id varchar NOT NULL);
		CREATE TABLE workspace_pages (workspace_id varchar NOT NULL, page_id varchar NOT NULL);`); err != nil {
		t.Fatalf("create lifecycle support tables: %v", err)
	}
	for _, fixture := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO workspaces(id) VALUES($1)`, []any{workspaceID}},
		{`INSERT INTO users(id) VALUES('owner-1')`, nil},
		{`INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES($1,'owner-1','owner','active')`, []any{workspaceID}},
		{`INSERT INTO workspace_sync_permission_generations(workspace_id,generation) VALUES($1,5)`, []any{workspaceID}},
		{`INSERT INTO snapshots(workspace_id,guid,blob,size,created_at,updated_at,created_by,updated_by) VALUES($1,$1,$2,$3,now(),now(),'owner-1','owner-1')`, []any{workspaceID, root, len(root)}},
		{`INSERT INTO snapshots(workspace_id,guid,blob,size,created_at,updated_at,created_by,updated_by) VALUES($1,$2,$3,$4,now(),now(),'owner-1','owner-1')`, []any{workspaceID, docID, doc, len(doc)}},
		{`INSERT INTO updates(workspace_id,guid,blob,created_at,created_by) VALUES($1,$2,$3,now(),'owner-1')`, []any{workspaceID, docID, fixtureUpdate(t, 8503, "old update")}},
		{`INSERT INTO snapshot_histories(workspace_id,guid) VALUES($1,$2)`, []any{workspaceID, docID}},
		{`INSERT INTO comments(workspace_id,doc_id) VALUES($1,$2)`, []any{workspaceID, docID}},
		{`INSERT INTO replies(workspace_id,doc_id) VALUES($1,$2)`, []any{workspaceID, docID}},
		{`INSERT INTO doc_grants(workspace_id,doc_id) VALUES($1,$2)`, []any{workspaceID, docID}},
		{`INSERT INTO doc_access_policies(workspace_id,doc_id) VALUES($1,$2)`, []any{workspaceID, docID}},
		{`INSERT INTO workspace_pages(workspace_id,page_id) VALUES($1,$2)`, []any{workspaceID, docID}},
		{`INSERT INTO effective_workspace_quota_states(workspace_id,owner_user_id,uses_owner_quota,seat_limit,storage_quota,readonly,known,stale,stale_after)
		 VALUES($1,'owner-1',false,10,1024,false,true,false,statement_timestamp()+interval '1 hour')`, []any{workspaceID}},
	} {
		if _, err := pool.Exec(ctx, fixture.query, fixture.args...); err != nil {
			t.Fatalf("insert lifecycle fixture: %v", err)
		}
	}

	repository := NewWorkspaceRepositoryWithEntitlementConfig(pool, "cloud")
	trashed, err := repository.ApplyWorkspaceDocLifecycle(ctx, "owner-1", workspaceID, docID, WorkspaceDocTrash, 5)
	if err != nil {
		t.Fatalf("trash document: %v", err)
	}
	if len(trashed.RootUpdate) == 0 || trashed.Timestamp <= 0 {
		t.Fatalf("trash result = %+v, want root delta and timestamp", trashed)
	}
	assertWorkspaceRootPage(t, repository, workspaceID, docID, true, true)

	if _, err := pool.Exec(ctx, `UPDATE effective_workspace_quota_states SET stale=true WHERE workspace_id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ApplyWorkspaceDocLifecycle(ctx, "owner-1", workspaceID, docID, WorkspaceDocRestore, 5); err != nil {
		t.Fatalf("restore should resolve canonical default quota despite stale projection: %v", err)
	}
	if _, err := repository.ApplyWorkspaceDocLifecycle(ctx, "owner-1", workspaceID, docID, WorkspaceDocTrash, 5); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, fmt.Sprintf("overflow-%d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES($1,$2,'member','active')`, workspaceID, fmt.Sprintf("overflow-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.ApplyWorkspaceDocLifecycle(ctx, "owner-1", workspaceID, docID, WorkspaceDocRestore, 5); !errors.Is(err, ErrWorkspaceQuotaReadonly) {
		t.Fatalf("restore over seat quota error = %v, want ErrWorkspaceQuotaReadonly", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id=$1 AND user_id <> 'owner-1'`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blobs(workspace_id,size,status) VALUES($1,$2,'completed')`, workspaceID, int64(10*1024*1024*1024+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ApplyWorkspaceDocLifecycle(ctx, "owner-1", workspaceID, docID, WorkspaceDocRestore, 5); !errors.Is(err, ErrWorkspaceQuotaReadonly) {
		t.Fatalf("restore over storage quota error = %v, want ErrWorkspaceQuotaReadonly", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM blobs WHERE workspace_id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	restored, err := repository.ApplyWorkspaceDocLifecycle(ctx, "owner-1", workspaceID, docID, WorkspaceDocRestore, 5)
	if err != nil {
		t.Fatalf("restore within quota: %v", err)
	}
	if restored.Timestamp <= trashed.Timestamp {
		t.Fatalf("restore timestamp %d did not advance beyond trash timestamp %d", restored.Timestamp, trashed.Timestamp)
	}
	assertWorkspaceRootPage(t, repository, workspaceID, docID, false, false)

	if _, err := repository.ApplyWorkspaceDocLifecycle(ctx, "owner-1", workspaceID, docID, WorkspaceDocDelete, 4); !errors.Is(err, ErrWorkspacePermissionGenerationChanged) {
		t.Fatalf("delete with stale generation error = %v, want ErrWorkspacePermissionGenerationChanged", err)
	}
	deleted, err := repository.ApplyWorkspaceDocLifecycle(ctx, "owner-1", workspaceID, docID, WorkspaceDocDelete, 5)
	if err != nil {
		t.Fatalf("delete document: %v", err)
	}
	if deleted.Timestamp <= restored.Timestamp {
		t.Fatalf("delete timestamp %d did not advance beyond restore timestamp %d", deleted.Timestamp, restored.Timestamp)
	}
	rootUpdate, _, _, err := repository.DiffWorkspace(ctx, workspaceID, workspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootDoc := ygo.NewDoc()
	if err := ygo.ApplyUpdate(rootDoc, rootUpdate); err != nil {
		t.Fatal(err)
	}
	allPages, _, err := projectWorkspaceRoot(rootDoc)
	if err != nil {
		t.Fatalf("project root after document delete: %v", err)
	}
	if _, exists := allPages[docID]; exists {
		t.Fatalf("deleted document %q remains in workspace root", docID)
	}
	var deletedRows int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM snapshots WHERE workspace_id=$1 AND guid=$2)
		+ (SELECT count(*) FROM updates WHERE workspace_id=$1 AND guid=$2)
		+ (SELECT count(*) FROM snapshot_histories WHERE workspace_id=$1 AND guid=$2)
		+ (SELECT count(*) FROM comments WHERE workspace_id=$1 AND doc_id=$2)
		+ (SELECT count(*) FROM replies WHERE workspace_id=$1 AND doc_id=$2)
		+ (SELECT count(*) FROM doc_grants WHERE workspace_id=$1 AND doc_id=$2)
		+ (SELECT count(*) FROM doc_access_policies WHERE workspace_id=$1 AND doc_id=$2)
		+ (SELECT count(*) FROM workspace_pages WHERE workspace_id=$1 AND page_id=$2)`, workspaceID, docID).Scan(&deletedRows); err != nil {
		t.Fatal(err)
	}
	if deletedRows != 0 {
		t.Fatalf("related document rows after delete = %d, want 0", deletedRows)
	}
}

func assertWorkspaceRootPage(t *testing.T, repository *WorkspaceRepository, workspaceID, docID string, trashed, hasTrashDate bool) {
	t.Helper()
	update, _, _, err := repository.DiffWorkspace(context.Background(), workspaceID, workspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := ygo.NewDoc()
	if err := ygo.ApplyUpdate(root, update); err != nil {
		t.Fatal(err)
	}
	meta := ygo.NewMap(root, "meta")
	pages, ok := meta.Get("pages").(*ygo.Array)
	if !ok {
		t.Fatalf("workspace root pages type = %T, want array", meta.Get("pages"))
	}
	for index := uint64(0); index < pages.Len(); index++ {
		page, ok := pages.Get(index).(*ygo.Map)
		if !ok {
			t.Fatalf("workspace root page %d type = %T, want map", index, pages.Get(index))
		}
		if page.Get("id") != docID {
			continue
		}
		if page.Get("trash") != trashed || (page.Get("trashDate") != nil) != hasTrashDate {
			t.Fatalf("workspace root metadata for %s: trash=%#v trashDate=%#v", docID, page.Get("trash"), page.Get("trashDate"))
		}
		return
	}
	t.Fatalf("workspace root has no page %q", docID)
}

func TestPostgresRestoreQuotaResolvesOwnerUsageAndWorkspaceCommercialQuota(t *testing.T) {
	pool := workspaceDocumentTestPool(t)
	ctx := context.Background()
	currentWorkspaceID := "quota-current-" + uuid.NewString()
	ownerWorkspaceID := "quota-owner-" + uuid.NewString()
	teamWorkspaceID := "quota-team-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE users (id varchar PRIMARY KEY);
		CREATE TABLE workspace_members (workspace_id varchar NOT NULL, user_id varchar NOT NULL, role text NOT NULL, state text NOT NULL);
		CREATE TABLE workspace_invitations (workspace_id varchar NOT NULL, status text NOT NULL);
		CREATE TABLE entitlements (
			target_type text NOT NULL, target_id varchar NOT NULL, source text NOT NULL, status text NOT NULL,
			plan text NOT NULL, quantity integer, starts_at timestamptz, expires_at timestamptz,
			grace_until timestamptz, signed_payload bytea, updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE blobs (
			workspace_id varchar NOT NULL, size bigint NOT NULL, deleted_at timestamptz, status text NOT NULL,
			reservation_expires_at timestamptz
		);
		CREATE TABLE comment_attachments (
			workspace_id varchar NOT NULL, size bigint NOT NULL, deleted_at timestamptz, status text NOT NULL,
			reservation_expires_at timestamptz
		);`); err != nil {
		t.Fatalf("create restore quota schema: %v", err)
	}
	for _, workspaceID := range []string{currentWorkspaceID, ownerWorkspaceID, teamWorkspaceID} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES($1)`, workspaceID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id) VALUES('quota-owner')`); err != nil {
		t.Fatal(err)
	}
	for _, workspaceID := range []string{currentWorkspaceID, ownerWorkspaceID, teamWorkspaceID} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES($1,'quota-owner','owner','active')`, workspaceID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO entitlements(target_type,target_id,source,status,plan)
		VALUES('user','quota-owner','cloud_subscription','active','pro')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO entitlements(target_type,target_id,source,status,plan,quantity)
		VALUES('workspace',$1,'cloud_subscription','active','team',1)`, teamWorkspaceID); err != nil {
		t.Fatal(err)
	}
	const gib = int64(1024 * 1024 * 1024)
	for _, fixture := range []struct {
		workspaceID string
		size        int64
	}{{currentWorkspaceID, 60 * gib}, {ownerWorkspaceID, 50 * gib}, {teamWorkspaceID, 200 * gib}} {
		if _, err := pool.Exec(ctx, `INSERT INTO blobs(workspace_id,size,status) VALUES($1,$2,'completed')`, fixture.workspaceID, fixture.size); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blobs(workspace_id,size,status,reservation_expires_at) VALUES($1,$2,'pending',clock_timestamp()-interval '1 minute')`, currentWorkspaceID, 100*gib); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO comment_attachments(workspace_id,size,status) VALUES($1,$2,'completed')`, currentWorkspaceID, 41*gib); err != nil {
		t.Fatal(err)
	}

	repository := NewWorkspaceRepositoryWithEntitlementConfig(pool, "cloud")
	checkQuota := func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		return repository.CheckWorkspaceMutationQuotaInTx(ctx, tx, currentWorkspaceID)
	}
	if err := checkQuota(); !errors.Is(err, ErrWorkspaceQuotaReadonly) {
		t.Fatalf("owner quota should include unentitled workspaces and comment attachments: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE blobs SET deleted_at=clock_timestamp() WHERE workspace_id=$1`, ownerWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE comment_attachments SET deleted_at=clock_timestamp() WHERE workspace_id=$1`, currentWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := checkQuota(); err != nil {
		t.Fatalf("commercial workspace storage and expired reservations must not consume owner quota: %v", err)
	}
}

func workspaceDocumentTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse NOTES_TEST_DATABASE_URL: %v", err)
	}
	if parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		t.Fatal("integration tests require a loopback PostgreSQL host")
	}
	if strings.Trim(parsed.Path, "/") != "notes_test" {
		t.Fatal("integration tests require a disposable database named notes_test")
	}
	adminPool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect integration PostgreSQL: %v", err)
	}
	schema := "notes_workspace_doc_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("parse integration PostgreSQL config: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("open schema-scoped PostgreSQL pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
	})
	if _, err := pool.Exec(context.Background(), `
		CREATE TABLE workspaces (id varchar PRIMARY KEY, name text, avatar_key text);
		CREATE TABLE workspace_sync_permission_generations (workspace_id varchar PRIMARY KEY, generation bigint NOT NULL DEFAULT 0);
		CREATE TABLE snapshots (
			workspace_id varchar NOT NULL, guid varchar NOT NULL, blob bytea NOT NULL,
			size bigint NOT NULL DEFAULT 0,
			created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL,
			created_by varchar, updated_by varchar,
			PRIMARY KEY (workspace_id, guid)
		);
		CREATE TABLE updates (
			workspace_id varchar NOT NULL, guid varchar NOT NULL, blob bytea NOT NULL,
			created_at timestamptz NOT NULL, created_by varchar,
			PRIMARY KEY (workspace_id, guid, created_at)
		);`); err != nil {
		t.Fatalf("create workspace document test schema: %v", err)
	}
	return pool
}
