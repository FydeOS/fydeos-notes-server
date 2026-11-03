// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const defaultNewAccountActionDelay = 24 * time.Hour

type Database interface {
	Begin(context.Context) (pgx.Tx, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type invitePolicyGate struct {
	db           Database
	config       config.Config
	bootstrapKey string
	deployment   entitlements.Deployment
}

type inviteActorFacts struct {
	userID        string
	email         string
	createdAt     time.Time
	registered    bool
	emailVerified bool
	disabled      bool
	plan          string
}

type inviteQuotaFacts struct {
	known          bool
	plan           string
	ownerUserID    string
	usesOwnerQuota bool
	seatLimit      int32
	memberCount    int32
	stale          bool
	staleAfter     *time.Time
}

type inviteAbuseSubject struct {
	kind   string
	key    string
	status string
}

type inviteAbuseFacts struct {
	valid    bool
	subjects []inviteAbuseSubject
}

type abuseRow struct {
	key                    string
	kind                   string
	status                 string
	action                 pgtype.Text
	reason                 pgtype.Text
	userID                 pgtype.Text
	actorEmailHash         pgtype.Text
	emailDomain            pgtype.Text
	evidenceSubjectKey     pgtype.Text
	evidenceWorkspaceID    pgtype.Text
	evidenceUserID         pgtype.Text
	evidenceActorEmailHash pgtype.Text
	evidenceSourceHash     pgtype.Text
	evidenceSourceASN      pgtype.Int8
	evidenceTargetDomains  []byte
	evidenceCounters       []byte
	evidenceAction         pgtype.Text
	evidenceReason         pgtype.Text
	actionAction           pgtype.Text
}

func (g *invitePolicyGate) Evaluate(ctx context.Context, actorID, workspaceID string) (bool, string, error) {
	if g == nil || g.db == nil {
		return false, "", errors.New("invite-link policy database is unavailable")
	}
	tx, err := g.db.Begin(ctx)
	if err != nil {
		return false, "", fmt.Errorf("begin invite-link policy snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return false, "", fmt.Errorf("read invite-link policy clock: %w", err)
	}
	var actor inviteActorFacts
	actor.userID = actorID
	if err := tx.QueryRow(ctx, `
		SELECT email, created_at, registered, email_verified IS NOT NULL, disabled
		FROM users WHERE id=$1`, actorID).Scan(&actor.email, &actor.createdAt, &actor.registered, &actor.emailVerified, &actor.disabled); err != nil {
		return false, "", fmt.Errorf("load invite-link actor facts: %w", err)
	}
	var workspaceName string
	if err := tx.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=$1`, workspaceID).Scan(&workspaceName); err != nil {
		return false, "", fmt.Errorf("load invite-link workspace: %w", err)
	}
	privateKey, err := auth.LoadAppPrivateKey(ctx, tx, g.config, g.bootstrapKey)
	if err != nil {
		return false, "", fmt.Errorf("load invite-link policy key: %w", err)
	}
	if strings.TrimSpace(privateKey) == "" {
		return false, workspaceName, nil
	}
	delay, err := loadNewAccountActionDelay(ctx, tx, g.config.RuntimeConfigPath)
	if err != nil {
		return false, "", err
	}
	var workspaceGrant, ownerGrant entitlements.Grant
	var quota inviteQuotaFacts
	var ownerID string
	err = tx.QueryRow(ctx, `
		SELECT user_id FROM workspace_members
		WHERE workspace_id=$1 AND role='owner' AND state='active' LIMIT 1`, workspaceID).Scan(&ownerID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("load invite-link quota owner: %w", err)
	}
	if err == nil {
		workspaceGrant, err = loadEntitlementGrant(ctx, tx, g.deployment, "workspace", workspaceID, workspaceID, now)
		if err != nil {
			return false, "", err
		}
		ownerGrant, err = loadEntitlementGrant(ctx, tx, g.deployment, "user", ownerID, workspaceID, now)
		if err != nil {
			return false, "", err
		}
		quota = inviteQuotaFacts{known: true, plan: ownerGrant.Plan}
		if workspaceGrant.Commercial {
			quota.plan = workspaceGrant.Plan
		}
	} else {
		quota = inviteQuotaFacts{known: false}
	}
	actorGrant, err := loadEntitlementGrant(ctx, tx, g.deployment, "user", actorID, workspaceID, now)
	if err != nil {
		return false, "", err
	}
	actor.plan = actorGrant.Plan
	abuse, err := loadInviteAbuseFacts(ctx, tx)
	if err != nil {
		return false, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, "", fmt.Errorf("commit invite-link policy snapshot: %w", err)
	}
	allowed, _ := inviteLinkPolicyDecision(g.deployment, privateKey, delay, now, actor, workspaceID, quota, abuse)
	return allowed, workspaceName, nil
}

func loadNewAccountActionDelay(ctx context.Context, db config.RuntimeConfigReader, path string) (time.Duration, error) {
	raw, found, err := config.ReadRuntimeValue(ctx, db, path, "auth.newAccountActionDelay")
	if err != nil {
		return 0, err
	}
	if !found {
		return defaultNewAccountActionDelay, nil
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || seconds < 0 || seconds > int64((time.Duration(1<<63-1))/time.Second) {
		return 0, errors.New("invalid auth.newAccountActionDelay")
	}
	return time.Duration(seconds) * time.Second, nil
}

func loadEntitlementGrant(ctx context.Context, tx pgx.Tx, deployment entitlements.Deployment, targetType, targetID, workspaceID string, now time.Time) (entitlements.Grant, error) {
	rows, err := tx.Query(ctx, `
		SELECT source, status, plan, quantity, starts_at, expires_at, grace_until, signed_payload
		FROM entitlements WHERE target_type=$1 AND target_id=$2 ORDER BY updated_at DESC`, targetType, targetID)
	if err != nil {
		return entitlements.Grant{}, fmt.Errorf("load %s entitlement facts: %w", targetType, err)
	}
	facts := make([]entitlements.Fact, 0)
	for rows.Next() {
		var fact entitlements.Fact
		var source, status, plan pgtype.Text
		var quantity pgtype.Int4
		var startsAt, expiresAt, graceUntil pgtype.Timestamptz
		var payload []byte
		if err := rows.Scan(&source, &status, &plan, &quantity, &startsAt, &expiresAt, &graceUntil, &payload); err != nil {
			rows.Close()
			return entitlements.Grant{}, err
		}
		fact.Source, fact.Status, fact.Plan = source.String, status.String, plan.String
		if quantity.Valid {
			value := quantity.Int32
			fact.Quantity = &value
		}
		fact.StartsAt, fact.ExpiresAt, fact.GraceUntil = nullableTime(startsAt), nullableTime(expiresAt), nullableTime(graceUntil)
		fact.SignedPayload = payload
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return entitlements.Grant{}, err
	}
	rows.Close()
	return entitlements.Resolve(entitlements.Context{
		Deployment: deployment, TargetType: targetType, WorkspaceID: workspaceID, Now: now,
	}, facts), nil
}

func nullableTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}

func loadInviteAbuseFacts(ctx context.Context, tx pgx.Tx) (inviteAbuseFacts, error) {
	rows, err := tx.Query(ctx, `
		SELECT subject.subject_key, subject.kind, subject.status, subject.action, subject.action_reason,
		       subject.user_id, subject.actor_email_hash, subject.email_domain,
		       evidence.subject_key, evidence.workspace_id, evidence.user_id, evidence.actor_email_hash,
		       evidence.source_prefix_hash, evidence.source_asn, evidence.target_domains, evidence.counters,
		       evidence.decision, evidence.reason
		FROM runtime_invite_abuse_subjects subject
		LEFT JOIN LATERAL (
		  SELECT evidence.* FROM runtime_invite_abuse_evidence evidence
		  WHERE evidence.subject_key=subject.subject_key ORDER BY evidence.id DESC LIMIT 1
		) evidence ON true
		WHERE subject.status <> 'active' OR subject.kind NOT IN ('actor_email','workspace','source_prefix_domain')
		ORDER BY subject.subject_key`)
	if err != nil {
		return inviteAbuseFacts{}, fmt.Errorf("load invite abuse policy facts: %w", err)
	}
	facts := inviteAbuseFacts{valid: true}
	for rows.Next() {
		var row abuseRow
		if err := rows.Scan(
			&row.key, &row.kind, &row.status, &row.action, &row.reason, &row.userID, &row.actorEmailHash,
			&row.emailDomain, &row.evidenceSubjectKey, &row.evidenceWorkspaceID, &row.evidenceUserID,
			&row.evidenceActorEmailHash, &row.evidenceSourceHash, &row.evidenceSourceASN,
			&row.evidenceTargetDomains, &row.evidenceCounters, &row.evidenceAction, &row.evidenceReason,
		); err != nil {
			rows.Close()
			return inviteAbuseFacts{}, err
		}
		if !validateAbuseRow(row) {
			facts.valid = false
			continue
		}
		facts.subjects = append(facts.subjects, inviteAbuseSubject{kind: row.kind, key: row.key, status: row.status})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return inviteAbuseFacts{}, err
	}
	rows.Close()
	return facts, nil
}

func validateAbuseRow(row abuseRow) bool {
	if !oneOf(row.kind, "actor_email", "workspace", "source_prefix_domain") || !oneOf(row.status, "quarantined", "banned") {
		return false
	}
	if !row.action.Valid || !row.reason.Valid || !row.evidenceAction.Valid || !row.evidenceReason.Valid || !row.evidenceSubjectKey.Valid ||
		row.evidenceAction.String != row.action.String || row.evidenceReason.String != row.reason.String || row.evidenceSubjectKey.String != row.key ||
		(row.actionAction.Valid && row.actionAction.String != row.action.String) ||
		(row.kind == "actor_email" && row.action.String != "quarantine_actor" && row.action.String != "ban_actor") ||
		(row.kind == "workspace" && row.action.String != "quarantine_workspace") ||
		(row.kind == "source_prefix_domain" && row.action.String != "quarantine_source_cohort") {
		return false
	}
	expectedStatus := "quarantined"
	switch {
	case row.kind == "actor_email" && row.action.String == "ban_actor" && row.reason.String == "high_risk_domain_burst":
		expectedStatus = "banned"
	case row.kind == "actor_email" && row.action.String == "quarantine_actor" && (row.reason.String == "unverified_high_risk_domain_burst" || row.reason.String == "high_risk_domain_burst"):
	case row.kind == "workspace" && row.action.String == "quarantine_workspace" && row.reason.String == "workspace_high_risk_domain_burst":
	case row.kind == "source_prefix_domain" && row.action.String == "quarantine_source_cohort" && row.reason.String == "source_high_risk_domain_burst":
	default:
		return false
	}
	if row.status != expectedStatus || !row.userID.Valid == (row.kind == "actor_email") {
		return false
	}
	if !row.evidenceUserID.Valid || row.evidenceUserID.String == "" || !row.evidenceWorkspaceID.Valid || row.evidenceWorkspaceID.String == "" ||
		!row.actorEmailHash.Valid || row.actorEmailHash.String != row.evidenceActorEmailHash.String ||
		!validHashIdentity(row.actorEmailHash.String, "actor_email_sha256:v1:", 64) || !row.emailDomain.Valid ||
		row.emailDomain.String != normalizeDomain(row.emailDomain.String) || !strings.Contains(row.emailDomain.String, ".") {
		return false
	}
	if row.kind == "actor_email" && (row.key != row.actorEmailHash.String || row.userID.String != row.evidenceUserID.String) {
		return false
	}
	if row.kind == "workspace" && row.key != workspaceSubjectKey(row.evidenceWorkspaceID.String) {
		return false
	}
	if row.kind == "source_prefix_domain" && (!validHashIdentity(row.key, "source_prefix_domain_sha256:v1:", 24) || !row.evidenceSourceHash.Valid || !validHashIdentity(row.evidenceSourceHash.String, "", 24)) {
		return false
	}
	if row.evidenceSourceHash.Valid && !validHashIdentity(row.evidenceSourceHash.String, "", 24) {
		return false
	}
	if row.evidenceSourceASN.Valid && (row.evidenceSourceASN.Int64 < 0 || row.evidenceSourceASN.Int64 > 1<<32-1) {
		return false
	}
	var counters map[string]json.RawMessage
	if json.Unmarshal(row.evidenceCounters, &counters) != nil || len(counters) != 1 {
		return false
	}
	var requested int64
	if json.Unmarshal(counters["requested"], &requested) != nil || requested < 1 || requested > 1<<31-1 {
		return false
	}
	var targets []map[string]json.RawMessage
	if json.Unmarshal(row.evidenceTargetDomains, &targets) != nil {
		return false
	}
	var total int64
	for _, target := range targets {
		var domain string
		var count int64
		if len(target) != 2 || json.Unmarshal(target["domain"], &domain) != nil || json.Unmarshal(target["count"], &count) != nil ||
			domain == "" || domain != normalizeDomain(domain) || count <= 0 || total > 1<<31-1-count {
			return false
		}
		total += count
	}
	return total == requested
}

func inviteLinkPolicyDecision(deployment entitlements.Deployment, salt string, delay time.Duration, now time.Time, actor inviteActorFacts, workspaceID string, quota inviteQuotaFacts, abuse inviteAbuseFacts) (bool, string) {
	if strings.TrimSpace(salt) == "" {
		return false, "policy_state_invalid"
	}
	if !validActorEmailDomain(actor.email) || !abuse.valid {
		return false, "policy_state_invalid"
	}
	actorKey := actorEmailSubjectKey(actor.email, salt)
	for _, subject := range abuse.subjects {
		if subject.kind == "actor_email" && subject.key == actorKey {
			return false, "abuse_subject"
		}
		if subject.kind == "workspace" && subject.key == workspaceSubjectKey(workspaceID) && subject.status == "quarantined" {
			return false, "abuse_workspace"
		}
	}
	if !quota.known {
		return false, "quota_state_unavailable"
	}
	if deployment == entitlements.Cloud && delay > 0 && !individualDelayExempt(actor.plan) && !workspaceDelayExempt(quota.plan) && now.Sub(actor.createdAt) < delay {
		return false, "new_account_action_delay"
	}
	return true, ""
}

func actorEmailSubjectKey(email, salt string) string {
	digest := sha256.Sum256([]byte(salt + ":" + strings.ToLower(strings.TrimSpace(email))))
	return "actor_email_sha256:v1:" + hex.EncodeToString(digest[:])
}

func workspaceSubjectKey(workspaceID string) string {
	digest := sha256.Sum256([]byte(workspaceID))
	return "workspace:v1:" + hex.EncodeToString(digest[:])[:24]
}

func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

func validActorEmailDomain(email string) bool {
	email = strings.TrimSpace(email)
	if strings.Count(email, "@") != 1 {
		return false
	}
	parts := strings.SplitN(email, "@", 2)
	local, domain := parts[0], strings.TrimSpace(parts[1])
	if local == "" || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") || strings.HasSuffix(domain, ".") || !strings.Contains(domain, ".") {
		return false
	}
	for _, b := range []byte(local) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("._+-", rune(b))) {
			return false
		}
	}
	for _, label := range strings.Split(normalizeDomain(domain), ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func individualDelayExempt(plan string) bool {
	return oneOf(plan, "pro", "lifetime_pro", "ai")
}

func workspaceDelayExempt(plan string) bool {
	return individualDelayExempt(plan) || oneOf(plan, "team", "selfhost_team")
}

func validHashIdentity(value, prefix string, length int) bool {
	hash, ok := strings.CutPrefix(value, prefix)
	if !ok || len(hash) != length {
		return false
	}
	for _, b := range hash {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

func oneOf(value string, values ...string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
