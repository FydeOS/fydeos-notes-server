// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// memberUnionSQL lists active members and invitees of workspace $1 whose
// name or email contains $2 (case-insensitively), oldest first.
const memberUnionSQL = `
	SELECT id, name, email, avatar_url, role, status FROM (
		SELECT u.id, u.name, u.email, u.avatar_url,
		       CASE m.role WHEN 'owner' THEN 'Owner' WHEN 'admin' THEN 'Admin' ELSE 'Collaborator' END AS role,
		       'Accepted' AS status, m.created_at
		FROM workspace_members m JOIN users u ON u.id=m.user_id
		WHERE m.workspace_id=$1 AND m.state='active'
		UNION ALL
		SELECT u.id, u.name, u.email, u.avatar_url,
		       CASE i.requested_role WHEN 'admin' THEN 'Admin' ELSE 'Collaborator' END,
		       CASE i.status WHEN 'pending' THEN 'Pending' WHEN 'waiting_review' THEN 'UnderReview'
		                     WHEN 'waiting_seat' THEN 'NeedMoreSeat' ELSE 'Accepted' END,
		       i.created_at
		FROM workspace_invitations i JOIN users u ON u.id=i.invitee_user_id
		WHERE i.workspace_id=$1 AND i.status IN ('pending','waiting_review','waiting_seat','accepted')
		  AND NOT EXISTS (SELECT 1 FROM workspace_members am WHERE am.workspace_id=$1 AND am.user_id=i.invitee_user_id AND am.state='active')
	) all_members
	WHERE $2 = '' OR strpos(lower(email), lower($2)) > 0 OR strpos(lower(name), lower($2)) > 0
	ORDER BY created_at, id
	OFFSET $3 LIMIT $4`

func listMemberUnion(ctx context.Context, q queryer, workspaceID, query string, offset, limit int) ([]Member, error) {
	if offset < 0 {
		offset = 0
	}
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	rows, err := q.Query(ctx, memberUnionSQL, workspaceID, query, offset, lim)
	if err != nil {
		return nil, fmt.Errorf("list workspace members: %w", err)
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Name, &m.Email, &m.AvatarURL, &m.Role, &m.Status); err != nil {
			return nil, fmt.Errorf("scan workspace member: %w", err)
		}
		m.InviteID = m.ID
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListMembersForUser lists members and invitees visible to a member.
func (r *Repository) ListMembersForUser(ctx context.Context, userID, workspaceID, query string, offset, limit int) ([]Member, error) {
	if utf16Len(query) > MemberSearchMaxLength {
		return nil, ErrMemberQueryTooLong
	}
	role, err := activeRole(ctx, r.db, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	if !isMemberRole(role) {
		return nil, ErrAccessDenied
	}
	return listMemberUnion(ctx, r.db, workspaceID, query, offset, limit)
}

// ListAdminWorkspaceMembers is the admin-console member listing.
func (r *Repository) ListAdminWorkspaceMembers(ctx context.Context, workspaceID string, skip, take int, query *string) ([]AdminWorkspaceMember, error) {
	keyword := ""
	if query != nil {
		keyword = strings.TrimSpace(*query)
	}
	members, err := listMemberUnion(ctx, r.db, workspaceID, keyword, skip, take)
	if err != nil {
		return nil, err
	}
	out := make([]AdminWorkspaceMember, 0, len(members))
	for _, m := range members {
		out = append(out, AdminWorkspaceMember{ID: m.ID, Name: m.Name, Email: m.Email, AvatarURL: m.AvatarURL, Role: m.Role, Status: m.Status})
	}
	return out, nil
}

// LeaveWorkspace removes the caller's own non-owner membership.
func (r *Repository) LeaveWorkspace(ctx context.Context, userID, workspaceID string) error {
	if userID == "" || workspaceID == "" {
		return ErrWorkspaceMemberNotFound
	}
	return r.inTx(ctx, nil, func(tx pgx.Tx) error {
		if err := lockWorkspaceRow(ctx, tx, workspaceID); err != nil {
			return err
		}
		var role string
		err := tx.QueryRow(ctx, `SELECT role FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active' FOR UPDATE`, workspaceID, userID).Scan(&role)
		if noRows(err) {
			return ErrWorkspaceMemberNotFound
		}
		if err != nil {
			return err
		}
		switch role {
		case "owner":
			return ErrWorkspaceOwnerCannotLeave
		case "admin", "member":
		default:
			return ErrAccessDenied
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active'`, workspaceID, userID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM workspace_invitations WHERE workspace_id=$1 AND invitee_user_id=$2`, workspaceID, userID)
		return err
	})
}

func lockWorkspaceRow(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&id)
	if noRows(err) {
		return ErrNotFound
	}
	return err
}

type roleTarget struct {
	found      bool
	invitation bool
	id         string
	role       string
	state      string
}

func lockRoleTarget(ctx context.Context, tx pgx.Tx, workspaceID, userID string) (roleTarget, error) {
	var t roleTarget
	err := tx.QueryRow(ctx, `SELECT id, role, state FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active' FOR UPDATE`, workspaceID, userID).Scan(&t.id, &t.role, &t.state)
	if err == nil {
		t.found = isMemberRole(t.role)
		return t, nil
	}
	if !noRows(err) {
		return t, err
	}
	err = tx.QueryRow(ctx, `SELECT id, requested_role, status FROM workspace_invitations WHERE workspace_id=$1 AND invitee_user_id=$2 ORDER BY created_at DESC, id DESC LIMIT 1 FOR UPDATE`, workspaceID, userID).Scan(&t.id, &t.role, &t.state)
	if noRows(err) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	t.invitation = true
	t.found = t.role == "admin" || t.role == "member"
	return t, nil
}

// TransitionWorkspaceRole changes, revokes, or transfers a member's role.
func (r *Repository) TransitionWorkspaceRole(ctx context.Context, actorUserID, workspaceID, targetUserID, newRole string) (WorkspaceRoleTransition, error) {
	var result WorkspaceRoleTransition
	switch newRole {
	case "admin", "member", "owner", "external":
	default:
		return result, ErrWorkspaceMemberNotFound
	}
	if actorUserID == "" || workspaceID == "" || targetUserID == "" {
		return result, ErrWorkspaceMemberNotFound
	}
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		if err := lockWorkspaceRow(ctx, tx, workspaceID); err != nil {
			return err
		}
		var actorRole string
		err := tx.QueryRow(ctx, `SELECT role FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active' FOR UPDATE`, workspaceID, actorUserID).Scan(&actorRole)
		if err != nil && !noRows(err) {
			return err
		}
		commercial, err := r.commercial(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if !(actorRole == "owner" || (actorRole == "admin" && commercial)) {
			return ErrWorkspaceRolePermissionDenied
		}
		target, err := lockRoleTarget(ctx, tx, workspaceID, targetUserID)
		if err != nil {
			return err
		}
		result = WorkspaceRoleTransition{PreviousRole: target.role, Role: newRole, PreviousState: target.state, Invitation: target.invitation}
		if newRole == "external" {
			switch {
			case !target.found:
				return ErrWorkspaceMemberNotFound
			case targetUserID == actorUserID:
				return ErrWorkspaceMemberCannotRevoke
			case !target.invitation && target.role == "owner":
				return ErrWorkspaceOwnerMustTransfer
			case target.role == "admin" && !commercial:
				return ErrWorkspaceRolePermissionDenied
			}
			if _, err := tx.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active'`, workspaceID, targetUserID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM workspace_invitations WHERE workspace_id=$1 AND invitee_user_id=$2`, workspaceID, targetUserID); err != nil {
				return err
			}
			result.Removed = true
			return nil
		}
		if !commercial {
			return ErrWorkspaceRolePermissionDenied
		}
		if !target.found {
			return ErrWorkspaceMemberNotFound
		}
		if !target.invitation && target.role == "owner" && newRole != "owner" {
			return ErrWorkspaceOwnerMustTransfer
		}
		if newRole == "owner" {
			if target.invitation {
				return ErrWorkspaceTargetMustBeActive
			}
			if actorRole != "owner" {
				return ErrWorkspaceRolePermissionDenied
			}
			if target.role != "owner" {
				rows, err := tx.Query(ctx, `UPDATE workspace_members SET role='admin', updated_at=CURRENT_TIMESTAMP
					WHERE workspace_id=$1 AND state='active' AND role='owner' AND user_id<>$2 RETURNING user_id`, workspaceID, targetUserID)
				if err != nil {
					return err
				}
				demoted, err := pgx.CollectRows(rows, pgx.RowTo[string])
				if err != nil {
					return err
				}
				if len(demoted) != 1 {
					return ErrOwnerMissing
				}
				if _, err := tx.Exec(ctx, `UPDATE workspace_members SET role='owner', updated_at=CURRENT_TIMESTAMP WHERE id=$1`, target.id); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE effective_workspace_quota_states SET owner_user_id=$2, known=false, stale=true, updated_at=CURRENT_TIMESTAMP WHERE workspace_id=$1`, workspaceID, targetUserID); err != nil {
					return err
				}
			}
			result.OwnerTransferred = true
			return nil
		}
		if target.invitation {
			_, err = tx.Exec(ctx, `UPDATE workspace_invitations SET requested_role=$2, updated_at=CURRENT_TIMESTAMP WHERE id=$1`, target.id, newRole)
		} else {
			_, err = tx.Exec(ctx, `UPDATE workspace_members SET role=$2, updated_at=CURRENT_TIMESTAMP WHERE id=$1`, target.id, newRole)
		}
		return err
	})
	if err != nil {
		return WorkspaceRoleTransition{}, err
	}
	return result, nil
}
