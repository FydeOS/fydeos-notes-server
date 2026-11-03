// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"context"
	"io"
	"mime/quotedprintable"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/mailquota"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMemberRemovedDeliveryRetriesAndAnonymizes(t *testing.T) {
	firstSMTP := startSMTPTestServer(t, 450, "temporary user@example.test code 123456 https://secret.example/reset")
	secondSMTP := startSMTPTestServer(t, 0, "")
	t.Setenv("MAILER_HOST", "127.0.0.1")
	t.Setenv("MAILER_PORT", strconv.Itoa(firstSMTP.port))
	t.Setenv("MAILER_SENDER", "Notes <notes@example.test>")
	t.Setenv("MAILER_SECURE", "false")
	t.Setenv("MAILER_IGNORE_TLS", "false")
	t.Setenv("MAILER_SERVERNAME", "notes.example.test")

	pool := mailDeliveryTestPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE app_configs (id text PRIMARY KEY, value jsonb NOT NULL)`,
		`CREATE TABLE users (id text PRIMARY KEY, email text NOT NULL, disabled boolean NOT NULL DEFAULT false)`,
		`CREATE TABLE workspaces (id text PRIMARY KEY, name text)`,
		`CREATE TABLE runtime_rolling_quota_counters (
			scope_key text NOT NULL, window_seconds integer NOT NULL, bucket_start timestamptz NOT NULL,
			count integer NOT NULL, expires_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
			PRIMARY KEY(scope_key,window_seconds,bucket_start)
		)`,
		`CREATE TABLE runtime_rolling_quota_reservations (
			id uuid NOT NULL DEFAULT gen_random_uuid(), scope_key text NOT NULL, window_seconds integer NOT NULL,
			bucket_start timestamptz NOT NULL, count integer NOT NULL, status text NOT NULL, expires_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE mail_deliveries (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), mail_name text NOT NULL, mail_class text NOT NULL,
			priority text NOT NULL DEFAULT 'normal', status text NOT NULL, dedupe_key text,
			recipient_email text, recipient_hash text NOT NULL, recipient_domain text NOT NULL,
			recipient_user_id text, actor_user_id text, workspace_id text, quota_decision jsonb, payload jsonb, send_after timestamptz NOT NULL DEFAULT now(),
			expires_at timestamptz, attempt_count integer NOT NULL DEFAULT 0, max_attempts integer NOT NULL DEFAULT 3,
			locked_by text, locked_until timestamptz, first_attempt_at timestamptz, last_attempt_at timestamptz,
			sent_at timestamptz, settled_at timestamptz, canceled_at timestamptz, failed_at timestamptz,
			provider_message_id text, provider_response text, last_error_code text, last_error text,
			retention_state text NOT NULL DEFAULT 'full', anonymized_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
			CHECK (status IN ('queued','sending','retry_wait','sent','skipped','failed','canceled')),
			CHECK (retention_state IN ('full','anonymized')),
			CHECK ((status IN ('queued','sending','retry_wait') AND retention_state='full' AND recipient_email IS NOT NULL AND payload IS NOT NULL)
			OR (status IN ('sent','skipped','failed','canceled') AND retention_state='anonymized' AND recipient_email IS NULL AND payload IS NULL AND anonymized_at IS NOT NULL))
		)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create mail delivery fixture: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,disabled) VALUES('user-1','user@example.test',false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name) VALUES('workspace-1','Project North Star')`); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(pool, config.Config{}, func(context.Context) (string, error) { return "test-private-key", nil }, nil)
	quotaKey := "mail:recipient:" + mailquota.RecipientScopeHash("user@example.test") + ":class:workspace_lifecycle"
	quotaNow := time.Now().UTC()
	quotaBucket := time.Unix(quotaNow.Unix()-quotaNow.Unix()%300, 0).UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO runtime_rolling_quota_counters(scope_key,window_seconds,bucket_start,count,expires_at,updated_at)
		VALUES($1,3600,$2,20,$3,$4)`, quotaKey, quotaBucket, quotaNow.Add(time.Hour), quotaNow); err != nil {
		t.Fatal(err)
	}
	if err := repository.QueueMemberRemoved(ctx, "user-1", "workspace-1"); err != nil {
		t.Fatalf("record quota-denied member removal: %v", err)
	}
	var deniedStatus, deniedDecision string
	var deniedRecipient *string
	var deniedPayload []byte
	if err := pool.QueryRow(ctx, `SELECT status,COALESCE(quota_decision->>'allowed',''),recipient_email,payload FROM mail_deliveries`).
		Scan(&deniedStatus, &deniedDecision, &deniedRecipient, &deniedPayload); err != nil {
		t.Fatal(err)
	}
	if deniedStatus != "skipped" || deniedDecision != "false" || deniedRecipient != nil || deniedPayload != nil {
		t.Fatalf("quota-denied delivery status=%q allowed=%q recipient=%v payload=%s", deniedStatus, deniedDecision, deniedRecipient, deniedPayload)
	}
	var quotaScopeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_rolling_quota_counters`).Scan(&quotaScopeCount); err != nil || quotaScopeCount != 1 {
		t.Fatalf("quota-denied request changed counters: scopes=%d err=%v", quotaScopeCount, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM mail_deliveries`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM runtime_rolling_quota_counters`); err != nil {
		t.Fatal(err)
	}
	if err := repository.QueueMemberRemoved(ctx, "user-1", "workspace-1"); err != nil {
		t.Fatalf("queue member removal: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_rolling_quota_counters`).Scan(&quotaScopeCount); err != nil || quotaScopeCount != 4 {
		t.Fatalf("accepted delivery committed quota scopes=%d err=%v; want 4", quotaScopeCount, err)
	}

	settings, err := LoadSettings(ctx, pool, config.Config{}, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorker(repository, NewSMTPSender(settings), settings)
	if err != nil {
		t.Fatal(err)
	}
	firstClaim, err := repository.ClaimReady(ctx, "mail-delivery-first", 1, settings.Lease)
	if err != nil || len(firstClaim) != 1 {
		t.Fatalf("first delivery claim = %d, %v; want one row", len(firstClaim), err)
	}
	competingClaim, err := repository.ClaimReady(ctx, "mail-delivery-second", 1, settings.Lease)
	if err != nil || len(competingClaim) != 0 {
		t.Fatalf("competing delivery claim = %d, %v; want no duplicate row", len(competingClaim), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE mail_deliveries SET locked_until=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := repository.ClaimReady(ctx, "mail-delivery-second", 1, settings.Lease)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("expired lease reclaim = %d, %v; want one row", len(reclaimed), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE mail_deliveries SET locked_until=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	worker.workerID = "mail-delivery-second"
	processed, err := worker.ProcessReady(ctx)
	if err != nil || processed != 1 {
		t.Fatalf("first worker batch processed=%d err=%v", processed, err)
	}
	var status, lastError string
	var recipient *string
	var payload []byte
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,recipient_email,payload,attempt_count,last_error FROM mail_deliveries`).Scan(&status, &recipient, &payload, &attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "retry_wait" || recipient == nil || len(payload) == 0 || attempts != 1 {
		t.Fatalf("retry row status=%q recipient=%v payload=%s attempts=%d", status, recipient, payload, attempts)
	}
	for _, secret := range []string{"user@example.test", "123456", "https://secret.example/reset"} {
		if strings.Contains(lastError, secret) {
			t.Fatalf("mail error retained sensitive value %q: %s", secret, lastError)
		}
	}
	if !strings.Contains(lastError, "[email]") || !strings.Contains(lastError, "[number]") || !strings.Contains(lastError, "[url]") {
		t.Fatalf("mail error redaction result = %q", lastError)
	}

	if _, err := pool.Exec(ctx, `UPDATE mail_deliveries SET send_after=clock_timestamp()-interval '1 second' WHERE status='retry_wait'`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILER_PORT", strconv.Itoa(secondSMTP.port))
	settings, err = LoadSettings(ctx, pool, config.Config{}, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	worker, err = NewWorker(repository, NewSMTPSender(settings), settings)
	if err != nil {
		t.Fatal(err)
	}
	processed, err = worker.ProcessReady(ctx)
	if err != nil || processed != 1 {
		t.Fatalf("second worker batch processed=%d err=%v", processed, err)
	}
	var retention, providerResponse string
	var storedLastError pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT status,recipient_email,payload,attempt_count,retention_state,COALESCE(provider_response,''),last_error FROM mail_deliveries`).Scan(&status, &recipient, &payload, &attempts, &retention, &providerResponse, &storedLastError); err != nil {
		t.Fatal(err)
	}
	if status != "sent" || recipient != nil || payload != nil || attempts != 2 || retention != "anonymized" {
		t.Fatalf("terminal mail row status=%q recipient=%v payload=%s attempts=%d retention=%q", status, recipient, payload, attempts, retention)
	}
	if storedLastError.Valid {
		t.Fatalf("successful retry retained previous error %q", storedLastError.String)
	}
	select {
	case message := <-secondSMTP.body:
		parts := strings.SplitN(message, "\r\n\r\n", 2)
		if len(parts) != 2 {
			t.Fatalf("member removal SMTP message lacks body separator: %q", message)
		}
		decodedBody, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(parts[1])))
		if err != nil {
			t.Fatalf("decode member removal SMTP body: %v", err)
		}
		if !strings.Contains(parts[0], "Subject: You have been removed from a workspace") || !strings.Contains(string(decodedBody), "Project North Star") {
			t.Fatalf("member removal SMTP message = %q", message)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not send the retried message")
	}

	if providerResponse != "" {
		t.Fatalf("unexpected provider response %q", providerResponse)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO mail_deliveries(mail_name,mail_class,status,recipient_email,recipient_hash,
		recipient_domain,recipient_user_id,workspace_id,payload,send_after,attempt_count,max_attempts,locked_by,locked_until)
		VALUES('MemberRemoved','workspace_lifecycle','sending','user@example.test','hash','example.test','user-1','workspace-1',
		'{}'::jsonb,now(),3,3,'crashed-worker',now()-interval '1 second')`); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repository.ClaimReady(ctx, "recovery-worker", 10, settings.Lease); err != nil || len(claimed) != 0 {
		t.Fatalf("claim after final-attempt crash = %d, %v; want exhausted lease settled without retry", len(claimed), err)
	}
	var exhaustedStatus, exhaustedCode, exhaustedProviderResponse, exhaustedRetention string
	if err := pool.QueryRow(ctx, `SELECT status,last_error_code,provider_response,retention_state FROM mail_deliveries
		WHERE locked_by IS NULL AND last_error_code='delivery_outcome_unknown'`).
		Scan(&exhaustedStatus, &exhaustedCode, &exhaustedProviderResponse, &exhaustedRetention); err != nil {
		t.Fatal(err)
	}
	if exhaustedStatus != "failed" || exhaustedCode != "delivery_outcome_unknown" || exhaustedRetention != "anonymized" ||
		!strings.Contains(exhaustedProviderResponse, "outcome is unknown") {
		t.Fatalf("exhausted lease status=%q code=%q response=%q retention=%q", exhaustedStatus, exhaustedCode, exhaustedProviderResponse, exhaustedRetention)
	}
	if _, err := pool.Exec(ctx, `UPDATE mail_deliveries SET settled_at=clock_timestamp()-interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	deleted, err := repository.DeleteAnonymizedBefore(ctx, time.Now().Add(-30*24*time.Hour), 10)
	if err != nil || deleted != 2 {
		t.Fatalf("delete retained mail rows = %d, %v; want 2", deleted, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runtime_rolling_quota_counters(scope_key,window_seconds,bucket_start,count,expires_at,updated_at)
		VALUES('expired-scope',3600,now()-interval '2 hours',1,now()-interval '1 hour',now())`); err != nil {
		t.Fatal(err)
	}
	var reservationID string
	if err := pool.QueryRow(ctx, `INSERT INTO runtime_rolling_quota_reservations(scope_key,window_seconds,bucket_start,count,status,expires_at)
		VALUES('expired-scope',3600,now()-interval '2 hours',1,'reserved',now()-interval '1 hour') RETURNING id::text`).Scan(&reservationID); err != nil {
		t.Fatal(err)
	}
	cleaned, err := repository.CleanupExpiredRollingQuota(ctx, 1)
	if err != nil || cleaned != 2 {
		t.Fatalf("rolling quota cleanup = %d, %v; want one counter deletion and one reservation expiry", cleaned, err)
	}
	var reservationStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM runtime_rolling_quota_reservations WHERE id=$1::uuid`, reservationID).Scan(&reservationStatus); err != nil || reservationStatus != "expired" {
		t.Fatalf("expired quota reservation status=%q err=%v", reservationStatus, err)
	}
	var remainingExpiredCounters int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_rolling_quota_counters WHERE scope_key='expired-scope'`).Scan(&remainingExpiredCounters); err != nil || remainingExpiredCounters != 0 {
		t.Fatalf("expired quota counters remaining=%d err=%v", remainingExpiredCounters, err)
	}
}

func TestMailQuotaCountsLegacyBucketsThatOverlapWindow(t *testing.T) {
	pool := mailDeliveryTestPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE runtime_rolling_quota_counters (
			scope_key text NOT NULL, window_seconds integer NOT NULL, bucket_start timestamptz NOT NULL,
			count integer NOT NULL, expires_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
			PRIMARY KEY(scope_key,window_seconds,bucket_start)
		)`,
		`CREATE TABLE runtime_rolling_quota_reservations (
			id uuid NOT NULL DEFAULT gen_random_uuid(), scope_key text NOT NULL, window_seconds integer NOT NULL,
			bucket_start timestamptz NOT NULL, count integer NOT NULL, status text NOT NULL, expires_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT now()
		)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create quota fixture: %v", err)
		}
	}

	now := time.Unix(1_800_000_101, 0).UTC()
	windowStart := now.Add(-time.Hour)
	legacyBucket := time.Unix(windowStart.Unix()-windowStart.Unix()%300, 0).UTC()
	if !legacyBucket.Before(windowStart) || !legacyBucket.Add(300*time.Second).After(windowStart) {
		t.Fatal("test bucket must start before and overlap the rolling-window cutoff")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runtime_rolling_quota_counters(scope_key,window_seconds,bucket_start,count,expires_at,updated_at)
		VALUES('legacy-hour-counter',3600,$1,20,$2,$3)`, legacyBucket, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	assertQuotaDenied := func(scope mailquota.Scope) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = mailquota.Commit(ctx, tx, []mailquota.Scope{scope}, now)
		_ = tx.Rollback(ctx)
		if err != mailquota.ErrDenied {
			t.Fatalf("legacy bucket was not included for %q: got %v, want quota denial", scope.Key, err)
		}
	}
	assertQuotaDenied(mailquota.Scope{Key: "legacy-hour-counter", WindowSeconds: 3600, Limit: 20, LegacyBucketSeconds: 300})

	providerWindowStart := now.Add(-time.Minute)
	legacyProviderBucket := time.Unix(providerWindowStart.Unix()-providerWindowStart.Unix()%10, 0).UTC()
	if !legacyProviderBucket.Before(providerWindowStart) || !legacyProviderBucket.Add(10*time.Second).After(providerWindowStart) {
		t.Fatal("provider test bucket must start before and overlap the rolling-window cutoff")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runtime_rolling_quota_reservations(scope_key,window_seconds,bucket_start,count,status,expires_at)
		VALUES('legacy-provider-reservation',60,$1,1,'reserved',$2)`, legacyProviderBucket, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertQuotaDenied(mailquota.Scope{Key: "legacy-provider-reservation", WindowSeconds: 60, Limit: 1, LegacyBucketSeconds: 10})
}

func mailDeliveryTestPool(t *testing.T) *pgxpool.Pool {
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
	schema := "notes_mail_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated mail schema: %v", err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("parse mail integration pool config: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("open isolated mail pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
	})
	return pool
}
