// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/database/migrations"
)

func oauthTestPool(t *testing.T) *pgxpool.Pool {
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
		t.Fatal("integration tests require a database named notes_test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "notes_oauth_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
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

// fakeProvider is an OAuth/OIDC provider served over loopback HTTP.
type fakeProvider struct {
	mu          sync.Mutex
	srv         *httptest.Server
	key         *rsa.PrivateKey
	tokenStatus int
	tokenExtra  map[string]any
	userinfo    any
	emails      any
	idClaims    jwt.MapClaims
}

func newFakeProvider(t *testing.T) *fakeProvider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{key: key, tokenStatus: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProvider) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/token":
		if f.tokenStatus != 200 {
			w.WriteHeader(f.tokenStatus)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		body := map[string]any{"access_token": "at-" + r.FormValue("code"), "token_type": "Bearer"}
		for k, v := range f.tokenExtra {
			body[k] = v
		}
		if f.idClaims != nil {
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, f.idClaims)
			tok.Header["kid"] = "k1"
			signed, _ := tok.SignedString(f.key)
			body["id_token"] = signed
		}
		json.NewEncoder(w).Encode(body)
	case "/userinfo", "/user":
		json.NewEncoder(w).Encode(f.userinfo)
	case "/user/emails":
		json.NewEncoder(w).Encode(f.emails)
	case "/jwks":
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	default:
		w.WriteHeader(404)
	}
}

type harness struct {
	t         *testing.T
	pool      *pgxpool.Pool
	fake      *fakeProvider
	cfg       config.Config
	loader    stubLoader
	preflight *PreflightHandler
	callback  *CallbackHandler
}

func newHarness(t *testing.T, runtime map[string]any) *harness {
	pool := oauthTestPool(t)
	fake := newFakeProvider(t)
	u := fake.srv.URL
	loader := stubLoader{}
	for _, name := range []string{"google", "github", "apple", "oidc", "fydeos"} {
		p := OAuthProviderConfig{Name: name, Label: providerLabels[name], Enabled: true, ClientID: name + "-client", ClientSecret: "secret",
			AuthorizationURL: u + "/auth", TokenURL: u + "/token", UserInfoURL: u + "/userinfo", JWKSURL: u + "/jwks"}
		switch name {
		case "github":
			p.UserInfoURL = u + "/user"
		case "oidc":
			p.Issuer = u
			p.Args = map[string]string{}
		case "fydeos":
			p.AccountServer = "https://account.example"
		case "apple":
			p.Issuer = ""
		}
		loader[name] = p
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"auth": runtime})
	os.WriteFile(path, raw, 0o600)
	cfg := config.Config{Host: "notes.example", HTTPS: true, RuntimeConfigPath: path}
	svc := NewCallbackServiceWithOAuthProviderSourceAndConfig(pool, loader, cfg)
	svc.clientFor = func(OAuthProviderConfig) *http.Client {
		return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &harness{t: t, pool: pool, fake: fake, cfg: cfg, loader: loader,
		preflight: NewPreflightHandlerWithLimiter(NewServiceWithOAuthProviderSource(loader, NewStateStore(pool)), cfg, newMemoryLimiter()),
		callback:  NewCallbackHandler(svc, cfg)}
}

// start runs a preflight and returns the state parameter.
func (h *harness) start(provider, client, nonce string, redirect string) string {
	h.t.Helper()
	body := map[string]any{"provider": provider, "client": client, "client_nonce": nonce}
	if redirect != "" {
		body["redirect_uri"] = redirect
	}
	raw, _ := json.Marshal(body)
	rec := doPreflight(h.preflight, http.MethodPost, "application/json", string(raw))
	if rec.Code != 200 {
		h.t.Fatalf("preflight %s = %d %s", provider, rec.Code, rec.Body)
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	u, _ := url.Parse(out["url"])
	return u.Query().Get("state")
}

func stateToken(state string) string {
	var env stateEnvelope
	json.Unmarshal([]byte(state), &env)
	return env.State
}

func (h *harness) call(path, contentType, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.callback.ServeHTTP(rec, req)
	return rec
}

func (h *harness) callJSON(fields map[string]any, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(fields)
	return h.call("/oauth/callback", "application/json", string(raw), mutate...)
}

func problemName(rec *httptest.ResponseRecorder) (string, map[string]any) {
	var p problem
	json.Unmarshal(rec.Body.Bytes(), &p)
	return p.Name, p.Data
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) googleUser(id, email string, verified bool) {
	h.fake.userinfo = map[string]any{"id": id, "email": email, "verified_email": verified, "name": "G User", "picture": "https://img/x"}
}

func TestCallbackRequestValidation(t *testing.T) {
	h := newHarness(t, nil)
	for name, tc := range map[string]struct {
		path, ct, body, want, data string
		status                     int
	}{
		"wrong type":     {"/oauth/callback", "text/plain", `{}`, "MISSING_OAUTH_QUERY_PARAMETER", "code", 400},
		"form on web":    {"/oauth/callback", "application/x-www-form-urlencoded", "code=a&state=b", "MISSING_OAUTH_QUERY_PARAMETER", "code", 400},
		"malformed json": {"/oauth/callback", "application/json", `{"code":`, "MISSING_OAUTH_QUERY_PARAMETER", "state", 400},
		"missing code":   {"/oauth/callback", "application/json", `{"state":"x"}`, "MISSING_OAUTH_QUERY_PARAMETER", "code", 400},
		"form extra":     {"/api/oauth/callback", "application/x-www-form-urlencoded", "code=a&state=b&x=1", "MISSING_OAUTH_QUERY_PARAMETER", "state", 400},
		"form dup":       {"/api/oauth/callback", "application/x-www-form-urlencoded", "code=a&code=b&state=c", "MISSING_OAUTH_QUERY_PARAMETER", "state", 400},
		"bad state":      {"/oauth/callback", "application/json", `{"code":"a","state":"nope","client_nonce":"n"}`, "INVALID_OAUTH_CALLBACK_STATE", "", 400},
		"unknown state":  {"/oauth/callback", "application/json", `{"code":"a","state":"` + uuid.NewString() + `","client_nonce":"n"}`, "OAUTH_STATE_EXPIRED", "", 400},
		"too large":      {"/oauth/callback", "application/json", `{"code":"` + strings.Repeat("a", 65<<10) + `"}`, "BAD_REQUEST", "", 413},
	} {
		rec := h.call(tc.path, tc.ct, tc.body)
		name2, data := problemName(rec)
		if rec.Code != tc.status || name2 != tc.want || (tc.data != "" && data["name"] != tc.data) || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := h.call("/oauth/callback", "application/json", `{"code":"`+strings.Repeat("a", 65<<10)+`"}`); !strings.Contains(rec.Body.String(), `"message":"Bad request."`) {
		t.Errorf("413 body = %s", rec.Body)
	}
	var rec *httptest.ResponseRecorder
	for i := 0; i < 15; i++ {
		rec = h.call("/oauth/callback", "text/plain", "")
	}
	if name, _ := problemName(rec); rec.Code != 429 || name != "TOO_MANY_REQUEST" || !strings.Contains(rec.Body.String(), `"type":"TOO_MANY_REQUESTS"`) {
		t.Fatalf("rate limit = %d %s", rec.Code, rec.Body)
	}
}

func TestGoogleCallbackFlow(t *testing.T) {
	h := newHarness(t, nil)
	h.googleUser("g-1", "New@Example.com", true)
	state := h.start("Google", "web", "nonce-1", "/after")

	rec := h.callJSON(map[string]any{"code": "c1", "state": state, "client_nonce": "wrong"})
	if name, _ := problemName(rec); name != "INVALID_AUTH_STATE" {
		t.Fatalf("wrong nonce = %s", rec.Body)
	}
	if n := h.count(`SELECT count(*) FROM runtime_states WHERE consumed_at IS NULL AND token_hash=$1`, sha256Hex(stateToken(state))); n != 1 {
		t.Fatal("state consumed by wrong nonce")
	}
	tampered := strings.Replace(state, `"provider":"Google"`, `"provider":"GitHub"`, 1)
	if rec := h.callJSON(map[string]any{"code": "c1", "state": tampered, "client_nonce": "nonce-1"}); func() string { n, _ := problemName(rec); return n }() != "INVALID_OAUTH_CALLBACK_STATE" {
		t.Fatalf("tampered = %s", rec.Body)
	}

	rec = h.callJSON(map[string]any{"code": "c1", "state": state, "client_nonce": "nonce-1"}, func(r *http.Request) { r.Header.Set(versionHeader, "0.1") })
	if rec.Code != 200 {
		t.Fatalf("success = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["redirectUri"] != "https://notes.example/after" || body["exchangeCode"] != nil {
		t.Fatalf("body = %v", body)
	}
	userID := body["id"].(string)
	cookies := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		cookies[c.Name] = c
	}
	s, u, c := cookies[auth.SessionCookieName], cookies[auth.UserCookieName], cookies[auth.CSRFCookieName]
	if s == nil || u == nil || c == nil || !s.HttpOnly || !s.Secure || u.HttpOnly || u.Secure || c.HttpOnly || !c.Secure || u.Value != userID || s.Path != "/" || s.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookies = %v", rec.Result().Cookies())
	}
	ns := providerNamespace(h.loader["google"])
	if h.count(`SELECT count(*) FROM users WHERE id=$1 AND registered AND email_verified IS NOT NULL AND name='G User'`, userID) != 1 ||
		h.count(`SELECT count(*) FROM user_connected_accounts WHERE user_id=$1 AND provider='google' AND provider_namespace=$2 AND provider_account_id='g-1'`, userID, ns) != 1 ||
		h.count(`SELECT count(*) FROM user_sessions WHERE user_id=$1 AND session_id=$2 AND sign_in_client_version='0.1'`, userID, s.Value) != 1 {
		t.Fatal("rows not created")
	}
	if rec := h.callJSON(map[string]any{"code": "c1", "state": state, "client_nonce": "nonce-1"}); func() string { n, _ := problemName(rec); return n }() != "OAUTH_STATE_EXPIRED" {
		t.Fatalf("replay = %s", rec.Body)
	}

	// Second sign-in reuses the multi-user session and keeps client version.
	state = h.start("Google", "web", "n2", "")
	rec = h.callJSON(map[string]any{"code": "c2", "state": state, "client_nonce": "n2"}, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.Value}) })
	if rec.Code != 200 {
		t.Fatalf("second = %s", rec.Body)
	}
	if h.count(`SELECT count(*) FROM user_sessions WHERE session_id=$1 AND sign_in_client_version='0.1'`, s.Value) != 1 || h.count(`SELECT count(*) FROM multiple_users_sessions`) != 1 {
		t.Fatal("session not reused")
	}

	// Disabled linked user.
	h.exec(`UPDATE users SET disabled=true WHERE id=$1`, userID)
	state = h.start("Google", "web", "n3", "")
	rec = h.callJSON(map[string]any{"code": "c3", "state": state, "client_nonce": "n3"})
	if name, data := problemName(rec); name != "INVALID_OAUTH_RESPONSE" || data["reason"] != "This account cannot sign in." {
		t.Fatalf("disabled = %s", rec.Body)
	}

	// Native client.
	h.exec(`UPDATE users SET disabled=false WHERE id=$1`, userID)
	state = h.start("Google", "affine", "n4", "")
	rec = h.callJSON(map[string]any{"code": "c4", "state": state, "client_nonce": "n4"}, func(r *http.Request) { r.Header.Set("x-affine-client-kind", "native") })
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body["exchangeCode"] == nil || body["id"] != userID {
		t.Fatalf("native = %s", rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Value != "" {
			t.Fatalf("native set cookie %v", c)
		}
	}
}

func TestGoogleCallbackErrors(t *testing.T) {
	h := newHarness(t, map[string]any{"allowSignupForOauth": false})
	h.googleUser("g-2", "x@example.com", true)
	state := h.start("Google", "web", "n", "")
	if rec := h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"}); rec.Code != 403 || !strings.Contains(rec.Body.String(), "SIGN_UP_FORBIDDEN") {
		t.Fatalf("signup forbidden = %d %s", rec.Code, rec.Body)
	}
	h.fake.tokenStatus = 400
	state = h.start("Google", "web", "n", "")
	rec := h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"})
	if name, data := problemName(rec); name != "INVALID_OAUTH_CALLBACK_CODE" || data["status"] != float64(400) || data["body"] != "" {
		t.Fatalf("token 400 = %s", rec.Body)
	}
	h.fake.tokenStatus = 200
	h.googleUser("g-2", "x@example.com", false)
	state = h.start("Google", "web", "n", "")
	rec = h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"})
	if name, data := problemName(rec); name != "INVALID_OAUTH_RESPONSE" || data["reason"] != "Invalid OAuth response." || !strings.Contains(rec.Body.String(), "Invalid OAuth response: Invalid OAuth response..") {
		t.Fatalf("unverified = %s", rec.Body)
	}
}

func TestLegacyNamespaceAndDuplicates(t *testing.T) {
	h := newHarness(t, nil)
	h.exec(`INSERT INTO users(id,name,email) VALUES('u1','A','legacy@example.com')`)
	h.exec(`INSERT INTO user_connected_accounts(id,user_id,provider,provider_account_id,updated_at) VALUES('l1','u1','google','g-legacy',now())`)
	h.googleUser("g-legacy", "legacy@example.com", true)
	state := h.start("Google", "web", "n", "")
	if rec := h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"u1"`) {
		t.Fatalf("legacy = %s", rec.Body)
	}
	if h.count(`SELECT count(*) FROM user_connected_accounts WHERE id='l1' AND provider_namespace=$1`, providerNamespace(h.loader["google"])) != 1 {
		t.Fatal("namespace not adopted")
	}

	h.exec(`DROP INDEX go_users_email_lower_unique_idx`) // legacy data may predate the unique index
	h.exec(`INSERT INTO users(id,name,email) VALUES('d1','A','dup@example.com'),('d2','B','DUP@example.com')`)
	h.fake.userinfo = map[string]any{"id": 42, "email": "dup@example.com", "login": "d"}
	h.fake.emails = []map[string]any{{"email": "dup@example.com", "verified": true, "primary": true}}
	state = h.start("GitHub", "web", "n", "")
	rec := h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"})
	if name, data := problemName(rec); name != "INVALID_OAUTH_RESPONSE" || data["reason"] != "This account cannot sign in." {
		t.Fatalf("github dup = %s", rec.Body)
	}
	h.fake.userinfo = map[string]any{"accountId": "f-1", "email": "dup@example.com"}
	state = h.start("FydeOS", "web", "n", "")
	if rec := h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"}); !strings.Contains(rec.Body.String(), "OAUTH_ACCOUNT_ALREADY_CONNECTED") {
		t.Fatalf("fydeos dup = %s", rec.Body)
	}
}

func TestFydeOSInvitedUser(t *testing.T) {
	h := newHarness(t, nil)
	h.exec(`INSERT INTO users(id,name,email,registered) VALUES('inv','I','inv@example.com',false),('reg','R','reg@example.com',true)`)
	h.exec(`INSERT INTO users(id,name,email,registered,password) VALUES('pw','P','pw@example.com',false,'hash')`)
	for _, email := range []string{"reg@example.com", "pw@example.com"} {
		h.fake.userinfo = map[string]any{"accountId": "f-" + email, "email": email, "emailVerified": true}
		state := h.start("FydeOS", "web", "n", "")
		if rec := h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"}); !strings.Contains(rec.Body.String(), "OAUTH_ACCOUNT_ALREADY_CONNECTED") {
			t.Fatalf("%s = %s", email, rec.Body)
		}
	}
	h.fake.userinfo = map[string]any{"accountId": "f-inv", "email": "inv@example.com", "imageUrl": "https://img/i"}
	state := h.start("FydeOS", "web", "n", "")
	if rec := h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"}); rec.Code != 200 {
		t.Fatalf("invited = %s", rec.Body)
	}
	if h.count(`SELECT count(*) FROM users WHERE id='inv' AND registered AND email_verified IS NOT NULL AND avatar_url='https://img/i'`) != 1 ||
		h.count(`SELECT count(*) FROM user_connected_accounts WHERE user_id='inv' AND provider='fydeos'`) != 1 {
		t.Fatal("invited user not linked")
	}
}

func TestGitHubEmailChoice(t *testing.T) {
	h := newHarness(t, nil)
	h.fake.userinfo = map[string]any{"id": 7, "email": "Profile@example.com", "login": "octo"}
	h.fake.emails = []map[string]any{{"email": "primary@example.com", "verified": true, "primary": true}, {"email": "profile@example.com", "verified": true}}
	state := h.start("GitHub", "web", "n", "")
	if rec := h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"}); rec.Code != 200 {
		t.Fatalf("github = %s", rec.Body)
	}
	if h.count(`SELECT count(*) FROM users WHERE email='profile@example.com' AND name='octo'`) != 1 {
		t.Fatal("profile email not chosen")
	}
	h.fake.userinfo = map[string]any{"id": 8, "login": "x"}
	h.fake.emails = []map[string]any{{"email": "u@example.com", "verified": false, "primary": true}}
	state = h.start("GitHub", "web", "n", "")
	if rec := h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"}); !strings.Contains(rec.Body.String(), "INVALID_OAUTH_RESPONSE") {
		t.Fatalf("no verified = %s", rec.Body)
	}
}

func TestOIDCCallback(t *testing.T) {
	h := newHarness(t, nil)
	run := func(mutate func(token string)) *httptest.ResponseRecorder {
		state := h.start("OIDC", "web", "n", "")
		token := stateToken(state)
		h.fake.idClaims = jwt.MapClaims{"iss": h.fake.srv.URL, "aud": "oidc-client", "sub": "s-1", "nonce": token, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
		h.fake.userinfo = map[string]any{"sub": "s-1", "email": "o@example.com", "email_verified": "yes", "mail": "alt@example.com"}
		mutate(token)
		return h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"})
	}
	if rec := run(func(string) { h.fake.idClaims["nonce"] = "other" }); !strings.Contains(rec.Body.String(), "INVALID_AUTH_STATE") {
		t.Fatalf("nonce = %s", rec.Body)
	}
	if rec := run(func(string) { h.fake.userinfo.(map[string]any)["sub"] = "s-2" }); !strings.Contains(rec.Body.String(), "INVALID_OAUTH_RESPONSE") {
		t.Fatalf("sub mismatch = %s", rec.Body)
	}
	p := h.loader["oidc"]
	p.Args = map[string]string{"claim_email": "mail"}
	h.loader["oidc"] = p
	if rec := run(func(string) {}); rec.Code != 200 {
		t.Fatalf("oidc = %s", rec.Body)
	}
	if h.count(`SELECT count(*) FROM users WHERE email='alt@example.com'`) != 1 {
		t.Fatal("claim_email not applied")
	}
	// A tampered PKCE challenge is rejected.
	state := h.start("OIDC", "web", "n", "")
	bad := strings.Replace(state, `"codeChallenge":"`, `"codeChallenge":"x`, 1)
	if rec := h.callJSON(map[string]any{"code": "c", "state": bad, "client_nonce": "n"}); !strings.Contains(rec.Body.String(), "INVALID_OAUTH_CALLBACK_STATE") {
		t.Fatalf("pkce = %s", rec.Body)
	}
}

func TestAppleCallback(t *testing.T) {
	h := newHarness(t, nil)
	state := h.start("Apple", "affine", "apple-nonce", "")
	form := url.Values{"code": {"ac"}, "state": {state}}.Encode()
	rec := h.call("/api/oauth/callback", "application/x-www-form-urlencoded", form)
	loc := rec.Header().Get("Location")
	if rec.Code != 302 || !strings.HasPrefix(loc, "https://notes.example/open-app/url?url=affine%3A%2F%2Fauthentication") {
		t.Fatalf("handoff = %d %s", rec.Code, loc)
	}
	if h.count(`SELECT count(*) FROM runtime_states WHERE consumed_at IS NULL AND token_hash=$1`, sha256Hex(stateToken(state))) != 1 {
		t.Fatal("handoff consumed state")
	}
	state = h.start("Apple", "web", "apple-nonce", "https://notes.example/home")
	h.fake.idClaims = jwt.MapClaims{"iss": appleIssuer, "aud": "apple-client", "sub": "a-1", "nonce": "apple-nonce", "email": "a@example.com", "email_verified": "true", "exp": time.Now().Add(time.Hour).Unix()}
	form = url.Values{"code": {"ac"}, "state": {state}, "user": {`{"name":{"firstName":"Ann","lastName":"Lee"}}`}}.Encode()
	rec = h.call("/api/oauth/callback", "application/x-www-form-urlencoded", form)
	if rec.Code != 302 || rec.Header().Get("Location") != "https://notes.example/home" || len(rec.Result().Cookies()) != 3 {
		t.Fatalf("apple web = %d %s %s", rec.Code, rec.Header(), rec.Body)
	}
	if h.count(`SELECT count(*) FROM users WHERE email='a@example.com' AND name='Ann Lee'`) != 1 {
		t.Fatal("apple user not created")
	}
}

func TestConcurrentCallbacks(t *testing.T) {
	h := newHarness(t, nil)
	h.googleUser("g-c", "c@example.com", true)
	state := h.start("Google", "web", "n", "")
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = h.callJSON(map[string]any{"code": "c", "state": state, "client_nonce": "n"}, func(r *http.Request) { r.RemoteAddr = "10.0.0." + string(rune('1'+i)) + ":1" }).Code
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == 200 {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("codes = %v", codes)
	}
	states := make([]string, 4)
	for i := range states {
		states[i] = h.start("Google", "web", "n", "")
	}
	h.googleUser("g-new", "new2@example.com", true)
	for i := range states {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = h.callJSON(map[string]any{"code": "c", "state": states[i], "client_nonce": "n"}).Code
		}(i)
	}
	wg.Wait()
	if h.count(`SELECT count(*) FROM users WHERE email='new2@example.com'`) != 1 || h.count(`SELECT count(*) FROM user_connected_accounts WHERE provider_account_id='g-new'`) != 1 {
		t.Fatalf("concurrent first sign-in codes = %v", codes)
	}
}
