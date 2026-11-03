// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"log/slog"

	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func (r *mutationResolver) publishWorkspaceMemberRemovalEffects(ctx context.Context, actorUserID, targetUserID, workspaceID string, transition workspaces.WorkspaceRoleTransition) error {
	if transition.PreviousState == "waiting_review" {
		if r.WorkspaceRoles.Notifications == nil {
			return ErrWorkspaceRoleMutationUnavailable
		}
		notificationID, created, err := r.WorkspaceRoles.Notifications.CreateInvitation(ctx, notifications.InvitationNotification{
			UserID: targetUserID, WorkspaceID: workspaceID, CreatedByUserID: actorUserID, Type: "InvitationReviewDeclined",
		})
		if err != nil {
			return err
		}
		if created && r.WorkspaceRoles.InvitationMailer != nil {
			if err := r.WorkspaceRoles.InvitationMailer.QueueInvitationReviewDeclined(ctx, targetUserID, actorUserID, workspaceID, notificationID); err != nil {
				slog.WarnContext(ctx, "could not queue invitation review declined email", "invitee_id", targetUserID, "workspace_id", workspaceID)
			}
		}
		if created && r.WorkspaceRoles.CountPublisher != nil {
			count, err := r.WorkspaceRoles.Notifications.CountUnread(ctx, targetUserID)
			if err != nil {
				slog.WarnContext(ctx, "load unread notification count after review decline failed", "error", err)
			} else {
				r.WorkspaceRoles.CountPublisher.PublishNotificationCountChanged(targetUserID, count, "created")
			}
		}
	} else if transition.Removed && transition.PreviousState == "active" && r.WorkspaceRoles.RemovalMailer != nil {
		if err := r.WorkspaceRoles.RemovalMailer.QueueMemberRemoved(ctx, targetUserID, workspaceID); err != nil {
			slog.ErrorContext(ctx, "queue member-removal email failed", "error", err)
		}
	}
	return nil
}

func workspaceRoleMutationResolverError(err error, workspaceID string) error {
	switch {
	case errors.Is(err, workspaces.ErrWorkspaceRolePermissionDenied),
		errors.Is(err, workspaces.ErrNotFound),
		errors.Is(err, workspaces.ErrWorkspaceMemberNotFound),
		errors.Is(err, workspaces.ErrWorkspaceMemberCannotRevoke),
		errors.Is(err, workspaces.ErrWorkspaceOwnerMustTransfer),
		errors.Is(err, workspaces.ErrWorkspaceTargetMustBeActive):
		return &WorkspaceRoleMutationError{Cause: err, WorkspaceID: workspaceID}
	default:
		return err
	}
}
