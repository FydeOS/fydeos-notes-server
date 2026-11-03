// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRuntimeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPolicy(t *testing.T) {
	ctx := context.Background()
	policy, err := LoadPolicy(ctx, nil, "", "selfhosted")
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Enabled || strings.Join(policy.AllowedProviders, ",") != "anthropic,fal,gemini,openai" || policy.CustomEndpointMode != "disabled" || policy.PrivateEndpointSupported {
		t.Fatalf("defaults = %+v", policy)
	}
	path := writeRuntimeConfig(t, `{"copilot":{"byok.allowCustomEndpoint":true,"byok.allowPrivateEndpoint":true,"byok.allowedProviders":["openai","gemini"]}}`)
	policy, err = LoadPolicy(ctx, nil, path, "selfhosted")
	if err != nil || policy.CustomEndpointMode != "enabled" || !policy.PrivateEndpointSupported || strings.Join(policy.AllowedProviders, ",") != "gemini,openai" {
		t.Fatalf("custom = %+v, %v", policy, err)
	}
	for _, deployment := range []string{"cloud", "affine"} {
		policy, err = LoadPolicy(ctx, nil, path, deployment)
		if err != nil || policy.CustomEndpointMode != "unavailable" || policy.PrivateEndpointSupported {
			t.Fatalf("%s = %+v, %v", deployment, policy, err)
		}
	}
	path = writeRuntimeConfig(t, `{"copilot":{"byok.allowPrivateEndpoint":true}}`)
	if policy, _ = LoadPolicy(ctx, nil, path, "selfhosted"); policy.PrivateEndpointSupported {
		t.Fatal("private without custom enabled")
	}
	for _, bad := range []string{`["openai","openai"]`, `["mistral"]`, `"openai"`} {
		path = writeRuntimeConfig(t, `{"copilot":{"byok.allowedProviders":`+bad+`}}`)
		if _, err := LoadPolicy(ctx, nil, path, "selfhosted"); err == nil || !strings.Contains(err.Error(), "invalid runtime configuration copilot.byok.allowedProviders") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
}

func staticResolver(addrs ...string) EndpointResolver {
	return func(context.Context, string) ([]netip.Addr, error) {
		out := make([]netip.Addr, len(addrs))
		for i, a := range addrs {
			out[i] = netip.MustParseAddr(a)
		}
		return out, nil
	}
}

func TestPolicyAdmit(t *testing.T) {
	ctx := context.Background()
	open := Policy{Enabled: true, AllowedProviders: []string{"openai"}, CustomEndpointMode: "enabled"}
	private := open
	private.PrivateEndpointSupported = true
	https := Endpoint{Kind: "openai_compatible", URL: "https://llm.example.com/v1", Dialect: "responses"}
	http := Endpoint{Kind: "openai_compatible", URL: "http://llm.example.com/v1", Dialect: "responses"}
	cases := []struct {
		name     string
		policy   Policy
		endpoint Endpoint
		addr     string
		ok       bool
	}{
		{"https public", open, https, "93.184.216.34", true},
		{"https private", open, https, "10.0.0.1", false},
		{"https private enabled", private, https, "10.0.0.1", true},
		{"http public", private, http, "93.184.216.34", false},
		{"http private", private, http, "192.168.1.2", true},
		{"metadata", private, https, "fd00:ec2::254", false},
		{"cgnat", open, https, "100.64.0.1", false},
		{"nat64 private", open, https, "64:ff9b::a00:1", false},
		{"nat64 public", open, https, "64:ff9b::5db8:d822", true},
		{"mapped loopback", open, https, "::ffff:127.0.0.1", false},
		{"mapped loopback private", private, https, "::ffff:127.0.0.1", true},
		{"public v6", open, https, "2606:4700::1", true},
		{"doc v6", open, https, "2001:db8::1", false},
		{"6to4", open, https, "2002:5db8:d822::1", false},
	}
	for _, tc := range cases {
		err := tc.policy.Admit(ctx, "openai", tc.endpoint, staticResolver(tc.addr))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	slow := func(ctx context.Context, string string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := open.Admit(deadline, "openai", https, slow); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout err = %v", err)
	}
	if err := open.Admit(ctx, "openai", https, staticResolver()); err == nil || !strings.Contains(err.Error(), "DNS resolution failed") {
		t.Fatalf("empty err = %v", err)
	}
	if err := open.Admit(ctx, "openai", Endpoint{Kind: "openai_compatible", URL: "https://localhost:8080", Dialect: "responses"}, staticResolver("93.184.216.34")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("localhost err = %v", err)
	}
	if err := (Policy{Enabled: true, AllowedProviders: []string{"openai"}, CustomEndpointMode: "unavailable"}).Admit(ctx, "openai", https, nil); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("unavailable err = %v", err)
	}
	if err := open.Admit(ctx, "gemini", Endpoint{Kind: "provider_default"}, nil); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatal("disallowed provider admitted")
	}
	if !open.Allows("openai", Endpoint{Kind: "provider_default"}) || (Policy{AllowedProviders: []string{"openai"}}).Allows("openai", Endpoint{Kind: "provider_default"}) {
		t.Fatal("Allows")
	}
}

func TestGuardedTransportRefusesPrivateDial(t *testing.T) {
	transport := newGuardedTransport("https", false, nil)
	if _, err := transport.DialContext(context.Background(), "tcp", "10.0.0.1:443"); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("dial private = %v", err)
	}
	pinned := newPinnedTransport("https", false, nil, "llm.example.com", []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if _, err := pinned.DialContext(context.Background(), "tcp", "llm.example.com:443"); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("dial pinned loopback = %v", err)
	}
	client := NewPublicHTTPSClient(time.Second)
	if _, err := client.Get("https://127.0.0.1:1/"); err == nil {
		t.Fatal("public client reached loopback")
	}
}
