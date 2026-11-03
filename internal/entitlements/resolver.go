// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package entitlements

import "time"

type Deployment string

const (
	Cloud      Deployment = "cloud"
	SelfHosted Deployment = "selfhosted"
)

type Limits struct {
	BlobLimit            int64
	SeatLimit            int32
	StorageQuota         int64
	HistoryPeriodSeconds int64
	CopilotActionLimit   *int32
}

const mebibyte int64 = 1024 * 1024

type Grant struct {
	Plan       string
	Commercial bool
	Limits     Limits
	Rights     Rights
	ExpiresAt  *time.Time
}

type Rights struct {
	CopilotBYOK      bool
	UnlimitedCopilot bool
}

type Fact struct {
	Source        string
	Status        string
	Plan          string
	Quantity      *int32
	StartsAt      *time.Time
	ExpiresAt     *time.Time
	GraceUntil    *time.Time
	SignedPayload []byte
}

type Context struct {
	Deployment  Deployment
	TargetType  string
	WorkspaceID string
	Now         time.Time
}

type candidate struct {
	statusPriority int
	planPriority   int
	grant          Grant
}

func ParseDeployment(value string) Deployment {
	if value == "cloud" || value == "affine" {
		return Cloud
	}
	return SelfHosted
}

func Resolve(ctx Context, facts []Fact) Grant {
	grant := fallback(ctx.Deployment)
	var best *candidate
	unlimitedCopilot := false
	for _, fact := range facts {
		statusPriority, active := activeStatus(fact, ctx.Now)
		if !active || (fact.Quantity != nil && (*fact.Quantity < 1 || *fact.Quantity > 100000)) {
			continue
		}
		resolved, planPriority, ok := resolveCandidate(ctx, fact)
		if !ok {
			continue
		}
		if resolved.Plan == "ai" {
			unlimitedCopilot = true
			continue
		}
		if best == nil || statusPriority > best.statusPriority || statusPriority == best.statusPriority && planPriority > best.planPriority {
			best = &candidate{statusPriority: statusPriority, planPriority: planPriority, grant: resolved}
		}
	}
	if best != nil {
		grant = best.grant
	}
	if unlimitedCopilot {
		grant.Rights.CopilotBYOK = true
		grant.Rights.UnlimitedCopilot = true
		grant.Limits.CopilotActionLimit = nil
	}
	return grant
}

func activeStatus(fact Fact, now time.Time) (int, bool) {
	if fact.StartsAt != nil && fact.StartsAt.After(now) {
		return 0, false
	}
	switch fact.Status {
	case "active":
		if fact.ExpiresAt != nil && !now.Before(*fact.ExpiresAt) {
			return 0, false
		}
		return 2, true
	case "grace":
		if fact.GraceUntil == nil || !now.Before(*fact.GraceUntil) {
			return 0, false
		}
		return 1, true
	default:
		return 0, false
	}
}

func resolveCandidate(ctx Context, fact Fact) (Grant, int, bool) {
	if ctx.Deployment == SelfHosted {
		// Self-hosted deployments resolve every target to the selfhost_free
		// fallback. Signed self-hosted license facts are ignored.
		return Grant{}, 0, false
	}
	if fact.Source != "cloud_subscription" && fact.Source != "admin_grant" {
		return Grant{}, 0, false
	}
	switch ctx.TargetType {
	case "workspace":
		if fact.Plan == "free" {
			return planGrant(fact.Plan, 0, fact.ExpiresAt), 0, true
		}
		if fact.Plan == "team" {
			return planGrant(fact.Plan, quantityOrDefault(fact.Quantity), fact.ExpiresAt), 4, true
		}
	case "user":
		switch fact.Plan {
		case "free":
			return planGrant(fact.Plan, 0, fact.ExpiresAt), 0, true
		case "pro":
			return planGrant(fact.Plan, 0, fact.ExpiresAt), 2, true
		case "lifetime_pro":
			return planGrant(fact.Plan, 0, fact.ExpiresAt), 3, true
		case "ai":
			return planGrant(fact.Plan, 0, fact.ExpiresAt), 1, true
		}
	case "instance":
		if fact.Plan == "free" {
			return planGrant(fact.Plan, 0, fact.ExpiresAt), 0, true
		}
	}
	return Grant{}, 0, false
}

func fallback(deployment Deployment) Grant {
	if deployment == Cloud {
		return planGrant("free", 0, nil)
	}
	return planGrant("selfhost_free", 0, nil)
}

func quantityOrDefault(quantity *int32) int32 {
	if quantity == nil {
		return 1
	}
	return *quantity
}

func planGrant(plan string, quantity int32, expiresAt *time.Time) Grant {
	limits := Limits{}
	commercial := false
	byok := false
	var copilotLimit *int32
	defaultCopilotLimit := int32(10)
	switch plan {
	case "pro":
		limits = Limits{BlobLimit: 100 * mebibyte, SeatLimit: 10, StorageQuota: 100 * 1024 * 1024 * 1024, HistoryPeriodSeconds: 30 * 24 * 60 * 60}
		byok = true
	case "lifetime_pro":
		limits = Limits{BlobLimit: 100 * mebibyte, SeatLimit: 10, StorageQuota: 1024 * 1024 * 1024 * 1024, HistoryPeriodSeconds: 30 * 24 * 60 * 60}
		byok = true
	case "team", "selfhost_team":
		if quantity < 1 {
			quantity = 1
		}
		limits = Limits{BlobLimit: 500 * mebibyte, SeatLimit: quantity, StorageQuota: (100 + int64(quantity)*20) * 1024 * 1024 * 1024, HistoryPeriodSeconds: 30 * 24 * 60 * 60}
		commercial = true
		byok = true
	case "ai":
		limits = Limits{BlobLimit: 10 * mebibyte, SeatLimit: 3, StorageQuota: 10 * 1024 * 1024 * 1024, HistoryPeriodSeconds: 7 * 24 * 60 * 60}
		byok = true
	case "selfhost_free":
		limits = Limits{BlobLimit: 100 * mebibyte, SeatLimit: 10, StorageQuota: 100 * 1024 * 1024 * 1024, HistoryPeriodSeconds: 30 * 24 * 60 * 60}
	default:
		limits = Limits{BlobLimit: 10 * mebibyte, SeatLimit: 3, StorageQuota: 10 * 1024 * 1024 * 1024, HistoryPeriodSeconds: 7 * 24 * 60 * 60}
	}
	if plan != "team" && plan != "selfhost_team" {
		copilotLimit = &defaultCopilotLimit
	}
	limits.CopilotActionLimit = copilotLimit
	return Grant{
		Plan: plan, Commercial: commercial, Limits: limits,
		Rights: Rights{CopilotBYOK: byok, UnlimitedCopilot: plan == "ai"}, ExpiresAt: expiresAt,
	}
}
