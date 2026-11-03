// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInviteLinkCreatorEvaluatesPostgresPolicyAndWorkspaceName(t *testing.T) {
	ctx := context.Background()
	pool := invitePolicyTestPool(t)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE users (
			id text PRIMARY KEY, email text NOT NULL, created_at timestamptz NOT NULL,
			registered boolean NOT NULL DEFAULT true, email_verified timestamptz, disabled boolean NOT NULL DEFAULT false
		);
		CREATE TABLE workspaces (id text PRIMARY KEY, name text NOT NULL);
		CREATE TABLE workspace_members (workspace_id text NOT NULL, user_id text NOT NULL, role text NOT NULL, state text NOT NULL);
		CREATE TABLE entitlements (
			target_type text NOT NULL, target_id text NOT NULL, source text NOT NULL, status text NOT NULL, plan text NOT NULL,
			quantity integer, starts_at timestamptz, expires_at timestamptz, grace_until timestamptz, signed_payload bytea,
			updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE app_configs (id text PRIMARY KEY, value jsonb NOT NULL);
		CREATE TABLE runtime_invite_abuse_subjects (
			subject_key text PRIMARY KEY, kind text NOT NULL, user_id text, actor_email_hash text, email_domain text,
			first_seen_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL, status text NOT NULL,
			action text, action_reason text, action_at timestamptz, updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE runtime_invite_abuse_evidence (
			id bigserial PRIMARY KEY, subject_key text NOT NULL REFERENCES runtime_invite_abuse_subjects(subject_key),
			workspace_id text, user_id text, actor_email_hash text, source_prefix_hash text, source_asn bigint,
			target_domains jsonb NOT NULL, counters jsonb NOT NULL, decision text NOT NULL, reason text NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users(id,email,created_at) VALUES('user-1','person@example.com',clock_timestamp()-interval '48 hours');
		INSERT INTO workspaces(id,name) VALUES('workspace-1','Product Notes');
		INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES('workspace-1','user-1','owner','active');
		INSERT INTO app_configs(id,value) VALUES('auth.newAccountActionDelay','0'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	redis := &fakeRedis{}
	store := NewRedisStore(redis, "https://notes.example.test")
	creator := NewCreator(pool, store, config.Config{Deployment: "affine"}, "policy-hash-secret")

	link, err := creator.Create(ctx, "user-1", "workspace-1", "OneDay")
	if err != nil {
		t.Fatalf("create allowed invite link: %v", err)
	}
	if link == nil || !link.Created || !strings.HasPrefix(link.URL, "https://notes.example.test/invite/") || len(redis.sets) != 2 {
		t.Fatalf("created invite link = %#v, Redis writes = %#v", link, redis.sets)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspaces SET name='docs.example.com' WHERE id='workspace-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := creator.Create(ctx, "user-1", "workspace-1", "OneWeek"); !errors.Is(err, ErrWorkspaceNameDenied) {
		t.Fatalf("URL-like workspace name error = %v", err)
	}
	if len(redis.sets) != 2 {
		t.Fatalf("workspace name denial wrote Redis keys: %#v", redis.sets)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE workspaces SET name='Product Notes' WHERE id='workspace-1';
		UPDATE users SET created_at=clock_timestamp()-interval '1 hour' WHERE id='user-1';
		UPDATE app_configs SET value='86400'::jsonb WHERE id='auth.newAccountActionDelay'`); err != nil {
		t.Fatal(err)
	}
	if _, err := creator.Create(ctx, "user-1", "workspace-1", "OneWeek"); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("new account policy error = %v", err)
	}
	if len(redis.sets) != 2 {
		t.Fatalf("policy denial wrote Redis keys: %#v", redis.sets)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE users SET created_at=clock_timestamp()-interval '48 hours' WHERE id='user-1';
		UPDATE app_configs SET value='0'::jsonb WHERE id='auth.newAccountActionDelay'`); err != nil {
		t.Fatal(err)
	}
	actorKey := actorEmailSubjectKey("person@example.com", "policy-hash-secret")
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_invite_abuse_subjects(subject_key,kind,user_id,actor_email_hash,email_domain,first_seen_at,last_seen_at,status,action,action_reason,action_at)
		VALUES($1,'actor_email','user-1',$1,'example.com',now(),now(),'banned','ban_actor','high_risk_domain_burst',now())`, actorKey); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_invite_abuse_evidence(subject_key,workspace_id,user_id,actor_email_hash,target_domains,counters,decision,reason)
		VALUES($1,'workspace-1','user-1',$1,'[{"domain":"example.com","count":1}]'::jsonb,'{"requested":1}'::jsonb,'ban_actor','high_risk_domain_burst')`, actorKey); err != nil {
		t.Fatal(err)
	}
	if _, err := creator.Create(ctx, "user-1", "workspace-1", "OneWeek"); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("matching abuse subject error = %v", err)
	}
	if len(redis.sets) != 2 {
		t.Fatalf("abuse denial wrote Redis keys: %#v", redis.sets)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM runtime_invite_abuse_evidence`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM runtime_invite_abuse_subjects`); err != nil {
		t.Fatal(err)
	}
	otherWorkspaceKey := workspaceSubjectKey("other-workspace")
	otherActorKey := actorEmailSubjectKey("someone@example.org", "policy-hash-secret")
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_invite_abuse_subjects(subject_key,kind,actor_email_hash,email_domain,first_seen_at,last_seen_at,status,action,action_reason,action_at)
		VALUES($1,'workspace',$2,'example.org',now(),now(),'quarantined','quarantine_workspace','workspace_high_risk_domain_burst',now())`, otherWorkspaceKey, otherActorKey); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_invite_abuse_evidence(subject_key,workspace_id,user_id,actor_email_hash,target_domains,counters,decision,reason)
		VALUES($1,'other-workspace','someone-1',$2,'[{"domain":"example.org","count":1}]'::jsonb,'{"requested":1}'::jsonb,'quarantine_workspace','mismatched-reason')`, otherWorkspaceKey, otherActorKey); err != nil {
		t.Fatal(err)
	}
	if _, err := creator.Create(ctx, "user-1", "workspace-1", "OneWeek"); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("malformed persisted abuse record error = %v", err)
	}
	mailPolicy := NewMemberLeaveMailPolicy(pool)
	if reason, err := mailPolicy.MemberLeaveMailSkipReason(ctx, "user-1", "workspace-1"); err != nil || reason != "policy_state_invalid" {
		t.Fatalf("malformed abuse state member-leave mail policy = %q, %v", reason, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM runtime_invite_abuse_evidence; DELETE FROM runtime_invite_abuse_subjects`); err != nil {
		t.Fatal(err)
	}
	workspaceKey := workspaceSubjectKey("workspace-1")
	workspaceActorHash := "actor_email_sha256:v1:" + strings.Repeat("b", 64)
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_invite_abuse_subjects(subject_key,kind,actor_email_hash,email_domain,first_seen_at,last_seen_at,status,action,action_reason,action_at)
		VALUES($1,'workspace',$2,'example.com',now(),now(),'quarantined','quarantine_workspace','workspace_high_risk_domain_burst',now())`, workspaceKey, workspaceActorHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_invite_abuse_evidence(subject_key,workspace_id,user_id,actor_email_hash,target_domains,counters,decision,reason)
		VALUES($1,'workspace-1','user-1',$2,'[{"domain":"example.com","count":1}]'::jsonb,'{"requested":1}'::jsonb,'quarantine_workspace','workspace_high_risk_domain_burst')`, workspaceKey, workspaceActorHash); err != nil {
		t.Fatal(err)
	}
	if reason, err := mailPolicy.MemberLeaveMailSkipReason(ctx, "user-1", "workspace-1"); err != nil || reason != "workspace_quarantined" {
		t.Fatalf("workspace quarantine member-leave mail policy = %q, %v", reason, err)
	}
}

func invitePolicyTestPool(t *testing.T) *pgxpool.Pool {
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
	schema := "notes_invitelinks_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	return pool
}
