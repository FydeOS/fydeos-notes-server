// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/database/migrations"
	"github.com/fydeos/fydeos-notes-server/internal/password"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func authTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse NOTES_TEST_DATABASE_URL: %v", err)
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
	schema := "notes_auth_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
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

const testPrivateKey = "integration-test-private-key"

type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	cfg  config.Config
}

func newFixture(t *testing.T) *fixture {
	t.Setenv("AFFINE_PRIVATE_KEY", testPrivateKey)
	return &fixture{t: t, pool: authTestPool(t), cfg: config.Config{NodeEnv: "test", Host: "localhost", Port: 3010}}
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f *fixture) user(email, plain string) string {
	f.t.Helper()
	id := uuid.NewString()
	var hash *string
	if plain != "" {
		h, err := password.Hash(plain)
		if err != nil {
			f.t.Fatal(err)
		}
		hash = &h
	}
	f.exec(`INSERT INTO users(id,name,email,password,email_verified) VALUES($1,$2,$3,$4,now())`, id, "User", email, hash)
	return id
}

// cookieSession creates a multi-user session holding the given users.
func (f *fixture) cookieSession(userIDs ...string) (string, []string) {
	sessionID := uuid.NewString()
	f.exec(`INSERT INTO multiple_users_sessions(id) VALUES($1)`, sessionID)
	var userSessions []string
	for i, u := range userIDs {
		id := uuid.NewString()
		f.exec(`INSERT INTO user_sessions(id,session_id,user_id,expires_at,created_at) VALUES($1,$2,$3,now()+interval '10 days',now()+make_interval(secs=>$4))`, id, sessionID, u, i)
		userSessions = append(userSessions, id)
	}
	return sessionID, userSessions
}

func jsonRequest(method, target string, body any) *http.Request {
	var reader *bytes.Reader
	if s, ok := body.(string); ok {
		reader = bytes.NewReader([]byte(s))
	} else {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, target, reader)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

func cookies(w *httptest.ResponseRecorder) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range w.Result().Cookies() {
		out[c.Name] = c
	}
	return out
}

func TestCookieResolution(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.user("first@example.com", "")
	second := f.user("second@example.com", "")
	sessionID, userSessions := f.cookieSession(first, second)
	resolver := NewSessionResolver(f.pool)
	request := func(session, user string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: session})
		if user != "" {
			r.AddCookie(&http.Cookie{Name: UserCookieName, Value: user})
		}
		return r
	}
	p, err := resolver.FromRequest(ctx, request(sessionID, ""))
	if err != nil || p.User.ID != second || p.AuthSessionID != "" || p.SessionID != sessionID {
		t.Fatalf("newest user principal=%+v err=%v", p, err)
	}
	if p, _ := resolver.FromRequest(ctx, request(sessionID, first)); p == nil || p.User.ID != first {
		t.Fatalf("narrowed principal=%+v", p)
	}
	if p, _ := resolver.FromRequest(ctx, request(sessionID, "bad user!")); p == nil || p.User.ID != second {
		t.Fatal("invalid user cookie must be ignored")
	}
	for _, s := range []string{"not-a-uuid", uuid.NewString()} {
		if _, err := resolver.FromRequest(ctx, request(s, "")); err != ErrNoSession {
			t.Fatalf("session %q err=%v", s, err)
		}
	}
	f.exec(`INSERT INTO user_features(id,user_id,reason,name,activated) VALUES(1,$1,'test','administrator',true)`, first)
	if p, _ := resolver.FromRequest(ctx, request(sessionID, first)); len(p.User.Features) != 1 {
		t.Fatalf("features=%v", p.User.Features)
	}
	f.exec(`UPDATE user_features SET expired_at=now()-interval '1 second'`)
	if p, _ := resolver.FromRequest(ctx, request(sessionID, first)); p.User.Features == nil || len(p.User.Features) != 0 {
		t.Fatalf("expired features=%v", p.User.Features)
	}
	// Linked revoked auth session hides the cookie session.
	f.exec(`INSERT INTO auth_sessions(id,user_session_id,installation_id,platform,idle_expires_at,absolute_expires_at,revoked_at) VALUES($1,$2,$3,'ios',now()+interval '1 day',now()+interval '2 days',now())`, uuid.NewString(), userSessions[0], uuid.NewString())
	if _, err := resolver.FromRequest(ctx, request(sessionID, first)); err != ErrNoSession {
		t.Fatalf("revoked linked session err=%v", err)
	}
	f.exec(`UPDATE users SET disabled=true WHERE id=$1`, second)
	if _, err := resolver.FromRequest(ctx, request(sessionID, second)); err != ErrNoSession {
		t.Fatalf("disabled err=%v", err)
	}

	// Refresh: remaining ≤ TTR extends, otherwise nothing changes.
	third := f.user("third@example.com", "")
	thirdSession, thirdUS := f.cookieSession(third)
	f.exec(`UPDATE user_sessions SET expires_at=now()+interval '1 day' WHERE id=$1`, thirdUS[0])
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r := request(thirdSession, third)
			r.Header.Set("x-affine-version", "1.2.3")
			if _, err := resolver.FromRequestWithResponse(ctx, w, r); err != nil {
				t.Errorf("refresh err=%v", err)
			}
		}()
	}
	wg.Wait()
	var expires time.Time
	var version *string
	if err := f.pool.QueryRow(ctx, `SELECT expires_at, refresh_client_version FROM user_sessions WHERE id=$1`, thirdUS[0]).Scan(&expires, &version); err != nil {
		t.Fatal(err)
	}
	if time.Until(expires) < 14*24*time.Hour || version == nil || *version != "1.2.3" {
		t.Fatalf("refreshed expiry=%v version=%v", expires, version)
	}
	w := httptest.NewRecorder()
	if _, err := resolver.FromRequestWithResponse(ctx, w, request(thirdSession, third)); err != nil || len(w.Result().Cookies()) != 0 {
		t.Fatalf("fresh session err=%v cookies=%v", err, w.Result().Cookies())
	}

	// Anonymous GET session with a stale cookie clears the cookies.
	w = httptest.NewRecorder()
	NewSessionHTTPHandler(resolver).ServeHTTP(w, request(uuid.NewString(), ""))
	if w.Body.String() != "{\"user\":null}\n" || len(w.Result().Cookies()) != 3 {
		t.Fatalf("anonymous session body=%q cookies=%d", w.Body.String(), len(w.Result().Cookies()))
	}
}

func signIn(f *fixture, h http.Handler, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := jsonRequest(http.MethodPost, "/api/auth/sign-in", body)
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPasswordSignIn(t *testing.T) {
	f := newFixture(t)
	f.cfg.HTTPS = true
	first := f.user("person@example.com", "correct horse")
	second := f.user("other@example.com", "battery staple")
	f.user("nopass@example.com", "")
	h := NewPasswordLoginHTTPHandlerWithDependencies(f.pool, f.cfg, nil, nil)
	w := signIn(f, h, map[string]string{"email": "Person@example.com", "password": "correct horse"}, nil)
	if w.Code != 200 || decode(t, w)["id"] != first {
		t.Fatalf("sign-in status=%d body=%s", w.Code, w.Body.String())
	}
	c := cookies(w)
	if !c[SessionCookieName].Secure || !c[SessionCookieName].HttpOnly || c[UserCookieName].Secure || c[CSRFCookieName].Value == "" {
		t.Fatalf("cookies=%v", c)
	}
	w = signIn(f, h, map[string]string{"email": "other@example.com", "password": "battery staple"}, func(r *http.Request) { r.AddCookie(c[SessionCookieName]) })
	if w.Code != 200 || cookies(w)[SessionCookieName].Value != c[SessionCookieName].Value {
		t.Fatalf("second sign-in status=%d", w.Code)
	}
	if n := f.count(`SELECT count(*) FROM user_sessions WHERE session_id=$1`, c[SessionCookieName].Value); n != 2 {
		t.Fatalf("multi-user session rows=%d", n)
	}
	_ = second

	before := f.count(`SELECT count(*) FROM user_sessions`)
	w = signIn(f, h, map[string]string{"email": "person@example.com", "password": "correct horse"}, func(r *http.Request) {
		r.Header.Set("x-affine-client-kind", "native")
		r.Header.Set("Origin", "capacitor://localhost")
	})
	if w.Code != 200 || decode(t, w)["exchangeCode"] == nil || f.count(`SELECT count(*) FROM user_sessions`) != before {
		t.Fatalf("native sign-in status=%d body=%s", w.Code, w.Body.String())
	}
	w = signIn(f, h, map[string]string{"email": "person@example.com", "password": "correct horse"}, func(r *http.Request) {
		r.Header.Set("x-affine-client-kind", "native")
		r.Header.Set("Origin", "https://evil")
	})
	if decode(t, w)["exchangeCode"] != nil {
		t.Fatal("native header with foreign origin treated as native")
	}

	for name, tc := range map[string]struct {
		body any
		want string
	}{
		"wrong password": {map[string]string{"email": "person@example.com", "password": "nope"}, "WRONG_SIGN_IN_CREDENTIALS"},
		"unknown email":  {map[string]string{"email": "ghost@example.com", "password": "nope"}, "WRONG_SIGN_IN_CREDENTIALS"},
		"no password":    {map[string]string{"email": "nopass@example.com", "password": "nope"}, "WRONG_SIGN_IN_METHOD"},
		"trailing value": {`{"email":"a@b.co"} {}`, "VALIDATION_ERROR"},
		"unknown field":  {`{"email":"a@b.co","x":"y"}`, "VALIDATION_ERROR"},
		"null password":  {`{"email":"a@b.co","password":null}`, "VALIDATION_ERROR"},
		"invalid email":  {map[string]string{"email": "nobody", "password": "x"}, "INVALID_EMAIL"},
	} {
		w := signIn(f, h, tc.body, func(r *http.Request) { r.RemoteAddr = "203.0.113." + name[:1] + ":1" })
		if w.Code != 400 || decode(t, w)["name"] != tc.want {
			t.Fatalf("%s: status=%d body=%s", name, w.Code, w.Body.String())
		}
	}
	w = signIn(f, h, map[string]string{"email": "a@b.co"}, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
	if w.Code != 400 || decode(t, w)["name"] != "VALIDATION_ERROR" {
		t.Fatalf("content type status=%d", w.Code)
	}
	w = signIn(f, h, `{"email":"`+strings.Repeat("a", 97<<10)+`"}`, nil)
	if w.Code != 413 {
		t.Fatalf("oversize status=%d", w.Code)
	}
	w = signIn(f, h, map[string]string{"email": "a@b.co"}, func(r *http.Request) { r.Header.Set("x-affine-version", strings.Repeat("1", 129)) })
	if w.Code != 400 || decode(t, w)["name"] != "UNSUPPORTED_CLIENT_VERSION" {
		t.Fatalf("version status=%d", w.Code)
	}
	for i := 1; i <= 6; i++ {
		w = signIn(f, h, map[string]string{"email": "limited@example.com", "password": "x"}, func(r *http.Request) { r.RemoteAddr = "198.51.100.9:1" })
	}
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("email limit status=%d", w.Code)
	}
	f.exec(`INSERT INTO app_configs(id,value,updated_at) VALUES('auth.signInRateLimit','{"ttl":"x"}',now())`)
	w = signIn(f, h, map[string]string{"email": "a@b.co", "password": "x"}, func(r *http.Request) { r.RemoteAddr = "198.51.100.10:1" })
	if w.Code != 503 || decode(t, w)["name"] != "SIGN_IN_TEMPORARILY_UNAVAILABLE" {
		t.Fatalf("bad policy status=%d", w.Code)
	}
}

func TestMagicLinkFlow(t *testing.T) {
	f := newFixture(t)
	known := f.user("known@example.com", "")
	signInHandler := NewPasswordLoginHTTPHandlerWithDependencies(f.pool, f.cfg, nil, nil)
	complete := NewMagicLinkHTTPHandler(f.pool, f.cfg, nil, testPrivateKey)
	prepare := func(email string, extra map[string]string) *httptest.ResponseRecorder {
		body := map[string]string{"email": email}
		for k, v := range extra {
			body[k] = v
		}
		return signIn(f, signInHandler, body, nil)
	}
	otpFor := func(email string) string {
		var raw []byte
		if err := f.pool.QueryRow(context.Background(), `SELECT payload FROM mail_deliveries WHERE recipient_email=$1 ORDER BY created_at DESC LIMIT 1`, email).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Name  string `json:"name"`
			Props struct {
				URL string `json:"url"`
				OTP string `json:"otp"`
			} `json:"props"`
		}
		_ = json.Unmarshal(raw, &payload)
		u, _ := url.Parse(payload.Props.URL)
		if len(payload.Props.OTP) != 6 || u.Query().Get("token") != payload.Props.OTP || u.Query().Get("email") != email {
			t.Fatalf("mail payload=%s", raw)
		}
		return payload.Props.OTP
	}
	finish := func(email, token string, nonce *string) *httptest.ResponseRecorder {
		body := map[string]string{"email": email, "token": token}
		if nonce != nil {
			body["client_nonce"] = *nonce
		}
		w := httptest.NewRecorder()
		complete.ServeHTTP(w, jsonRequest(http.MethodPost, "/api/auth/magic-link", body))
		return w
	}
	if w := prepare(" known@example.com ", nil); w.Code != 200 || decode(t, w)["email"] != "known@example.com" {
		t.Fatalf("prepare status=%d body=%s", w.Code, w.Body.String())
	}
	if f.count(`SELECT count(*) FROM mail_deliveries WHERE mail_name='SignIn'`) != 1 {
		t.Fatal("SignIn mail not queued")
	}
	otp := otpFor("known@example.com")
	w := finish("known@example.com", otp, nil)
	if w.Code != 200 || decode(t, w)["id"] != known || cookies(w)[SessionCookieName] == nil {
		t.Fatalf("complete status=%d body=%s", w.Code, w.Body.String())
	}
	if w := finish("known@example.com", otp, nil); decode(t, w)["name"] != "INVALID_EMAIL_TOKEN" {
		t.Fatal("replay accepted")
	}

	// New user sign-up.
	prepare("fresh.person@example.com", nil)
	w = finish("fresh.person@example.com", otpFor("fresh.person@example.com"), nil)
	if w.Code != 200 || f.count(`SELECT count(*) FROM users WHERE email='fresh.person@example.com' AND name='fresh.person' AND email_verified IS NOT NULL AND registered`) != 1 {
		t.Fatalf("sign-up status=%d body=%s", w.Code, w.Body.String())
	}

	// Nonce mismatch does not count an attempt.
	prepare("known@example.com", map[string]string{"client_nonce": "a"})
	otp = otpFor("known@example.com")
	b := "b"
	for _, nonce := range []*string{&b, nil} {
		if w := finish("known@example.com", otp, nonce); decode(t, w)["name"] != "INVALID_AUTH_STATE" {
			t.Fatalf("nonce mismatch body=%s", w.Body.String())
		}
	}
	if f.count(`SELECT attempts FROM runtime_states WHERE purpose='magic_link_otp' AND lookup_key='known@example.com'`) != 0 {
		t.Fatal("nonce mismatch counted an attempt")
	}
	// Ten wrong OTPs consume the state.
	for range 10 {
		finish("known@example.com", "000000x", ptr("a"))
	}
	if w := finish("known@example.com", otp, ptr("a")); decode(t, w)["name"] != "INVALID_EMAIL_TOKEN" {
		t.Fatal("OTP accepted after 10 failures")
	}
	// Epoch bump between prepare and complete.
	prepare("known@example.com", nil)
	otp = otpFor("known@example.com")
	f.exec(`UPDATE users SET auth_epoch=auth_epoch+1 WHERE id=$1`, known)
	if w := finish("known@example.com", otp, nil); decode(t, w)["name"] != "INVALID_EMAIL_TOKEN" {
		t.Fatal("stale epoch accepted")
	}
	if w := finish("known@example.com", "", nil); decode(t, w)["name"] != "EMAIL_TOKEN_NOT_FOUND" {
		t.Fatalf("missing token body=%s", w.Body.String())
	}

	// Callback and sign-up policy.
	if w := prepare("known@example.com", map[string]string{"callbackUrl": "https://evil.example/x"}); w.Code != 403 {
		t.Fatalf("unsafe callback status=%d", w.Code)
	}
	if w := prepare("known@example.com", map[string]string{"callbackUrl": "/magic-link?redirect_uri=https://accounts.google.com/x"}); w.Code != 200 {
		t.Fatalf("google redirect status=%d body=%s", w.Code, w.Body.String())
	}
	f.exec(`INSERT INTO app_configs(id,value,updated_at) VALUES('auth.allowSignup','false',now())`)
	if w := prepare("nobody@example.com", nil); w.Code != 403 || decode(t, w)["name"] != "SIGN_UP_FORBIDDEN" {
		t.Fatalf("signup forbidden status=%d", w.Code)
	}
	prod := NewPasswordLoginHTTPHandlerWithDependencies(f.pool, config.Config{Host: "localhost"}, nil, nil)
	t.Setenv("MAILER_HOST", "")
	if w := signIn(f, prod, map[string]string{"email": "known@example.com"}, nil); w.Code != 500 || decode(t, w)["name"] != "EMAIL_SERVICE_NOT_CONFIGURED" {
		t.Fatalf("mail not configured status=%d", w.Code)
	}
}

func ptr(s string) *string { return &s }

func nativeCall(h http.Handler, path string, body any) *httptest.ResponseRecorder {
	r := jsonRequest(http.MethodPost, path, body)
	r.Header.Set("x-affine-client-kind", "native")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func issueCode(t *testing.T, f *fixture, userID string) string {
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	code, err := IssueNativeExchangeCode(ctx, tx, userID, 0, nil, time.Now())
	if err != nil || tx.Commit(ctx) != nil {
		t.Fatal(err)
	}
	return code
}

func TestNativeSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	userID := f.user("native@example.com", "")
	h := NewNativeSessionHTTPHandler(f.pool, f.cfg, testPrivateKey)
	exchangeBody := func(code string) map[string]string {
		return map[string]string{"code": code, "installationId": uuid.NewString(), "platform": "ios", "deviceName": "  Phone  "}
	}
	code := issueCode(t, f, userID)
	w := nativeCall(h, "/api/auth/session/exchange", exchangeBody(code))
	if w.Code != 201 {
		t.Fatalf("exchange status=%d body=%s", w.Code, w.Body.String())
	}
	pair := decode(t, w)
	if f.count(`SELECT count(*) FROM auth_refresh_tokens WHERE generation=0`) != 1 || f.count(`SELECT count(*) FROM auth_sessions WHERE device_name='Phone'`) != 1 {
		t.Fatal("exchange rows missing")
	}
	if w := nativeCall(h, "/api/auth/session/exchange", exchangeBody(code)); decode(t, w)["code"] != "INVALID_AUTH_STATE" {
		t.Fatal("exchange code reused")
	}
	// Validation and native checks.
	bad := exchangeBody(issueCode(t, f, userID))
	bad["platform"] = "windows"
	if w := nativeCall(h, "/api/auth/session/exchange", bad); decode(t, w)["code"] != "VALIDATION_ERROR" {
		t.Fatal("bad platform accepted")
	}
	bad = exchangeBody(issueCode(t, f, userID))
	bad["installationId"] = strings.ToUpper(uuid.NewString())
	if w := nativeCall(h, "/api/auth/session/exchange", bad); decode(t, w)["code"] != "VALIDATION_ERROR" {
		t.Fatal("uppercase uuid accepted")
	}
	r := jsonRequest(http.MethodPost, "/api/auth/session/exchange", exchangeBody("x"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("non-native exchange status=%d", w.Code)
	}

	// Bearer access token resolves.
	resolver := NewSessionResolver(f.pool)
	bearer := httptest.NewRequest(http.MethodGet, "/", nil)
	bearer.Header.Set("Authorization", "Bearer "+pair["accessToken"].(string))
	p, err := resolver.FromRequest(ctx, bearer)
	if err != nil || p.AuthSessionID != pair["session"].(map[string]any)["id"] || p.AuthenticatedAt == nil {
		t.Fatalf("bearer principal=%+v err=%v", p, err)
	}

	// Refresh, grace replay, then reuse.
	oldToken := pair["refreshToken"].(string)
	w = nativeCall(h, "/api/auth/session/refresh", map[string]string{"refreshToken": oldToken})
	if w.Code != 201 {
		t.Fatalf("refresh status=%d body=%s", w.Code, w.Body.String())
	}
	next := decode(t, w)["refreshToken"].(string)
	w = nativeCall(h, "/api/auth/session/refresh", map[string]string{"refreshToken": oldToken})
	if w.Code != 201 || decode(t, w)["refreshToken"] != next {
		t.Fatalf("grace replay status=%d body=%s", w.Code, w.Body.String())
	}
	w = nativeCall(h, "/api/auth/session/refresh", map[string]string{"refreshToken": oldToken})
	if decode(t, w)["code"] != "REFRESH_TOKEN_REUSED" {
		t.Fatalf("third use body=%s", w.Body.String())
	}
	if w := nativeCall(h, "/api/auth/session/refresh", map[string]string{"refreshToken": next}); decode(t, w)["code"] != "AUTH_SESSION_REVOKED" {
		t.Fatalf("successor after reuse body=%s", w.Body.String())
	}

	// Disabled user revokes the session.
	w = nativeCall(h, "/api/auth/session/exchange", exchangeBody(issueCode(t, f, userID)))
	token := decode(t, w)["refreshToken"].(string)
	f.exec(`UPDATE users SET disabled=true WHERE id=$1`, userID)
	if w := nativeCall(h, "/api/auth/session/refresh", map[string]string{"refreshToken": token}); decode(t, w)["code"] != "AUTH_SESSION_REVOKED" {
		t.Fatalf("disabled refresh body=%s", w.Body.String())
	}
	if f.count(`SELECT count(*) FROM auth_sessions WHERE revoke_reason='user_disabled'`) != 1 {
		t.Fatal("disabled session not revoked")
	}
	f.exec(`UPDATE users SET disabled=false WHERE id=$1`, userID)

	// Revoke is idempotent and opaque.
	w = nativeCall(h, "/api/auth/session/exchange", exchangeBody(issueCode(t, f, userID)))
	token = decode(t, w)["refreshToken"].(string)
	for _, garbage := range []string{"garbage", "aff_rt_v1." + randomB64(18) + "." + randomB64(32)} {
		if w := nativeCall(h, "/api/auth/session/revoke", map[string]string{"refreshToken": garbage}); w.Code != 201 {
			t.Fatalf("garbage revoke status=%d", w.Code)
		}
	}
	if w := nativeCall(h, "/api/auth/session/revoke", map[string]string{"refreshToken": token}); w.Code != 201 {
		t.Fatalf("revoke status=%d", w.Code)
	}
	if f.count(`SELECT count(*) FROM auth_sessions WHERE revoke_reason='refresh_token_revoke'`) != 1 {
		t.Fatal("revoke did not revoke the session")
	}
	r = httptest.NewRequest(http.MethodPost, "/api/auth/session/revoke", strings.NewReader("not json"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("non-native revoke status=%d", w.Code)
	}

	// Rate limit: 31st refresh within a minute.
	w = nativeCall(h, "/api/auth/session/exchange", exchangeBody(issueCode(t, f, userID)))
	token = decode(t, w)["refreshToken"].(string)
	var last *httptest.ResponseRecorder
	for range 31 {
		last = nativeCall(h, "/api/auth/session/refresh", map[string]string{"refreshToken": token})
	}
	if last.Code != 429 {
		t.Fatalf("31st refresh status=%d body=%s", last.Code, last.Body.String())
	}

	// No private key → 503 on refresh.
	w = nativeCall(h, "/api/auth/session/exchange", exchangeBody(issueCode(t, f, userID)))
	token = decode(t, w)["refreshToken"].(string)
	noKey := NewNativeSessionHTTPHandler(f.pool, f.cfg, "")
	if w := nativeCall(noKey, "/api/auth/session/refresh", map[string]string{"refreshToken": token}); w.Code != 503 {
		t.Fatalf("no key status=%d", w.Code)
	}
	f.exec(`INSERT INTO app_configs(id,value,updated_at) VALUES('auth.token.accessTokenTtl','30',now())`)
	if w := nativeCall(h, "/api/auth/session/exchange", exchangeBody(issueCode(t, f, userID))); w.Code != 503 {
		t.Fatalf("bad policy status=%d", w.Code)
	}
}

func TestSessionManagementAndSignOut(t *testing.T) {
	f := newFixture(t)
	first := f.user("a@example.com", "pw")
	second := f.user("b@example.com", "pw")
	f.exec(`INSERT INTO user_connected_accounts(id,user_id,provider,provider_account_id,updated_at) VALUES('1',$1,'github','x',now()),('2',$1,'apple','y',now()),('3',$1,'github','z',now())`, first)
	resolver := NewSessionResolver(f.pool)
	sessionID, _ := f.cookieSession(first, second)
	withCookies := func(r *http.Request, csrf bool) *http.Request {
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionID})
		r.AddCookie(&http.Cookie{Name: UserCookieName, Value: first})
		if csrf {
			r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "csrf"})
			r.Header.Set("x-affine-csrf-token", "csrf")
		}
		return r
	}
	methods := NewBoundMethodsHTTPHandler(resolver, NewBoundMethodsRepository(f.pool))
	w := httptest.NewRecorder()
	methods.ServeHTTP(w, withCookies(httptest.NewRequest(http.MethodGet, "/api/auth/methods", nil), false))
	if w.Body.String() != `{"password":{"bound":true},"oauth":{"bound":true,"providers":["apple","github"]},"passkey":{"bound":false,"count":0}}`+"\n" {
		t.Fatalf("methods=%s", w.Body.String())
	}
	w = httptest.NewRecorder()
	methods.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/auth/methods", nil))
	if w.Code != 401 {
		t.Fatalf("anonymous methods=%d", w.Code)
	}

	native := NewNativeSessionHTTPHandler(f.pool, f.cfg, testPrivateKey)
	pair := decode(t, nativeCall(native, "/api/auth/session/exchange", map[string]string{"code": issueCode(t, f, first), "installationId": uuid.NewString(), "platform": "android"}))
	otherPair := decode(t, nativeCall(native, "/api/auth/session/exchange", map[string]string{"code": issueCode(t, f, second), "installationId": uuid.NewString(), "platform": "android"}))
	sessions := NewSessionManagementHTTPHandler(resolver, NewPostgresSessionManagementRepository(f.pool))
	r := httptest.NewRequest(http.MethodGet, "/api/auth/sessions", nil)
	r.Header.Set("Authorization", "Bearer "+pair["accessToken"].(string))
	w = httptest.NewRecorder()
	sessions.ServeHTTP(w, r)
	var list []AuthSession
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list) != 1 || !list[0].Current {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	otherID := otherPair["session"].(map[string]any)["id"].(string)
	del := func(id string, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodDelete, "/api/auth/sessions/"+id, nil)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		sessions.ServeHTTP(w, withCookies(r, csrf))
		return w
	}
	if w := del(otherID, false); w.Code != 403 {
		t.Fatalf("delete without csrf=%d", w.Code)
	}
	if w := del("not-a-uuid", true); w.Code != 400 {
		t.Fatalf("delete bad id=%d", w.Code)
	}
	if w := del(otherID, true); w.Code != 200 || f.count(`SELECT count(*) FROM auth_sessions WHERE revoked_at IS NOT NULL`) != 0 {
		t.Fatalf("delete other user's session=%d", w.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "/api/auth/sessions/revoke-all", nil)
	w = httptest.NewRecorder()
	sessions.ServeHTTP(w, withCookies(r, true))
	if w.Code != 201 || f.count(`SELECT count(*) FROM user_sessions WHERE user_id=$1`, first) != 0 {
		t.Fatalf("revoke-all status=%d", w.Code)
	}

	// Sign-out of one user of a two-user session, then of the whole session.
	sessionID, _ = f.cookieSession(first, second)
	signOut := NewSignOutHTTPHandler(resolver, NewPostgresCookieSessionRepository(f.pool))
	post := func(target string, csrf bool) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		signOut.ServeHTTP(w, withCookies(httptest.NewRequest(http.MethodPost, target, nil), csrf))
		return w
	}
	if w := post("/api/auth/sign-out", false); w.Code != 403 {
		t.Fatalf("sign-out without csrf=%d", w.Code)
	}
	w = post("/api/auth/sign-out?user_id="+second, true)
	var userCookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == UserCookieName {
			userCookie = c.Value
		}
	}
	if w.Code != 200 || f.count(`SELECT count(*) FROM user_sessions WHERE session_id=$1`, sessionID) != 1 || userCookie != first {
		t.Fatalf("partial sign-out status=%d user cookie=%q", w.Code, userCookie)
	}
	w = post("/api/auth/sign-out", true)
	if w.Code != 200 || f.count(`SELECT count(*) FROM multiple_users_sessions WHERE id=$1`, sessionID) != 0 || cookies(w)[SessionCookieName].MaxAge >= 0 {
		t.Fatalf("full sign-out status=%d", w.Code)
	}
	w = httptest.NewRecorder()
	signOut.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/auth/sign-out", nil))
	if w.Code != 200 || w.Body.String() != "{}\n" {
		t.Fatalf("anonymous sign-out=%d", w.Code)
	}
}

func TestOpenAppAndPreflight(t *testing.T) {
	f := newFixture(t)
	userID := f.user("app@example.com", "pw")
	disabled := f.user("off@example.com", "pw")
	f.exec(`UPDATE users SET disabled=true WHERE id=$1`, disabled)
	sessionID, _ := f.cookieSession(userID)
	resolver := NewSessionResolver(f.pool)
	h := NewOpenAppHTTPHandler(f.pool, f.cfg, resolver, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/auth/open-app/sign-in-code", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionID})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("code status=%d body=%s", w.Code, w.Body.String())
	}
	code := decode(t, w)["code"].(string)
	for i, want := range []int{200, 400} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, jsonRequest(http.MethodPost, "/api/auth/open-app/sign-in", map[string]string{"code": code}))
		if w.Code != want {
			t.Fatalf("sign-in %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/auth/open-app/sign-in-code", nil))
	if w.Code != 403 {
		t.Fatalf("anonymous code status=%d", w.Code)
	}

	pre := NewLoginPreflightHTTPHandler(f.pool, f.cfg, nil, nil, nil)
	ask := func(body any, version string) (int, map[string]any) {
		r := jsonRequest(http.MethodPost, "/api/auth/preflight", body)
		if version != "" {
			r.Header.Set("x-affine-version", version)
		}
		w := httptest.NewRecorder()
		pre.ServeHTTP(w, r)
		return w.Code, decode(t, w)
	}
	available := func(out map[string]any, method string) bool {
		return out["methods"].(map[string]any)[method].(map[string]any)["available"].(bool)
	}
	status, out := ask(map[string]string{"email": "APP@example.com"}, "")
	if status != 201 || out["registered"] != true || !available(out, "password") || !available(out, "magicLink") {
		t.Fatalf("preflight known=%d %v", status, out)
	}
	if _, out := ask(map[string]string{"email": "off@example.com"}, ""); available(out, "password") || available(out, "magicLink") {
		t.Fatalf("disabled user=%v", out)
	}
	if status, out := ask(`{"email":"x","extra":1}`, ""); status != 400 || out["name"] != "INVALID_EMAIL" {
		t.Fatalf("extra key=%d %v", status, out)
	}
	f.exec(`INSERT INTO app_configs(id,value,updated_at) VALUES('auth.allowSignup','false',now()),('client.versionControl.enabled','true',now()),('client.versionControl.requiredVersion','">=1.0.0"',now())`)
	if _, out := ask(map[string]string{"email": "new@example.com"}, "1.0.0"); available(out, "magicLink") || available(out, "oauth") {
		t.Fatalf("unknown signup disabled=%v", out)
	}
	if status, out := ask(map[string]string{"email": "app@example.com"}, "0.30.0"); status != 403 || out["name"] != "UNSUPPORTED_CLIENT_VERSION" || out["data"].(map[string]any)["requiredVersion"] != ">=1.0.0" {
		t.Fatalf("version gate=%d %v", status, out)
	}
}

func TestSigningKeyRingAndKeyCreation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor := f.user("admin@example.com", "")
	resolver := NewSessionResolver(f.pool)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer a.b.c")
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = loadKeyring(ctx, f.pool) }()
	}
	wg.Wait()
	if f.count(`SELECT jsonb_array_length(value) FROM app_configs WHERE id='auth.session.signingKeys'`) != 1 {
		t.Fatal("concurrent creation did not leave one key")
	}
	ring := NewAuthSigningKeyRing(f.pool, f.cfg)
	meta, err := ring.Metadata(ctx)
	if err != nil || len(meta) != 1 {
		t.Fatalf("metadata=%v err=%v", meta, err)
	}
	if _, err := ring.Rotate(ctx, actor, "stale"); err != ErrSigningKeyChanged {
		t.Fatalf("stale rotate err=%v", err)
	}
	rotated, err := ring.Rotate(ctx, actor, meta[0].ID)
	if err != nil || len(rotated) != 2 || rotated[0].Status != "retiring" || rotated[1].Source != "admin" ||
		rotated[0].VerifyUntil.Sub(*rotated[0].RetiredAt) != 930*time.Second {
		t.Fatalf("rotated=%+v err=%v", rotated, err)
	}
	if _, err := ring.Delete(ctx, actor, "missing"); err != ErrSigningKeyNotFound {
		t.Fatalf("missing delete err=%v", err)
	}
	if _, err := ring.Delete(ctx, actor, rotated[0].ID); err != ErrSigningKeyCannotDelete {
		t.Fatalf("unexpired delete err=%v", err)
	}
	// A token signed by the retiring key still verifies before verifyUntil.
	keys, _ := loadKeyring(ctx, f.pool)
	old, _ := keys.find(rotated[0].ID)
	now := time.Now()
	token, _ := signAccessToken(old, "u", "s", now, time.Minute)
	_, parts, _ := parseAccessTokenKid(token)
	if _, err := verifyAccessToken(parts, old, now); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE app_configs SET value=jsonb_set(value,'{0,secret}','"short"') WHERE id='auth.session.signingKeys'`)
	if _, err := ring.Metadata(ctx); err == nil {
		t.Fatal("malformed keyring accepted")
	}
	if _, err := resolver.FromRequest(ctx, r); err == nil {
		t.Fatal("token verification with malformed keyring succeeded")
	}
}

func TestEnsureStablePrivateKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	t.Setenv("NOTES_PRIVATE_KEY_SECRET", "wrap-secret")
	if err := EnsureStablePrivateKey(ctx, f.pool, " ", ""); err == nil {
		t.Fatal("blank secret accepted")
	}
	if err := EnsureStablePrivateKey(ctx, f.pool, "wrap-secret", ""); err != nil {
		t.Fatal(err)
	}
	var first string
	_ = f.pool.QueryRow(ctx, `SELECT value #>> '{}' FROM app_configs WHERE id='crypto.privateKey'`).Scan(&first)
	key, err := LoadAppPrivateKey(ctx, f.pool, f.cfg, "")
	if err != nil || validP256PEM(key) != nil || !strings.HasPrefix(first, "fydeos-private-key:v1:") {
		t.Fatalf("loaded key err=%v stored=%q", err, first)
	}
	if err := EnsureStablePrivateKey(ctx, f.pool, "wrap-secret", ""); err != nil {
		t.Fatal(err)
	}
	var second string
	_ = f.pool.QueryRow(ctx, `SELECT value #>> '{}' FROM app_configs WHERE id='crypto.privateKey'`).Scan(&second)
	if first != second {
		t.Fatal("stored envelope changed on rerun")
	}
	// Plaintext is re-sealed with the same PEM.
	f.exec(`UPDATE app_configs SET value=to_jsonb($1::text) WHERE id='crypto.privateKey'`, key)
	if err := EnsureStablePrivateKey(ctx, f.pool, "wrap-secret", ""); err != nil {
		t.Fatal(err)
	}
	if again, _ := LoadAppPrivateKey(ctx, f.pool, f.cfg, ""); again != key {
		t.Fatal("re-sealed key differs")
	}
	// Missing key with encrypted dependants fails.
	f.exec(`DELETE FROM app_configs WHERE id='crypto.privateKey'`)
	userID := f.user("dep@example.com", "")
	_, us := f.cookieSession(userID)
	f.exec(`INSERT INTO auth_sessions(id,user_session_id,installation_id,platform,idle_expires_at,absolute_expires_at) VALUES('as1',$1,'i','ios',now(),now())`, us[0])
	f.exec(`INSERT INTO auth_refresh_tokens(id,auth_session_id,generation,secret_hash,expires_at,successor_ciphertext,successor_expires_at) VALUES('rt1','as1',0,'h',now(),'c',now())`)
	if err := EnsureStablePrivateKey(ctx, f.pool, "wrap-secret", ""); err == nil {
		t.Fatal("missing key with dependants accepted")
	}
}

func TestCaptchaHashcash(t *testing.T) {
	f := newFixture(t)
	f.exec(`INSERT INTO app_configs(id,value,updated_at) VALUES('captcha.enabled','true',now()),('captcha.config','{"turnstile":{"secret":"s","siteKey":"k"},"challenge":{"bits":16}}',now())`)
	service := NewCaptchaService(f.pool, f.cfg)
	get := httptest.NewRequest(http.MethodGet, "/api/auth/captcha", nil)
	get.Header.Set("x-affine-client-kind", "native")
	w := httptest.NewRecorder()
	NewCaptchaHTTPHandler(service, nil).ServeHTTP(w, get)
	out := decode(t, w)
	if out["provider"] != "hashcash" {
		t.Fatalf("captcha=%v", out)
	}
	stamp := makeStamp(t, 16, out["resource"].(string), time.Now())
	verify := func(headers map[string]string) error {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return service.Verify(context.Background(), r)
	}
	h := map[string]string{"x-captcha-token": stamp, "x-captcha-provider": "hashcash", "x-captcha-challenge": out["challenge"].(string)}
	if err := verify(h); err != nil {
		t.Fatalf("valid stamp err=%v", err)
	}
	if err := verify(h); err == nil {
		t.Fatal("challenge reused")
	}
	if err := verify(map[string]string{"x-captcha-token": "t"}); err != errCaptchaCredential {
		t.Fatalf("missing provider err=%v", err)
	}
	if err := verify(map[string]string{"x-captcha-token": "t", "x-captcha-provider": "turnstile", "x-captcha-challenge": "c"}); err != errCaptchaCredential {
		t.Fatalf("turnstile with challenge err=%v", err)
	}
	turnstile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("response") == "down" {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"action":"` + r.Form.Get("response") + `","hostname":"localhost"}`))
	}))
	defer turnstile.Close()
	previous := turnstileVerifyURL
	turnstileVerifyURL = turnstile.URL
	defer func() { turnstileVerifyURL = previous }()
	for token, want := range map[string]error{"auth-sign-in": nil, "other-action": errCaptchaRejected, "down": errCaptchaUnavailable} {
		if err := verify(map[string]string{"x-captcha-token": token, "x-captcha-provider": "turnstile"}); err != want {
			t.Fatalf("turnstile %s err=%v", token, err)
		}
	}
}
