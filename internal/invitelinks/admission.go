// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrWorkspaceInviteQuotaExceeded = errors.New("workspace invite quota exceeded")
	ErrWorkspaceInviteForbidden     = errors.New("workspace invite action forbidden")
)

const inviteReservationTTL = 120 * time.Second

type WorkspaceInviteQuotaTargetDomain struct {
	Domain string
	Count  int
}

type WorkspaceInviteQuotaAdmission struct {
	db           Database
	config       config.Config
	bootstrapKey string
	inlineWorker string
}

type inviteQuotaSnapshot struct {
	actor         inviteActorFacts
	workspace     inviteQuotaWorkspaceFacts
	quota         inviteQuotaFacts
	activity      inviteQuotaActivityFacts
	abuse         inviteAbuseFacts
	policy        inviteQuotaPolicyConfig
	trustedSource bool
	now           time.Time
}

type inviteAbuseActionRequired struct {
	Action     string
	SubjectKey string
	EvidenceID int64
	ActionID   int64
}

type quotaViolation struct {
	scopeKey      string
	windowSeconds int32
	limit         int32
	current       int32
	requested     int32
}

func NewWorkspaceInviteQuotaAdmission(db Database, cfg config.Config, bootstrapKey string) *WorkspaceInviteQuotaAdmission {
	return &WorkspaceInviteQuotaAdmission{db: db, config: cfg, bootstrapKey: bootstrapKey, inlineWorker: "go-inline:" + uuid.NewString()}
}

func (a *WorkspaceInviteQuotaAdmission) AdmitWorkspaceInvites(ctx context.Context, actorID, workspaceID string, count int, targetDomains []WorkspaceInviteQuotaTargetDomain) (string, error) {
	if a == nil || a.db == nil || actorID == "" || workspaceID == "" || count <= 0 || int64(count) > math.MaxInt32 {
		return "", ErrWorkspaceInviteQuotaExceeded
	}
	domains := make([]inviteQuotaTargetDomain, 0, len(targetDomains))
	for _, target := range targetDomains {
		if target.Count <= 0 || int64(target.Count) > math.MaxInt32 {
			return "", ErrWorkspaceInviteQuotaExceeded
		}
		domains = append(domains, inviteQuotaTargetDomain{Domain: target.Domain, Count: int32(target.Count)})
	}
	facts := requestFactsFromContext(ctx)
	if principal := auth.PrincipalFromContext(ctx); principal != nil && principal.SessionID != "" {
		facts.RequestID = principal.SessionID
	}
	if facts.RequestID == "" {
		facts.RequestID = facts.ClientIP
	}
	if facts.RequestID == "" {
		facts.RequestID = uuid.NewString()
	}
	input := workspaceInviteQuotaInput{
		ActorUserID: actorID, WorkspaceID: workspaceID, RequestID: facts.RequestID,
		TargetCount: int32(count), TargetDomains: domains,
	}
	snapshot, err := a.loadSnapshot(ctx, actorID, workspaceID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	if snapshot.trustedSource {
		input.Source = &inviteQuotaSourceFacts{Trusted: true, IP: facts.ClientIP, ASN: parseInviteSourceASN(facts.ASN)}
	}
	plan, err := planWorkspaceInviteQuota(input, snapshot.actor, snapshot.workspace, snapshot.quota, snapshot.activity, snapshot.abuse, snapshot.policy, snapshot.now)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	if plan.decision == inviteQuotaDeny {
		if plan.persistence != nil {
			action, err := a.recordInviteAbuseAction(ctx, input, *plan.persistence)
			if err != nil {
				return "", fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
			}
			if err := a.executeInviteAbuseAction(ctx, action, actorID, workspaceID, a.inlineWorker); err != nil {
				slog.ErrorContext(ctx, "execute workspace invite abuse action", "workspace_id", workspaceID, "actor_id", actorID, "action", action.Action, "action_id", action.ActionID, "error", err)
			}
		}
		if plan.reason == "abuse_subject" || plan.reason == "new_account_action_delay" || plan.persistence != nil {
			return "", ErrWorkspaceInviteForbidden
		}
		return "", ErrWorkspaceInviteQuotaExceeded
	}
	reservationID, violation, err := a.reserveScopes(ctx, input.RequestID, plan.scopes)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	if violation != nil {
		return "", ErrWorkspaceInviteQuotaExceeded
	}
	return reservationID, nil
}

func (a *WorkspaceInviteQuotaAdmission) CommitWorkspaceInviteQuota(ctx context.Context, reservationID string, count int, targetDomains []WorkspaceInviteQuotaTargetDomain) error {
	if a == nil || a.db == nil {
		return ErrWorkspaceInviteQuotaExceeded
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := a.CommitWorkspaceInviteQuotaTx(ctx, tx, reservationID, count, targetDomains); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	return nil
}

func (a *WorkspaceInviteQuotaAdmission) CommitWorkspaceInviteQuotaTx(ctx context.Context, tx pgx.Tx, reservationID string, count int, targetDomains []WorkspaceInviteQuotaTargetDomain) error {
	if a == nil || a.db == nil || tx == nil {
		return ErrWorkspaceInviteQuotaExceeded
	}
	if _, err := uuid.Parse(reservationID); err != nil || count < 0 || int64(count) > math.MaxInt32 {
		return ErrWorkspaceInviteQuotaExceeded
	}
	domains := make([]inviteQuotaTargetDomain, 0, len(targetDomains))
	for _, target := range targetDomains {
		if target.Count < 0 || int64(target.Count) > math.MaxInt32 {
			return ErrWorkspaceInviteQuotaExceeded
		}
		domains = append(domains, inviteQuotaTargetDomain{Domain: target.Domain, Count: int32(target.Count)})
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	rows, err := tx.Query(ctx, `
		SELECT scope_key, window_seconds, bucket_start, count
		FROM runtime_rolling_quota_reservations
		WHERE id=$1::uuid AND status='reserved'
		ORDER BY scope_key
		FOR UPDATE`, reservationID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	type reservedScope struct {
		key           string
		windowSeconds int32
		bucketStart   time.Time
		count         int32
	}
	reserved := make([]reservedScope, 0)
	for rows.Next() {
		var row reservedScope
		if err := rows.Scan(&row.key, &row.windowSeconds, &row.bucketStart, &row.count); err != nil {
			rows.Close()
			return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
		}
		reserved = append(reserved, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	rows.Close()
	if len(reserved) == 0 {
		return ErrWorkspaceInviteQuotaExceeded
	}
	keys := make([]string, len(reserved))
	for index := range reserved {
		keys[index] = reserved[index].key
	}
	if err := lockInviteQuotaScopes(ctx, tx, keys); err != nil {
		return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	usage, err := planInviteQuotaSettlement(keys, int32(count), domains)
	if err != nil || len(usage) != len(reserved) {
		if err == nil {
			err = errors.New("invite quota settlement does not match reserved scopes")
		}
		return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	for index, row := range reserved {
		actual := usage[index]
		if actual > row.count {
			actual = row.count
		}
		if actual < 0 {
			actual = 0
		}
		if actual == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime_rolling_quota_counters(scope_key,window_seconds,bucket_start,count,expires_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT(scope_key,window_seconds,bucket_start) DO UPDATE SET
				count=runtime_rolling_quota_counters.count+EXCLUDED.count,
				expires_at=GREATEST(runtime_rolling_quota_counters.expires_at,EXCLUDED.expires_at),
				updated_at=EXCLUDED.updated_at`, row.key, row.windowSeconds, row.bucketStart, actual,
			now.Add(time.Duration(row.windowSeconds*2)*time.Second), now); err != nil {
			return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
		}
	}
	status := "committed"
	if count == 0 {
		status = "released"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runtime_rolling_quota_reservations SET status=$2,
			committed_at=CASE WHEN $2='committed' THEN $3 ELSE committed_at END,
			released_at=CASE WHEN $2='released' THEN $3 ELSE released_at END,
			updated_at=$3
		WHERE id=$1::uuid AND status='reserved'`, reservationID, status, now); err != nil {
		return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	return nil
}

func (a *WorkspaceInviteQuotaAdmission) ReleaseWorkspaceInviteQuota(ctx context.Context, reservationID string) error {
	if a == nil || a.db == nil {
		return ErrWorkspaceInviteQuotaExceeded
	}
	if _, err := uuid.Parse(reservationID); err != nil {
		return ErrWorkspaceInviteQuotaExceeded
	}
	err := a.db.QueryRow(ctx, `UPDATE runtime_rolling_quota_reservations
		SET status='released',released_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE id=$1::uuid AND status='reserved' RETURNING id`, reservationID).Scan(&reservationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWorkspaceInviteQuotaExceeded, err)
	}
	return nil
}

func (a *WorkspaceInviteQuotaAdmission) loadSnapshot(ctx context.Context, actorID, workspaceID string) (inviteQuotaSnapshot, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return inviteQuotaSnapshot{}, fmt.Errorf("begin invite quota snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var snapshot inviteQuotaSnapshot
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&snapshot.now); err != nil {
		return inviteQuotaSnapshot{}, fmt.Errorf("read invite quota clock: %w", err)
	}
	snapshot.actor.userID = actorID
	if err := tx.QueryRow(ctx, `
		SELECT email,created_at,registered,email_verified IS NOT NULL,disabled
		FROM users WHERE id=$1`, actorID).Scan(&snapshot.actor.email, &snapshot.actor.createdAt,
		&snapshot.actor.registered, &snapshot.actor.emailVerified, &snapshot.actor.disabled); err != nil {
		return inviteQuotaSnapshot{}, fmt.Errorf("load invite quota actor: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT created_at FROM workspaces WHERE id=$1`, workspaceID).Scan(&snapshot.workspace.createdAt); err != nil {
		return inviteQuotaSnapshot{}, fmt.Errorf("load invite quota workspace: %w", err)
	}
	privateKey, err := auth.LoadAppPrivateKey(ctx, tx, a.config, a.bootstrapKey)
	if err != nil {
		return inviteQuotaSnapshot{}, fmt.Errorf("load invite quota subject key: %w", err)
	}
	delay, err := loadNewAccountActionDelay(ctx, tx, a.config.RuntimeConfigPath)
	if err != nil {
		return inviteQuotaSnapshot{}, err
	}
	trustedSource, err := loadTrustedCloudflareHeaders(ctx, tx, a.config.RuntimeConfigPath)
	if err != nil {
		return inviteQuotaSnapshot{}, err
	}
	snapshot.policy = inviteQuotaPolicyConfig{
		deployment: entitlements.ParseDeployment(a.config.Deployment), newAccountActionDelay: delay, subjectHashSalt: privateKey,
	}
	snapshot.trustedSource = trustedSource
	var ownerID string
	ownerErr := tx.QueryRow(ctx, `SELECT user_id FROM workspace_members
		WHERE workspace_id=$1 AND role='owner' AND state='active' LIMIT 1`, workspaceID).Scan(&ownerID)
	if ownerErr != nil && !errors.Is(ownerErr, pgx.ErrNoRows) {
		return inviteQuotaSnapshot{}, fmt.Errorf("load invite quota owner: %w", ownerErr)
	}
	if ownerErr == nil {
		workspaceGrant, err := loadEntitlementGrant(ctx, tx, snapshot.policy.deployment, "workspace", workspaceID, workspaceID, snapshot.now)
		if err != nil {
			return inviteQuotaSnapshot{}, err
		}
		ownerGrant, err := loadEntitlementGrant(ctx, tx, snapshot.policy.deployment, "user", ownerID, workspaceID, snapshot.now)
		if err != nil {
			return inviteQuotaSnapshot{}, err
		}
		chargeGrant := ownerGrant
		if workspaceGrant.Commercial {
			chargeGrant = workspaceGrant
			snapshot.quota.usesOwnerQuota = false
		} else {
			snapshot.quota.usesOwnerQuota = true
		}
		snapshot.quota.known = true
		snapshot.quota.plan = chargeGrant.Plan
		snapshot.quota.ownerUserID = ownerID
		snapshot.quota.seatLimit = chargeGrant.Limits.SeatLimit
		if err := tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND state='active') +
			(SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1 AND status::text=ANY($2))`,
			workspaceID, []string{"pending", "waiting_review", "waiting_seat"}).Scan(&snapshot.quota.memberCount); err != nil {
			return inviteQuotaSnapshot{}, fmt.Errorf("count invite quota seats: %w", err)
		}
	}
	actorGrant, err := loadEntitlementGrant(ctx, tx, snapshot.policy.deployment, "user", actorID, workspaceID, snapshot.now)
	if err != nil {
		return inviteQuotaSnapshot{}, err
	}
	if snapshot.policy.deployment == entitlements.Cloud {
		snapshot.actor.plan = actorGrant.Plan
	}
	if err := tx.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE inviter_user_id=$1 AND created_at >= clock_timestamp()-interval '7 days')::bigint,
			COUNT(*) FILTER (WHERE inviter_user_id=$1 AND accepted_at >= clock_timestamp()-interval '7 days')::bigint,
			COUNT(*) FILTER (WHERE workspace_id=$2 AND status::text=ANY($3))::bigint,
			COUNT(*) FILTER (WHERE workspace_id=$2 AND created_at >= clock_timestamp()-interval '7 days')::bigint,
			COUNT(*) FILTER (WHERE workspace_id=$2 AND accepted_at >= clock_timestamp()-interval '7 days')::bigint
		FROM workspace_invitations WHERE inviter_user_id=$1 OR workspace_id=$2`, actorID, workspaceID,
		[]string{"pending", "waiting_review", "waiting_seat"}).Scan(&snapshot.activity.actorCreated7d,
		&snapshot.activity.actorAccepted7d, &snapshot.activity.workspacePending,
		&snapshot.activity.workspaceCreated7d, &snapshot.activity.workspaceAccepted7d); err != nil {
		return inviteQuotaSnapshot{}, fmt.Errorf("load invite quota activity: %w", err)
	}
	snapshot.abuse, err = loadInviteAbuseFacts(ctx, tx)
	if err != nil {
		return inviteQuotaSnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return inviteQuotaSnapshot{}, fmt.Errorf("commit invite quota snapshot: %w", err)
	}
	return snapshot, nil
}

func loadTrustedCloudflareHeaders(ctx context.Context, reader config.RuntimeConfigReader, path string) (bool, error) {
	raw, found, err := config.ReadRuntimeValue(ctx, reader, path, "auth.trustedCloudflareHeaders")
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	var value *bool
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return false, errors.New("invalid auth.trustedCloudflareHeaders")
	}
	return *value, nil
}

func parseInviteSourceASN(value string) uint32 {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil || parsed == 0 {
		return 0
	}
	return uint32(parsed)
}

func (a *WorkspaceInviteQuotaAdmission) databaseClock(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := a.db.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read rolling quota clock: %w", err)
	}
	return now, nil
}

func (a *WorkspaceInviteQuotaAdmission) reserveScopes(ctx context.Context, requestID string, scopes []inviteQuotaScope) (string, *quotaViolation, error) {
	if len(scopes) == 0 {
		return "", nil, errors.New("invite quota reservation has no scopes")
	}
	now, err := a.databaseClock(ctx)
	if err != nil {
		return "", nil, err
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("begin rolling quota reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	keys := make([]string, len(scopes))
	for index := range scopes {
		keys[index] = scopes[index].key
	}
	if err := lockInviteQuotaScopes(ctx, tx, keys); err != nil {
		return "", nil, err
	}
	reservationID := uuid.NewString()
	for _, scope := range scopes {
		bucket := inviteQuotaBucketStart(now, scope.bucketSeconds)
		windowStart := now.Add(-time.Duration(scope.windowSeconds) * time.Second)
		var committed, reserved int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(count),0)::bigint FROM runtime_rolling_quota_counters
			WHERE scope_key=$1 AND window_seconds=$2 AND bucket_start >= $3`, scope.key, scope.windowSeconds, windowStart).Scan(&committed); err != nil {
			return "", nil, fmt.Errorf("read rolling quota counters: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(count),0)::bigint FROM runtime_rolling_quota_reservations
			WHERE scope_key=$1 AND window_seconds=$2 AND bucket_start >= $3
			AND status='reserved' AND expires_at>$4`, scope.key, scope.windowSeconds, windowStart, now).Scan(&reserved); err != nil {
			return "", nil, fmt.Errorf("read rolling quota reservations: %w", err)
		}
		current := saturatingInt32(committed + reserved)
		if int64(current)+int64(scope.requested) > int64(scope.limit) {
			if err := tx.Rollback(ctx); err != nil {
				return "", nil, fmt.Errorf("rollback denied rolling quota reservation: %w", err)
			}
			return "", &quotaViolation{scopeKey: scope.key, windowSeconds: scope.windowSeconds,
				limit: scope.limit, current: current, requested: scope.requested}, nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO runtime_rolling_quota_reservations
			(id,scope_key,window_seconds,bucket_start,count,status,purpose,request_id,expires_at)
			VALUES($1::uuid,$2,$3,$4,$5,'reserved',$6,$7,$8)`, reservationID, scope.key, scope.windowSeconds,
			bucket, scope.requested, workspaceInviteQuotaPurpose, requestID, now.Add(inviteReservationTTL)); err != nil {
			return "", nil, fmt.Errorf("create rolling quota reservation: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", nil, fmt.Errorf("commit rolling quota reservation: %w", err)
	}
	return reservationID, nil, nil
}

func lockInviteQuotaScopes(ctx context.Context, tx pgx.Tx, keys []string) error {
	unique := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		unique[key] = struct{}{}
	}
	sorted := make([]string, 0, len(unique))
	for key := range unique {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	for _, key := range sorted {
		digest := sha256.Sum256([]byte(key))
		lockKey := int64(uint64(digest[0])<<56 | uint64(digest[1])<<48 | uint64(digest[2])<<40 | uint64(digest[3])<<32 |
			uint64(digest[4])<<24 | uint64(digest[5])<<16 | uint64(digest[6])<<8 | uint64(digest[7]))
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
			return fmt.Errorf("lock rolling quota scope: %w", err)
		}
	}
	return nil
}

func inviteQuotaBucketStart(now time.Time, bucketSeconds int64) time.Time {
	seconds := now.Unix()
	return time.Unix(seconds-seconds%bucketSeconds, 0).UTC()
}

func saturatingInt32(value int64) int32 {
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	if value < math.MinInt32 {
		return math.MinInt32
	}
	return int32(value)
}

func (a *WorkspaceInviteQuotaAdmission) recordInviteAbuseAction(ctx context.Context, input workspaceInviteQuotaInput, plan inviteQuotaAbusePersistencePlan) (inviteAbuseActionRequired, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return inviteAbuseActionRequired{}, fmt.Errorf("begin invite abuse transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var userID any
	if plan.subjectUserID != "" {
		userID = plan.subjectUserID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runtime_invite_abuse_subjects(
		subject_key,kind,user_id,actor_email_hash,email_domain,first_seen_at,last_seen_at,status,action,action_reason,action_at)
		VALUES($1,$2,$3,$4,$5,clock_timestamp(),clock_timestamp(),$6,$7,$8,clock_timestamp())
		ON CONFLICT(subject_key) DO UPDATE SET user_id=EXCLUDED.user_id,actor_email_hash=EXCLUDED.actor_email_hash,
		email_domain=EXCLUDED.email_domain,last_seen_at=clock_timestamp(),status=EXCLUDED.status,
		action=EXCLUDED.action,action_reason=EXCLUDED.action_reason,action_at=clock_timestamp(),updated_at=clock_timestamp()`,
		plan.key, plan.kind, userID, plan.actorEmailHash, plan.actorDomain, plan.status, plan.action, plan.reason); err != nil {
		return inviteAbuseActionRequired{}, fmt.Errorf("upsert invite abuse subject: %w", err)
	}
	targets := make([]map[string]any, 0, len(plan.targetDomains))
	for _, target := range plan.targetDomains {
		targets = append(targets, map[string]any{"domain": normalizeDomain(target.Domain), "count": target.Count})
	}
	targetJSON, err := json.Marshal(targets)
	if err != nil {
		return inviteAbuseActionRequired{}, fmt.Errorf("encode invite abuse domains: %w", err)
	}
	counters, err := json.Marshal(map[string]int32{"requested": plan.requested})
	if err != nil {
		return inviteAbuseActionRequired{}, fmt.Errorf("encode invite abuse counters: %w", err)
	}
	var userRef any
	if plan.subjectUserID != "" {
		userRef = plan.subjectUserID
	}
	var sourceHash any
	if plan.sourcePrefixHash != "" {
		sourceHash = plan.sourcePrefixHash
	}
	var sourceASN any
	if plan.sourceASN != nil {
		sourceASN = *plan.sourceASN
	}
	var evidenceID int64
	if err := tx.QueryRow(ctx, `INSERT INTO runtime_invite_abuse_evidence(
		subject_key,request_id,workspace_id,user_id,actor_email_hash,source_prefix_hash,source_asn,
		target_domains,counters,decision,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10,$11) RETURNING id`, plan.key, input.RequestID,
		input.WorkspaceID, userRef, plan.actorEmailHash, sourceHash, sourceASN, targetJSON, counters, plan.action, plan.reason).Scan(&evidenceID); err != nil {
		return inviteAbuseActionRequired{}, fmt.Errorf("insert invite abuse evidence: %w", err)
	}
	var actionID int64
	if err := tx.QueryRow(ctx, `INSERT INTO runtime_invite_abuse_actions(subject_key,evidence_id,action,status,next_attempt_at)
		VALUES($1,$2,$3,'pending',clock_timestamp()) RETURNING id`, plan.key, evidenceID, plan.action).Scan(&actionID); err != nil {
		return inviteAbuseActionRequired{}, fmt.Errorf("insert invite abuse action: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return inviteAbuseActionRequired{}, fmt.Errorf("commit invite abuse action: %w", err)
	}
	return inviteAbuseActionRequired{Action: plan.action, SubjectKey: plan.key, EvidenceID: evidenceID, ActionID: actionID}, nil
}
