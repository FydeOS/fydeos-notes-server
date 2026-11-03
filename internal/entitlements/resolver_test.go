// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package entitlements

import (
	"testing"
	"time"
)

func TestResolveUsesDeploymentFallbackAndPlanPriority(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Hour)
	graceUntil := now.Add(2 * time.Hour)
	quantity := int32(5)

	cloudDefault := Resolve(Context{Deployment: Cloud, TargetType: "workspace", Now: now}, nil)
	if cloudDefault.Plan != "free" || cloudDefault.Limits.BlobLimit != 10*1024*1024 || cloudDefault.Limits.SeatLimit != 3 || cloudDefault.Limits.StorageQuota != 10*1024*1024*1024 {
		t.Fatalf("cloud default grant = %+v", cloudDefault)
	}
	selfHostedDefault := Resolve(Context{Deployment: SelfHosted, TargetType: "workspace", Now: now}, nil)
	if selfHostedDefault.Plan != "selfhost_free" || selfHostedDefault.Limits.SeatLimit != 10 || selfHostedDefault.Limits.StorageQuota != 100*1024*1024*1024 {
		t.Fatalf("self-hosted default grant = %+v", selfHostedDefault)
	}
	grant := Resolve(Context{Deployment: Cloud, TargetType: "workspace", Now: now}, []Fact{
		{Source: "cloud_subscription", Status: "grace", Plan: "team", Quantity: &quantity, GraceUntil: &graceUntil},
		{Source: "admin_grant", Status: "active", Plan: "free", ExpiresAt: &expiry},
	})
	if grant.Plan != "free" || grant.Commercial || grant.Limits.SeatLimit != 3 {
		t.Fatalf("active free grant should outrank a grace team grant: %+v", grant)
	}

	grant = Resolve(Context{Deployment: Cloud, TargetType: "workspace", Now: now}, []Fact{
		{Source: "cloud_subscription", Status: "active", Plan: "free"},
		{Source: "cloud_subscription", Status: "active", Plan: "team", Quantity: &quantity},
	})
	if grant.Plan != "team" || !grant.Commercial || grant.Limits.SeatLimit != 5 || grant.Limits.StorageQuota != 200*1024*1024*1024 {
		t.Fatalf("team grant = %+v", grant)
	}
}

func TestPlanGrantsExposePerBlobLimits(t *testing.T) {
	tests := []struct {
		plan string
		want int64
	}{
		{plan: "free", want: 10 * 1024 * 1024},
		{plan: "ai", want: 10 * 1024 * 1024},
		{plan: "pro", want: 100 * 1024 * 1024},
		{plan: "lifetime_pro", want: 100 * 1024 * 1024},
		{plan: "team", want: 500 * 1024 * 1024},
		{plan: "selfhost_team", want: 500 * 1024 * 1024},
		{plan: "selfhost_free", want: 100 * 1024 * 1024},
	}
	for _, test := range tests {
		t.Run(test.plan, func(t *testing.T) {
			grant := planGrant(test.plan, 1, nil)
			if grant.Limits.BlobLimit != test.want {
				t.Fatalf("blob limit = %d, want %d", grant.Limits.BlobLimit, test.want)
			}
			wantHistory := int64(30 * 24 * 60 * 60)
			if test.plan == "free" || test.plan == "ai" {
				wantHistory = 7 * 24 * 60 * 60
			}
			if grant.Limits.HistoryPeriodSeconds != wantHistory {
				t.Fatalf("history period = %d, want %d", grant.Limits.HistoryPeriodSeconds, wantHistory)
			}
		})
	}
}

func TestResolveAIEntitlementOverlaysCopilotRightsWithoutReplacingBasePlan(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	grant := Resolve(Context{Deployment: Cloud, TargetType: "user", Now: now}, []Fact{
		{Source: "cloud_subscription", Status: "active", Plan: "pro", ExpiresAt: &expiresAt},
		{Source: "admin_grant", Status: "active", Plan: "ai", ExpiresAt: &expiresAt},
	})
	if grant.Plan != "pro" || grant.Limits.SeatLimit != 10 || !grant.Rights.CopilotBYOK || !grant.Rights.UnlimitedCopilot || grant.Limits.CopilotActionLimit != nil {
		t.Fatalf("AI overlay grant = %+v", grant)
	}

	teamLimit := planGrant("team", 2, nil).Limits.CopilotActionLimit
	if teamLimit != nil {
		t.Fatalf("team copilot action limit = %d, want unlimited", *teamLimit)
	}
	freeLimit := planGrant("free", 0, nil).Limits.CopilotActionLimit
	if freeLimit == nil || *freeLimit != 10 {
		t.Fatalf("free copilot action limit = %v, want 10", freeLimit)
	}
}

func TestResolveRejectsIneligibleAndInactiveEntitlements(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	expired := now
	grant := Resolve(Context{Deployment: Cloud, TargetType: "workspace", Now: now}, []Fact{
		{Source: "selfhost_license", Status: "active", Plan: "team"},
		{Source: "cloud_subscription", Status: "active", Plan: "pro"},
		{Source: "cloud_subscription", Status: "active", Plan: "team", StartsAt: &future},
		{Source: "cloud_subscription", Status: "active", Plan: "team", ExpiresAt: &expired},
	})
	if grant.Plan != "free" || grant.Commercial {
		t.Fatalf("ineligible or inactive entitlements changed fallback: %+v", grant)
	}
}

func TestResolveSelfHostedIgnoresLicenseFacts(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	quantity := int32(20)
	grant := Resolve(Context{Deployment: SelfHosted, TargetType: "workspace", WorkspaceID: "workspace-1", Now: now}, []Fact{
		{Source: "selfhost_license", Status: "active", Plan: "selfhost_team", Quantity: &quantity, SignedPayload: []byte(`{}`)},
		{Source: "admin_grant", Status: "active", Plan: "team", Quantity: &quantity},
	})
	if grant.Plan != "selfhost_free" || grant.Commercial || grant.Limits.SeatLimit != 10 {
		t.Fatalf("self-hosted license facts changed the fallback grant: %+v", grant)
	}
}
