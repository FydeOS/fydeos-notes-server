// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package calendar

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestCalDAVDiscoveryAndCalendarListing(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/caldav":
			w.WriteHeader(http.StatusNotImplemented)
		case r.Method == "PROPFIND" && r.URL.Path == "/caldav/":
			_, _ = w.Write([]byte(`<multistatus xmlns="DAV:"><response><href>/caldav/</href><propstat><prop><current-user-principal><href>/principals/user/</href></current-user-principal></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`))
		case r.Method == "PROPFIND" && r.URL.Path == "/principals/user/":
			_, _ = w.Write([]byte(`<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><response><href>/principals/user/</href><propstat><prop><C:calendar-home-set><href>/calendars/user/</href></C:calendar-home-set></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`))
		case r.Method == "PROPFIND" && r.URL.Path == "/calendars/user/":
			_, _ = w.Write([]byte(`<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:IC="http://apple.com/ns/ical/"><response><href>/calendars/user/</href><propstat><prop><resourcetype><collection/></resourcetype></prop><status>HTTP/1.1 200 OK</status></propstat></response><response><href>/calendars/user/home/</href><propstat><prop><displayname>Home</displayname><resourcetype><collection/><C:calendar/></resourcetype><C:calendar-timezone>UTC</C:calendar-timezone><IC:calendar-color>#ff0000</IC:calendar-color></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	privateNetwork := false
	cfg := ProviderConfig{Enabled: true, AllowInsecureHTTP: true, BlockPrivateNetwork: &privateNetwork, RequestTimeoutMS: 1000, maxRedirects: 5}
	preset := ProviderPreset{ID: "test", ServerURL: server.URL + "/caldav/", AuthType: "basic"}
	c := newClient(cfg, preset, "user", "pass", func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	base, err := c.resolveDiscoveryURL(context.Background())
	if err != nil || base != preset.ServerURL {
		t.Fatalf("discovery URL=%q err=%v", base, err)
	}
	principal, err := c.currentPrincipal(context.Background(), base)
	if err != nil || principal != server.URL+"/principals/user/" {
		t.Fatalf("principal=%q err=%v", principal, err)
	}
	home, err := c.calendarHome(context.Background(), principal, base)
	if err != nil || home != server.URL+"/calendars/user/" {
		t.Fatalf("calendar home=%q err=%v", home, err)
	}
	items, err := c.listCalendars(context.Background(), home)
	if err != nil {
		t.Fatalf("list calendars: %v", err)
	}
	if len(items) != 1 || items[0].ID != server.URL+"/calendars/user/home/" || items[0].DisplayName == nil || *items[0].DisplayName != "Home" || items[0].Timezone == nil || *items[0].Timezone != "UTC" || items[0].Color == nil || *items[0].Color != "#ff0000" {
		t.Fatalf("calendars=%#v", items)
	}
	if len(requests) != 4 {
		t.Fatalf("CalDAV requests=%v", requests)
	}
}

func TestCalDAVCredentialEnvelopeRoundTrip(t *testing.T) {
	encoded, err := encryptCredential("private-key", "application-password")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decryptCredential("private-key", encoded)
	if err != nil || decoded != "application-password" {
		t.Fatalf("decoded credential=%q err=%v", decoded, err)
	}
	if _, err := decryptCredential("different-key", encoded); err == nil {
		t.Fatal("credential decrypted with a different key")
	}
}

func TestCalDAVBlocksPrivateTargetsByDefaultAndPinsResolvedAddress(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	c := newClient(ProviderConfig{AllowInsecureHTTP: true, RequestTimeoutMS: 1000, maxRedirects: 5}, ProviderPreset{ServerURL: server.URL}, "user", "pass", func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	_, err := c.request(context.Background(), http.MethodGet, server.URL, "", "")
	if err == nil || !strings.Contains(err.Error(), "private network") {
		t.Fatalf("private endpoint error=%v", err)
	}
	if called {
		t.Fatal("private endpoint was contacted")
	}

	if unsafeAddress(netip.MustParseAddr("169.254.169.254"), false) == false {
		t.Fatal("metadata address was allowed when private network blocking was disabled")
	}
	for _, address := range []string{"100.64.0.1", "100.127.255.254"} {
		if !unsafeAddress(netip.MustParseAddr(address), true) {
			t.Errorf("shared address %s was allowed with private network blocking enabled", address)
		}
	}
	if unsafeAddress(netip.MustParseAddr("8.8.8.8"), true) {
		t.Fatal("public address was blocked")
	}
}

func TestCalDAVEnforcesAllowedHostsAndRedirectLimit(t *testing.T) {
	privateNetwork := false
	addresses := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	blocked := newClient(ProviderConfig{AllowInsecureHTTP: true, AllowedHosts: []string{"example.com"}, BlockPrivateNetwork: &privateNetwork, RequestTimeoutMS: 1000, maxRedirects: 5}, ProviderPreset{}, "user", "pass", addresses)
	if _, err := blocked.request(context.Background(), http.MethodGet, "http://127.0.0.1/", "", ""); err == nil {
		t.Fatal("host outside the allowlist was accepted")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	limited := newClient(ProviderConfig{AllowInsecureHTTP: true, BlockPrivateNetwork: &privateNetwork, RequestTimeoutMS: 1000, maxRedirects: 0}, ProviderPreset{}, "user", "pass", addresses)
	_, err := limited.request(context.Background(), http.MethodGet, server.URL, "", "")
	var policyError *Error
	if !errors.As(err, &policyError) || policyError.Code != "caldav_max_redirects" {
		t.Fatalf("redirect limit error=%v", err)
	}
}

func TestEncryptedCredentialMatchesLegacyAESGCMLayout(t *testing.T) {
	privateKey, password := "stable test private key", "app-password"
	encoded, err := encryptCredential(privateKey, password)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) < 24 {
		t.Fatalf("decode legacy envelope: len=%d err=%v", len(data), err)
	}
	key := sha256.Sum256([]byte(privateKey))
	block, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCMWithTagSize(block, 12)
	nonce, tag, ciphertext := data[:12], data[12:24], data[24:]
	sealed := append(append([]byte(nil), ciphertext...), tag...)
	plain, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil || string(plain) != password {
		t.Fatalf("decrypt old CryptoHelper envelope=%q err=%v", plain, err)
	}
}

func TestCalDAVDigestAuthenticationFallback(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasPrefix(r.Header.Get("Authorization"), "Digest ") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", `Digest realm="test", nonce="nonce", qop="auth", algorithm=MD5, opaque="opaque"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	privateNetwork := false
	c := newClient(ProviderConfig{AllowInsecureHTTP: true, BlockPrivateNetwork: &privateNetwork, RequestTimeoutMS: 1000, maxRedirects: 5}, ProviderPreset{AuthType: "auto"}, "user", "pass", func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	response, err := c.request(context.Background(), http.MethodGet, server.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls != 2 || c.authType != "digest" {
		t.Fatalf("requests=%d auth type=%q", calls, c.authType)
	}
}
