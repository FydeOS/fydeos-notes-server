// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	entpg "github.com/fydeos/fydeos-notes-server/internal/entitlements/postgres"
	"github.com/jackc/pgx/v5"
)

func planDisplayName(plan string) string {
	switch plan {
	case "pro", "selfhost_free":
		return "Pro"
	case "lifetime_pro":
		return "Lifetime Pro"
	case "ai":
		return "AI"
	case "team", "selfhost_team":
		return "Team"
	}
	return "Free"
}

var sizeUnits = []string{"B", "KB", "MB", "GB", "TB", "PB", "EB", "ZB", "YB"}

func humanSize(n int64) string {
	if n == 0 {
		return "0 B"
	}
	v := float64(n)
	i := 0
	for math.Abs(v) >= 1024 && i < len(sizeUnits)-1 {
		v /= 1024
		i++
	}
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + " " + sizeUnits[i]
}

func humanPeriod(seconds int64) string {
	return strconv.FormatInt(int64(math.Round(float64(seconds)/86400)), 10) + " days"
}

// FormatUserQuota renders a user quota with human-readable values.
func FormatUserQuota(state UserQuotaState) UserQuota {
	limit := state.CopilotActionLimit
	if state.UnlimitedCopilot {
		limit = nil
	}
	copilot := "Unlimited"
	if limit != nil && *limit != 0 {
		copilot = strconv.Itoa(int(*limit)) + " times"
	}
	name := planDisplayName(state.Plan)
	return UserQuota{
		Name: name, BlobLimit: state.BlobLimit, StorageQuota: state.StorageQuota, UsedStorageQuota: state.UsedStorageQuota,
		HistoryPeriod: state.HistoryPeriodSeconds, MemberLimit: state.SeatLimit, CopilotActionLimit: limit,
		HumanReadable: UserQuotaHumanReadable{
			Name: name, BlobLimit: humanSize(state.BlobLimit), StorageQuota: humanSize(state.StorageQuota),
			UsedStorageQuota: humanSize(state.UsedStorageQuota), HistoryPeriod: humanPeriod(state.HistoryPeriodSeconds),
			MemberLimit: strconv.Itoa(int(state.SeatLimit)), CopilotActionLimit: copilot,
		},
	}
}

func storageUsed(ctx context.Context, q queryer, workspaceIDs []string, now time.Time) (int64, error) {
	if len(workspaceIDs) == 0 {
		return 0, nil
	}
	var used int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE((SELECT sum(size) FROM blobs WHERE workspace_id = ANY($1::text[]) AND deleted_at IS NULL
		                   AND (status='completed' OR (status='pending' AND reservation_expires_at > $2))), 0)
		     + COALESCE((SELECT sum(size) FROM comment_attachments WHERE workspace_id = ANY($1::text[]) AND deleted_at IS NULL
		                   AND (status='completed' OR (status='pending' AND reservation_expires_at > $2))), 0)`, workspaceIDs, now).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("read storage usage: %w", err)
	}
	return used, nil
}

// ownerPersonalWorkspaces lists the owner's non-commercial owned workspaces.
func (r *Repository) ownerPersonalWorkspaces(ctx context.Context, q queryer, owner string, now time.Time) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT workspace_id FROM workspace_members WHERE user_id=$1 AND role='owner' AND state='active' ORDER BY workspace_id`, owner)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	facts, err := entpg.LoadFacts(ctx, q, "workspace", ids)
	if err != nil {
		return nil, err
	}
	out := ids[:0]
	for _, id := range ids {
		g := entitlements.Resolve(entitlements.Context{Deployment: entitlements.ParseDeployment(r.deployment), TargetType: "workspace", WorkspaceID: id, Now: now}, facts[id])
		if !g.Commercial {
			out = append(out, id)
		}
	}
	return out, nil
}

// ReadUserQuotaState reads the user's quota at database time.
func (r *Repository) ReadUserQuotaState(ctx context.Context, userID string) (UserQuotaState, error) {
	var out UserQuotaState
	err := r.inTx(ctx, readOnlySnapshot, func(tx pgx.Tx) error {
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		g, err := r.grantFor(ctx, tx, "user", userID, now)
		if err != nil {
			return err
		}
		ids, err := r.ownerPersonalWorkspaces(ctx, tx, userID, now)
		if err != nil {
			return err
		}
		used, err := storageUsed(ctx, tx, ids, now)
		if err != nil {
			return err
		}
		out = UserQuotaState{
			Plan: g.Plan, SeatLimit: g.Limits.SeatLimit, BlobLimit: g.Limits.BlobLimit, StorageQuota: g.Limits.StorageQuota,
			UsedStorageQuota: used, HistoryPeriodSeconds: g.Limits.HistoryPeriodSeconds, CopilotActionLimit: g.Limits.CopilotActionLimit,
			UnlimitedCopilot: g.Rights.UnlimitedCopilot,
		}
		return nil
	})
	return out, err
}

// ReadWorkspaceQuotaState reads the effective quota of a workspace.
func (r *Repository) ReadWorkspaceQuotaState(ctx context.Context, workspaceID string) (WorkspaceQuotaState, error) {
	var out WorkspaceQuotaState
	err := r.inTx(ctx, readOnlySnapshot, func(tx pgx.Tx) error {
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		var owner string
		err = tx.QueryRow(ctx, `SELECT user_id FROM workspace_members WHERE workspace_id=$1 AND role='owner' AND state='active' ORDER BY updated_at DESC, id LIMIT 1`, workspaceID).Scan(&owner)
		if noRows(err) {
			exists, err := workspaceExists(ctx, tx, workspaceID)
			if err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
			return ErrOwnerMissing
		}
		if err != nil {
			return err
		}
		g, err := r.grantFor(ctx, tx, "workspace", workspaceID, now)
		if err != nil {
			return err
		}
		ids := []string{workspaceID}
		usesOwner := !g.Commercial
		if usesOwner {
			if g, err = r.grantFor(ctx, tx, "user", owner, now); err != nil {
				return err
			}
			if ids, err = r.ownerPersonalWorkspaces(ctx, tx, owner, now); err != nil {
				return err
			}
		}
		used, err := storageUsed(ctx, tx, ids, now)
		if err != nil {
			return err
		}
		var members int32
		if err := tx.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND state='active')
			     + (SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1 AND status IN `+openInvitationStatuses+`)`, workspaceID).Scan(&members); err != nil {
			return err
		}
		over := members - g.Limits.SeatLimit
		if over < 0 {
			over = 0
		}
		reasons := []string{}
		if over > 0 {
			reasons = append(reasons, "member_overflow")
		}
		if used > g.Limits.StorageQuota {
			reasons = append(reasons, "storage_overflow")
		}
		out = WorkspaceQuotaState{
			Plan: g.Plan, OwnerUserID: owner, UsesOwnerQuota: usesOwner, SeatLimit: g.Limits.SeatLimit, MemberCount: members,
			OvercapacityMemberCount: over, BlobLimit: g.Limits.BlobLimit, StorageQuota: g.Limits.StorageQuota, UsedStorageQuota: used,
			HistoryPeriodSeconds: g.Limits.HistoryPeriodSeconds, Readonly: len(reasons) > 0, ReadonlyReasons: reasons,
			UnlimitedCopilot: g.Rights.UnlimitedCopilot,
		}
		return nil
	})
	return out, err
}

// ReadQuota renders the workspace quota for display.
func (r *Repository) ReadQuota(ctx context.Context, workspaceID string) (Quota, error) {
	s, err := r.ReadWorkspaceQuotaState(ctx, workspaceID)
	if err != nil {
		return Quota{}, err
	}
	name := planDisplayName(s.Plan)
	return Quota{
		Name: name, BlobLimit: s.BlobLimit, StorageQuota: s.StorageQuota, UsedStorageQuota: s.UsedStorageQuota, HistoryPeriod: s.HistoryPeriodSeconds,
		MemberLimit: s.SeatLimit, MemberCount: s.MemberCount, OvercapacityMemberCount: s.OvercapacityMemberCount,
		HumanReadable: QuotaHumanReadable{
			Name: name, BlobLimit: humanSize(s.BlobLimit), StorageQuota: humanSize(s.StorageQuota), StorageQuotaUsed: humanSize(s.UsedStorageQuota),
			HistoryPeriod: humanPeriod(s.HistoryPeriodSeconds), MemberLimit: strconv.Itoa(int(s.SeatLimit)),
			MemberCount: strconv.Itoa(int(s.MemberCount)), OvercapacityMemberCount: strconv.Itoa(int(s.OvercapacityMemberCount)),
		},
	}, nil
}
