// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
)

func TestWorkspaceInviteRequestFactsTrustConfiguredProxyHeaders(t *testing.T) {
	tests := []struct {
		name                   string
		principal              *auth.Principal
		remoteAddr             string
		headers                map[string]string
		trustedProxyCIDRs      []netip.Prefix
		trustedCloudflareCIDRs []netip.Prefix
		wantID                 string
		wantIP                 string
		wantASN                string
	}{
		{name: "session id wins on a trusted Cloudflare peer", principal: &auth.Principal{SessionID: "session-1"}, remoteAddr: "173.245.48.12:1234", headers: map[string]string{"CF-Connecting-IP": "203.0.113.1", "X-Forwarded-For": "203.0.113.2, 10.0.0.1", "CF-Ray": "ray-1", "x-affine-cf-asn": "64500"}, trustedCloudflareCIDRs: []netip.Prefix{netip.MustParsePrefix("173.245.48.0/20")}, wantID: "session-1", wantIP: "203.0.113.1", wantASN: "64500"},
		{name: "trusted proxy forwarding chain determines client ip", remoteAddr: "10.0.0.1:1234", headers: map[string]string{"X-Forwarded-For": "203.0.113.2, 10.0.0.2", "CF-Ray": "ray-1", "x-affine-cf-asn": "64500"}, trustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, wantID: "ray-1", wantIP: "203.0.113.2", wantASN: "64500"},
		{name: "untrusted headers are ignored", remoteAddr: "192.0.2.9:1234", headers: map[string]string{"CF-Connecting-IP": "203.0.113.1", "X-Forwarded-For": "203.0.113.2, 10.0.0.1", "X-Real-IP": "203.0.113.3", "CF-Ray": "ray-1", "x-affine-cf-asn": "64500"}, wantID: "192.0.2.9", wantIP: "192.0.2.9"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/graphql", nil)
			request.RemoteAddr = test.remoteAddr
			for name, value := range test.headers {
				request.Header.Set(name, value)
			}
			facts := workspaceInviteRequestFacts(request, test.principal, config.Config{
				TrustedProxyCIDRs:      test.trustedProxyCIDRs,
				TrustedCloudflareCIDRs: test.trustedCloudflareCIDRs,
			})
			if facts.RequestID != test.wantID || facts.ClientIP != test.wantIP || facts.ASN != test.wantASN {
				t.Fatalf("request facts = %#v, want request id %q, client IP %q, and ASN %q", facts, test.wantID, test.wantIP, test.wantASN)
			}
		})
	}
}

func TestWorkspaceInviteQuotaErrorsMatchLegacyGraphQLCategories(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		typeID string
	}{
		{name: "abuse action", err: invitelinks.ErrWorkspaceInviteForbidden, status: 403, typeID: "ACTION_FORBIDDEN"},
		{name: "quota limit", err: invitelinks.ErrWorkspaceInviteQuotaExceeded, status: 429, typeID: "TOO_MANY_REQUESTS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			presented := presentGraphQLError(context.Background(), test.err)
			if presented.Extensions["status"] != test.status || presented.Extensions["type"] != test.typeID {
				t.Fatalf("GraphQL extensions = %#v", presented.Extensions)
			}
		})
	}
}
