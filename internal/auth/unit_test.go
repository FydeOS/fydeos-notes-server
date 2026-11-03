// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/sha3"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func testKey(id string) signingKey {
	now := time.Now().UTC()
	return signingKey{ID: id, Secret: randomB64(32), Status: "active", CreatedAt: &now, Source: "auto"}
}

func TestAccessTokenRoundTripAndRejections(t *testing.T) {
	key := testKey("auth-test")
	now := time.Now()
	token, err := signAccessToken(key, "user-1", "session-1", now, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	kid, parts, err := parseAccessTokenKid(token)
	if err != nil || kid != "auth-test" {
		t.Fatalf("kid=%q err=%v", kid, err)
	}
	claims, err := verifyAccessToken(parts, key, now)
	if err != nil || claims.Sub != "user-1" || claims.Sid != "session-1" {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	// Tampered signature.
	tampered := strings.Split(token, ".")
	tampered[2] = b64.EncodeToString([]byte("wrong-signature-wrong-signature!"))
	if _, err := verifyAccessToken(tampered, key, now); !errors.Is(err, ErrAccessTokenInvalid) {
		t.Fatalf("tampered err=%v", err)
	}
	// Wrong secret.
	if _, err := verifyAccessToken(parts, testKey("auth-test"), now); !errors.Is(err, ErrAccessTokenInvalid) {
		t.Fatalf("wrong key err=%v", err)
	}
	// Expiry with 30 s skew: 10 s past exp accepted, 31 s rejected.
	if _, err := verifyAccessToken(parts, key, now.Add(15*time.Minute+10*time.Second)); err != nil {
		t.Fatalf("skew accept err=%v", err)
	}
	if _, err := verifyAccessToken(parts, key, now.Add(15*time.Minute+31*time.Second)); !errors.Is(err, ErrAccessTokenExpired) {
		t.Fatalf("expired err=%v", err)
	}
	// Wrong alg header.
	header := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"auth-test"}`))
	if _, _, err := parseAccessTokenKid(header + "." + parts[1] + "." + parts[2]); !errors.Is(err, ErrAccessTokenInvalid) {
		t.Fatalf("alg err=%v", err)
	}
	// Wrong audience, correctly signed.
	bad := forgeToken(t, key, map[string]any{"sub": "u", "sid": "s", "typ": "session_access", "iss": "affine", "aud": "other", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
	_, badParts, _ := parseAccessTokenKid(bad)
	if _, err := verifyAccessToken(badParts, key, now); !errors.Is(err, ErrAccessTokenInvalid) {
		t.Fatalf("aud err=%v", err)
	}
}

func forgeToken(t *testing.T, key signingKey, claims map[string]any) string {
	t.Helper()
	token, err := signAccessToken(key, "x", "y", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	payload, _ := json.Marshal(claims)
	parts[1] = b64.EncodeToString(payload)
	secret, _ := key.secretBytes()
	return parts[0] + "." + parts[1] + "." + hmacSign(secret, parts[0]+"."+parts[1])
}

func TestBearerHeaderParsing(t *testing.T) {
	for header, want := range map[string]error{"Bearer": ErrAccessTokenInvalid, "Bearer a b": ErrAccessTokenInvalid, "bearer tok": nil} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", header)
		_, isBearer, err := bearerToken(r)
		if !isBearer || !errors.Is(err, want) {
			t.Fatalf("%q: bearer=%v err=%v", header, isBearer, err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Basic x")
	if _, isBearer, _ := bearerToken(r); isBearer {
		t.Fatal("Basic must fall back to cookies")
	}
}

func TestKeyringValidation(t *testing.T) {
	good := keyring{testKey("a")}
	if err := good.validate(0); err != nil {
		t.Fatal(err)
	}
	two := keyring{testKey("a"), testKey("b")}
	short := keyring{testKey("a")}
	short[0].Secret = randomB64(16)
	source := keyring{testKey("a")}
	source[0].Source = "manual"
	for name, ks := range map[string]keyring{"two actives": two, "short secret": short, "bad source": source} {
		if err := ks.validate(0); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	now := time.Now().UTC()
	tight := now.Add(time.Minute)
	retiring := testKey("old")
	retiring.Status, retiring.RetiredAt, retiring.VerifyUntil = "retiring", &now, &tight
	if err := (keyring{retiring, testKey("new")}).validate(15 * time.Minute); err == nil {
		t.Fatal("verification window shorter than access TTL accepted for admin operations")
	}
	if err := (keyring{retiring, testKey("new")}).validate(0); err != nil {
		t.Fatalf("persisted keyring rejected: %v", err)
	}
	auto := newAutoKeyring(now)
	if err := auto.validate(0); err != nil || !strings.HasPrefix(auto[0].ID, "auth-") {
		t.Fatalf("auto keyring %+v err=%v", auto, err)
	}
}

func TestWriteAccessTokenError(t *testing.T) {
	cases := map[error]string{
		ErrAccessTokenExpired: "ACCESS_TOKEN_EXPIRED", ErrAccessTokenInvalid: "ACCESS_TOKEN_INVALID",
		ErrAuthSessionExpired: "AUTH_SESSION_EXPIRED", ErrAuthSessionRevoked: "AUTH_SESSION_REVOKED",
	}
	for err, code := range cases {
		w := httptest.NewRecorder()
		if !WriteAccessTokenError(w, fmt.Errorf("wrapped: %w", err)) || w.Code != 401 {
			t.Fatalf("%v not written", err)
		}
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if body["code"] != code || body["name"] != code || body["type"] != "AUTHENTICATION_REQUIRED" {
			t.Fatalf("body=%v", body)
		}
	}
	w := httptest.NewRecorder()
	if WriteAccessTokenError(w, ErrNoSession) || w.Body.Len() != 0 {
		t.Fatal("other errors must not be written")
	}
}

func TestIsNativeClientRequest(t *testing.T) {
	for origin, want := range map[string]bool{"": true, "capacitor://localhost": true, "ionic://localhost": true, "https://localhost": true, "https://evil": false} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("x-affine-client-kind", "native")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if IsNativeClientRequest(r) != want {
			t.Fatalf("origin %q", origin)
		}
	}
}

func TestReadLimiter(t *testing.T) {
	l := NewReadLimiter(1)
	release, ok := l.TryAcquire()
	if !ok {
		t.Fatal("first acquire failed")
	}
	if _, ok := l.TryAcquire(); ok {
		t.Fatal("second acquire succeeded")
	}
	release()
	again, ok := l.TryAcquire()
	if !ok {
		t.Fatal("acquire after release failed")
	}
	again()
	var nilLimiter *ReadLimiter
	if _, ok := nilLimiter.TryAcquire(); !ok {
		t.Fatal("nil limiter must allow")
	}
	// A saturated limiter makes read-limited handlers answer 429.
	h := NewSessionHTTPHandler(NewSessionResolver(nil), l)
	hold, _ := l.TryAcquire()
	defer hold()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/auth/session", nil))
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestMemoryLimiter(t *testing.T) {
	l := NewRedisPasswordLoginLimiter(nil)
	ctx := context.Background()
	now := time.Now()
	for i := 1; i <= 21; i++ {
		if got := l.AllowStrict(ctx, "198.51.100.1", now); got != (i <= 20) {
			t.Fatalf("strict hit %d allowed=%v", i, got)
		}
	}
	if !l.AllowStrict(ctx, "", now) {
		t.Fatal("empty ip must be allowed")
	}
	for i := 1; i <= 6; i++ {
		if got := l.Allow(ctx, "A@example.com", "", defaultSignInPolicy, now); got != (i <= 5) {
			t.Fatalf("email hit %d allowed=%v", i, got)
		}
	}
	if !l.Allow(ctx, "a@example.com", "", defaultSignInPolicy, now.Add(61*time.Second)) {
		t.Fatal("window did not reset")
	}
}

func TestPrivateKeyEnvelopeAndSuccessor(t *testing.T) {
	pemText, err := generateP256PEM()
	if err != nil || validP256PEM(pemText) != nil {
		t.Fatalf("generated key invalid: %v", err)
	}
	sealed, err := sealPrivateKey("secret-one", pemText)
	if err != nil || !strings.HasPrefix(sealed, "fydeos-private-key:v1:") {
		t.Fatalf("sealed=%q err=%v", sealed, err)
	}
	if opened, err := openPrivateKey("secret-one", sealed); err != nil || opened != pemText {
		t.Fatalf("open err=%v", err)
	}
	if _, err := openPrivateKey("secret-two", sealed); err == nil {
		t.Fatal("wrong secret decrypted")
	}
	if _, err := openPrivateKey("", sealed); err == nil {
		t.Fatal("blank secret accepted")
	}
	if err := validP256PEM(pemText + "\ntrailing"); err == nil {
		t.Fatal("trailing data accepted")
	}
	box, err := sealSuccessor(pemText, "aff_rt_v1.x.y", "src", "sess")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := openSuccessor(pemText, box, "src", "sess"); err != nil || got != "aff_rt_v1.x.y" {
		t.Fatalf("successor=%q err=%v", got, err)
	}
	if _, err := openSuccessor(pemText, box, "src", "other"); err == nil {
		t.Fatal("successor opened with another session id")
	}
}

func TestPrivateKeyWrappingSecret(t *testing.T) {
	t.Setenv("NOTES_PRIVATE_KEY_SECRET", "  ")
	t.Setenv("OAUTH_FYDEOS_CLIENT_SECRET", "fallback")
	if PrivateKeyWrappingSecret() != "fallback" {
		t.Fatal("fallback not used")
	}
	t.Setenv("NOTES_PRIVATE_KEY_SECRET", "primary")
	if PrivateKeyWrappingSecret() != "primary" {
		t.Fatal("primary not preferred")
	}
}

func TestRefreshTokenFormat(t *testing.T) {
	token, id, hash := newRefreshToken()
	gotID, secret, ok := parseRefreshToken(token)
	if !ok || gotID != id || !hashMatches(hash, secret) {
		t.Fatalf("token %q did not round trip", token)
	}
	for _, bad := range []string{"", "aff_rt_v1.", "aff_rt_v1.abc.def", token + "=", strings.Replace(token, "aff_rt_v1", "aff_rt_v2", 1)} {
		if _, _, ok := parseRefreshToken(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func makeStamp(t *testing.T, bits int, resource string, at time.Time) string {
	t.Helper()
	for counter := 0; ; counter++ {
		stamp := fmt.Sprintf("1:%d:%s:%s::rand:%d", bits, at.UTC().Format("20060102150405"), resource, counter)
		sum := sha3.Sum256([]byte(stamp))
		if strings.HasPrefix(hex.EncodeToString(sum[:]), strings.Repeat("0", bits/4)) {
			return stamp
		}
	}
}

func TestHashcashStamp(t *testing.T) {
	now := time.Now()
	stamp := makeStamp(t, 16, "res", now)
	if !validStamp(stamp, "res", 16, now) {
		t.Fatal("valid stamp rejected")
	}
	if validStamp(stamp, "other", 16, now) {
		t.Fatal("wrong resource accepted")
	}
	if validStamp(makeStamp(t, 16, "res", now.Add(-6*time.Minute)), "res", 16, now) {
		t.Fatal("old stamp accepted")
	}
	if validStamp(stamp, "res", 20, now) {
		t.Fatal("too few bits accepted")
	}
}

func TestURLHelpers(t *testing.T) {
	got := appendQuery("https://h/p?source=admin&tag=hello%20world&tilde=%7E&star=%2A", [2]string{"userId", "u 1"}, [2]string{"token", "t"})
	if got != "https://h/p?source=admin&tag=hello+world&tilde=%7E&star=*&userId=u+1&token=t" {
		t.Fatalf("appendQuery=%q", got)
	}
	cfg := config.Config{HTTPS: true, Host: "notes.example.com"}
	r := httptest.NewRequest(http.MethodPost, "https://notes.example.com/api/auth/sign-in", nil)
	cases := map[string]bool{
		"":                                true,
		"/magic-link":                     true,
		"https://evil.example/x":          false,
		"/x\\y":                           false,
		"https://user@notes.example.com/": false,
		"javascript:alert(1)":             false,
		"/m?redirect_uri=https://accounts.google.com/o": true,
		"/m?redirect_uri=https://evil.com":              false,
		"/m?redirect_uri=/workspace":                    true,
		"https://NOTES.example.com:443/ok":              true,
	}
	for raw, ok := range cases {
		_, err := resolveCallbackURL(r, cfg, raw, "/magic-link")
		if (err == nil) != ok {
			t.Fatalf("%q: err=%v want ok=%v", raw, err, ok)
		}
	}
}

func TestEmailRules(t *testing.T) {
	for email, ok := range map[string]bool{"a@b.co": true, "a.b+c@sub.example.com": true, ".a@b.co": false, "a..b@b.co": false, "a@b": false, "a@-b.co": false, "a b@c.co": false} {
		if strictEmail(email) != ok {
			t.Fatalf("strictEmail(%q)", email)
		}
	}
	if !validSignInEmail("user@example.com") || validSignInEmail("User <user@example.com>") || validSignInEmail("nobody") {
		t.Fatal("validSignInEmail")
	}
}

type fakeConfig map[string]string

type fakeRow struct {
	value string
	ok    bool
}

func (r fakeRow) Scan(dest ...any) error {
	if !r.ok {
		return errNoRowsForTest
	}
	*(dest[0].(*[]byte)) = []byte(r.value)
	return nil
}

func TestClientVersionGate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	db := fakeConfig{"client.versionControl.enabled": "true", "client.versionControl.requiredVersion": `">=1.0.0"`}
	if ok, req, err := checkClientVersion(ctx, db, config.Config{}, "0.30.0", now); err != nil || ok || req != ">=1.0.0" {
		t.Fatalf("ok=%v req=%q err=%v", ok, req, err)
	}
	if ok, _, _ := checkClientVersion(ctx, db, config.Config{}, "v1.2.0-beta.1", now); !ok {
		t.Fatal("prerelease not included")
	}
	canary := config.Config{Namespace: "canary"}
	if ok, _, _ := checkClientVersion(ctx, db, canary, "2026.10.20-canary.3", now); !ok {
		t.Fatal("recent canary rejected")
	}
	old := now.AddDate(0, 0, -70)
	if ok, req, _ := checkClientVersion(ctx, db, canary, fmt.Sprintf("%d.%d.%d-canary.1", old.Year(), old.Month(), old.Day()), now); ok || req != "canary (within 2 months)" {
		t.Fatalf("old canary ok=%v req=%q", ok, req)
	}
	if ok, _, _ := checkClientVersion(ctx, fakeConfig{}, config.Config{}, "", now); !ok {
		t.Fatal("disabled gate rejected")
	}
}

func TestSignInPolicyParsing(t *testing.T) {
	ctx := context.Background()
	if p, err := loadSignInPolicy(ctx, fakeConfig{"auth.signInRateLimit": `{"ttl":1000,"ipLimit":3,"emailLimit":2}`}, config.Config{}); err != nil || p.TTL != time.Second || p.IPLimit != 3 {
		t.Fatalf("policy=%+v err=%v", p, err)
	}
	for _, bad := range []string{`{"ttl":0}`, `{"other":1}`, `{"ttl":1}{}`, `{"ipLimit":3000000000}`} {
		if _, err := loadSignInPolicy(ctx, fakeConfig{"auth.signInRateLimit": bad}, config.Config{}); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestTokenPolicyRange(t *testing.T) {
	if _, err := loadTokenPolicy(context.Background(), fakeConfig{"auth.token.accessTokenTtl": "30"}, config.Config{}); err == nil {
		t.Fatal("access TTL 30 accepted")
	}
	p, err := loadTokenPolicy(context.Background(), fakeConfig{}, config.Config{})
	if err != nil || p.access != 15*time.Minute || p.grace != 30*time.Second {
		t.Fatalf("defaults=%+v err=%v", p, err)
	}
}
