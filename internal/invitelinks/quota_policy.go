// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
)

const workspaceInviteQuotaPurpose = "workspace_invite"

type workspaceInviteQuotaInput struct {
	ActorUserID   string
	WorkspaceID   string
	RequestID     string
	TargetCount   int32
	TargetDomains []inviteQuotaTargetDomain
	Source        *inviteQuotaSourceFacts
}

type inviteQuotaTargetDomain struct {
	Domain string
	Count  int32
}

type inviteQuotaSourceFacts struct {
	Trusted bool
	IP      string
	ASN     uint32
}

type inviteQuotaWorkspaceFacts struct {
	createdAt time.Time
}

type inviteQuotaActivityFacts struct {
	actorCreated7d      int32
	actorAccepted7d     int32
	workspacePending    int32
	workspaceCreated7d  int32
	workspaceAccepted7d int32
}

type inviteQuotaPolicyConfig struct {
	deployment            entitlements.Deployment
	newAccountActionDelay time.Duration
	subjectHashSalt       string
}

type inviteQuotaScope struct {
	key           string
	windowSeconds int32
	bucketSeconds int64
	limit         int32
	requested     int32
}

type inviteQuotaDecision string

const (
	inviteQuotaAllow   inviteQuotaDecision = "allow"
	inviteQuotaReserve inviteQuotaDecision = "reserve"
	inviteQuotaDeny    inviteQuotaDecision = "deny"
)

type inviteQuotaAbusePersistencePlan struct {
	kind             string
	key              string
	status           string
	action           string
	reason           string
	subjectUserID    string
	actorEmailHash   string
	actorDomain      string
	sourcePrefixHash string
	sourceASN        *int64
	targetDomains    []inviteQuotaTargetDomain
	requested        int32
}

type workspaceInviteQuotaPlan struct {
	decision          inviteQuotaDecision
	reason            string
	scopeKey          string
	retryAfterSeconds int32
	scopes            []inviteQuotaScope
	persistence       *inviteQuotaAbusePersistencePlan
}

func planWorkspaceInviteQuota(
	input workspaceInviteQuotaInput,
	actor inviteActorFacts,
	workspace inviteQuotaWorkspaceFacts,
	quota inviteQuotaFacts,
	activity inviteQuotaActivityFacts,
	abuse inviteAbuseFacts,
	config inviteQuotaPolicyConfig,
	now time.Time,
) (workspaceInviteQuotaPlan, error) {
	if strings.TrimSpace(config.subjectHashSalt) == "" {
		return workspaceInviteQuotaPlan{}, errors.New("invite subject hash salt is required")
	}
	domains, err := validateInviteQuotaInput(input)
	if err != nil {
		return workspaceInviteQuotaPlan{}, err
	}
	_, ok := actorEmailDomain(actor.email)
	if !ok {
		return workspaceInviteQuotaPlan{}, errors.New("invalid actor email")
	}
	if !abuse.valid {
		return denyInviteQuota("policy_state_invalid", "", 0), nil
	}

	actorSubject := actorEmailSubjectKey(actor.email, config.subjectHashSalt)
	for _, subject := range abuse.subjects {
		if subject.kind == "actor_email" && subject.key == actorSubject {
			return denyInviteQuota("abuse_subject", "invite:actor_subject:"+actorSubject, 0), nil
		}
		if subject.kind == "workspace" && subject.key == workspaceSubjectKey(input.WorkspaceID) && subject.status == "quarantined" {
			return denyInviteQuota("abuse_workspace", "invite:workspace_subject:"+subject.key, 0), nil
		}
	}
	if prefix, exists := inviteQuotaSourcePrefix(input.Source); exists {
		for _, target := range domains {
			key := sourceCohortSubjectKey(prefix, target.Domain)
			for _, subject := range abuse.subjects {
				if subject.kind == "source_prefix_domain" && subject.key == key && subject.status == "quarantined" {
					return denyInviteQuota("abuse_source_cohort", "invite:source_cohort_subject:"+key, 0), nil
				}
			}
		}
	}
	if !quota.known {
		return denyInviteQuota("quota_state_unavailable", "", 0), nil
	}
	if quota.stale || quota.staleAfter != nil && !quota.staleAfter.After(now) {
		return denyInviteQuota("quota_projection_stale", "", 0), nil
	}
	if retryAfter := inviteQuotaActionDelay(config, actor, quota, now); retryAfter > 0 {
		return denyInviteQuota("new_account_action_delay", "", retryAfter), nil
	}

	if persistence := evaluateInviteQuotaAbuse(input, actor, domains, config); persistence != nil {
		plan := denyInviteQuota(persistence.reason, inviteAbuseScopeKey(*persistence), 0)
		plan.persistence = persistence
		return plan, nil
	}

	scopes, err := buildWorkspaceInviteQuotaScopes(input, actor, workspace, quota, activity, config, domains, now)
	if err != nil {
		return workspaceInviteQuotaPlan{}, err
	}
	return workspaceInviteQuotaPlan{decision: inviteQuotaReserve, scopes: scopes}, nil
}

func denyInviteQuota(reason, scopeKey string, retryAfter int32) workspaceInviteQuotaPlan {
	return workspaceInviteQuotaPlan{decision: inviteQuotaDeny, reason: reason, scopeKey: scopeKey, retryAfterSeconds: retryAfter}
}

func validateInviteQuotaInput(input workspaceInviteQuotaInput) ([]inviteQuotaTargetDomain, error) {
	if input.TargetCount <= 0 {
		return nil, errors.New("invite target count must be positive")
	}
	if len(input.TargetDomains) == 0 {
		return nil, errors.New("invite target domains are required")
	}
	aggregated := make(map[string]int32, len(input.TargetDomains))
	var total int64
	for _, target := range input.TargetDomains {
		if target.Count <= 0 {
			return nil, errors.New("invite target domain count must be positive")
		}
		domain := normalizeDomain(target.Domain)
		if !validDomain(domain) {
			return nil, errors.New("invalid invite target domain")
		}
		count := int64(aggregated[domain]) + int64(target.Count)
		if count > math.MaxInt32 {
			return nil, errors.New("invite target count overflow")
		}
		aggregated[domain] = int32(count)
		total += int64(target.Count)
	}
	if total != int64(input.TargetCount) {
		return nil, errors.New("invite target domain counts do not match target count")
	}
	domains := make([]inviteQuotaTargetDomain, 0, len(aggregated))
	for domain, count := range aggregated {
		domains = append(domains, inviteQuotaTargetDomain{Domain: domain, Count: count})
	}
	sort.Slice(domains, func(i, j int) bool { return domains[i].Domain < domains[j].Domain })
	return domains, nil
}

func validDomain(domain string) bool {
	if domain == "" || !strings.Contains(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func actorEmailDomain(email string) (string, bool) {
	if !validActorEmailDomain(email) {
		return "", false
	}
	_, domain, _ := strings.Cut(strings.TrimSpace(email), "@")
	return normalizeDomain(domain), true
}

func inviteQuotaActionDelay(config inviteQuotaPolicyConfig, actor inviteActorFacts, quota inviteQuotaFacts, now time.Time) int32 {
	if config.deployment == entitlements.SelfHosted || config.newAccountActionDelay <= 0 ||
		individualDelayExempt(actor.plan) || workspaceDelayExempt(quota.plan) {
		return 0
	}
	remaining := int64(config.newAccountActionDelay/time.Second) - int64(now.Sub(actor.createdAt)/time.Second)
	if remaining <= 0 {
		return 0
	}
	if remaining > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(remaining)
}

func buildWorkspaceInviteQuotaScopes(
	input workspaceInviteQuotaInput,
	actor inviteActorFacts,
	workspace inviteQuotaWorkspaceFacts,
	quota inviteQuotaFacts,
	activity inviteQuotaActivityFacts,
	config inviteQuotaPolicyConfig,
	domains []inviteQuotaTargetDomain,
	now time.Time,
) ([]inviteQuotaScope, error) {
	single, perHour, perDay, perWeek, err := baseWorkspaceInviteLimits(now, actor, workspace, quota, activity)
	if err != nil {
		return nil, err
	}
	if input.TargetCount > single {
		return []inviteQuotaScope{newInviteQuotaScope("invite:single_request:"+input.ActorUserID, 60, single, input.TargetCount)}, nil
	}
	quotaSubject, seatLimit := inviteQuotaSubject(input.WorkspaceID, quota)
	quotaWeek, err := inviteQuotaWeeklyCeiling(quota.plan, seatLimit)
	if err != nil {
		return nil, err
	}
	seatWeek, err := checkedMultiplyInviteCount(seatLimit, 2)
	if err != nil {
		return nil, err
	}
	quotaWeek = minInt32(quotaWeek, seatWeek)
	actorSubject := actorEmailSubjectKey(actor.email, config.subjectHashSalt)
	scopes := []inviteQuotaScope{
		newInviteQuotaScope("invite:user:"+input.ActorUserID, 3600, perHour, input.TargetCount),
		newInviteQuotaScope("invite:user:"+input.ActorUserID, 86400, perDay, input.TargetCount),
		newInviteQuotaScope("invite:user:"+input.ActorUserID, 604800, perWeek, input.TargetCount),
		newInviteQuotaScope("invite:actor_subject:"+actorSubject, 604800, perWeek, input.TargetCount),
		newInviteQuotaScope("invite:workspace:"+input.WorkspaceID, 3600, maxInt32(perHour, 5), input.TargetCount),
		newInviteQuotaScope("invite:workspace:"+input.WorkspaceID, 86400, maxInt32(perDay, 10), input.TargetCount),
		newInviteQuotaScope("invite:quota_subject:"+quotaSubject, 604800, quotaWeek, input.TargetCount),
	}
	for _, target := range domains {
		domain := normalizeDomain(target.Domain)
		limitWeek := quotaWeek
		if highRiskInviteDomain(domain) {
			limitWeek = minInt32(limitWeek, maxInt32(seatLimit/2, 2))
		}
		scopes = append(scopes,
			newInviteQuotaScope("invite:user_domain:"+input.ActorUserID+":"+domain, 86400, maxInt32(minInt32(perDay, limitWeek), 1), target.Count),
			newInviteQuotaScope("invite:workspace_domain:"+input.WorkspaceID+":"+domain, 86400, maxInt32(minInt32(maxInt32(perDay, 10), limitWeek), 1), target.Count),
			newInviteQuotaScope("invite:quota_subject_domain:"+quotaSubject+":"+domain, 604800, limitWeek, target.Count),
		)
		globalHour, globalDay := int32(100), int32(2000)
		if highRiskInviteDomain(domain) {
			globalHour, globalDay = 30, 500
		}
		scopes = append(scopes,
			newInviteQuotaScope("invite:target_domain_global:"+domain, 60, globalHour, target.Count),
			newInviteQuotaScope("invite:target_domain_global:"+domain, 86400, globalDay, target.Count),
		)
	}
	if prefix, exists := inviteQuotaSourcePrefix(input.Source); exists {
		scopes = append(scopes,
			newInviteQuotaScope("invite:source_prefix:"+prefix, 3600, 30, input.TargetCount),
			newInviteQuotaScope("invite:source_prefix:"+prefix, 86400, 100, input.TargetCount),
		)
		for _, target := range domains {
			hour, day := int32(15), int32(50)
			if highRiskInviteDomain(target.Domain) {
				hour, day = 5, 15
			}
			key := "invite:source_prefix_domain:" + prefix + ":" + normalizeDomain(target.Domain)
			scopes = append(scopes,
				newInviteQuotaScope(key, 3600, hour, target.Count),
				newInviteQuotaScope(key, 86400, day, target.Count),
			)
		}
	}
	if input.Source != nil && input.Source.Trusted && input.Source.ASN > 0 {
		for _, target := range domains {
			hour, day := int32(150), int32(500)
			if highRiskInviteDomain(target.Domain) {
				hour, day = 50, 150
			}
			key := "invite:source_asn_domain:" + strconv.FormatUint(uint64(input.Source.ASN), 10) + ":" + normalizeDomain(target.Domain)
			scopes = append(scopes,
				newInviteQuotaScope(key, 3600, hour, target.Count),
				newInviteQuotaScope(key, 86400, day, target.Count),
			)
		}
	}
	return scopes, nil
}

func baseWorkspaceInviteLimits(now time.Time, actor inviteActorFacts, workspace inviteQuotaWorkspaceFacts, quota inviteQuotaFacts, activity inviteQuotaActivityFacts) (int32, int32, int32, int32, error) {
	if quota.seatLimit < 0 || quota.memberCount < 0 || activity.actorCreated7d < 0 || activity.actorAccepted7d < 0 ||
		activity.workspacePending < 0 || activity.workspaceCreated7d < 0 || activity.workspaceAccepted7d < 0 {
		return 0, 0, 0, 0, errors.New("invite quota facts contain negative counts")
	}
	if actor.disabled || !actor.registered {
		return 0, 0, 0, 0, nil
	}
	single, perHour, perDay, perWeek := int32(5), int32(5), int32(15), int32(30)
	accountAge := now.Sub(actor.createdAt)
	workspaceAge := now.Sub(workspace.createdAt)
	switch {
	case !actor.emailVerified:
		single, perHour, perDay, perWeek = 1, 1, 3, 5
	case accountAge < 7*24*time.Hour:
		single, perHour, perDay, perWeek = 3, 3, 5, 10
	case accountAge < 30*24*time.Hour:
		single, perHour, perDay, perWeek = 5, 5, 15, 30
	default:
		switch {
		case isPaidInviteTeam(quota.plan):
			single, perHour, perDay = minInt32(quota.seatLimit, 20), 20, 50
		case isProInvitePlan(quota.plan):
			single, perHour, perDay, perWeek = 8, 8, 20, 50
		}
	}
	if workspaceAge < 24*time.Hour || quota.memberCount <= 1 {
		single = minInt32(single, 3)
		perDay = minInt32(perDay, 10)
	} else if workspaceAge < 7*24*time.Hour || quota.memberCount <= 5 {
		single = maxInt32(ceilHalfInviteCount(single), 1)
		perHour = maxInt32(ceilHalfInviteCount(perHour), 1)
		perDay = maxInt32(ceilHalfInviteCount(perDay), 1)
	}
	seatPendingLimit, err := checkedMultiplyInviteCount(quota.seatLimit, 2)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if activity.workspacePending > maxInt32(seatPendingLimit, 6) {
		single = minInt32(single, 2)
		perDay = minInt32(perDay, 5)
	}
	actorAcceptedThreshold, err := checkedMultiplyInviteCount(activity.actorAccepted7d, 10)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if activity.actorCreated7d >= 5 && actorAcceptedThreshold < activity.actorCreated7d {
		single, perHour, perDay, perWeek = minInt32(single, 2), minInt32(perHour, 2), minInt32(perDay, 5), minInt32(perWeek, 10)
	}
	workspaceAcceptedThreshold, err := checkedMultiplyInviteCount(activity.workspaceAccepted7d, 10)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if activity.workspaceCreated7d >= 10 && workspaceAcceptedThreshold < activity.workspaceCreated7d {
		perDay, perWeek = minInt32(perDay, 10), minInt32(perWeek, 20)
	}
	seatWeek, err := inviteQuotaWeeklyCeiling(quota.plan, quota.seatLimit)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	seatWeek, err = checkedMultiplyBoundedMin(seatWeek, quota.seatLimit, 2)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	perWeek = minInt32(perWeek, seatWeek)
	return single, perHour, perDay, perWeek, nil
}

func evaluateInviteQuotaAbuse(input workspaceInviteQuotaInput, actor inviteActorFacts, domains []inviteQuotaTargetDomain, config inviteQuotaPolicyConfig) *inviteQuotaAbusePersistencePlan {
	var highRiskCount int64
	var highRiskDomains []inviteQuotaTargetDomain
	for _, target := range domains {
		if highRiskInviteDomain(target.Domain) {
			highRiskCount += int64(target.Count)
			highRiskDomains = append(highRiskDomains, target)
		}
	}
	if highRiskCount > math.MaxInt32 {
		return nil
	}
	count := int32(highRiskCount)
	var reason, action, kind, key, status string
	prefix, hasSourcePrefix := inviteQuotaSourcePrefix(input.Source)
	switch {
	case !actor.emailVerified && count >= 3:
		reason, action, kind, key, status = "unverified_high_risk_domain_burst", "quarantine_actor", "actor_email", actorEmailSubjectKey(actor.email, config.subjectHashSalt), "quarantined"
	case input.TargetCount >= 30 && int64(count)*2 >= int64(input.TargetCount):
		reason, action, kind, key, status = "workspace_high_risk_domain_burst", "quarantine_workspace", "workspace", workspaceSubjectKey(input.WorkspaceID), "quarantined"
	case hasSourcePrefix && count >= 10 && int64(count)*2 >= int64(input.TargetCount):
		domain := highestRiskInviteDomain(highRiskDomains)
		reason, action, kind, key, status = "source_high_risk_domain_burst", "quarantine_source_cohort", "source_prefix_domain", sourceCohortSubjectKey(prefix, domain), "quarantined"
	case input.TargetCount >= 10 && int64(count)*2 >= int64(input.TargetCount):
		reason, kind, key, status = "high_risk_domain_burst", "actor_email", actorEmailSubjectKey(actor.email, config.subjectHashSalt), "quarantined"
		if input.TargetCount >= 20 {
			action, status = "ban_actor", "banned"
		} else {
			action = "quarantine_actor"
		}
	default:
		return nil
	}
	actorDomain, _ := actorEmailDomain(actor.email)
	plan := &inviteQuotaAbusePersistencePlan{
		kind: kind, key: key, status: status, action: action, reason: reason,
		actorEmailHash: actorEmailSubjectKey(actor.email, config.subjectHashSalt), actorDomain: actorDomain,
		targetDomains: append([]inviteQuotaTargetDomain(nil), domains...), requested: input.TargetCount,
	}
	if kind == "actor_email" {
		plan.subjectUserID = input.ActorUserID
	}
	if prefix, exists := inviteQuotaSourcePrefix(input.Source); exists {
		hash := shortInviteQuotaHash(prefix)
		plan.sourcePrefixHash = hash
	}
	if input.Source != nil && input.Source.Trusted && input.Source.ASN > 0 {
		asn := int64(input.Source.ASN)
		plan.sourceASN = &asn
	}
	return plan
}

func inviteAbuseScopeKey(plan inviteQuotaAbusePersistencePlan) string {
	switch plan.kind {
	case "actor_email":
		return "invite:actor_subject:" + plan.key
	case "workspace":
		return "invite:workspace_subject:" + plan.key
	default:
		return "invite:source_cohort_subject:" + plan.key
	}
}

func highestRiskInviteDomain(domains []inviteQuotaTargetDomain) string {
	if len(domains) == 0 {
		return ""
	}
	sort.Slice(domains, func(i, j int) bool {
		if domains[i].Count == domains[j].Count {
			return domains[i].Domain < domains[j].Domain
		}
		return domains[i].Count > domains[j].Count
	})
	return normalizeDomain(domains[0].Domain)
}

func inviteQuotaSourcePrefix(source *inviteQuotaSourceFacts) (string, bool) {
	if source == nil || !source.Trusted || source.IP == "" {
		return "", false
	}
	address, err := netip.ParseAddr(source.IP)
	if err != nil || address.Zone() != "" {
		return "", false
	}
	address = address.Unmap()
	bits := 48
	if address.Is4() {
		bits = 24
	}
	return netip.PrefixFrom(address, bits).Masked().String(), true
}

func sourceCohortSubjectKey(prefix, domain string) string {
	return "source_prefix_domain_sha256:v1:" + shortInviteQuotaHash(prefix+":"+normalizeDomain(domain))
}

func shortInviteQuotaHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:24]
}

func highRiskInviteDomain(domain string) bool {
	switch normalizeDomain(domain) {
	case "qq.com", "proton.me", "protonmail.com", "163.com", "126.com", "outlook.com", "hotmail.com":
		return true
	default:
		return false
	}
}

func inviteQuotaSubject(workspaceID string, quota inviteQuotaFacts) (string, int32) {
	if quota.usesOwnerQuota || !isPaidInviteTeam(quota.plan) {
		ownerID := quota.ownerUserID
		if ownerID == "" {
			ownerID = workspaceID
		}
		return "owner:" + ownerID, quota.seatLimit
	}
	return "workspace:" + workspaceID, quota.seatLimit
}

func inviteQuotaWeeklyCeiling(plan string, seatLimit int32) (int32, error) {
	if seatLimit < 0 {
		return 0, errors.New("invite quota seat limit is negative")
	}
	if isPaidInviteTeam(plan) {
		return checkedMultiplyInviteCount(seatLimit, 2)
	}
	if isProInvitePlan(plan) {
		return 20, nil
	}
	return 10, nil
}

func isPaidInviteTeam(plan string) bool { return oneOf(plan, "team", "selfhost_team") }
func isProInvitePlan(plan string) bool  { return oneOf(plan, "pro", "lifetime_pro") }

func newInviteQuotaScope(key string, windowSeconds int32, limit, requested int32) inviteQuotaScope {
	bucketSeconds := int64(60)
	switch windowSeconds {
	case 60:
		bucketSeconds = 10
	case 3600:
		bucketSeconds = 300
	case 86400:
		bucketSeconds = 3600
	case 604800:
		bucketSeconds = 21600
	}
	return inviteQuotaScope{key: key, windowSeconds: windowSeconds, bucketSeconds: bucketSeconds, limit: limit, requested: requested}
}

func planInviteQuotaSettlement(scopeKeys []string, targetCount int32, targetDomains []inviteQuotaTargetDomain) ([]int32, error) {
	if targetCount < 0 {
		return nil, errors.New("invite settlement count is negative")
	}
	domainUsage := make(map[string]int32, len(targetDomains))
	var total int64
	for _, target := range targetDomains {
		if target.Count < 0 {
			return nil, errors.New("invite settlement domain count is negative")
		}
		domain := normalizeDomain(target.Domain)
		count := int64(domainUsage[domain]) + int64(target.Count)
		if count > math.MaxInt32 {
			return nil, errors.New("invite settlement domain count overflow")
		}
		domainUsage[domain] = int32(count)
		total += int64(target.Count)
	}
	if total != int64(targetCount) {
		return nil, errors.New("invite settlement domain counts do not match target count")
	}
	usage := make([]int32, 0, len(scopeKeys))
	for _, key := range scopeKeys {
		domain, isDomainScope, ok := inviteQuotaScopeDomain(key)
		if !ok {
			return nil, fmt.Errorf("invalid invite quota scope identity %q", key)
		}
		if !isDomainScope {
			usage = append(usage, targetCount)
			continue
		}
		count, found := domainUsage[domain]
		if !found {
			return nil, fmt.Errorf("invite quota scope domain %q was not requested", domain)
		}
		usage = append(usage, count)
	}
	return usage, nil
}

func inviteQuotaScopeDomain(key string) (domain string, isDomainScope, ok bool) {
	plainPrefixes := []string{
		"invite:single_request:", "invite:user:", "invite:workspace:", "invite:quota_subject:owner:",
		"invite:quota_subject:workspace:", "invite:source_prefix:",
	}
	for _, prefix := range plainPrefixes {
		if identity, found := strings.CutPrefix(key, prefix); found && identity != "" {
			return "", false, true
		}
	}
	if identity, found := strings.CutPrefix(key, "invite:actor_subject:"); found && validHashIdentity(identity, "actor_email_sha256:v1:", 64) {
		return "", false, true
	}
	if value, found := strings.CutPrefix(key, "invite:target_domain_global:"); found && value != "" {
		return value, true, true
	}
	for _, prefix := range []string{
		"invite:user_domain:", "invite:workspace_domain:", "invite:quota_subject_domain:owner:",
		"invite:quota_subject_domain:workspace:", "invite:source_prefix_domain:", "invite:source_asn_domain:",
	} {
		identity, found := strings.CutPrefix(key, prefix)
		if !found || !strings.Contains(identity, ":") {
			continue
		}
		domain := identity[strings.LastIndex(identity, ":")+1:]
		if domain == "" {
			return "", false, false
		}
		return domain, true, true
	}
	return "", false, false
}

func checkedMultiplyInviteCount(left, right int32) (int32, error) {
	value := int64(left) * int64(right)
	if value < math.MinInt32 || value > math.MaxInt32 {
		return 0, errors.New("invite quota arithmetic overflow")
	}
	return int32(value), nil
}

func checkedMultiplyBoundedMin(value, left, right int32) (int32, error) {
	product, err := checkedMultiplyInviteCount(left, right)
	if err != nil {
		return 0, err
	}
	return minInt32(value, product), nil
}

func ceilHalfInviteCount(value int32) int32 { return value/2 + value%2 }

func minInt32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
