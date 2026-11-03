// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/maildelivery"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
)

func TestSendTestEmailGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	nonAdminSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, uuid.NewString(), nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}

	sender := &smtpTestEmailSenderFake{}
	handler := NewHandlerWithServices(
		config.Config{Deployment: "selfhosted"},
		auth.NewSessionResolver(pool), nil, workspaces.NewRepository(pool), nil, nil, nil, nil,
		HandlerDependencies{SMTPTestEmailSender: sender},
	)
	mutation := `mutation sendTestEmail($config: JSONObject!) { sendTestEmail(config: $config) }`
	variables := map[string]any{"config": map[string]any{
		"name": "smtp.example.test", "host": "mail.example.test", "port": 2525,
		"sender": "Notes <notes@example.test>", "username": "smtp-user", "password": "smtp-secret",
		"ignoreTLS": true,
	}}

	denied := graphRequest(t, handler, mutation, variables, nonAdminSessionID, fixture.sharedUserID)
	deniedErrors, ok := denied["errors"].([]any)
	if !ok || len(deniedErrors) != 1 {
		t.Fatalf("non-admin sendTestEmail = %#v, want forbidden error", denied)
	}
	deniedExtensions := deniedErrors[0].(map[string]any)["extensions"].(map[string]any)
	if deniedExtensions["name"] != "action_forbidden" || deniedExtensions["status"] != float64(http.StatusForbidden) {
		t.Fatalf("non-admin sendTestEmail error = %#v", deniedExtensions)
	}
	if sender.calls != 0 {
		t.Fatalf("SMTP test sender calls = %d, want 0 for non-admin", sender.calls)
	}

	adminResult := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if got := adminResult["data"].(map[string]any)["sendTestEmail"]; got != true {
		t.Fatalf("admin sendTestEmail = %#v, want true", adminResult)
	}
	wantConfig := maildelivery.SMTPConfig{
		Name: "smtp.example.test", Host: "mail.example.test", Port: 2525,
		Sender: "Notes <notes@example.test>", Username: "smtp-user", Password: "smtp-secret", IgnoreTLS: true,
	}
	if sender.calls != 1 || sender.settings != wantConfig || sender.recipient != "web@example.test" {
		t.Fatalf("SMTP test sender received calls=%d config=%#v recipient=%q", sender.calls, sender.settings, sender.recipient)
	}
}

func TestSendTestEmailErrorPresentation(t *testing.T) {
	for _, test := range []struct {
		phase  string
		prefix string
		cause  string
	}{
		{phase: "verify", prefix: "Failed to verify your SMTP configuration. Cause: ", cause: "connection refused"},
		{phase: "send", prefix: "Failed to send test email. Cause: ", cause: "recipient rejected"},
	} {
		t.Run(test.phase, func(t *testing.T) {
			presented := presentGraphQLError(context.Background(), &maildelivery.SMTPTestError{
				Phase: test.phase, Cause: errors.New(test.cause),
			})
			if presented.Message != test.prefix+test.cause {
				t.Fatalf("presented message = %q", presented.Message)
			}
			if presented.Extensions["status"] != http.StatusBadRequest || presented.Extensions["name"] != "bad_request" {
				t.Fatalf("presented extensions = %#v", presented.Extensions)
			}
		})
	}
}

type smtpTestEmailSenderFake struct {
	calls     int
	settings  maildelivery.SMTPConfig
	recipient string
	err       error
}

func (f *smtpTestEmailSenderFake) SendTestEmail(_ context.Context, settings maildelivery.SMTPConfig, recipient string) error {
	f.calls++
	f.settings = settings
	f.recipient = recipient
	return f.err
}
