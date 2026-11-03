// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/identity"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const maxEmailInvitations = 512

const openInvitationStatuses = `('pending','waiting_review','waiting_seat')`

func invitationStatusName(status string) *string {
	switch status {
	case "pending":
		return ptr("Pending")
	case "waiting_review":
		return ptr("UnderReview")
	case "waiting_seat":
		return ptr("NeedMoreSeat")
	case "accepted":
		return ptr("Accepted")
	}
	return nil
}

// InvitationByID resolves an invite id to a membership or invitation.
func (r *Repository) InvitationByID(ctx context.Context, inviteID string) (InvitationRecord, error) {
	var rec InvitationRecord
	err := r.db.QueryRow(ctx, `SELECT workspace_id, user_id, source, role FROM workspace_members WHERE id=$1`, inviteID).
		Scan(&rec.WorkspaceID, &rec.InviteeID, &rec.Kind, &rec.RequestedRole)
	if err == nil {
		rec.Status = ptr("Accepted")
		return rec, nil
	}
	if !noRows(err) {
		return rec, err
	}
	var inviter *string
	var status string
	err = r.db.QueryRow(ctx, `SELECT workspace_id, invitee_user_id, inviter_user_id, kind, requested_role, status
		FROM workspace_invitations WHERE id=$1 AND invitee_user_id IS NOT NULL`, inviteID).
		Scan(&rec.WorkspaceID, &rec.InviteeID, &inviter, &rec.Kind, &rec.RequestedRole, &status)
	if noRows(err) {
		return InvitationRecord{}, ErrInvitationNotFound
	}
	if err != nil {
		return InvitationRecord{}, err
	}
	if inviter != nil {
		rec.InviterID = *inviter
	}
	rec.Status = invitationStatusName(status)
	return rec, nil
}

// InvitationStatus reports the user's membership or invitation status.
func (r *Repository) InvitationStatus(ctx context.Context, workspaceID, userID string) (*string, error) {
	role, err := activeRole(ctx, r.db, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	if role != "" {
		return ptr("Accepted"), nil
	}
	var status string
	err = r.db.QueryRow(ctx, `SELECT status FROM workspace_invitations WHERE workspace_id=$1 AND invitee_user_id=$2 ORDER BY created_at DESC LIMIT 1`, workspaceID, userID).Scan(&status)
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return invitationStatusName(status), nil
}

// InvitationWorkspace returns what an invitation shows about a workspace.
func (r *Repository) InvitationWorkspace(ctx context.Context, workspaceID string) (InvitationWorkspace, error) {
	var w InvitationWorkspace
	var name *string
	err := r.db.QueryRow(ctx, `SELECT id, name, avatar_key FROM workspaces WHERE id=$1`, workspaceID).Scan(&w.ID, &name, &w.AvatarKey)
	if noRows(err) {
		return w, ErrInvitationNotFound
	}
	if err != nil {
		return w, err
	}
	w.Name = "Untitled Workspace"
	if name != nil && *name != "" {
		w.Name = *name
	}
	if w.OwnerID, err = earliestOwner(ctx, r.db, workspaceID); err != nil {
		return w, err
	}
	if w.OwnerID == "" {
		return InvitationWorkspace{}, ErrInvitationOwnerNotFound
	}
	return w, nil
}

// InvitationReviewers lists active owners and admins, owner first.
func (r *Repository) InvitationReviewers(ctx context.Context, workspaceID string) ([]string, error) {
	rows, err := r.db.Query(ctx, `SELECT user_id FROM workspace_members WHERE workspace_id=$1 AND state='active' AND role IN ('owner','admin')
		ORDER BY (role='owner') DESC, created_at, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, ErrInvitationOwnerNotFound
	}
	return ids, nil
}

// lockInvitationScope takes the invitation write locks in their fixed
// order and returns the verified owner.
func lockInvitationScope(ctx context.Context, tx pgx.Tx, workspaceID string, users ...string) (string, error) {
	owner, err := earliestOwner(ctx, tx, workspaceID)
	if err != nil {
		return "", err
	}
	if owner == "" {
		return "", ErrInvitationOwnerNotFound
	}
	ids := sortedUnique(append([]string{owner}, users...))
	if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id = ANY($1::text[]) ORDER BY id FOR UPDATE`, ids); err != nil {
		return "", fmt.Errorf("lock invitation users: %w", err)
	}
	if err := lockWorkspaceRow(ctx, tx, workspaceID); err != nil {
		if err == ErrNotFound {
			return "", ErrInvitationNotFound
		}
		return "", err
	}
	again, err := earliestOwner(ctx, tx, workspaceID)
	if err != nil {
		return "", err
	}
	if again != owner {
		return "", ErrQuotaStateUnknown
	}
	return owner, nil
}

type seatMode int

const (
	seatReserve seatMode = iota
	seatActivate
)

func (r *Repository) checkSeats(ctx context.Context, tx pgx.Tx, workspaceID, owner string, mode seatMode) error {
	now, err := dbNow(ctx, tx)
	if err != nil {
		return err
	}
	grant, err := r.grantFor(ctx, tx, "workspace", workspaceID, now)
	if err != nil {
		return err
	}
	if !grant.Commercial {
		if grant, err = r.grantFor(ctx, tx, "user", owner, now); err != nil {
			return err
		}
	}
	var charged int64
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND state='active')
		     + (SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1 AND status IN `+openInvitationStatuses+`)`, workspaceID).Scan(&charged); err != nil {
		return err
	}
	limit := int64(grant.Limits.SeatLimit)
	if (mode == seatReserve && charged >= limit) || (mode == seatActivate && charged > limit) {
		return ErrInvitationNoSeats
	}
	return nil
}

func canReviewInvitations(ctx context.Context, r *Repository, tx pgx.Tx, workspaceID, actor string) (bool, error) {
	role, err := activeRole(ctx, tx, workspaceID, actor)
	if err != nil {
		return false, err
	}
	if role == "owner" {
		return true, nil
	}
	if role != "admin" {
		return false, nil
	}
	return r.commercial(ctx, tx, workspaceID)
}

type lockedInvitation struct {
	id, kind, status, role string
	inviter                *string
}

func (r *Repository) activateLocked(ctx context.Context, tx pgx.Tx, workspaceID, inviteeID, owner string, inv lockedInvitation) (InvitationAcceptance, error) {
	role, err := activeRole(ctx, tx, workspaceID, inviteeID)
	if err != nil {
		return InvitationAcceptance{}, err
	}
	if role != "" {
		return InvitationAcceptance{}, ErrInvitationAlreadyMember
	}
	if err := r.checkSeats(ctx, tx, workspaceID, owner, seatActivate); err != nil {
		return InvitationAcceptance{}, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO workspace_members(id, workspace_id, user_id, role, state, source) VALUES ($1,$2,$3,$4,'active',$5) ON CONFLICT DO NOTHING`,
		inv.id, workspaceID, inviteeID, inv.role, inv.kind)
	if err != nil {
		return InvitationAcceptance{}, fmt.Errorf("activate invitation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return InvitationAcceptance{}, ErrInvitationAlreadyMember
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_invitations WHERE id=$1`, inv.id); err != nil {
		return InvitationAcceptance{}, err
	}
	inviter := owner
	if inv.inviter != nil && *inv.inviter != "" {
		inviter = *inv.inviter
	}
	return InvitationAcceptance{InviteID: inv.id, WorkspaceID: workspaceID, InviteeID: inviteeID, InviterID: inviter, OwnerID: owner}, nil
}

func scanLockedInvitation(row pgx.Row) (lockedInvitation, bool, error) {
	var inv lockedInvitation
	err := row.Scan(&inv.id, &inv.kind, &inv.status, &inv.role, &inv.inviter)
	if noRows(err) {
		return inv, false, nil
	}
	return inv, err == nil, err
}

func (r *Repository) activateEmail(ctx context.Context, workspaceID, inviteID, inviteeID string) (InvitationAcceptance, error) {
	var out InvitationAcceptance
	if workspaceID == "" || inviteeID == "" {
		return out, ErrInvitationInvalid
	}
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		owner, err := lockInvitationScope(ctx, tx, workspaceID, inviteeID)
		if err != nil {
			return err
		}
		const cols = `SELECT id, kind, status, requested_role, inviter_user_id FROM workspace_invitations`
		var row pgx.Row
		if inviteID != "" {
			row = tx.QueryRow(ctx, cols+` WHERE id=$1 AND workspace_id=$2 AND invitee_user_id=$3 FOR UPDATE`, inviteID, workspaceID, inviteeID)
		} else {
			row = tx.QueryRow(ctx, cols+` WHERE workspace_id=$1 AND invitee_user_id=$2 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, workspaceID, inviteeID)
		}
		inv, found, err := scanLockedInvitation(row)
		if err != nil {
			return err
		}
		if !found || inv.kind != "email" || (inv.status != "pending" && inv.status != "waiting_seat") || (inv.role != "admin" && inv.role != "member") {
			return ErrInvitationInvalid
		}
		out, err = r.activateLocked(ctx, tx, workspaceID, inviteeID, owner, inv)
		return err
	})
	return out, err
}

// ActivateEmailInvitation accepts one specific email invitation.
func (r *Repository) ActivateEmailInvitation(ctx context.Context, workspaceID, inviteID, inviteeID string) (InvitationAcceptance, error) {
	if inviteID == "" {
		return InvitationAcceptance{}, ErrInvitationInvalid
	}
	return r.activateEmail(ctx, workspaceID, inviteID, inviteeID)
}

// ActivatePendingInvitation accepts the invitee's email invitation.
func (r *Repository) ActivatePendingInvitation(ctx context.Context, workspaceID, inviteeID string) (InvitationAcceptance, error) {
	return r.activateEmail(ctx, workspaceID, "", inviteeID)
}

// ApproveReviewInvitation lets an owner or commercial admin accept a
// link-review request.
func (r *Repository) ApproveReviewInvitation(ctx context.Context, actorUserID, workspaceID, inviteeID string) (InvitationAcceptance, error) {
	var out InvitationAcceptance
	if actorUserID == "" || workspaceID == "" || inviteeID == "" {
		return out, ErrInvitationInvalid
	}
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		owner, err := lockInvitationScope(ctx, tx, workspaceID, actorUserID, inviteeID)
		if err != nil {
			return err
		}
		ok, err := canReviewInvitations(ctx, r, tx, workspaceID, actorUserID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrWorkspaceRolePermissionDenied
		}
		inv, found, err := scanLockedInvitation(tx.QueryRow(ctx, `SELECT id, kind, status, requested_role, inviter_user_id FROM workspace_invitations
			WHERE workspace_id=$1 AND invitee_user_id=$2 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, workspaceID, inviteeID))
		if err != nil {
			return err
		}
		if !found {
			return ErrWorkspaceMemberNotFound
		}
		if inv.kind != "link" || inv.status != "waiting_review" || inv.role != "member" {
			return ErrInvitationInvalid
		}
		out, err = r.activateLocked(ctx, tx, workspaceID, inviteeID, owner, inv)
		return err
	})
	return out, err
}

// ReserveLinkReviewSeat records an invite-link join request awaiting review.
func (r *Repository) ReserveLinkReviewSeat(ctx context.Context, workspaceID, inviteeID, inviterID string) (InvitationAcceptance, error) {
	var out InvitationAcceptance
	if workspaceID == "" || inviteeID == "" {
		return out, ErrInvitationInvalid
	}
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		owner, err := lockInvitationScope(ctx, tx, workspaceID, inviteeID, inviterID)
		if err != nil {
			return err
		}
		inviter := inviterID
		if inviter != "" {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND NOT disabled)`, inviter).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				inviter = ""
			}
		}
		if inviter == "" {
			inviter = owner
		}
		var occupied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active')
			OR EXISTS(SELECT 1 FROM workspace_invitations WHERE workspace_id=$1 AND invitee_user_id=$2 AND status IN `+openInvitationStatuses+`)`,
			workspaceID, inviteeID).Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return ErrInvitationAlreadyMember
		}
		if err := r.checkSeats(ctx, tx, workspaceID, owner, seatReserve); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_invitations WHERE workspace_id=$1 AND invitee_user_id=$2`, workspaceID, inviteeID); err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO workspace_invitations(id, workspace_id, invitee_user_id, inviter_user_id, requested_role, status, kind)
			VALUES ($4,$1,$2,$3,'member','waiting_review','link') RETURNING id`, workspaceID, inviteeID, inviter, newRowID()).Scan(&id); err != nil {
			return fmt.Errorf("reserve review seat: %w", err)
		}
		out = InvitationAcceptance{InviteID: id, WorkspaceID: workspaceID, InviteeID: inviteeID, InviterID: inviter, OwnerID: owner}
		return nil
	})
	return out, err
}

// EmailInvitationAlreadyExists reports whether the (normalized) email is a
// member or already invited.
func (r *Repository) EmailInvitationAlreadyExists(ctx context.Context, workspaceID, email string) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM workspace_members m JOIN users u ON u.id=m.user_id
		               WHERE m.workspace_id=$1 AND m.state='active' AND lower(u.email)=lower($2))
		    OR EXISTS (SELECT 1 FROM workspace_invitations i LEFT JOIN users u ON u.id=i.invitee_user_id
		               WHERE i.workspace_id=$1 AND (i.normalized_email=$2 OR lower(u.email)=lower($2)))`, workspaceID, email).Scan(&ok)
	return ok, err
}

// ReserveEmailInvitations creates pending email invitations atomically.
func (r *Repository) ReserveEmailInvitations(ctx context.Context, actorID, workspaceID string, emails []string) ([]EmailInvitationReservation, error) {
	return r.reserveEmails(ctx, actorID, workspaceID, emails, "", nil)
}

// ReserveEmailInvitationsWithQuota also settles invite quota in the same
// transaction.
func (r *Repository) ReserveEmailInvitationsWithQuota(ctx context.Context, actorID, workspaceID string, emails []string, reservationID string, settlement EmailInvitationQuotaSettlement) ([]EmailInvitationReservation, error) {
	if reservationID == "" || settlement == nil {
		return nil, ErrInvitationInvalid
	}
	return r.reserveEmails(ctx, actorID, workspaceID, emails, reservationID, settlement)
}

func emailDomain(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return email[i+1:]
	}
	return ""
}

func placeholderName(email string) string {
	if i := strings.Index(email, "@"); i > 0 {
		return email[:i]
	}
	return "Invited user"
}

func (r *Repository) reserveEmails(ctx context.Context, actorID, workspaceID string, emails []string, reservationID string, settlement EmailInvitationQuotaSettlement) ([]EmailInvitationReservation, error) {
	if actorID == "" || workspaceID == "" || len(emails) == 0 || len(emails) > maxEmailInvitations {
		return nil, ErrInvitationInvalid
	}
	seen := make(map[string]struct{}, len(emails))
	for _, e := range emails {
		if e == "" {
			return nil, ErrInvitationInvalid
		}
		if _, dup := seen[e]; dup {
			return nil, ErrInvitationInvalid
		}
		seen[e] = struct{}{}
	}
	sortedEmails := append([]string(nil), emails...)
	sort.Strings(sortedEmails)
	var out []EmailInvitationReservation
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		for _, e := range sortedEmails {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 8301))`, e); err != nil {
				return fmt.Errorf("lock invitation email: %w", err)
			}
			if err := identity.LockEmail(ctx, tx, e); err != nil {
				return err
			}
		}
		var name *string
		err := tx.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=$1`, workspaceID).Scan(&name)
		if noRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		owner, err := earliestOwner(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if owner == "" {
			return ErrInvitationOwnerNotFound
		}
		if name != nil && invitelinks.ContainsURLOrDomain(*name) {
			return invitelinks.ErrWorkspaceNameDenied
		}
		userIDs := make(map[string]string, len(emails))
		for _, e := range emails {
			var id string
			err := tx.QueryRow(ctx, `SELECT id FROM users WHERE lower(email)=lower($1) ORDER BY registered DESC, created_at, id LIMIT 1`, e).Scan(&id)
			if noRows(err) {
				id = uuid.NewString()
				if _, err := tx.Exec(ctx, `INSERT INTO users(id, name, email, registered, disabled, created_at) VALUES ($1,$2,$3,false,false,CURRENT_TIMESTAMP)`, id, placeholderName(e), e); err != nil {
					return fmt.Errorf("create invited user: %w", err)
				}
			} else if err != nil {
				return err
			}
			userIDs[e] = id
		}
		lockUsers := []string{actorID}
		for _, id := range userIDs {
			lockUsers = append(lockUsers, id)
		}
		if _, err := lockInvitationScopeVerify(ctx, tx, workspaceID, owner, lockUsers); err != nil {
			return err
		}
		ok, err := canReviewInvitations(ctx, r, tx, workspaceID, actorID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrWorkspaceRolePermissionDenied
		}
		domains := map[string]int{}
		for _, e := range emails {
			uid := userIDs[e]
			var occupied bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active')
				OR EXISTS(SELECT 1 FROM workspace_invitations WHERE workspace_id=$1 AND (invitee_user_id=$2 OR normalized_email=$3))`,
				workspaceID, uid, e).Scan(&occupied); err != nil {
				return err
			}
			if occupied {
				return &InvitationSpaceError{Cause: ErrInvitationAlreadyMember, WorkspaceID: workspaceID}
			}
			if err := r.checkSeats(ctx, tx, workspaceID, owner, seatReserve); err != nil {
				return err
			}
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO workspace_invitations(id, workspace_id, invitee_user_id, normalized_email, inviter_user_id, requested_role, status, kind)
				VALUES ($5,$1,$2,$3,$4,'member','pending','email') RETURNING id`, workspaceID, uid, e, actorID, newRowID()).Scan(&id); err != nil {
				return fmt.Errorf("reserve email invitation: %w", err)
			}
			out = append(out, EmailInvitationReservation{ID: id, UserID: uid, Email: e})
			domains[emailDomain(e)]++
		}
		if settlement != nil {
			targets := make([]invitelinks.WorkspaceInviteQuotaTargetDomain, 0, len(domains))
			for d, n := range domains {
				targets = append(targets, invitelinks.WorkspaceInviteQuotaTargetDomain{Domain: d, Count: n})
			}
			sort.Slice(targets, func(i, j int) bool { return targets[i].Domain < targets[j].Domain })
			if err := settlement.CommitWorkspaceInviteQuotaTx(ctx, tx, reservationID, len(out), targets); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// lockInvitationScopeVerify locks users and the workspace row for an owner
// that was read earlier, failing when ownership moved meanwhile.
func lockInvitationScopeVerify(ctx context.Context, tx pgx.Tx, workspaceID, owner string, users []string) (string, error) {
	ids := sortedUnique(append([]string{owner}, users...))
	if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id = ANY($1::text[]) ORDER BY id FOR UPDATE`, ids); err != nil {
		return "", fmt.Errorf("lock invitation users: %w", err)
	}
	if err := lockWorkspaceRow(ctx, tx, workspaceID); err != nil {
		return "", err
	}
	again, err := earliestOwner(ctx, tx, workspaceID)
	if err != nil {
		return "", err
	}
	if again != owner {
		return "", ErrQuotaStateUnknown
	}
	return owner, nil
}
