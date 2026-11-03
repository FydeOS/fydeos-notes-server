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
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkspaceInviteQuotaAdmissionReservesCommitsReleasesAndDenies(t *testing.T) {
	ctx := context.Background()
	pool := inviteQuotaTestPool(t)
	createInviteQuotaFixtureSchema(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO users(id,email,created_at,email_verified) VALUES('actor-1','person@example.com',clock_timestamp()-interval '60 days',clock_timestamp());
		INSERT INTO workspaces(id,name,created_at) VALUES('workspace-1','Product Notes',clock_timestamp()-interval '60 days');
		INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES('workspace-1','actor-1','owner','active');
		INSERT INTO app_configs(id,value) VALUES('auth.newAccountActionDelay','0'::jsonb),('auth.trustedCloudflareHeaders','true'::jsonb);`); err != nil {
		t.Fatal(err)
	}
	admission := NewWorkspaceInviteQuotaAdmission(pool, config.Config{Deployment: "affine"}, "policy-hash-secret")
	requestCtx := WithRequestFacts(ctx, RequestFacts{RequestID: "session-tracker", ClientIP: "203.0.113.17", ASN: "64500"})
	domains := []WorkspaceInviteQuotaTargetDomain{{Domain: "example.com", Count: 1}}
	reservationID, err := admission.AdmitWorkspaceInvites(requestCtx, "actor-1", "workspace-1", 1, domains)
	if err != nil {
		t.Fatalf("reserve invite quota: %v", err)
	}
	if _, err := uuid.Parse(reservationID); err != nil {
		t.Fatalf("reservation id %q is not a UUID: %v", reservationID, err)
	}
	var reservedRows int
	var requestID string
	if err := pool.QueryRow(ctx, `SELECT count(*),min(request_id) FROM runtime_rolling_quota_reservations WHERE id=$1::uuid AND status='reserved' AND purpose='workspace_invite'`, reservationID).Scan(&reservedRows, &requestID); err != nil || reservedRows != 18 || requestID != "session-tracker" {
		t.Fatalf("reserved rows/request id = %d/%q, err %v", reservedRows, requestID, err)
	}
	var trustedSourceScopes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_rolling_quota_reservations WHERE id=$1::uuid AND scope_key LIKE 'invite:source_%'`, reservationID).Scan(&trustedSourceScopes); err != nil || trustedSourceScopes != 6 {
		t.Fatalf("trusted source quota scopes = %d, err %v", trustedSourceScopes, err)
	}
	if err := admission.CommitWorkspaceInviteQuota(ctx, reservationID, 1, domains); err != nil {
		t.Fatalf("commit invite quota: %v", err)
	}
	var committedRows, usage int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_rolling_quota_reservations WHERE id=$1::uuid AND status='committed'`, reservationID).Scan(&committedRows); err != nil || committedRows != 18 {
		t.Fatalf("committed reservation rows = %d, err %v", committedRows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count FROM runtime_rolling_quota_counters WHERE scope_key='invite:user:actor-1' AND window_seconds=3600`).Scan(&usage); err != nil || usage != 1 {
		t.Fatalf("committed actor quota usage = %d, err %v", usage, err)
	}

	secondReservation, err := admission.AdmitWorkspaceInvites(requestCtx, "actor-1", "workspace-1", 1, domains)
	if err != nil {
		t.Fatalf("reserve second invite quota: %v", err)
	}
	if err := admission.ReleaseWorkspaceInviteQuota(ctx, secondReservation); err != nil {
		t.Fatalf("release invite quota: %v", err)
	}
	var releasedRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_rolling_quota_reservations WHERE id=$1::uuid AND status='released'`, secondReservation).Scan(&releasedRows); err != nil || releasedRows != 18 {
		t.Fatalf("released reservation rows = %d, err %v", releasedRows, err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO runtime_rolling_quota_counters(scope_key,window_seconds,bucket_start,count,expires_at)
		VALUES('invite:user:actor-1',86400,date_trunc('hour',clock_timestamp()),10,clock_timestamp()+interval '2 days')
		ON CONFLICT(scope_key,window_seconds,bucket_start) DO UPDATE SET count=10`); err != nil {
		t.Fatal(err)
	}
	if _, err := admission.AdmitWorkspaceInvites(requestCtx, "actor-1", "workspace-1", 1, domains); !errors.Is(err, ErrWorkspaceInviteQuotaExceeded) {
		t.Fatalf("quota denial = %v", err)
	}
}

func TestWorkspaceInviteQuotaSettlementUsesCallerTransaction(t *testing.T) {
	ctx := context.Background()
	pool := inviteQuotaTestPool(t)
	createInviteQuotaFixtureSchema(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO users(id,email,created_at,email_verified) VALUES('actor-1','person@example.com',clock_timestamp()-interval '60 days',clock_timestamp());
		INSERT INTO workspaces(id,name,created_at) VALUES('workspace-1','Product Notes',clock_timestamp()-interval '60 days');
		INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES('workspace-1','actor-1','owner','active');
		INSERT INTO app_configs(id,value) VALUES('auth.newAccountActionDelay','0'::jsonb);`); err != nil {
		t.Fatal(err)
	}
	admission := NewWorkspaceInviteQuotaAdmission(pool, config.Config{Deployment: "affine"}, "policy-hash-secret")
	domains := []WorkspaceInviteQuotaTargetDomain{{Domain: "example.com", Count: 1}}
	reservationID, err := admission.AdmitWorkspaceInvites(ctx, "actor-1", "workspace-1", 1, domains)
	if err != nil {
		t.Fatalf("reserve invite quota: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.CommitWorkspaceInviteQuotaTx(ctx, tx, reservationID, 1, domains); err != nil {
		t.Fatalf("settle invite quota in caller transaction: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var counters int
	if err := pool.QueryRow(ctx, `SELECT status FROM runtime_rolling_quota_reservations WHERE id=$1::uuid LIMIT 1`, reservationID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_rolling_quota_counters WHERE scope_key='invite:user:actor-1'`).Scan(&counters); err != nil {
		t.Fatal(err)
	}
	if status != "reserved" || counters != 0 {
		t.Fatalf("rolled back settlement = reservation %q, counters %d", status, counters)
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.CommitWorkspaceInviteQuotaTx(ctx, tx, reservationID, 1, domains); err != nil {
		t.Fatalf("retry invite quota settlement: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM runtime_rolling_quota_reservations WHERE id=$1::uuid LIMIT 1`, reservationID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count FROM runtime_rolling_quota_counters WHERE scope_key='invite:user:actor-1' AND window_seconds=3600`).Scan(&counters); err != nil {
		t.Fatal(err)
	}
	if status != "committed" || counters != 1 {
		t.Fatalf("committed settlement = reservation %q, counters %d", status, counters)
	}
}

func TestWorkspaceInviteQuotaAdmissionPersistsAbuseEvidence(t *testing.T) {
	ctx := context.Background()
	pool := inviteQuotaTestPool(t)
	createInviteQuotaFixtureSchema(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO users(id,email,created_at,registered,email_verified) VALUES('actor-1','person@example.com',clock_timestamp()-interval '60 days',true,NULL);
		INSERT INTO workspaces(id,name,created_at) VALUES('workspace-1','Product Notes',clock_timestamp()-interval '60 days');
		INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES('workspace-1','actor-1','owner','active');
		INSERT INTO app_configs(id,value) VALUES('auth.newAccountActionDelay','0'::jsonb);
		INSERT INTO workspace_invitations(id,workspace_id,normalized_email,inviter_user_id,status,created_at)
		VALUES('invite-1','workspace-1','invitee@example.com','actor-1','pending',clock_timestamp());
		INSERT INTO mail_deliveries(actor_user_id,workspace_id,abuse_subject_key,status,recipient_email,payload)
		VALUES('actor-1','workspace-1','pending-subject','queued','invitee@example.com','{}'::jsonb);
		INSERT INTO user_sessions(id,user_id) VALUES('user-session-1','actor-1');
		INSERT INTO auth_sessions(id,user_session_id) VALUES('auth-session-1','user-session-1');
		INSERT INTO auth_refresh_tokens(id,auth_session_id) VALUES('refresh-1','auth-session-1');`); err != nil {
		t.Fatal(err)
	}
	admission := NewWorkspaceInviteQuotaAdmission(pool, config.Config{Deployment: "affine"}, "policy-hash-secret")
	requestCtx := WithRequestFacts(ctx, RequestFacts{RequestID: "session-tracker"})
	domains := []WorkspaceInviteQuotaTargetDomain{{Domain: "qq.com", Count: 3}}
	if _, err := admission.AdmitWorkspaceInvites(requestCtx, "actor-1", "workspace-1", 3, domains); !errors.Is(err, ErrWorkspaceInviteForbidden) {
		t.Fatalf("abuse denial = %v", err)
	}
	var subjectStatus, action, reason string
	var evidenceRows, actionRows, pendingInvitations, activeMail, activeUserSessions int
	var mailStatus, mailError string
	var authRevoked, refreshRevoked bool
	if err := pool.QueryRow(ctx, `SELECT status,action,action_reason FROM runtime_invite_abuse_subjects`).Scan(&subjectStatus, &action, &reason); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_invite_abuse_evidence WHERE request_id='session-tracker' AND target_domains='[{"domain":"qq.com","count":3}]'::jsonb AND counters='{"requested":3}'::jsonb`).Scan(&evidenceRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_invite_abuse_actions WHERE action='quarantine_actor' AND status='succeeded'`).Scan(&actionRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE inviter_user_id='actor-1' AND status='pending'`).Scan(&pendingInvitations); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status,last_error_code FROM mail_deliveries`).Scan(&mailStatus, &mailError); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mail_deliveries WHERE status IN ('queued','retry_wait','sending')`).Scan(&activeMail); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE user_id='actor-1'`).Scan(&activeUserSessions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM auth_sessions WHERE id='auth-session-1'`).Scan(&authRevoked); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM auth_refresh_tokens WHERE id='refresh-1'`).Scan(&refreshRevoked); err != nil {
		t.Fatal(err)
	}
	if subjectStatus != "quarantined" || action != "quarantine_actor" || reason != "unverified_high_risk_domain_burst" || evidenceRows != 1 || actionRows != 1 || pendingInvitations != 0 || mailStatus != "canceled" || mailError != "actor_quarantined" || activeMail != 0 || activeUserSessions != 0 || !authRevoked || !refreshRevoked {
		t.Fatalf("abuse disposition: subject=%q action=%q reason=%q evidence=%d actions=%d invites=%d mail=%q/%q active_mail=%d sessions=%d auth_revoked=%t refresh_revoked=%t", subjectStatus, action, reason, evidenceRows, actionRows, pendingInvitations, mailStatus, mailError, activeMail, activeUserSessions, authRevoked, refreshRevoked)
	}
	if _, err := pool.Exec(ctx, `UPDATE runtime_invite_abuse_actions SET status='retry_wait',next_attempt_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	admission.processRetryableInviteAbuseActions(ctx)
	var retriedStatus string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,attempts FROM runtime_invite_abuse_actions`).Scan(&retriedStatus, &attempts); err != nil || retriedStatus != "succeeded" || attempts != 2 {
		t.Fatalf("retried abuse action status/attempts = %q/%d, err %v", retriedStatus, attempts, err)
	}
}

func inviteQuotaTestPool(t *testing.T) *pgxpool.Pool {
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
	schema := "notes_invite_quota_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

func createInviteQuotaFixtureSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE users (
			id text PRIMARY KEY, email text NOT NULL, created_at timestamptz NOT NULL,
			registered boolean NOT NULL DEFAULT true, email_verified timestamptz, disabled boolean NOT NULL DEFAULT false,
			auth_epoch integer NOT NULL DEFAULT 0
		);
		CREATE TABLE workspaces (id text PRIMARY KEY, name text NOT NULL, created_at timestamptz NOT NULL);
		CREATE TABLE workspace_members (workspace_id text NOT NULL, user_id text NOT NULL, role text NOT NULL, state text NOT NULL);
		CREATE TABLE workspace_invitations (
			id text PRIMARY KEY, workspace_id text NOT NULL, invitee_user_id text, normalized_email text,
			inviter_user_id text, status text NOT NULL, accepted_at timestamptz, created_at timestamptz NOT NULL
		);
		CREATE TABLE entitlements (
			target_type text NOT NULL, target_id text NOT NULL, source text NOT NULL, status text NOT NULL, plan text NOT NULL,
			quantity integer, starts_at timestamptz, expires_at timestamptz, grace_until timestamptz, signed_payload bytea,
			updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE app_configs (id text PRIMARY KEY, value jsonb NOT NULL);
		CREATE TABLE runtime_rolling_quota_counters (
			scope_key text NOT NULL, window_seconds integer NOT NULL, bucket_start timestamptz NOT NULL,
			count integer NOT NULL, expires_at timestamptz NOT NULL, updated_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY(scope_key,window_seconds,bucket_start)
		);
		CREATE TABLE runtime_rolling_quota_reservations (
			id uuid NOT NULL, scope_key text NOT NULL, window_seconds integer NOT NULL, bucket_start timestamptz NOT NULL,
			count integer NOT NULL, status text NOT NULL, purpose text NOT NULL, request_id text, expires_at timestamptz NOT NULL,
			committed_at timestamptz, released_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(id,scope_key,window_seconds,bucket_start)
		);
		CREATE TABLE runtime_invite_abuse_subjects (
			subject_key text PRIMARY KEY, kind text NOT NULL, user_id text, actor_email_hash text, email_domain text,
			first_seen_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL, status text NOT NULL,
			action text, action_reason text, action_at timestamptz, updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE runtime_invite_abuse_evidence (
			id bigserial PRIMARY KEY, subject_key text NOT NULL REFERENCES runtime_invite_abuse_subjects(subject_key),
			request_id text, workspace_id text, user_id text, actor_email_hash text, source_prefix_hash text, source_asn bigint,
			target_domains jsonb NOT NULL, counters jsonb NOT NULL, decision text NOT NULL, reason text NOT NULL
		);
		CREATE TABLE runtime_invite_abuse_actions (
			id bigserial PRIMARY KEY, subject_key text NOT NULL REFERENCES runtime_invite_abuse_subjects(subject_key),
			evidence_id bigint NOT NULL REFERENCES runtime_invite_abuse_evidence(id), action text NOT NULL, status text NOT NULL,
			next_attempt_at timestamptz, attempts integer NOT NULL DEFAULT 0, locked_by text, locked_until timestamptz,
			last_error text, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE mail_deliveries (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), actor_user_id text, workspace_id text, abuse_subject_key text,
			status text NOT NULL, canceled_at timestamptz, settled_at timestamptz, locked_by text, locked_until timestamptz,
			recipient_email text, payload jsonb, retention_state text, anonymized_at timestamptz, last_error_code text,
			updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE user_sessions(id text PRIMARY KEY,user_id text NOT NULL);
		CREATE TABLE auth_sessions(id text PRIMARY KEY,user_session_id text NOT NULL,revoked_at timestamptz,revoke_reason text);
		CREATE TABLE auth_refresh_tokens(id text PRIMARY KEY,auth_session_id text NOT NULL,revoked_at timestamptz);`); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceInviteQuotaSourceASNRequiresPositiveUint32(t *testing.T) {
	for _, test := range []struct {
		value string
		want  uint32
	}{{"0", 0}, {"64500", 64500}, {"4294967295", 4294967295}, {"4294967296", 0}, {"invalid", 0}} {
		if got := parseInviteSourceASN(test.value); got != test.want {
			t.Errorf("parseInviteSourceASN(%q) = %d, want %d", test.value, got, test.want)
		}
	}
}

func TestInviteQuotaBucketStartUsesEpochBucket(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 34, 56, 987000000, time.UTC)
	if got, want := inviteQuotaBucketStart(now, 300), time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("bucket start = %s, want %s", got, want)
	}
}
