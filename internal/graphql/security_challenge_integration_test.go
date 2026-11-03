// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/password"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5"
)

func TestGraphQLSecurityEmailChangeFlowMatchesClientOperations(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	for _, statement := range []string{
		`CREATE TABLE runtime_states (purpose text NOT NULL,token_hash text NOT NULL,lookup_key text,payload jsonb NOT NULL,attempts integer NOT NULL DEFAULT 0,consumed_at timestamptz,expires_at timestamptz NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(purpose,token_hash))`,
		`CREATE TABLE runtime_rolling_quota_counters (scope_key text NOT NULL,window_seconds integer NOT NULL,bucket_start timestamptz NOT NULL,count integer NOT NULL,expires_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,PRIMARY KEY(scope_key,window_seconds,bucket_start))`,
		`CREATE TABLE runtime_rolling_quota_reservations (id uuid NOT NULL DEFAULT gen_random_uuid(),scope_key text NOT NULL,window_seconds integer NOT NULL,bucket_start timestamptz NOT NULL,count integer NOT NULL,status text NOT NULL,expires_at timestamptz NOT NULL,updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE mail_deliveries (id uuid PRIMARY KEY DEFAULT gen_random_uuid(),mail_name text NOT NULL,mail_class text NOT NULL,priority text NOT NULL,status text NOT NULL,dedupe_key text,recipient_email text,recipient_hash text NOT NULL,recipient_domain text NOT NULL,recipient_user_id text,quota_decision jsonb,payload jsonb,send_after timestamptz NOT NULL,expires_at timestamptz,max_attempts integer NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp())`,
		`CREATE UNIQUE INDEX mail_deliveries_dedupe_key_idx ON mail_deliveries(dedupe_key) WHERE dedupe_key IS NOT NULL`,
		`INSERT INTO app_configs(id,value) VALUES('crypto.privateKey','"graph-security-test-key"'::jsonb),('server.name','"Test Notes"'::jsonb)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create security challenge GraphQL schema: %v", err)
		}
	}
	cfg := config.Config{NodeEnv: "test", HTTPS: true, Host: "localhost", Port: 3010, Deployment: "cloud"}
	userRepository := users.NewRepository(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	securityChallenges := auth.NewSecurityChallengeService(pool, cfg, "graph-security-test-key")
	publisher := &profilePublisherRecorder{}
	handler := NewHandlerWithServices(cfg, auth.NewSessionResolver(pool), userService, workspaceRepository, nil, publisher, nil, nil, HandlerDependencies{
		SecurityChallenges: SecurityChallengeDependencies{Service: securityChallenges},
	})

	prepared := graphRequest(t, handler, `mutation($callbackUrl: String!) { sendChangeEmail(callbackUrl: $callbackUrl) }`, map[string]any{
		"callbackUrl": "/email-change",
	}, fixture.sessionID, fixture.userID)
	preparedData, ok := prepared["data"].(map[string]any)
	if !ok || preparedData["sendChangeEmail"] != true {
		t.Fatalf("sendChangeEmail result = %#v", prepared)
	}
	changeURL := securityGraphMailURL(t, pool, "ChangeEmail")
	changeToken := changeURL.Query().Get("token")
	verified := graphRequest(t, handler, `mutation($callbackUrl: String!, $email: String!, $token: String!) {
		sendVerifyChangeEmail(callbackUrl: $callbackUrl, email: $email, token: $token)
	}`, map[string]any{
		"callbackUrl": "/email-change-verify", "email": "New@example.test", "token": changeToken,
	}, fixture.sessionID, fixture.userID)
	if verified["data"].(map[string]any)["sendVerifyChangeEmail"] != true {
		t.Fatalf("sendVerifyChangeEmail result = %#v", verified)
	}
	verifyURL := securityGraphMailURL(t, pool, "VerifyChangeEmail")
	if verifyURL.Query().Get("email") != "New@example.test" || verifyURL.Query().Get("token") == "" {
		t.Fatalf("verify URL = %q", verifyURL.String())
	}
	unsafe := graphRequest(t, handler, `mutation($callbackUrl: String!) { sendChangeEmail(callbackUrl: $callbackUrl) }`, map[string]any{
		"callbackUrl": "https://evil.example/email-change",
	}, fixture.sessionID, fixture.userID)
	unsafeErrors, ok := unsafe["errors"].([]any)
	if !ok || len(unsafeErrors) != 1 || unsafeErrors[0].(map[string]any)["extensions"].(map[string]any)["name"] != "action_forbidden" {
		t.Fatalf("unsafe callback result = %#v", unsafe)
	}
	invalidChallenge := graphRequest(t, handler, `mutation($email: String!, $token: String!) {
		changeEmail(email: $email, token: $token) { id }
	}`, map[string]any{"email": verifyURL.Query().Get("email"), "token": "invalid-token"}, fixture.sessionID, fixture.userID)
	invalidErrors, hasInvalidErrors := invalidChallenge["errors"].([]any)
	if !hasInvalidErrors || len(invalidErrors) == 0 || len(publisher.userIDs) != 0 {
		t.Fatalf("invalid changeEmail challenge result=%#v publisher=%#v", invalidChallenge, publisher.userIDs)
	}
	changed := graphRequest(t, handler, `mutation($email: String!, $token: String!) {
		changeEmail(email: $email, token: $token) { id email }
	}`, map[string]any{
		"email": verifyURL.Query().Get("email"), "token": verifyURL.Query().Get("token"),
	}, fixture.sessionID, fixture.userID)
	if got := changed["data"].(map[string]any)["changeEmail"].(map[string]any)["email"]; got != "web@example.test" {
		t.Fatalf("changeEmail returned user snapshot email=%v, want pre-mutation user snapshot", got)
	}
	if len(publisher.userIDs) != 1 || publisher.userIDs[0] != fixture.userID {
		t.Fatalf("changeEmail published user updates=%v, want [%s]", publisher.userIDs, fixture.userID)
	}
	var email string
	if err := pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, fixture.userID).Scan(&email); err != nil || email != "New@example.test" {
		t.Fatalf("persisted email=%q err=%v", email, err)
	}
	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE user_id=$1`, fixture.userID).Scan(&sessionCount); err != nil || sessionCount != 0 {
		t.Fatalf("email change left user sessions=%d err=%v", sessionCount, err)
	}
	var changedPayload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM mail_deliveries WHERE mail_name='EmailChanged' ORDER BY created_at DESC LIMIT 1`).Scan(&changedPayload); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Props map[string]string `json:"props"`
	}
	if err := json.Unmarshal(changedPayload, &payload); err != nil || payload.Props["to"] != "New@example.test" {
		t.Fatalf("EmailChanged payload=%s err=%v", changedPayload, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES('security-password-user-session',$1,$2,now())`, fixture.sessionID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	preparedPassword := graphRequest(t, handler, `mutation($callbackUrl: String!) { sendChangePasswordEmail(callbackUrl: $callbackUrl) }`, map[string]any{
		"callbackUrl": "/password-change",
	}, fixture.sessionID, fixture.userID)
	if preparedPassword["data"].(map[string]any)["sendChangePasswordEmail"] != true {
		t.Fatalf("sendChangePasswordEmail result = %#v", preparedPassword)
	}
	passwordURL := securityGraphMailURL(t, pool, "ChangePassword")
	changedPassword := graphRequest(t, handler, `mutation($userId: String!, $token: String!, $newPassword: String!) {
		changePassword(userId: $userId, token: $token, newPassword: $newPassword)
	}`, map[string]any{
		"userId": fixture.userID, "token": passwordURL.Query().Get("token"), "newPassword": "graph-new-password",
	}, "", "")
	if changedPassword["data"].(map[string]any)["changePassword"] != true {
		t.Fatalf("changePassword result = %#v", changedPassword)
	}
	if len(publisher.userIDs) != 2 || publisher.userIDs[1] != fixture.userID {
		t.Fatalf("changePassword published user updates=%v, want two updates for %s", publisher.userIDs, fixture.userID)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET email_verified=NULL WHERE id=$1`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES('security-verify-email-user-session',$1,$2,now())`, fixture.sessionID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	preparedVerification := graphRequest(t, handler, `mutation($callbackUrl: String!) { sendVerifyEmail(callbackUrl: $callbackUrl) }`, map[string]any{
		"callbackUrl": "/email-verification",
	}, fixture.sessionID, fixture.userID)
	verificationData, hasVerificationData := preparedVerification["data"].(map[string]any)
	if !hasVerificationData || verificationData["sendVerifyEmail"] != true {
		t.Fatalf("sendVerifyEmail result = %#v", preparedVerification)
	}
	verificationURL := securityGraphMailURL(t, pool, "VerifyEmail")
	verificationToken := verificationURL.Query().Get("token")
	if verificationToken == "" || verificationURL.Query().Has("userId") {
		t.Fatalf("VerifyEmail URL = %q, want a token without userId", verificationURL.String())
	}
	verificationTokenHash := sha256.Sum256([]byte(verificationToken))
	tag, err := pool.Exec(ctx, `UPDATE runtime_states SET expires_at=clock_timestamp()-INTERVAL '1 second'
		WHERE purpose='auth_challenge:verify_email' AND token_hash=$1`, hex.EncodeToString(verificationTokenHash[:]))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("expire verification challenge rows=%d err=%v", tag.RowsAffected(), err)
	}
	var authEpochBeforeVerification int
	if err := pool.QueryRow(ctx, `SELECT auth_epoch FROM users WHERE id=$1`, fixture.userID).Scan(&authEpochBeforeVerification); err != nil {
		t.Fatal(err)
	}
	var verifiedAt *time.Time
	expiredVerification := graphRequest(t, handler, `mutation($token: String!) { verifyEmail(token: $token) }`, map[string]any{
		"token": verificationToken,
	}, fixture.sessionID, fixture.userID)
	if _, ok := expiredVerification["errors"].([]any); !ok {
		t.Fatalf("expired verifyEmail token result = %#v, want an error", expiredVerification)
	}
	var expiredConsumedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT consumed_at FROM runtime_states WHERE purpose='auth_challenge:verify_email' AND token_hash=$1`, hex.EncodeToString(verificationTokenHash[:])).Scan(&expiredConsumedAt); err != nil || expiredConsumedAt != nil {
		t.Fatalf("expired verification challenge consumed_at=%v err=%v", expiredConsumedAt, err)
	}
	if err := pool.QueryRow(ctx, `SELECT email_verified FROM users WHERE id=$1`, fixture.userID).Scan(&verifiedAt); err != nil || verifiedAt != nil {
		t.Fatalf("expired verifyEmail token changed email_verified=%v err=%v", verifiedAt, err)
	}
	preparedVerification = graphRequest(t, handler, `mutation($callbackUrl: String!) { sendVerifyEmail(callbackUrl: $callbackUrl) }`, map[string]any{
		"callbackUrl": "/email-verification",
	}, fixture.sessionID, fixture.userID)
	if preparedVerification["data"].(map[string]any)["sendVerifyEmail"] != true {
		t.Fatalf("sendVerifyEmail retry result = %#v", preparedVerification)
	}
	verificationURL = securityGraphMailURL(t, pool, "VerifyEmail")
	verificationToken = verificationURL.Query().Get("token")
	verificationTokenHash = sha256.Sum256([]byte(verificationToken))
	invalidVerification := graphRequest(t, handler, `mutation($token: String!) { verifyEmail(token: $token) }`, map[string]any{
		"token": "invalid-token",
	}, fixture.sessionID, fixture.userID)
	if _, ok := invalidVerification["errors"].([]any); !ok {
		t.Fatalf("invalid verifyEmail token result = %#v, want an error", invalidVerification)
	}
	if err := pool.QueryRow(ctx, `SELECT email_verified FROM users WHERE id=$1`, fixture.userID).Scan(&verifiedAt); err != nil || verifiedAt != nil {
		t.Fatalf("invalid verifyEmail token changed email_verified=%v err=%v", verifiedAt, err)
	}
	const otherUserID = "security-verify-email-other-user"
	const otherSessionID = "security-verify-email-other-session"
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email,created_at,disabled,auth_epoch)
		VALUES($1,'Other User','other-verify@example.test',now(),false,0)`, otherUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, otherSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at)
		VALUES('security-verify-email-other-user-session',$1,$2,now())`, otherSessionID, otherUserID); err != nil {
		t.Fatal(err)
	}
	wrongUserVerification := graphRequest(t, handler, `mutation($token: String!) { verifyEmail(token: $token) }`, map[string]any{
		"token": verificationToken,
	}, otherSessionID, otherUserID)
	if _, ok := wrongUserVerification["errors"].([]any); !ok {
		t.Fatalf("wrong-user verifyEmail token result = %#v, want an error", wrongUserVerification)
	}
	var wrongUserVerifiedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT email_verified FROM users WHERE id=$1`, otherUserID).Scan(&wrongUserVerifiedAt); err != nil || wrongUserVerifiedAt != nil {
		t.Fatalf("wrong-user verifyEmail changed email_verified=%v err=%v", wrongUserVerifiedAt, err)
	}
	var wrongUserConsumedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT consumed_at FROM runtime_states WHERE purpose='auth_challenge:verify_email' AND token_hash=$1`, hex.EncodeToString(verificationTokenHash[:])).Scan(&wrongUserConsumedAt); err != nil || wrongUserConsumedAt != nil {
		t.Fatalf("wrong-user verification challenge consumed_at=%v err=%v", wrongUserConsumedAt, err)
	}
	verifiedEmail := graphRequest(t, handler, `mutation($token: String!) { verifyEmail(token: $token) }`, map[string]any{
		"token": verificationToken,
	}, fixture.sessionID, fixture.userID)
	if verifiedEmail["data"].(map[string]any)["verifyEmail"] != true {
		t.Fatalf("verifyEmail result = %#v", verifiedEmail)
	}
	var authEpochAfterVerification int
	if err := pool.QueryRow(ctx, `SELECT email_verified,auth_epoch FROM users WHERE id=$1`, fixture.userID).Scan(&verifiedAt, &authEpochAfterVerification); err != nil || verifiedAt == nil || authEpochAfterVerification != authEpochBeforeVerification {
		t.Fatalf("verifyEmail state email_verified=%v auth_epoch=%d before=%d err=%v", verifiedAt, authEpochAfterVerification, authEpochBeforeVerification, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE user_id=$1`, fixture.userID).Scan(&sessionCount); err != nil || sessionCount != 1 {
		t.Fatalf("verifyEmail changed user sessions=%d err=%v, want one", sessionCount, err)
	}
	if len(publisher.userIDs) != 2 {
		t.Fatalf("verifyEmail published user updates=%v, want none", publisher.userIDs)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET email_verified=NULL WHERE id=$1`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	replayedVerification := graphRequest(t, handler, `mutation($token: String!) { verifyEmail(token: $token) }`, map[string]any{
		"token": verificationToken,
	}, fixture.sessionID, fixture.userID)
	if _, ok := replayedVerification["errors"].([]any); !ok {
		t.Fatalf("replayed verifyEmail token result = %#v, want an error", replayedVerification)
	}

	unauthenticated := graphRequest(t, handler, `mutation($callbackUrl: String!) { sendChangeEmail(callbackUrl: $callbackUrl) }`, map[string]any{
		"callbackUrl": "/email-change",
	}, "", "")
	if !strings.Contains(graphErrors(unauthenticated), ErrAuthenticationRequired.Error()) {
		t.Fatalf("unauthenticated sendChangeEmail = %#v", unauthenticated)
	}
	unauthenticatedVerification := graphRequest(t, handler, `mutation($token: String!) { verifyEmail(token: $token) }`, map[string]any{
		"token": verificationToken,
	}, "", "")
	if !strings.Contains(graphErrors(unauthenticatedVerification), ErrAuthenticationRequired.Error()) {
		t.Fatalf("unauthenticated verifyEmail = %#v", unauthenticatedVerification)
	}
}

func TestAdminCreateChangePasswordURLCompletesClientPasswordReset(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	for _, statement := range []string{
		`CREATE TABLE runtime_states (purpose text NOT NULL,token_hash text NOT NULL,lookup_key text,payload jsonb NOT NULL,attempts integer NOT NULL DEFAULT 0,consumed_at timestamptz,expires_at timestamptz NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(purpose,token_hash))`,
		`INSERT INTO app_configs(id,value) VALUES('crypto.privateKey','"admin-password-reset-test-key"'::jsonb)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create Admin password reset schema: %v", err)
		}
	}
	cfg := config.Config{NodeEnv: "test", HTTPS: true, Host: "localhost", Port: 3010, Deployment: "affine"}
	workspaceRepository := workspaces.NewRepository(pool)
	userService := users.NewService(users.NewRepository(pool), workspaces.NewAuthorizationService(workspaceRepository))
	challengeService := auth.NewSecurityChallengeService(pool, cfg, "admin-password-reset-test-key")
	handler := NewHandlerWithServices(cfg, auth.NewSessionResolver(pool), userService, workspaceRepository, nil, nil, nil, nil, HandlerDependencies{
		SecurityChallenges: SecurityChallengeDependencies{Service: challengeService},
	})
	mutation := `mutation($callbackUrl: String!, $userId: String!) {
		createChangePasswordUrl(callbackUrl: $callbackUrl, userId: $userId)
	}`
	created := graphRequest(t, handler, mutation, map[string]any{
		"callbackUrl": "/auth/changePassword?source=admin&tag=hello%20world&tilde=%7E&star=%2A&tag=second",
		"userId":      fixture.userID,
	}, fixture.sessionID, fixture.userID)
	if created["errors"] != nil {
		t.Fatalf("admin createChangePasswordUrl errors = %#v", created["errors"])
	}
	resetURLString, ok := created["data"].(map[string]any)["createChangePasswordUrl"].(string)
	if !ok {
		t.Fatalf("admin password reset URL = %#v", created)
	}
	resetURL, err := url.Parse(resetURLString)
	if err != nil {
		t.Fatalf("parse admin password reset URL: %v", err)
	}
	resetToken := resetURL.Query().Get("token")
	if resetURL.Scheme != "https" || resetURL.Host != "localhost:3010" || resetURL.Path != "/auth/changePassword" ||
		resetURL.Query().Get("source") != "admin" || resetURL.Query().Get("userId") != fixture.userID || resetToken == "" {
		t.Fatalf("admin password reset URL = %q", resetURLString)
	}
	expectedRawQuery := "source=admin&tag=hello+world&tilde=%7E&star=*&tag=second&userId=" + url.QueryEscape(fixture.userID) + "&token=" + url.QueryEscape(resetToken)
	if resetURL.RawQuery != expectedRawQuery {
		t.Fatalf("admin password reset query = %q, want ordered legacy encoding %q", resetURL.RawQuery, expectedRawQuery)
	}
	tokenHash := sha256.Sum256([]byte(resetToken))
	var storedHash string
	var payload []byte
	var expiresAt time.Time
	if err := pool.QueryRow(ctx, `SELECT token_hash,payload,expires_at FROM runtime_states
		WHERE purpose='auth_challenge:change_password'`).Scan(&storedHash, &payload, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if storedHash != hex.EncodeToString(tokenHash[:]) || storedHash == resetToken {
		t.Fatalf("stored reset token hash = %q, want SHA-256 digest", storedHash)
	}
	var state struct {
		UserID    string `json:"userId"`
		AuthEpoch int    `json:"authEpoch"`
	}
	if err := json.Unmarshal(payload, &state); err != nil || state.UserID != fixture.userID || state.AuthEpoch != 0 {
		t.Fatalf("password reset challenge payload = %#v, err=%v", state, err)
	}
	remaining := time.Until(expiresAt)
	if remaining < 29*time.Minute || remaining > 30*time.Minute {
		t.Fatalf("password reset challenge lifetime = %s, want 30 minutes", remaining)
	}

	const nonAdminSessionID = "admin-password-reset-non-admin-session"
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, "admin-password-reset-non-admin-user-session", nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	denied := graphRequest(t, handler, mutation, map[string]any{
		"callbackUrl": "/auth/changePassword",
		"userId":      fixture.userID,
	}, nonAdminSessionID, fixture.sharedUserID)
	assertGraphQLAdminDenied(t, denied)
	unsafe := graphRequest(t, handler, mutation, map[string]any{
		"callbackUrl": "https://evil.example/auth/changePassword",
		"userId":      fixture.userID,
	}, fixture.sessionID, fixture.userID)
	assertSigningKeyError(t, unsafe, "action_forbidden", http.StatusForbidden)
	var challengeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_states WHERE purpose='auth_challenge:change_password'`).Scan(&challengeCount); err != nil || challengeCount != 1 {
		t.Fatalf("rejected Admin password reset requests left %d challenges, err=%v", challengeCount, err)
	}

	wrongUser := graphRequest(t, handler, `mutation($newPassword: String!, $token: String!, $userId: String!) {
		changePassword(newPassword: $newPassword, token: $token, userId: $userId)
	}`, map[string]any{
		"newPassword": "wrong-user-reset-password",
		"token":       resetToken,
		"userId":      fixture.sharedUserID,
	}, "", "")
	if _, ok := wrongUser["errors"].([]any); !ok {
		t.Fatalf("wrong-user password reset = %#v, want an error", wrongUser)
	}
	var consumedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT consumed_at FROM runtime_states
		WHERE purpose='auth_challenge:change_password' AND token_hash=$1`, storedHash).Scan(&consumedAt); err != nil || consumedAt != nil {
		t.Fatalf("wrong-user attempt consumed target user's challenge: consumed_at=%v err=%v", consumedAt, err)
	}

	completed := graphRequest(t, handler, `mutation($newPassword: String!, $token: String!, $userId: String!) {
		changePassword(newPassword: $newPassword, token: $token, userId: $userId)
	}`, map[string]any{
		"newPassword": "admin-reset-password",
		"token":       resetToken,
		"userId":      fixture.userID,
	}, "", "")
	if completed["errors"] != nil || completed["data"].(map[string]any)["changePassword"] != true {
		t.Fatalf("anonymous password reset completion = %#v", completed)
	}
	var storedPassword string
	var authEpoch, sessions int
	if err := pool.QueryRow(ctx, `SELECT password,auth_epoch FROM users WHERE id=$1`, fixture.userID).Scan(&storedPassword, &authEpoch); err != nil || !password.Verify(storedPassword, "admin-reset-password") || authEpoch != 1 {
		t.Fatalf("reset user password verified=%v auth_epoch=%d err=%v", password.Verify(storedPassword, "admin-reset-password"), authEpoch, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE user_id=$1`, fixture.userID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("password reset left %d user sessions, err=%v", sessions, err)
	}
	replay := graphRequest(t, handler, `mutation($newPassword: String!, $token: String!, $userId: String!) {
		changePassword(newPassword: $newPassword, token: $token, userId: $userId)
	}`, map[string]any{
		"newPassword": "replayed-admin-reset-password",
		"token":       resetToken,
		"userId":      fixture.userID,
	}, "", "")
	if _, ok := replay["errors"].([]any); !ok {
		t.Fatalf("replayed Admin password reset = %#v, want an error", replay)
	}
}

func securityGraphMailURL(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, name string) *url.URL {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM mail_deliveries WHERE mail_name=$1 ORDER BY created_at DESC LIMIT 1`, name).Scan(&payload); err != nil {
		t.Fatalf("read %s delivery: %v", name, err)
	}
	var decoded struct {
		Props map[string]string `json:"props"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode %s delivery: %v", name, err)
	}
	parsed, err := url.Parse(decoded.Props["url"])
	if err != nil {
		t.Fatalf("parse %s URL: %v", name, err)
	}
	return parsed
}
