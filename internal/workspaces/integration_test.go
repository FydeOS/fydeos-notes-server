// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/database/migrations"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// testPool returns a pool scoped to a fresh, fully migrated schema.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if h := parsed.Hostname(); h != "localhost" && h != "127.0.0.1" && h != "::1" {
		t.Fatal("integration tests require a loopback PostgreSQL host")
	}
	if strings.Trim(parsed.Path, "/") != "notes_test" {
		t.Fatal("integration tests require a disposable database named notes_test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "notes_workspaces_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func addUser(t *testing.T, pool *pgxpool.Pool, id string) string {
	mustExec(t, pool, `INSERT INTO users(id,name,email) VALUES($1,$1,$2)`, id, id+"@example.test")
	return id
}

func addMember(t *testing.T, pool *pgxpool.Pool, ws, user, role string) {
	mustExec(t, pool, `INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES($1,$2,$3,'active')`, ws, user, role)
}

func addTeamEntitlement(t *testing.T, pool *pgxpool.Pool, ws string) {
	mustExec(t, pool, `INSERT INTO entitlements(id,target_type,target_id,source,plan,status,quantity,starts_at) VALUES($1,'workspace',$2,'cloud_subscription','team','active',10,now()-interval '1 day')`, uuid.NewString(), ws)
}

func setKnownQuota(t *testing.T, pool *pgxpool.Pool, ws string, usesOwner bool) {
	mustExec(t, pool, `INSERT INTO effective_workspace_quota_states(workspace_id,plan,seat_limit,blob_limit,storage_quota,history_period_seconds,known,stale,uses_owner_quota)
		VALUES($1,'team',10,1,1,1,true,false,$2) ON CONFLICT (workspace_id) DO UPDATE SET known=true, stale=false, uses_owner_quota=$2`, ws, usesOwner)
}

func TestIntegrationWorkspaceLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRepository(pool)
	owner := addUser(t, pool, "owner")
	stranger := addUser(t, pool, "stranger")
	member := addUser(t, pool, "member")
	admin := addUser(t, pool, "admin")

	ws, err := r.CreateForUser(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.GetForUser(ctx, ws.ID, owner)
	if err != nil || got.Team || got.Initialized || got.OwnerID != owner {
		t.Fatalf("GetForUser = %+v, %v", got, err)
	}
	var vis string
	var sharing bool
	if err := pool.QueryRow(ctx, `SELECT visibility, sharing_enabled FROM workspace_access_policies WHERE workspace_id=$1`, ws.ID).Scan(&vis, &sharing); err != nil || vis != "private" || !sharing {
		t.Fatalf("policy = %s %v %v", vis, sharing, err)
	}
	if _, err := r.GetForUser(ctx, ws.ID, stranger); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("stranger = %v", err)
	}
	if _, err := r.GetForUser(ctx, "missing", owner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}

	clock := time.Now()
	r.now = func() time.Time { return clock }
	if err := r.UpsertRootDocument(ctx, ws.ID, owner, []byte("new")); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return clock.Add(-time.Hour) }
	if err := r.UpsertRootDocument(ctx, ws.ID, owner, []byte("old")); err != nil {
		t.Fatal(err)
	}
	var blob []byte
	if err := pool.QueryRow(ctx, `SELECT blob FROM snapshots WHERE workspace_id=$1 AND guid=$1`, ws.ID).Scan(&blob); err != nil || string(blob) != "new" {
		t.Fatalf("root snapshot = %q %v", blob, err)
	}
	if got, _ := r.GetForUser(ctx, ws.ID, owner); !got.Initialized {
		t.Fatal("workspace not initialized after root upsert")
	}

	addMember(t, pool, ws.ID, member, "member")
	addMember(t, pool, ws.ID, admin, "admin")
	yes := true
	if _, err := r.UpdateForUser(ctx, ws.ID, member, SettingsUpdate{Public: &yes}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("member update = %v", err)
	}
	if _, err := r.UpdateForUser(ctx, ws.ID, owner, SettingsUpdate{Public: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT visibility, sharing_enabled FROM workspace_access_policies WHERE workspace_id=$1`, ws.ID).Scan(&vis, &sharing); err != nil || vis != "public" || !sharing {
		t.Fatalf("updated policy = %s %v %v", vis, sharing, err)
	}

	mustExec(t, pool, `INSERT INTO ai_sessions_metadata(id,user_id,workspace_id,prompt_name) VALUES('s1',$1,$2,'p')`, stranger, ws.ID)
	if _, err := r.DeleteForUser(ctx, ws.ID, admin); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("admin delete = %v", err)
	}
	users, err := r.DeleteForUser(ctx, ws.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{admin, member, owner, stranger}
	sort.Strings(want)
	if fmt.Sprint(users) != fmt.Sprint(want) {
		t.Fatalf("deleted users = %v", users)
	}
	var jobUsers []string
	if err := pool.QueryRow(ctx, `SELECT user_ids FROM workspace_storage_cleanup_jobs WHERE workspace_id=$1`, ws.ID).Scan(&jobUsers); err != nil || fmt.Sprint(jobUsers) != fmt.Sprint(want) {
		t.Fatalf("cleanup job = %v %v", jobUsers, err)
	}
	if exists, _ := workspaceExists(ctx, pool, ws.ID); exists {
		t.Fatal("workspace not deleted")
	}
}

type bumpingQuerier struct {
	*pgxpool.Pool
	workspace string
}

func (b bumpingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "workspace_access_policies") {
		_, _ = b.Pool.Exec(ctx, `INSERT INTO workspace_sync_permission_generations(workspace_id,generation) VALUES($1,1)
			ON CONFLICT (workspace_id) DO UPDATE SET generation=workspace_sync_permission_generations.generation+1`, b.workspace)
	}
	return b.Pool.QueryRow(ctx, sql, args...)
}

func TestIntegrationAuthorization(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRepositoryWithDeployment(pool, "cloud")
	auth := NewAuthorizationService(r)
	owner := addUser(t, pool, "owner")
	member := addUser(t, pool, "member")
	outsider := addUser(t, pool, "outsider")
	ws, err := r.CreateForUser(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	addMember(t, pool, ws.ID, member, "member")
	mustExec(t, pool, `INSERT INTO doc_access_policies(workspace_id,doc_id,member_default_role) VALUES($1,'doc','reader')`, ws.ID)

	a, err := auth.AuthorizeDocument(ctx, member, ws.ID, "doc")
	if err != nil || !a.Read || !a.HistoryRead || !a.UsersRead || a.Update {
		t.Fatalf("member reader default = %+v %v", a, err)
	}
	mustExec(t, pool, `INSERT INTO doc_grants(workspace_id,doc_id,principal_type,principal_id,role) VALUES($1,'doc','user',$2,'editor'),($1,'doc','user',$3,'manager')`, ws.ID, member, outsider)
	if a, _ := auth.AuthorizeDocument(ctx, member, ws.ID, "doc"); !a.Update {
		t.Fatal("explicit editor ignored")
	}
	if a, _ := auth.AuthorizeDocument(ctx, outsider, ws.ID, "doc"); !a.Update || a.Manage {
		t.Fatalf("external grant = %+v", a)
	}
	mustExec(t, pool, `UPDATE workspace_access_policies SET sharing_enabled=false WHERE workspace_id=$1`, ws.ID)
	if a, _ := auth.AuthorizeDocument(ctx, outsider, ws.ID, "doc"); a.Read {
		t.Fatal("external grant with sharing off")
	}

	if _, err := auth.GetWorkspaceAccess(ctx, outsider, ws.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("private workspace = %v", err)
	}
	mustExec(t, pool, `UPDATE workspace_access_policies SET sharing_enabled=true, visibility='public' WHERE workspace_id=$1`, ws.ID)
	access, err := auth.GetWorkspaceAccess(ctx, outsider, ws.ID)
	if err != nil || access.Role != "External" || access.Permissions["Workspace_Sync"] {
		t.Fatalf("public workspace = %+v %v", access, err)
	}
	addTeamEntitlement(t, pool, ws.ID)
	if _, err := auth.GetWorkspaceAccess(ctx, owner, ws.ID); !errors.Is(err, ErrQuotaStateUnknown) {
		t.Fatalf("unknown team = %v", err)
	}
	if access, err := auth.GetWorkspaceRolePermissions(ctx, owner, ws.ID); err != nil || access.Role != "Owner" || !access.Permissions["Workspace_Administrators_Manage"] {
		t.Fatalf("role permissions = %+v %v", access, err)
	}

	sync, err := auth.AuthorizeWorkspaceSync(ctx, member, ws.ID, []string{"doc"})
	if err != nil || !sync.CanSync || !sync.Documents["doc"].Update {
		t.Fatalf("sync = %+v %v", sync, err)
	}
	gen, err := r.SyncPermissionGeneration(ctx, ws.ID)
	if err != nil || gen != sync.Generation {
		t.Fatalf("generation = %d %v (snapshot %d)", gen, err, sync.Generation)
	}

	bumping := &Repository{db: bumpingQuerier{Pool: pool, workspace: ws.ID}, deployment: "selfhosted"}
	if _, err := bumping.ReadWorkspaceSyncSnapshot(ctx, member, ws.ID, nil); !errors.Is(err, ErrPermissionGenerationChanged) {
		t.Fatalf("bumped snapshot = %v", err)
	}
	if _, err := r.ReadWorkspaceSyncSnapshot(ctx, member, "missing", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing workspace snapshot = %v", err)
	}

	reader := NewDocumentReadService(r, nil)
	batch, err := reader.ReadDocumentPermissionsBatch(ctx, member, ws.ID, []string{"b", "a", "a", ""})
	if err != nil || len(batch) != 2 || batch["a"] == nil || batch["b"] == nil {
		t.Fatalf("batch = %v %v", batch, err)
	}
}

func TestIntegrationMembersAndRoles(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRepositoryWithDeployment(pool, "cloud")
	owner := addUser(t, pool, "owner")
	member := addUser(t, pool, "member")
	admin := addUser(t, pool, "admin")
	invitee := addUser(t, pool, "invitee")
	ws, _ := r.CreateForUser(ctx, owner)
	addMember(t, pool, ws.ID, member, "member")
	addMember(t, pool, ws.ID, admin, "admin")
	mustExec(t, pool, `INSERT INTO workspace_invitations(workspace_id,invitee_user_id,status,kind) VALUES($1,$2,'waiting_seat','email')`, ws.ID, invitee)

	if _, err := r.TransitionWorkspaceRole(ctx, member, ws.ID, "nobody", "admin"); !errors.Is(err, ErrWorkspaceRolePermissionDenied) {
		t.Fatalf("member transition = %v", err)
	}
	if _, err := r.TransitionWorkspaceRole(ctx, owner, ws.ID, member, "admin"); !errors.Is(err, ErrWorkspaceRolePermissionDenied) {
		t.Fatalf("promotion without commercial = %v", err)
	}
	if _, err := r.TransitionWorkspaceRole(ctx, owner, ws.ID, owner, "external"); !errors.Is(err, ErrWorkspaceMemberCannotRevoke) {
		t.Fatalf("self revoke = %v", err)
	}

	if _, err := r.ListMembersForUser(ctx, member, ws.ID, strings.Repeat("a", 256), 0, 10); !errors.Is(err, ErrMemberQueryTooLong) {
		t.Fatalf("long query = %v", err)
	}
	members, err := r.ListMembersForUser(ctx, member, ws.ID, "", 0, 10)
	if err != nil || len(members) != 4 || members[3].Status != "NeedMoreSeat" || members[0].Role != "Owner" {
		t.Fatalf("members = %+v %v", members, err)
	}
	if n, _ := r.MemberCount(ctx, ws.ID); n != 4 {
		t.Fatalf("member count = %d", n)
	}

	addTeamEntitlement(t, pool, ws.ID)
	setKnownQuota(t, pool, ws.ID, false)
	if _, err := r.TransitionWorkspaceRole(ctx, admin, ws.ID, owner, "external"); !errors.Is(err, ErrWorkspaceOwnerMustTransfer) {
		t.Fatalf("revoke owner = %v", err)
	}
	if _, err := r.TransitionWorkspaceRole(ctx, owner, ws.ID, invitee, "owner"); !errors.Is(err, ErrWorkspaceTargetMustBeActive) {
		t.Fatalf("transfer to invitee = %v", err)
	}
	tr, err := r.TransitionWorkspaceRole(ctx, owner, ws.ID, admin, "owner")
	if err != nil || !tr.OwnerTransferred {
		t.Fatalf("transfer = %+v %v", tr, err)
	}
	var stale bool
	var quotaOwner string
	if err := pool.QueryRow(ctx, `SELECT stale, owner_user_id FROM effective_workspace_quota_states WHERE workspace_id=$1`, ws.ID).Scan(&stale, &quotaOwner); err != nil || !stale || quotaOwner != admin {
		t.Fatalf("quota row = %v %s %v", stale, quotaOwner, err)
	}
	if role, _ := activeRole(ctx, pool, ws.ID, owner); role != "admin" {
		t.Fatalf("old owner role = %s", role)
	}
	tr, err = r.TransitionWorkspaceRole(ctx, admin, ws.ID, member, "external")
	if err != nil || !tr.Removed || tr.PreviousState != "active" {
		t.Fatalf("revoke member = %+v %v", tr, err)
	}

	if err := r.LeaveWorkspace(ctx, admin, ws.ID); !errors.Is(err, ErrWorkspaceOwnerCannotLeave) {
		t.Fatalf("owner leave = %v", err)
	}
	if err := r.LeaveWorkspace(ctx, owner, ws.ID); err != nil {
		t.Fatalf("leave = %v", err)
	}
	if err := r.LeaveWorkspace(ctx, member, ws.ID); !errors.Is(err, ErrWorkspaceMemberNotFound) {
		t.Fatalf("non-member leave = %v", err)
	}
}

type settlementRecorder struct {
	calls   int
	domains []invitelinks.WorkspaceInviteQuotaTargetDomain
	err     error
}

func (s *settlementRecorder) CommitWorkspaceInviteQuotaTx(_ context.Context, _ pgx.Tx, _ string, _ int, d []invitelinks.WorkspaceInviteQuotaTargetDomain) error {
	s.calls++
	s.domains = d
	return s.err
}

func TestIntegrationInvitations(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRepository(pool)
	owner := addUser(t, pool, "owner")
	member := addUser(t, pool, "member")
	ws, _ := r.CreateForUser(ctx, owner)
	addMember(t, pool, ws.ID, member, "member")
	seats := entitlements.Resolve(entitlements.Context{Deployment: entitlements.SelfHosted, TargetType: "user", Now: time.Now()}, nil).Limits.SeatLimit
	if seats < 4 {
		t.Skipf("default seat limit %d too small for this scenario", seats)
	}

	if _, err := r.ReserveEmailInvitations(ctx, owner, ws.ID, []string{"a@x.test", "a@x.test"}); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("duplicate = %v", err)
	}
	if _, err := r.ReserveEmailInvitations(ctx, owner, ws.ID, make([]string, 513)); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("513 = %v", err)
	}
	res, err := r.ReserveEmailInvitations(ctx, owner, ws.ID, []string{"new@x.test"})
	if err != nil || len(res) != 1 {
		t.Fatalf("reserve = %+v %v", res, err)
	}
	var registered bool
	var name string
	if err := pool.QueryRow(ctx, `SELECT registered, name FROM users WHERE id=$1`, res[0].UserID).Scan(&registered, &name); err != nil || registered || name != "new" {
		t.Fatalf("placeholder user = %v %q %v", registered, name, err)
	}
	_, err = r.ReserveEmailInvitations(ctx, owner, ws.ID, []string{"fresh@x.test", "new@x.test"})
	var spaceErr *InvitationSpaceError
	if !errors.As(err, &spaceErr) || spaceErr.WorkspaceID != ws.ID || !errors.Is(err, ErrInvitationAlreadyMember) {
		t.Fatalf("already invited = %v", err)
	}
	var fresh int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email='fresh@x.test'`).Scan(&fresh)
	if fresh != 0 {
		t.Fatal("failed reservation persisted a user")
	}
	if _, err := r.ReserveEmailInvitations(ctx, member, ws.ID, []string{"m@x.test"}); !errors.Is(err, ErrWorkspaceRolePermissionDenied) {
		t.Fatalf("member invite = %v", err)
	}

	rec := &settlementRecorder{}
	if _, err := r.ReserveEmailInvitationsWithQuota(ctx, owner, ws.ID, []string{"b@z.test", "c@y.test", "d@z.test"}, "res-1", rec); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 1 || fmt.Sprint(rec.domains) != "[{y.test 1} {z.test 2}]" {
		t.Fatalf("settlement = %d %v", rec.calls, rec.domains)
	}
	failing := &settlementRecorder{err: errors.New("boom")}
	if _, err := r.ReserveEmailInvitationsWithQuota(ctx, owner, ws.ID, []string{"e@z.test"}, "res-2", failing); err == nil {
		t.Fatal("settlement failure ignored")
	}
	if ok, _ := r.EmailInvitationAlreadyExists(ctx, ws.ID, "e@z.test"); ok {
		t.Fatal("rolled-back invitation persisted")
	}
	if ok, _ := r.EmailInvitationAlreadyExists(ctx, ws.ID, "member@example.test"); !ok {
		t.Fatal("member email not detected")
	}

	acc, err := r.ActivateEmailInvitation(ctx, ws.ID, res[0].ID, res[0].UserID)
	if err != nil || acc.InviterID != owner || acc.InviteID != res[0].ID {
		t.Fatalf("activate = %+v %v", acc, err)
	}
	if st, _ := r.InvitationStatus(ctx, ws.ID, res[0].UserID); st == nil || *st != "Accepted" {
		t.Fatalf("status after activation = %v", st)
	}

	joiner := addUser(t, pool, "joiner")
	if _, err := r.ReserveLinkReviewSeat(ctx, ws.ID, joiner, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReserveLinkReviewSeat(ctx, ws.ID, joiner, ""); !errors.Is(err, ErrInvitationAlreadyMember) {
		t.Fatalf("second link reservation = %v", err)
	}
	if _, err := r.ActivatePendingInvitation(ctx, ws.ID, joiner); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("activate link invite = %v", err)
	}
	if _, err := r.ApproveReviewInvitation(ctx, member, ws.ID, joiner); !errors.Is(err, ErrWorkspaceRolePermissionDenied) {
		t.Fatalf("member approve = %v", err)
	}
	if _, err := r.ApproveReviewInvitation(ctx, owner, ws.ID, joiner); err != nil {
		t.Fatal(err)
	}
	if role, _ := activeRole(ctx, pool, ws.ID, joiner); role != "member" {
		t.Fatalf("approved role = %q", role)
	}

	if st, err := r.InvitationStatus(ctx, ws.ID, "stranger"); st != nil || err != nil {
		t.Fatalf("stranger status = %v %v", st, err)
	}
	info, err := r.InvitationWorkspace(ctx, ws.ID)
	if err != nil || info.Name != "Untitled Workspace" || info.OwnerID != owner {
		t.Fatalf("invitation workspace = %+v %v", info, err)
	}
	mustExec(t, pool, `UPDATE workspaces SET name='visit example.com' WHERE id=$1`, ws.ID)
	if _, err := r.ReserveEmailInvitations(ctx, owner, ws.ID, []string{"z@x.test"}); !errors.Is(err, invitelinks.ErrWorkspaceNameDenied) {
		t.Fatalf("domain name = %v", err)
	}
}

type quotaGuard struct{ err error }

func (q quotaGuard) CheckWorkspaceMutationQuotaInTx(context.Context, pgx.Tx, string) error {
	return q.err
}

type ownerNotifier struct{ reasons []string }

func (n *ownerNotifier) PublishDocumentRoleChanged(_, _, reason string) {
	n.reasons = append(n.reasons, reason)
}

type alwaysGate bool

func (g alwaysGate) ShouldRepairDocOwner(context.Context, string, string) bool { return bool(g) }

func TestIntegrationDocumentACL(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRepository(pool)
	owner := addUser(t, pool, "owner")
	member := addUser(t, pool, "member")
	reader := addUser(t, pool, "reader")
	outsider := addUser(t, pool, "outsider")
	ws, _ := r.CreateForUser(ctx, owner)
	addMember(t, pool, ws.ID, member, "member")
	addMember(t, pool, ws.ID, reader, "member")
	mustExec(t, pool, `INSERT INTO doc_grants(workspace_id,doc_id,principal_type,principal_id,role) VALUES($1,'doc','user',$2,'reader')`, ws.ID, reader)
	mustExec(t, pool, `INSERT INTO doc_access_policies(workspace_id,doc_id,member_default_role) VALUES($1,'doc','reader')`, ws.ID)

	roles := NewDocumentRoleService(r, nil)
	if err := roles.GrantDocRoles(ctx, reader, ws.ID, "doc", []string{member}, "editor"); !errors.Is(err, ErrDocRoleDenied) {
		t.Fatalf("reader grant = %v", err)
	}
	if err := roles.GrantDocRoles(ctx, owner, ws.ID, "doc", []string{member}, "editor"); !errors.Is(err, ErrDocQuotaUnavailable) {
		t.Fatalf("expansion without guard = %v", err)
	}
	guarded := NewDocumentRoleService(r, quotaGuard{err: errors.New("over quota")})
	if err := guarded.GrantDocRoles(ctx, reader, ws.ID, "doc", []string{member}, "editor"); !errors.Is(err, ErrDocRoleDenied) {
		t.Fatalf("quota error leaked before authorization: %v", err)
	}
	ok := NewDocumentRoleService(r, quotaGuard{})
	if err := ok.GrantDocRoles(ctx, owner, ws.ID, "doc", []string{member, member}, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := roles.GrantDocRoles(ctx, owner, ws.ID, "doc", []string{member}, "reader"); err != nil {
		t.Fatalf("contraction without guard = %v", err)
	}
	if err := ok.TransitionDocRole(ctx, owner, ws.ID, "doc", outsider, "owner"); !errors.Is(err, ErrDocOwnerTargetInactive) {
		t.Fatalf("owner to outsider = %v", err)
	}
	mustExec(t, pool, `INSERT INTO doc_grants(workspace_id,doc_id,principal_type,principal_id,role) VALUES($1,'doc','user',$2,'owner')`, ws.ID, owner)
	if err := ok.TransitionDocRole(ctx, owner, ws.ID, "doc", owner, "editor"); !errors.Is(err, ErrDocOwnerMustTransfer) {
		t.Fatalf("demote owner = %v", err)
	}
	if err := ok.TransitionDocRole(ctx, owner, ws.ID, "doc", member, "owner"); err != nil {
		t.Fatal(err)
	}
	grants, _ := readGrants(ctx, pool, ws.ID, "doc", []string{owner, member})
	if grants[owner] != "manager" || grants[member] != "owner" {
		t.Fatalf("after transfer = %v", grants)
	}
	if err := ok.SetDocDefaultRole(ctx, member, ws.ID, "doc", "none"); err != nil {
		t.Fatal(err)
	}

	pub := NewDocumentPublicationService(r, quotaGuard{})
	if _, err := pub.SetDocPublished(ctx, member, ws.ID, "doc", 0, true); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("publish without content = %v", err)
	}
	if _, err := pub.SetDocPublished(ctx, member, ws.ID, "doc", 0, false); !errors.Is(err, ErrDocumentNotPublic) {
		t.Fatalf("unpublish private = %v", err)
	}
	mustExec(t, pool, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at,created_by,updated_by) VALUES($1,'doc','\x00',now(),now(),$2,$2)`, ws.ID, owner)
	meta, err := pub.SetDocPublished(ctx, member, ws.ID, "doc", 1, true)
	if err != nil || !meta.Public || meta.Mode != 1 || meta.DefaultRole != "none" {
		t.Fatalf("publish = %+v %v", meta, err)
	}
	readSvc := NewDocumentReadService(r, nil)
	public, err := readSvc.ReadPublicDocuments(ctx, ws.ID)
	if err != nil || len(public) != 1 || public[0].DocID != "doc" {
		t.Fatalf("public docs = %+v %v", public, err)
	}
	share, err := r.ReadDocShareState(ctx, ws.ID, "doc")
	if err != nil || share == nil || !share.Public || share.Mode != "Edgeless" || share.DefaultRole != "None" {
		t.Fatalf("share state = %+v %v", share, err)
	}

	page, err := readSvc.ReadDocumentGrants(ctx, owner, ws.ID, "doc", 1, 0, nil)
	if err != nil || len(page.Users) != 1 || !page.HasNext || page.EndCursor == nil {
		t.Fatalf("grants page = %+v %v", page, err)
	}
	after, err := DecodeDocGrantCursor(*page.EndCursor)
	if err != nil || !after.Equal(page.Users[0].CreatedAt) {
		t.Fatalf("cursor = %v %v", after, err)
	}
	next, err := readSvc.ReadDocumentGrants(ctx, owner, ws.ID, "doc", 10, 0, after)
	if err != nil || len(next.Users) != page.TotalCount-1 {
		t.Fatalf("next page = %+v %v", next, err)
	}
	if _, err := readSvc.ReadDocumentGrants(ctx, outsider, ws.ID, "doc", 1, 0, nil); !errors.Is(err, ErrDocRoleDenied) {
		t.Fatalf("outsider grants = %v", err)
	}
	if _, err := readSvc.PaginateWorkspaceDocuments(ctx, owner, ws.ID, DocumentListInput{First: 0}); err == nil {
		t.Fatal("first=0 accepted")
	}
	if _, err := readSvc.PaginateWorkspaceDocuments(ctx, owner, ws.ID, DocumentListInput{First: 101}); err == nil {
		t.Fatal("first=101 accepted")
	}

	// Owner repair: a doc with content and no ACL gets its creator as owner.
	mustExec(t, pool, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at,created_by) VALUES($1,'fresh','\x00',now(),now(),$2)`, ws.ID, member)
	noRepair := NewDocumentReadService(r, alwaysGate(false))
	if _, err := noRepair.ReadDocument(ctx, member, ws.ID, "fresh"); err != nil {
		t.Fatal(err)
	}
	if g, _ := readGrants(ctx, pool, ws.ID, "fresh", []string{member}); len(g) != 0 {
		t.Fatal("repair ran with gate closed")
	}
	n := &ownerNotifier{}
	repair := NewDocumentReadService(r, alwaysGate(true))
	repair.SetDocumentOwnerRepairNotifier(n)
	if _, err := repair.ReadDocument(ctx, member, ws.ID, "fresh"); err != nil {
		t.Fatal(err)
	}
	if g, _ := readGrants(ctx, pool, ws.ID, "fresh", []string{member}); g[member] != "owner" || fmt.Sprint(n.reasons) != "[owner-repaired]" {
		t.Fatalf("repair = %v %v", g, n.reasons)
	}

	// Previews follow the sharing switches.
	mustExec(t, pool, `UPDATE workspace_access_policies SET sharing_enabled=false WHERE workspace_id=$1`, ws.ID)
	if p, err := readSvc.ReadDocumentPreview(ctx, ws.ID, "fresh"); err != nil || p.Exposure != PreviewDenied || p.Title != "" {
		t.Fatalf("denied preview = %+v %v", p, err)
	}
}

func TestIntegrationAnalyticsAndAdmin(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRepository(pool)
	owner := addUser(t, pool, "owner")
	ws, _ := r.CreateForUser(ctx, owner)

	var rdb redis.Cmdable
	if raw := os.Getenv("NOTES_TEST_REDIS_URL"); raw != "" {
		opts, err := redis.ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		client := redis.NewClient(opts)
		t.Cleanup(func() { _ = client.Close() })
		rdb = client
	}
	day := time.Now().UTC()
	doc := "doc-" + uuid.NewString()
	withRedis := NewDocumentAnalyticsService(r, rdb)
	visitor := DocVisitorID(ws.ID, doc, "tracker")
	for i := 0; i < 2; i++ {
		if err := withRedis.RecordDocView(ctx, ws.ID, doc, visitor, true, owner, day); err != nil {
			t.Fatal(err)
		}
	}
	var total, unique, guest int64
	if err := pool.QueryRow(ctx, `SELECT total_views, unique_views, guest_views FROM workspace_doc_view_daily WHERE workspace_id=$1 AND doc_id=$2`, ws.ID, doc).Scan(&total, &unique, &guest); err != nil {
		t.Fatal(err)
	}
	wantUnique := int64(2)
	if rdb != nil {
		wantUnique = 1
	}
	if total != 2 || unique != wantUnique || guest != 2 {
		t.Fatalf("views = %d/%d/%d", total, unique, guest)
	}

	thirty := 30
	analytics, err := withRedis.GetDocPageAnalytics(ctx, owner, ws.ID, doc, DocPageAnalyticsInput{WindowDays: &thirty})
	if err != nil || analytics.Window.EffectiveSize != 7 || analytics.Window.RequestedSize != 30 || len(analytics.Series) != 7 || analytics.Summary.TotalViews != 2 {
		t.Fatalf("analytics = %+v %v", analytics, err)
	}
	members, err := withRedis.PaginateDocLastAccessedMembers(ctx, owner, ws.ID, doc, DocMemberLastAccessPagination{}, nil, true)
	if err != nil || len(members.Members) != 1 || *members.TotalCount != 1 {
		t.Fatalf("last access = %+v %v", members, err)
	}

	if _, err := withRedis.PaginateAdminSharedLinks(ctx, AdminSharedLinksInput{OrderBy: "Bogus"}); !errors.Is(err, ErrInvalidAdminSharedLinksCursor) {
		t.Fatalf("bogus order = %v", err)
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("pub-%d", i)
		mustExec(t, pool, `INSERT INTO doc_access_policies(workspace_id,doc_id,visibility,public_role,published_at) VALUES($1,$2,'public','external',now())`, ws.ID, id)
		mustExec(t, pool, `INSERT INTO workspace_doc_view_daily(workspace_id,doc_id,date,total_views) VALUES($1,$2,current_date,$3)`, ws.ID, id, 10-i)
	}
	one := 2
	first, err := withRedis.PaginateAdminSharedLinks(ctx, AdminSharedLinksInput{OrderBy: AdminSharedLinksViewsDesc, First: &one, IncludeTotal: true})
	if err != nil || len(first.Edges) != 2 || !first.HasNextPage || *first.TotalCount != 3 {
		t.Fatalf("shared links page 1 = %+v %v", first, err)
	}
	second, err := withRedis.PaginateAdminSharedLinks(ctx, AdminSharedLinksInput{OrderBy: AdminSharedLinksViewsDesc, First: &one, After: first.EndCursor})
	if err != nil || len(second.Edges) != 1 || second.Edges[0].Node.DocID != "pub-2" {
		t.Fatalf("shared links page 2 = %+v %v", second, err)
	}

	hours := 100
	dash, err := withRedis.GetAdminDashboard(ctx, AdminDashboardInput{SyncHistoryHours: &hours})
	if err != nil || dash.SyncWindow.EffectiveSize != 72 || len(dash.SyncActiveUsersTimeline) != 72*60 {
		t.Fatalf("dashboard sync window = %+v %v", dash.SyncWindow, err)
	}
	last := dash.WorkspaceStorageHistory[len(dash.WorkspaceStorageHistory)-1]
	if last.Value != dash.WorkspaceStorageBytes {
		t.Fatalf("storage history tail = %+v", last)
	}

	if err := r.UpdateAdminWorkspace(ctx, ws.ID, AdminWorkspaceUpdate{Name: ptr("named"), NameSet: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateAdminWorkspace(ctx, ws.ID, AdminWorkspaceUpdate{NameSet: true}); err != nil {
		t.Fatal(err)
	}
	if w, err := r.GetAdminWorkspace(ctx, ws.ID); err != nil || w == nil || w.Name != nil || w.Owner == nil || w.Owner.ID != owner {
		t.Fatalf("admin workspace = %+v %v", w, err)
	}
	if err := r.UpdateAdminWorkspace(ctx, "missing", AdminWorkspaceUpdate{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing admin update = %v", err)
	}

	if _, err := r.UpdateCalendarAccountRefreshInterval(ctx, owner, "acc", 0); !errors.Is(err, ErrInvalidCalendarRefreshInterval) {
		t.Fatalf("interval 0 = %v", err)
	}
	if a, err := r.UpdateCalendarAccountRefreshInterval(ctx, owner, "someone-elses", 5); a != nil || err != nil {
		t.Fatalf("unknown account = %v %v", a, err)
	}

	if n, err := r.UpdateIgnoredWorkspaceDocs(ctx, ws.ID, []string{"a", "a", "b"}, nil); n != 2 || err != nil {
		t.Fatalf("ignore add = %d %v", n, err)
	}
	if n, err := r.UpdateIgnoredWorkspaceDocs(ctx, ws.ID, []string{"a", "a", "b"}, nil); n != 0 || err != nil {
		t.Fatalf("ignore repeat = %d %v", n, err)
	}
	if docs, total, err := r.ListIgnoredWorkspaceDocs(ctx, ws.ID, 1, 0); err != nil || total != 2 || len(docs) != 1 {
		t.Fatalf("ignored list = %v %d %v", docs, total, err)
	}
}

func TestIntegrationQuota(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRepositoryWithDeployment(pool, "cloud")
	owner := addUser(t, pool, "owner")
	personal, _ := r.CreateForUser(ctx, owner)
	team, _ := r.CreateForUser(ctx, owner)
	addTeamEntitlement(t, pool, team.ID)
	mustExec(t, pool, `INSERT INTO blobs(workspace_id,key,size,mime) VALUES($1,'a',100,'x'),($2,'b',7,'x')`, personal.ID, team.ID)

	q, err := r.ReadQuota(ctx, team.ID)
	if err != nil || q.Name != "Team" || q.UsedStorageQuota != 7 {
		t.Fatalf("team quota = %+v %v", q, err)
	}
	q, err = r.ReadQuota(ctx, personal.ID)
	if err != nil || q.UsedStorageQuota != 100 {
		t.Fatalf("personal quota = %+v %v", q, err)
	}
	state, err := r.ReadWorkspaceQuotaState(ctx, personal.ID)
	if err != nil || !state.UsesOwnerQuota || state.OwnerUserID != owner {
		t.Fatalf("personal state = %+v %v", state, err)
	}
	for i := int32(0); i <= state.SeatLimit; i++ {
		u := addUser(t, pool, fmt.Sprintf("seat-%d", i))
		addMember(t, pool, personal.ID, u, "member")
	}
	state, err = r.ReadWorkspaceQuotaState(ctx, personal.ID)
	if err != nil || !state.Readonly || fmt.Sprint(state.ReadonlyReasons) != "[member_overflow]" {
		t.Fatalf("overflow state = %+v %v", state, err)
	}
	if _, err := r.ReadWorkspaceQuotaState(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing quota = %v", err)
	}
}
