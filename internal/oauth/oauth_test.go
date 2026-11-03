// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func writeRuntimeConfig(t *testing.T, providers map[string]any) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(map[string]any{"oauth": map[string]any{"providers": providers}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Config{RuntimeConfigPath: path}
}

func testSource(cfg config.Config) *RuntimeOAuthProviderSource {
	s := NewRuntimeOAuthProviderSource(nil, cfg)
	s.discover = func(_ context.Context, p OAuthProviderConfig) (OAuthProviderConfig, error) {
		if strings.Contains(p.Issuer, "bad") {
			return p, errors.New("unreachable")
		}
		p.AuthorizationURL, p.TokenURL, p.UserInfoURL, p.JWKSURL = p.Issuer+"/auth", p.Issuer+"/token", p.Issuer+"/userinfo", p.Issuer+"/jwks"
		return p, nil
	}
	return s
}

func available(t *testing.T, cfg config.Config) ([]string, error) {
	t.Helper()
	return NewProviderCatalogWithOAuthProviderSource(testSource(cfg)).AvailableProviders(context.Background())
}

func TestCatalog(t *testing.T) {
	got, err := available(t, config.Config{})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty catalog = %#v, %v", got, err)
	}
	got, err = available(t, writeRuntimeConfig(t, map[string]any{"google": map[string]any{"clientId": "c", "clientSecret": "s"}}))
	if err != nil || !reflect.DeepEqual(got, []string{"google"}) {
		t.Fatalf("google = %#v, %v", got, err)
	}
	got, err = available(t, writeRuntimeConfig(t, map[string]any{"apple": map[string]any{"clientId": "c", "args": map[string]string{"privateKey": "k", "keyId": "i", "teamId": "t"}}}))
	if err != nil || !reflect.DeepEqual(got, []string{"apple"}) {
		t.Fatalf("apple = %#v, %v", got, err)
	}
	got, err = available(t, writeRuntimeConfig(t, map[string]any{"oidc": map[string]any{"clientId": "c", "clientSecret": "s", "issuer": "https://bad.example"}}))
	if err != nil || len(got) != 0 {
		t.Fatalf("bad oidc = %#v, %v", got, err)
	}
	all := writeRuntimeConfig(t, map[string]any{
		"oidc":   map[string]any{"clientId": "c", "clientSecret": "s", "issuer": "https://id.example/"},
		"fydeos": map[string]any{"clientId": "c", "clientSecret": "s", "accountServer": "https://account.example"},
		"apple":  map[string]any{"clientId": "c", "clientSecret": "s"},
		"github": map[string]any{"clientId": "c", "clientSecret": "s"},
		"google": map[string]any{"clientId": "c", "clientSecret": "s"},
	})
	got, err = available(t, all)
	if err != nil || !reflect.DeepEqual(got, []string{"google", "github", "apple", "fydeos", "oidc"}) {
		t.Fatalf("order = %#v, %v", got, err)
	}
	p, err := testSource(all).LoadProvider(context.Background(), "OIDC")
	if err != nil || p.Issuer != "https://id.example" || p.Label != "OIDC" {
		t.Fatalf("oidc = %#v, %v", p, err)
	}
	if _, err := testSource(all).LoadProvider(context.Background(), "Twitter"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown provider err = %v", err)
	}
}

func TestProviderConfigErrors(t *testing.T) {
	for name, providers := range map[string]map[string]any{
		"google issuer":      {"google": map[string]any{"clientId": "c", "clientSecret": "s", "issuer": "https://x"}},
		"null secret":        {"google": map[string]any{"clientId": "c", "clientSecret": nil}},
		"null arg":           {"github": map[string]any{"clientId": "c", "args": map[string]any{"a": nil}}},
		"account server":     {"github": map[string]any{"clientId": "c", "clientSecret": "s", "accountServer": "https://x"}},
		"fydeos partial":     {"fydeos": map[string]any{"clientId": "c"}},
		"fydeos http":        {"fydeos": map[string]any{"clientId": "c", "clientSecret": "s", "accountServer": "http://example.com"}},
		"fydeos path":        {"fydeos": map[string]any{"clientId": "c", "clientSecret": "s", "accountServer": "https://example.com/x"}},
		"fydeos args":        {"fydeos": map[string]any{"args": map[string]any{"a": "b"}}},
		"fydeos wrong types": {"fydeos": map[string]any{"clientId": 5}},
	} {
		cfg := writeRuntimeConfig(t, providers)
		var key string
		for k := range providers {
			key = k
		}
		if _, err := testSource(cfg).LoadProvider(context.Background(), key); err == nil || errors.Is(err, ErrProviderDisabled) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	cfg := writeRuntimeConfig(t, map[string]any{"fydeos": map[string]any{"clientId": "c", "clientSecret": "s", "accountServer": "https://Example.com:443/"}})
	p, err := testSource(cfg).LoadProvider(context.Background(), "fydeos")
	if err != nil || !p.Enabled || p.AccountServer != "https://example.com" || p.TokenURL != "https://example.com/oauth2/token" {
		t.Fatalf("fydeos = %#v, %v", p, err)
	}
	cfg.FydeOSOAuth = config.FydeOSOAuthEnvironment{Configured: true, AccountServer: "http://localhost:8080", ClientID: "env", ClientSecret: "es", AllowPrivateNetwork: true}
	p, err = testSource(cfg).LoadProvider(context.Background(), "FydeOS")
	if err != nil || p.ClientID != "env" || p.AccountServer != "http://localhost:8080" {
		t.Fatalf("env override = %#v, %v", p, err)
	}
	cfg.FydeOSOAuth = config.FydeOSOAuthEnvironment{Configured: true}
	if p, err := testSource(cfg).LoadProvider(context.Background(), "fydeos"); err != nil || p.Enabled {
		t.Fatalf("blank env = %#v, %v", p, err)
	}
	if p, err := testSource(config.Config{}).LoadProvider(context.Background(), "github"); err != nil || p.Enabled || p.Label != "GitHub" {
		t.Fatalf("unconfigured = %#v, %v", p, err)
	}
}

func TestNamespaceFormula(t *testing.T) {
	sum := sha256.Sum256([]byte("https://accounts.google.com\nclient"))
	want := "oauth:google:" + hex.EncodeToString(sum[:16])
	if got := providerNamespace(OAuthProviderConfig{Name: "google", ClientID: "client"}); got != want {
		t.Fatalf("namespace = %s, want %s", got, want)
	}
}

func TestMemoryLimiter(t *testing.T) {
	l := newMemoryLimiter()
	now := time.Now()
	for i := 0; i < 20; i++ {
		if ok, _, _ := l.Allow(context.Background(), "ip", now); !ok {
			t.Fatalf("request %d blocked", i+1)
		}
	}
	ok, remaining, reset := l.Allow(context.Background(), "ip", now)
	if ok || remaining != 0 || !reset.Equal(now.Add(time.Minute)) {
		t.Fatalf("21st = %v %d %v", ok, remaining, reset)
	}
	if ok, _, _ := l.Allow(context.Background(), "ip", now.Add(time.Minute)); !ok {
		t.Fatal("new window blocked")
	}
	for i := 0; i < maxTrackedKeys+50; i++ {
		l.Allow(context.Background(), "k"+string(rune(i)), now)
	}
	if len(l.windows) > maxTrackedKeys+1 {
		t.Fatalf("tracked %d keys", len(l.windows))
	}
}

func TestRedisLimiterFallsBack(t *testing.T) {
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer down.Close()
	for _, l := range []RateLimiter{NewRedisStrictLimiter(nil), NewRedisStrictLimiter(down)} {
		now := time.Now()
		for i := 0; i < 20; i++ {
			l.Allow(context.Background(), "ip", now)
		}
		if ok, _, _ := l.Allow(context.Background(), "ip", now); ok {
			t.Fatal("fallback limiter did not enforce limit")
		}
	}
}

func TestRedisLimiter(t *testing.T) {
	addr := os.Getenv("NOTES_TEST_REDIS_URL")
	if addr == "" {
		t.Skip("set NOTES_TEST_REDIS_URL")
	}
	opts, err := redis.ParseURL(addr)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(opts)
	defer client.Close()
	l := NewRedisStrictLimiter(client)
	id := "test-" + time.Now().Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(id))
	defer client.Del(context.Background(), redisKeyPrefix+hex.EncodeToString(sum[:]))
	now := time.Now()
	for i := 0; i < 20; i++ {
		if ok, rem, _ := l.Allow(context.Background(), id, now); !ok || rem != 19-i {
			t.Fatalf("hit %d: %v %d", i, ok, rem)
		}
	}
	ok, _, reset := l.Allow(context.Background(), id, now)
	if ok || reset.Sub(now) <= 0 || reset.Sub(now) > time.Minute {
		t.Fatalf("21st = %v reset in %v", ok, reset.Sub(now))
	}
}

func TestRestrictedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "/big":
			w.Write([]byte(`{"a":"` + strings.Repeat("x", 2<<20) + `"}`))
		default:
			if r.Header.Get("User-Agent") != userAgent || r.Header.Get("Accept") != "application/json" {
				w.WriteHeader(400)
			}
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	var dst map[string]any
	blocked := newRestrictedClient(egressPolicy{srv.URL: false})
	if err := getJSON(context.Background(), blocked, srv.URL+"/x", "", &dst); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("loopback http without private flag err = %v", err)
	}
	httpsPublic := egressPolicy{}
	httpsPublic.add(strings.Replace(srv.URL, "http://", "https://", 1), false)
	if err := getJSON(context.Background(), newRestrictedClient(httpsPublic), strings.Replace(srv.URL, "http://", "https://", 1)+"/x", "", &dst); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("loopback dial err = %v", err)
	}
	policy := egressPolicy{}
	policy.add(srv.URL, true)
	allowed := newRestrictedClient(policy)
	if err := getJSON(context.Background(), allowed, srv.URL+"/ok", "", &dst); err != nil {
		t.Fatalf("allowed err = %v", err)
	}
	if err := getJSON(context.Background(), allowed, "https://example.com/", "", &dst); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("other origin err = %v", err)
	}
	var status *providerStatusError
	if err := getJSON(context.Background(), allowed, srv.URL+"/redirect", "", &dst); !errors.As(err, &status) || status.status != http.StatusFound {
		t.Fatalf("redirect err = %v", err)
	}
	if err := getJSON(context.Background(), allowed, srv.URL+"/big", "", &dst); !errors.Is(err, ErrInvalidOAuthResponse) {
		t.Fatalf("big err = %v", err)
	}
}

func TestDiscoverOIDC(t *testing.T) {
	var issuer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": issuer + "/", "authorization_endpoint": issuer + "/a", "token_endpoint": issuer + "/t", "userinfo_endpoint": issuer + "/u", "jwks_uri": issuer + "/j"})
	}))
	defer srv.Close()
	issuer = srv.URL
	p, err := discoverOIDC(context.Background(), OAuthProviderConfig{Name: "oidc", Issuer: issuer, AllowPrivateNetwork: true})
	if err != nil || p.TokenURL != issuer+"/t" {
		t.Fatalf("discover = %#v, %v", p, err)
	}
	if _, err := discoverOIDC(context.Background(), OAuthProviderConfig{Name: "oidc", Issuer: issuer}); err == nil {
		t.Fatal("http issuer without private flag accepted")
	}
	issuer = "https://other.example"
	if _, err := discoverOIDC(context.Background(), OAuthProviderConfig{Name: "oidc", Issuer: srv.URL, AllowPrivateNetwork: true}); err == nil {
		t.Fatal("mismatched issuer accepted")
	}
}

// --- preflight ---

type memStates struct {
	saved map[string][]byte
	ttl   int
	fail  bool
}

func (m *memStates) SaveOAuthState(_ context.Context, hash string, payload []byte, ttl int) error {
	if m.fail {
		return errors.New("db down")
	}
	if m.saved == nil {
		m.saved = map[string][]byte{}
	}
	m.saved[hash] = payload
	m.ttl = ttl
	return nil
}

type stubLoader map[string]OAuthProviderConfig

func (s stubLoader) LoadProvider(_ context.Context, name string) (OAuthProviderConfig, error) {
	canonical, label, ok := canonicalProvider(name)
	if !ok {
		return OAuthProviderConfig{}, ErrUnknownProvider
	}
	if p, ok := s[canonical]; ok {
		return p, nil
	}
	return OAuthProviderConfig{Name: canonical, Label: label}, nil
}

func enabledProviders() stubLoader {
	return stubLoader{
		"google": {Name: "google", Label: "Google", Enabled: true, ClientID: "gc", ClientSecret: "gs"},
		"apple":  {Name: "apple", Label: "Apple", Enabled: true, ClientID: "ac", Args: map[string]string{"privateKey": "secret-key", "keyId": "k", "teamId": "t", "extra": "1"}},
		"oidc":   {Name: "oidc", Label: "OIDC", Enabled: true, ClientID: "oc", ClientSecret: "os", Issuer: "https://id.example", AuthorizationURL: "https://id.example/auth", TokenURL: "https://id.example/token", Args: map[string]string{"claim_email": "mail"}},
		"fydeos": {Name: "fydeos", Label: "FydeOS", Enabled: true, ClientID: "fc", ClientSecret: "fs", AccountServer: "https://account.example"},
	}
}

var testCfg = config.Config{Host: "notes.example", HTTPS: true, Hosts: []string{"alt.example:8443"}}

func doPreflight(h http.Handler, method, contentType, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/oauth/preflight", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body["error"]
}

func TestPreflightHandler(t *testing.T) {
	states := &memStates{}
	h := NewPreflightHandlerWithLimiter(NewServiceWithOAuthProviderSource(enabledProviders(), states), testCfg, newMemoryLimiter())
	rec := doPreflight(h, http.MethodGet, "", "")
	if rec.Code != 405 || rec.Header().Get("Allow") != "POST" || errorCode(rec) != "method_not_allowed" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	const js = "application/json"
	rec = doPreflight(h, http.MethodPost, js, `{"provider":"Google","client":"web","client_nonce":"n1","redirect_uri":"/workspace"}`, func(r *http.Request) { r.Header.Set(versionHeader, "1.2.3") })
	if rec.Code != 200 || rec.Header().Get("Connection") != "" || rec.Header().Get("X-RateLimit-Limit") != "20" {
		t.Fatalf("google = %d %s %v", rec.Code, rec.Body, rec.Header())
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	u, _ := url.Parse(out["url"])
	q := u.Query()
	if u.Host != "accounts.google.com" || q.Get("prompt") != "select_account" || q.Get("access_type") != "offline" || q.Get("scope") != "openid email profile" || q.Get("redirect_uri") != "https://notes.example/oauth/callback" || q.Get("client_id") != "gc" {
		t.Fatalf("google url = %s", out["url"])
	}
	var env stateEnvelope
	if err := json.Unmarshal([]byte(q.Get("state")), &env); err != nil || env.Provider != "Google" || env.Client != "web" || env.PKCE != nil {
		t.Fatalf("envelope = %s", q.Get("state"))
	}
	var payload statePayload
	if err := json.Unmarshal(states.saved[sha256Hex(env.State)], &payload); err != nil || states.ttl != 3*3600 {
		t.Fatalf("stored = %s", states.saved)
	}
	if payload.Provider != "google" || payload.ProviderLabel != "Google" || *payload.RedirectURI != "https://notes.example/workspace" || *payload.ClientVersion != "1.2.3" || payload.PKCEVerifier != nil || payload.ClientNonce != "n1" {
		t.Fatalf("payload = %#v", payload)
	}

	rec = doPreflight(h, http.MethodPost, js, `{"provider":"OIDC","client":"affine","client_nonce":"n"}`)
	json.Unmarshal(rec.Body.Bytes(), &out)
	u, _ = url.Parse(out["url"])
	q = u.Query()
	json.Unmarshal([]byte(q.Get("state")), &env)
	json.Unmarshal(states.saved[sha256Hex(env.State)], &payload)
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") != env.State || q.Get("code_challenge") != pkceChallenge(*payload.PKCEVerifier) || env.PKCE.CodeChallenge != q.Get("code_challenge") || q.Get("claim_email") != "" || len(*payload.PKCEVerifier) != 128 {
		t.Fatalf("oidc url = %s", out["url"])
	}

	rec = doPreflight(h, http.MethodPost, js, `{"provider":"Apple","client":"affine","client_nonce":"cn"}`)
	json.Unmarshal(rec.Body.Bytes(), &out)
	if strings.Contains(out["url"], "secret-key") {
		t.Fatal("private key leaked")
	}
	q, _ = url.ParseQuery(strings.SplitN(out["url"], "?", 2)[1])
	if q.Get("redirect_uri") != "https://notes.example/api/oauth/callback" || q.Get("response_mode") != "form_post" || q.Get("nonce") != "cn" || q.Get("extra") != "1" || q.Get("keyId") != "" {
		t.Fatalf("apple url = %s", out["url"])
	}

	rec = doPreflight(h, http.MethodPost, js, `{"provider":"FydeOS","client":"web","client_nonce":"n"}`, func(r *http.Request) { r.Host = "ALT.example:8443" })
	json.Unmarshal(rec.Body.Bytes(), &out)
	q, _ = url.ParseQuery(strings.SplitN(out["url"], "?", 2)[1])
	if !strings.HasPrefix(out["url"], "https://account.example/signin?") || q.Get("protocol") != "oauth2" || q.Get("scope") != "profile" || q.Get("redirect_uri") != "https://alt.example:8443/oauth/callback" {
		t.Fatalf("fydeos url = %s", out["url"])
	}

	for name, tc := range map[string]struct {
		ct, body string
		status   int
		code     string
	}{
		"unknown field":    {js, `{"provider":"Google","client":"web","client_nonce":"n","x":1}`, 400, "invalid_oauth_preflight"},
		"text plain":       {"text/plain", `{"provider":"Google","client":"web","client_nonce":"n"}`, 400, "invalid_oauth_preflight"},
		"trailing":         {js, `{"provider":"Google","client":"web","client_nonce":"n"}{}`, 400, "invalid_oauth_preflight"},
		"missing nonce":    {js, `{"provider":"Google","client":"web"}`, 400, "invalid_oauth_preflight"},
		"long nonce":       {js, `{"provider":"Google","client":"web","client_nonce":"` + strings.Repeat("n", 513) + `"}`, 400, "invalid_oauth_preflight"},
		"too large":        {js, `{"provider":"` + strings.Repeat("x", 17<<10) + `"}`, 413, "request_body_too_large"},
		"desktop":          {js, `{"provider":"Google","client":"desktop","client_nonce":"n"}`, 400, "invalid_oauth_preflight"},
		"fydeos affine":    {js, `{"provider":"FydeOS","client":"affine","client_nonce":"n"}`, 400, "invalid_oauth_preflight"},
		"evil redirect":    {js, `{"provider":"Google","client":"web","client_nonce":"n","redirect_uri":"https://evil.example"}`, 403, "action_forbidden"},
		"github redirect":  {js, `{"provider":"Google","client":"web","client_nonce":"n","redirect_uri":"https://sub.github.com/x"}`, 200, ""},
		"null redirect":    {js, `{"provider":"Google","client":"web","client_nonce":"n","redirect_uri":null}`, 200, ""},
		"disabled":         {js, `{"provider":"GitHub","client":"web","client_nonce":"n"}`, 400, "unknown_oauth_provider"},
		"unknown provider": {js, `{"provider":"Twitter","client":"web","client_nonce":"n"}`, 400, "unknown_oauth_provider"},
	} {
		rec := doPreflight(h, http.MethodPost, tc.ct, tc.body)
		if rec.Code != tc.status || errorCode(rec) != tc.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	rec = doPreflight(h, http.MethodPost, js, `{"provider":"Google","client":"web","client_nonce":"n"}`, func(r *http.Request) { r.Header.Set(versionHeader, strings.Repeat("v", 129)) })
	if rec.Code != 400 {
		t.Fatalf("long version = %d", rec.Code)
	}

	failing := NewPreflightHandlerWithLimiter(NewServiceWithOAuthProviderSource(enabledProviders(), &memStates{fail: true}), testCfg, newMemoryLimiter())
	if rec := doPreflight(failing, http.MethodPost, js, `{"provider":"Google","client":"web","client_nonce":"n"}`); rec.Code != 503 || errorCode(rec) != "oauth_temporarily_unavailable" {
		t.Fatalf("storage failure = %d %s", rec.Code, rec.Body)
	}
}

func TestPreflightRateLimit(t *testing.T) {
	h := NewPreflightHandlerWithLimiter(NewServiceWithOAuthProviderSource(enabledProviders(), &memStates{}), testCfg, NewRedisStrictLimiter(nil))
	var rec *httptest.ResponseRecorder
	for i := 0; i < 21; i++ {
		rec = doPreflight(h, http.MethodPost, "application/json", `{"provider":"Google","client":"web","client_nonce":"n"}`)
	}
	if rec.Code != 429 || errorCode(rec) != "too_many_requests" || rec.Header().Get("X-RateLimit-Remaining") != "0" || rec.Header().Get("Retry-After") < "1" {
		t.Fatalf("21st = %d %v", rec.Code, rec.Header())
	}
}

func TestParseCallbackState(t *testing.T) {
	id := "6f1d3b4e-1111-4222-8333-944455556666"
	if tok, env, err := parseCallbackState(id); err != nil || tok != id || env != nil {
		t.Fatal(tok, env, err)
	}
	if tok, env, err := parseCallbackState(`{"state":"` + id + `","provider":"Google","client":"web"}`); err != nil || tok != id || env.Provider != "Google" {
		t.Fatal(tok, env, err)
	}
	for _, bad := range []string{"nope", `{"state":"x","provider":"Google","client":"web"}`, `[]`} {
		if _, _, err := parseCallbackState(bad); !errors.Is(err, ErrInvalidCallbackState) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestParseBoolishAndValidation(t *testing.T) {
	for v, want := range map[any]bool{true: true, "YES": true, "1": true, "no": false, "False": false} {
		if got, ok := parseBoolish(v); !ok || got != want {
			t.Errorf("%v => %v %v", v, got, ok)
		}
	}
	if _, ok := parseBoolish("maybe"); ok {
		t.Error("maybe accepted")
	}
	for email, ok := range map[string]bool{"a@b.c": true, "a b@c": false, "a@@b": false, "@b": false, "a@": false} {
		if validEmail(email) != ok {
			t.Errorf("email %q", email)
		}
	}
}
