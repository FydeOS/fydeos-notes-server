// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"strconv"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
)

func TestBuildWorkspaceInviteQuotaScopes(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	input := workspaceInviteQuotaInput{
		ActorUserID: "actor-1", WorkspaceID: "workspace-1", TargetCount: 1,
		TargetDomains: []inviteQuotaTargetDomain{{Domain: "example.com", Count: 1}},
	}
	actor := inviteActorFacts{
		userID: "actor-1", email: "person@example.com", createdAt: now.Add(-60 * 24 * time.Hour),
		registered: true, emailVerified: true,
	}
	workspace := inviteQuotaWorkspaceFacts{createdAt: now.Add(-60 * 24 * time.Hour)}
	quota := inviteQuotaFacts{known: true, plan: "free", ownerUserID: "owner-1", usesOwnerQuota: true, seatLimit: 10, memberCount: 8}
	config := inviteQuotaPolicyConfig{deployment: entitlements.Cloud, subjectHashSalt: "private-key"}

	plan, err := planWorkspaceInviteQuota(input, actor, workspace, quota, inviteQuotaActivityFacts{}, inviteAbuseFacts{valid: true}, config, now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.decision != inviteQuotaReserve {
		t.Fatalf("decision = %q, want reserve", plan.decision)
	}
	got := make(map[string]int32, len(plan.scopes))
	for _, scope := range plan.scopes {
		got[scope.key+":"+strconv.Itoa(int(scope.windowSeconds))] = scope.limit
		if scope.requested != 1 || scope.bucketSeconds < 1 {
			t.Errorf("scope %q has invalid request/bucket: %+v", scope.key, scope)
		}
	}
	actorSubject := actorEmailSubjectKey(actor.email, config.subjectHashSalt)
	want := map[string]int32{
		"invite:user:actor-1:3600":                                     5,
		"invite:user:actor-1:86400":                                    15,
		"invite:user:actor-1:604800":                                   10,
		"invite:actor_subject:" + actorSubject + ":604800":             10,
		"invite:workspace:workspace-1:3600":                            5,
		"invite:workspace:workspace-1:86400":                           15,
		"invite:quota_subject:owner:owner-1:604800":                    10,
		"invite:user_domain:actor-1:example.com:86400":                 10,
		"invite:workspace_domain:workspace-1:example.com:86400":        10,
		"invite:quota_subject_domain:owner:owner-1:example.com:604800": 10,
		"invite:target_domain_global:example.com:60":                   100,
		"invite:target_domain_global:example.com:86400":                2000,
	}
	if len(got) != len(want) {
		t.Fatalf("scope count = %d, want %d: %#v", len(got), len(want), got)
	}
	for key, limit := range want {
		if got[key] != limit {
			t.Errorf("scope %q limit = %d, want %d", key, got[key], limit)
		}
	}
}

func TestWorkspaceInviteQuotaUsesTrustedSourceScopesAndRiskLimits(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	input := workspaceInviteQuotaInput{
		ActorUserID: "actor-1", WorkspaceID: "workspace-1", TargetCount: 1,
		TargetDomains: []inviteQuotaTargetDomain{{Domain: "qq.com", Count: 1}},
		Source:        &inviteQuotaSourceFacts{Trusted: true, IP: "203.0.113.45", ASN: 64500},
	}
	actor := inviteActorFacts{email: "person@example.com", createdAt: now.Add(-60 * 24 * time.Hour), registered: true, emailVerified: true}
	workspace := inviteQuotaWorkspaceFacts{createdAt: now.Add(-60 * 24 * time.Hour)}
	quota := inviteQuotaFacts{known: true, plan: "free", ownerUserID: "owner-1", usesOwnerQuota: true, seatLimit: 10, memberCount: 8}
	config := inviteQuotaPolicyConfig{deployment: entitlements.Cloud, subjectHashSalt: "private-key"}
	plan, err := planWorkspaceInviteQuota(input, actor, workspace, quota, inviteQuotaActivityFacts{}, inviteAbuseFacts{valid: true}, config, now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.decision != inviteQuotaReserve {
		t.Fatalf("decision = %q, want reserve", plan.decision)
	}
	got := make(map[string]int32, len(plan.scopes))
	for _, scope := range plan.scopes {
		got[scope.key+":"+strconv.Itoa(int(scope.windowSeconds))] = scope.limit
	}
	want := map[string]int32{
		"invite:source_prefix:203.0.113.0/24:3600":                30,
		"invite:source_prefix:203.0.113.0/24:86400":               100,
		"invite:source_prefix_domain:203.0.113.0/24:qq.com:3600":  5,
		"invite:source_prefix_domain:203.0.113.0/24:qq.com:86400": 15,
		"invite:source_asn_domain:64500:qq.com:3600":              50,
		"invite:source_asn_domain:64500:qq.com:86400":             150,
		"invite:target_domain_global:qq.com:60":                   30,
		"invite:target_domain_global:qq.com:86400":                500,
		"invite:user_domain:actor-1:qq.com:86400":                 5,
	}
	for key, limit := range want {
		if got[key] != limit {
			t.Errorf("scope %q limit = %d, want %d", key, got[key], limit)
		}
	}
}

func TestWorkspaceInviteQuotaAbuseDecisions(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	baseActor := inviteActorFacts{userID: "actor-1", email: "person@example.com", createdAt: now.Add(-60 * 24 * time.Hour), registered: true, emailVerified: true}
	workspace := inviteQuotaWorkspaceFacts{createdAt: now.Add(-60 * 24 * time.Hour)}
	quota := inviteQuotaFacts{known: true, plan: "free", ownerUserID: "owner-1", usesOwnerQuota: true, seatLimit: 20, memberCount: 8}
	config := inviteQuotaPolicyConfig{deployment: entitlements.Cloud, subjectHashSalt: "private-key"}
	tests := []struct {
		name       string
		actor      inviteActorFacts
		count      int32
		domain     string
		domainN    int32
		source     *inviteQuotaSourceFacts
		wantReason string
		wantAction string
		wantKind   string
	}{
		{name: "unverified actor", actor: func() inviteActorFacts { actor := baseActor; actor.emailVerified = false; return actor }(), count: 3, domain: "qq.com", domainN: 3, wantReason: "unverified_high_risk_domain_burst", wantAction: "quarantine_actor", wantKind: "actor_email"},
		{name: "workspace burst", actor: baseActor, count: 30, domain: "qq.com", domainN: 15, wantReason: "workspace_high_risk_domain_burst", wantAction: "quarantine_workspace", wantKind: "workspace"},
		{name: "source cohort burst", actor: baseActor, count: 20, domain: "qq.com", domainN: 10, source: &inviteQuotaSourceFacts{Trusted: true, IP: "203.0.113.4"}, wantReason: "source_high_risk_domain_burst", wantAction: "quarantine_source_cohort", wantKind: "source_prefix_domain"},
		{name: "actor burst", actor: baseActor, count: 10, domain: "qq.com", domainN: 5, wantReason: "high_risk_domain_burst", wantAction: "quarantine_actor", wantKind: "actor_email"},
		{name: "actor ban threshold", actor: baseActor, count: 20, domain: "qq.com", domainN: 10, wantReason: "high_risk_domain_burst", wantAction: "ban_actor", wantKind: "actor_email"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			domains := []inviteQuotaTargetDomain{{Domain: test.domain, Count: test.domainN}}
			if remaining := test.count - test.domainN; remaining > 0 {
				domains = append(domains, inviteQuotaTargetDomain{Domain: "example.com", Count: remaining})
			}
			input := workspaceInviteQuotaInput{ActorUserID: "actor-1", WorkspaceID: "workspace-1", TargetCount: test.count,
				TargetDomains: domains, Source: test.source}
			plan, err := planWorkspaceInviteQuota(input, test.actor, workspace, quota, inviteQuotaActivityFacts{}, inviteAbuseFacts{valid: true}, config, now)
			if err != nil {
				t.Fatal(err)
			}
			if plan.decision != inviteQuotaDeny || plan.reason != test.wantReason || plan.persistence == nil {
				t.Fatalf("plan = %+v, want abuse denial %q", plan, test.wantReason)
			}
			if plan.persistence.action != test.wantAction || plan.persistence.kind != test.wantKind || plan.persistence.requested != test.count {
				t.Fatalf("abuse persistence = %+v", plan.persistence)
			}
		})
	}
}

func TestWorkspaceInviteQuotaDeniesUnsafeStateAndDelay(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	input := workspaceInviteQuotaInput{ActorUserID: "actor-1", WorkspaceID: "workspace-1", TargetCount: 1,
		TargetDomains: []inviteQuotaTargetDomain{{Domain: "example.com", Count: 1}}}
	actor := inviteActorFacts{userID: "actor-1", email: "person@example.com", createdAt: now.Add(-time.Hour), registered: true, emailVerified: true}
	workspace := inviteQuotaWorkspaceFacts{createdAt: now.Add(-30 * 24 * time.Hour)}
	quota := inviteQuotaFacts{known: true, plan: "free", ownerUserID: "owner-1", usesOwnerQuota: true, seatLimit: 3, memberCount: 4}
	config := inviteQuotaPolicyConfig{deployment: entitlements.Cloud, subjectHashSalt: "private-key", newAccountActionDelay: 24 * time.Hour}

	plan, err := planWorkspaceInviteQuota(input, actor, workspace, quota, inviteQuotaActivityFacts{}, inviteAbuseFacts{valid: true}, config, now)
	if err != nil || plan.reason != "new_account_action_delay" || plan.retryAfterSeconds != int32(23*time.Hour/time.Second) {
		t.Fatalf("new-account plan = %+v, err %v", plan, err)
	}
	quota.known = false
	plan, err = planWorkspaceInviteQuota(input, actor, workspace, quota, inviteQuotaActivityFacts{}, inviteAbuseFacts{valid: true}, config, now)
	if err != nil || plan.reason != "quota_state_unavailable" {
		t.Fatalf("missing quota plan = %+v, err %v", plan, err)
	}
	quota.known = true
	abuse := inviteAbuseFacts{valid: false}
	plan, err = planWorkspaceInviteQuota(input, actor, workspace, quota, inviteQuotaActivityFacts{}, abuse, config, now)
	if err != nil || plan.reason != "policy_state_invalid" {
		t.Fatalf("invalid abuse state plan = %+v, err %v", plan, err)
	}
}

func TestPlanInviteQuotaSettlementUsesActualDomainCounts(t *testing.T) {
	keys := []string{
		"invite:user:actor-1",
		"invite:target_domain_global:example.com",
		"invite:workspace_domain:workspace-1:example.com",
		"invite:target_domain_global:other.test",
	}
	usage, err := planInviteQuotaSettlement(keys, 3, []inviteQuotaTargetDomain{{Domain: "example.com", Count: 2}, {Domain: "other.test", Count: 1}})
	if err != nil {
		t.Fatal(err)
	}
	want := []int32{3, 2, 2, 1}
	for index := range want {
		if usage[index] != want[index] {
			t.Errorf("scope usage %d = %d, want %d", index, usage[index], want[index])
		}
	}
	if _, err := planInviteQuotaSettlement(keys, 2, []inviteQuotaTargetDomain{{Domain: "example.com", Count: 1}}); err == nil {
		t.Fatal("inconsistent target count was accepted")
	}
}
