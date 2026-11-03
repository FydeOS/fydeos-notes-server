// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"log/slog"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

var ErrWorkspaceInvitationAcceptanceUnavailable = errors.New("workspace invitation acceptance is unavailable")

func (r *mutationResolver) approveMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return false, ErrAuthenticationRequired
	}
	dependencies := r.InvitationAcceptance
	if dependencies.Repository == nil || dependencies.Notifications == nil {
		return false, ErrWorkspaceInvitationAcceptanceUnavailable
	}
	allowed, err := dependencies.Repository.CanManageWorkspaceUsers(ctx, principal.User.ID, workspaceID)
	if err != nil {
		return false, &WorkspaceRoleMutationError{Cause: err, WorkspaceID: workspaceID}
	}
	if !allowed {
		return false, &WorkspaceMemberPermissionDeniedError{WorkspaceID: workspaceID}
	}
	status, err := dependencies.Repository.InvitationStatus(ctx, workspaceID, userID)
	if err != nil {
		return false, err
	}
	if status == nil {
		return false, &WorkspaceRoleMutationError{Cause: workspaces.ErrWorkspaceMemberNotFound, WorkspaceID: workspaceID}
	}
	if *status != "UnderReview" {
		return true, nil
	}

	approved, err := dependencies.Repository.ApproveReviewInvitation(ctx, principal.User.ID, workspaceID, userID)
	if err != nil {
		if errors.Is(err, workspaces.ErrInvitationNoSeats) || errors.Is(err, workspaces.ErrInvitationAlreadyMember) {
			return false, invitationErrorForWorkspace(err, workspaceID)
		}
		if errors.Is(err, workspaces.ErrWorkspaceRolePermissionDenied) || errors.Is(err, workspaces.ErrNotFound) || errors.Is(err, workspaces.ErrWorkspaceMemberNotFound) {
			return false, &WorkspaceRoleMutationError{Cause: err, WorkspaceID: workspaceID}
		}
		return false, err
	}
	r.publishWorkspaceMembersUpdated(approved.WorkspaceID)
	notificationID, created, err := dependencies.Notifications.CreateInvitation(ctx, notifications.InvitationNotification{
		UserID: approved.InviteeID, WorkspaceID: approved.WorkspaceID,
		CreatedByUserID: principal.User.ID, InviteID: approved.InviteID, Type: "InvitationReviewApproved",
	})
	if err != nil {
		return false, err
	}
	if created {
		if dependencies.InvitationMailer != nil {
			if err := dependencies.InvitationMailer.QueueInvitationReviewApproved(ctx, approved.InviteeID, principal.User.ID, approved.WorkspaceID, notificationID); err != nil {
				slog.WarnContext(ctx, "could not queue invitation review approval email", "workspace_id", approved.WorkspaceID, "invitee_id", approved.InviteeID)
			}
		}
		r.publishInvitationNotificationCount(ctx, approved.InviteeID)
	}
	return true, nil
}

func (r *mutationResolver) acceptInviteByID(ctx context.Context, inviteID string) (bool, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return false, ErrAuthenticationRequired
	}
	dependencies := r.InvitationAcceptance
	if dependencies.Repository == nil || dependencies.LinkLookup == nil || dependencies.Notifications == nil {
		return false, ErrWorkspaceInvitationAcceptanceUnavailable
	}

	record, err := dependencies.Repository.InvitationByID(ctx, inviteID)
	if err == nil {
		if record.InviteeID != principal.User.ID {
			return false, workspaces.ErrInvitationAccountMismatch
		}
		accepted, err := dependencies.Repository.ActivateEmailInvitation(ctx, record.WorkspaceID, inviteID, principal.User.ID)
		if err != nil {
			return false, invitationErrorForWorkspace(err, record.WorkspaceID)
		}
		r.publishWorkspaceMembersUpdated(accepted.WorkspaceID)
		_, created, err := dependencies.Notifications.CreateInvitation(ctx, notifications.InvitationNotification{
			UserID: accepted.InviterID, WorkspaceID: accepted.WorkspaceID,
			CreatedByUserID: accepted.InviteeID, InviteID: accepted.InviteID, Type: "InvitationAccepted",
		})
		if err != nil {
			return false, err
		}
		if created {
			r.publishInvitationNotificationCount(ctx, accepted.InviterID)
		}
		return true, nil
	}
	if !errors.Is(err, workspaces.ErrInvitationNotFound) {
		return false, err
	}

	target, err := dependencies.LinkLookup.LookupByID(ctx, inviteID)
	if err != nil {
		return false, err
	}
	if target == nil || target.WorkspaceID == "" {
		return false, workspaces.ErrInvitationInvalid
	}
	status, err := dependencies.Repository.InvitationStatus(ctx, target.WorkspaceID, principal.User.ID)
	if err != nil {
		return false, err
	}
	if status != nil {
		if *status != "Pending" {
			return false, invitationErrorForWorkspace(workspaces.ErrInvitationAlreadyMember, target.WorkspaceID)
		}
		accepted, err := dependencies.Repository.ActivatePendingInvitation(ctx, target.WorkspaceID, principal.User.ID)
		if err != nil {
			return false, invitationErrorForWorkspace(err, target.WorkspaceID)
		}
		r.publishWorkspaceMembersUpdated(accepted.WorkspaceID)
		_, created, err := dependencies.Notifications.CreateInvitation(ctx, notifications.InvitationNotification{
			UserID: accepted.InviterID, WorkspaceID: accepted.WorkspaceID,
			CreatedByUserID: accepted.InviteeID, InviteID: accepted.InviteID, Type: "InvitationAccepted",
		})
		if err != nil {
			return false, err
		}
		if created {
			r.publishInvitationNotificationCount(ctx, accepted.InviterID)
		}
		return true, nil
	}

	reserved, err := dependencies.Repository.ReserveLinkReviewSeat(ctx, target.WorkspaceID, principal.User.ID, target.InviterID)
	if err != nil {
		return false, invitationErrorForWorkspace(err, target.WorkspaceID)
	}
	reviewers, err := dependencies.Repository.InvitationReviewers(ctx, target.WorkspaceID)
	if err != nil {
		return false, err
	}
	for _, reviewerID := range reviewers {
		notificationID, created, err := dependencies.Notifications.CreateInvitation(ctx, notifications.InvitationNotification{
			UserID: reviewerID, WorkspaceID: reserved.WorkspaceID,
			CreatedByUserID: reserved.InviteeID, InviteID: reserved.InviteID, Type: "InvitationReviewRequest",
		})
		if err != nil {
			slog.WarnContext(ctx, "could not create invitation review notification", "reviewer_id", reviewerID)
			continue
		}
		if created {
			if dependencies.InvitationMailer != nil {
				if err := dependencies.InvitationMailer.QueueInvitationReviewRequest(ctx, reviewerID, reserved.InviteeID, reserved.WorkspaceID, notificationID); err != nil {
					slog.WarnContext(ctx, "could not queue invitation review request email", "reviewer_id", reviewerID, "workspace_id", reserved.WorkspaceID)
				}
			}
			r.publishInvitationNotificationCount(ctx, reviewerID)
		}
	}
	r.publishWorkspaceMembersUpdated(reserved.WorkspaceID)
	return true, nil
}

func invitationErrorForWorkspace(err error, workspaceID string) error {
	if errors.Is(err, workspaces.ErrInvitationNoSeats) || errors.Is(err, workspaces.ErrInvitationAlreadyMember) {
		return &workspaces.InvitationSpaceError{Cause: err, WorkspaceID: workspaceID}
	}
	return err
}

func (r *mutationResolver) publishWorkspaceMembersUpdated(workspaceID string) {
	if r.InvitationAcceptance.MembersPublisher != nil {
		r.InvitationAcceptance.MembersPublisher.PublishWorkspaceMembersUpdated(workspaceID)
	}
}

func (r *mutationResolver) publishInvitationNotificationCount(ctx context.Context, userID string) {
	dependencies := r.InvitationAcceptance
	if dependencies.CountPublisher == nil || dependencies.Notifications == nil {
		return
	}
	count, err := dependencies.Notifications.CountUnread(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "could not count unread invitation notifications", "user_id", userID)
		return
	}
	dependencies.CountPublisher.PublishNotificationCountChanged(userID, count, "created")
}
