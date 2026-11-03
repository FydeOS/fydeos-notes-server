// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/maildelivery"
)

func TestNormalizeMailDeliveryWindowUsesUTCBoundaries(t *testing.T) {
	now := time.Date(2026, time.October, 19, 10, 35, 12, 0, time.FixedZone("UTC+8", 8*60*60))
	hours, err := normalizeMailDeliveryWindow(24, now)
	if err != nil {
		t.Fatal(err)
	}
	if hours.bucket != model.TimeBucketHour || hours.requestedSize != 24 || hours.effectiveSize != 24 ||
		!hours.from.Equal(time.Date(2026, time.October, 18, 3, 0, 0, 0, time.UTC)) ||
		!hours.to.Equal(time.Date(2026, time.October, 19, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("24-hour window = %#v", hours)
	}

	days, err := normalizeMailDeliveryWindow(168, now)
	if err != nil {
		t.Fatal(err)
	}
	if days.bucket != model.TimeBucketDay || days.requestedSize != 168 || days.effectiveSize != 7 ||
		!days.from.Equal(time.Date(2026, time.October, 13, 0, 0, 0, 0, time.UTC)) ||
		!days.to.Equal(time.Date(2026, time.October, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("168-hour window = %#v", days)
	}
	for _, invalid := range []int{0, 48, 169} {
		if _, err := normalizeMailDeliveryWindow(invalid, now); err == nil {
			t.Errorf("normalize window %d succeeded", invalid)
		}
	}
}

func TestAdminMailDeliveriesGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `CREATE TABLE mail_deliveries (
		mail_name text NOT NULL,
		mail_class text NOT NULL,
		status text NOT NULL,
		created_at timestamptz NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := []struct {
		name, class, status string
		age                 time.Duration
	}{
		{"SignIn", "auth", "sent", 90 * time.Minute},
		{"MagicLink", "auth", "failed", 3 * time.Hour},
		{"Invitation", "workspace_invitation", "skipped", 4 * time.Hour},
		{"Mention", "notification", "canceled", 5 * time.Hour},
		{"Invite", "collaboration_notice", "queued", 6 * time.Hour},
		{"Digest", "transactional", "sending", 7 * time.Hour},
		{"Webhook", "webhook", "retry_wait", 8 * time.Hour},
		{"A", "alpha", "sent", 9 * time.Hour},
		{"B", "beta", "sent", 10 * time.Hour},
		{"D", "delta", "sent", 11 * time.Hour},
		{"E", "epsilon", "sent", 12 * time.Hour},
		{"Z", "zeta", "sent", 13 * time.Hour},
		{"Old", "auth", "failed", 25 * time.Hour},
	}
	for _, row := range rows {
		if _, err := pool.Exec(ctx, `INSERT INTO mail_deliveries(mail_name,mail_class,status,created_at) VALUES($1,$2,$3,$4)`, row.name, row.class, row.status, now.Add(-row.age)); err != nil {
			t.Fatal(err)
		}
	}
	repository := maildelivery.NewRepository(pool, config.Config{}, nil, nil)
	queryWindow, err := normalizeMailDeliveryWindow(24, now)
	if err != nil {
		t.Fatal(err)
	}
	aggregates, err := repository.AdminAggregate(ctx, queryWindow.from, queryWindow.to, queryWindow.databaseBucket())
	if err != nil || len(aggregates) != 12 {
		t.Fatalf("repository aggregate count=%d err=%v, want 12 rows", len(aggregates), err)
	}
	handler := NewHandlerWithServices(config.Config{Deployment: "selfhosted", Host: "localhost", Port: 3010}, auth.NewSessionResolver(pool), nil, nil, nil, nil, nil, nil, HandlerDependencies{
		MailDeliveryAnalytics: repository,
	})
	query := `query($input: AdminMailDeliveriesInput){adminMailDeliveries(input:$input){window{from to timezone bucket requestedSize effectiveSize} summary{total sent failed skipped canceled queued sending retryWait successRate} byStatus{key label total points{bucket count}} byType{key label total points{bucket count}} byOutcome{key label total points{bucket count}}}}`

	admin := graphRequest(t, handler, query, map[string]any{"input": map[string]any{"hours": 24}}, fixture.sessionID, fixture.userID)
	if admin["errors"] != nil {
		t.Fatalf("admin analytics query errors = %#v", admin["errors"])
	}
	analytics := admin["data"].(map[string]any)["adminMailDeliveries"].(map[string]any)
	window := analytics["window"].(map[string]any)
	if window["timezone"] != "UTC" || window["bucket"] != "Hour" || window["requestedSize"] != float64(24) || window["effectiveSize"] != float64(24) {
		t.Fatalf("24-hour analytics window = %#v", window)
	}
	from, err := time.Parse(time.RFC3339Nano, window["from"].(string))
	if err != nil {
		t.Fatal(err)
	}
	to, err := time.Parse(time.RFC3339Nano, window["to"].(string))
	if err != nil || to.Sub(from) != 24*time.Hour || to.Minute() != 0 || to.Second() != 0 {
		t.Fatalf("24-hour window bounds = %s .. %s, err=%v", window["from"], window["to"], err)
	}
	summary := analytics["summary"].(map[string]any)
	if summary["total"] != float64(12) || summary["sent"] != float64(6) || summary["failed"] != float64(1) ||
		summary["skipped"] != float64(1) || summary["canceled"] != float64(1) || summary["queued"] != float64(1) ||
		summary["sending"] != float64(1) || summary["retryWait"] != float64(1) || summary["successRate"] != float64(2.0/3.0) {
		t.Fatalf("24-hour analytics summary = %#v", summary)
	}
	statusSeries := analytics["byStatus"].([]any)
	if len(statusSeries) != 7 {
		t.Fatalf("status series count = %d, want 7", len(statusSeries))
	}
	for _, raw := range statusSeries {
		series := raw.(map[string]any)
		if len(series["points"].([]any)) != 24 {
			t.Fatalf("status %q has %d buckets, want 24", series["key"], len(series["points"].([]any)))
		}
	}
	types := analytics["byType"].([]any)
	if len(types) != 6 {
		t.Fatalf("mail type series count = %d, want top 6", len(types))
	}
	wantTypeKeys := []string{"auth", "alpha", "beta", "collaboration_notice", "delta", "epsilon"}
	for i, raw := range types {
		if key := raw.(map[string]any)["key"]; key != wantTypeKeys[i] {
			t.Fatalf("mail type %d = %v, want %q", i, key, wantTypeKeys[i])
		}
	}
	outcomes := analytics["byOutcome"].([]any)
	wantOutcomes := []struct {
		key   string
		total float64
	}{{"successful", 6}, {"unsuccessful", 3}, {"pending", 3}}
	for i, raw := range outcomes {
		series := raw.(map[string]any)
		if series["key"] != wantOutcomes[i].key || series["total"] != wantOutcomes[i].total {
			t.Fatalf("outcome %d = %#v, want %#v", i, series, wantOutcomes[i])
		}
	}

	weekly := graphRequest(t, handler, query, map[string]any{"input": map[string]any{"hours": 168}}, fixture.sessionID, fixture.userID)
	if weekly["errors"] != nil {
		t.Fatalf("weekly analytics query errors = %#v", weekly["errors"])
	}
	weeklyData := weekly["data"].(map[string]any)["adminMailDeliveries"].(map[string]any)
	weeklyWindow := weeklyData["window"].(map[string]any)
	weeklySummary := weeklyData["summary"].(map[string]any)
	if weeklyWindow["bucket"] != "Day" || weeklyWindow["requestedSize"] != float64(168) || weeklyWindow["effectiveSize"] != float64(7) || weeklySummary["total"] != float64(13) {
		t.Fatalf("weekly analytics = %#v summary=%#v", weeklyWindow, weeklySummary)
	}

	invalid := graphRequest(t, handler, query, map[string]any{"input": map[string]any{"hours": 48}}, fixture.sessionID, fixture.userID)
	if invalid["errors"] == nil {
		t.Fatal("unsupported window did not fail")
	}
	invalidError := invalid["errors"].([]any)[0].(map[string]any)
	invalidExtensions := invalidError["extensions"].(map[string]any)
	if invalidError["message"] != "Mail delivery analytics window must be 24 or 168 hours." || invalidExtensions["status"] != float64(http.StatusBadRequest) || invalidExtensions["name"] != "bad_request" {
		t.Fatalf("invalid window error = %#v", invalidError)
	}

	for _, caller := range []struct {
		name, sessionID, userID string
	}{{"non-admin", fixture.sessionID, fixture.sharedUserID}, {"anonymous", "", ""}} {
		denied := graphRequest(t, handler, query, map[string]any{"input": map[string]any{"hours": 24}}, caller.sessionID, caller.userID)
		if denied["errors"] == nil {
			t.Fatalf("%s query was allowed", caller.name)
		}
		deniedError := denied["errors"].([]any)[0].(map[string]any)
		deniedExtensions := deniedError["extensions"].(map[string]any)
		if deniedExtensions["status"] != float64(http.StatusForbidden) || deniedExtensions["name"] != "action_forbidden" {
			t.Fatalf("%s denial = %#v", caller.name, deniedError)
		}
	}

	defaulted := graphRequest(t, handler, `query { adminMailDeliveries { window { bucket requestedSize effectiveSize } } }`, nil, fixture.sessionID, fixture.userID)
	if defaulted["errors"] != nil {
		t.Fatalf("default-window query errors = %#v", defaulted["errors"])
	}
	defaultWindow := defaulted["data"].(map[string]any)["adminMailDeliveries"].(map[string]any)["window"].(map[string]any)
	if defaultWindow["bucket"] != "Hour" || defaultWindow["requestedSize"] != float64(24) || defaultWindow["effectiveSize"] != float64(24) {
		t.Fatalf("default analytics window = %#v", defaultWindow)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE mail_deliveries`); err != nil {
		t.Fatal(err)
	}
	databaseFailure := graphRequest(t, handler, query, map[string]any{"input": map[string]any{"hours": 24}}, fixture.sessionID, fixture.userID)
	if databaseFailure["errors"] == nil {
		t.Fatal("database failure did not fail")
	}
	databaseError := databaseFailure["errors"].([]any)[0].(map[string]any)
	databaseExtensions := databaseError["extensions"].(map[string]any)
	if databaseError["message"] != "internal_server_error" || databaseExtensions["status"] != float64(http.StatusInternalServerError) || databaseExtensions["name"] != "internal_server_error" {
		t.Fatalf("database failure leaked details or changed legacy error class: %#v", databaseError)
	}
}

func TestAdminMailDeliveryAnalyticsUnavailableIsSanitized(t *testing.T) {
	resolver := &Resolver{MailDeliveryAnalytics: mailDeliveryAnalyticsReaderFunc(func(context.Context, time.Time, time.Time, string) ([]maildelivery.AdminAggregate, error) {
		return nil, fmt.Errorf("private database detail")
	})}
	principalContext := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "admin", Features: []string{administratorFeatureName}}})
	_, err := resolver.adminMailDeliveries(principalContext, nil)
	if !errors.Is(err, ErrMailDeliveryAnalyticsUnavailable) || err.Error() != ErrMailDeliveryAnalyticsUnavailable.Error() {
		t.Fatalf("sanitized analytics error = %v", err)
	}
}

type mailDeliveryAnalyticsReaderFunc func(context.Context, time.Time, time.Time, string) ([]maildelivery.AdminAggregate, error)

func (f mailDeliveryAnalyticsReaderFunc) AdminAggregate(ctx context.Context, from, to time.Time, bucket string) ([]maildelivery.AdminAggregate, error) {
	return f(ctx, from, to, bucket)
}
