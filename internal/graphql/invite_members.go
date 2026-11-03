// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"log/slog"
	"net/mail"
	"sort"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type inviteCandidate struct {
	index      int
	email      string
	normalized string
	domain     string
}

func (r *mutationResolver) inviteMembers(ctx context.Context, workspaceID string, emails []string) ([]*model.InviteResult, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	dependencies := r.WorkspaceInvitations
	if dependencies.Repository == nil || dependencies.Admission == nil {
		return nil, ErrWorkspaceInviteMutationUnavailable
	}
	allowed, err := dependencies.Repository.CanManageWorkspaceUsers(ctx, principal.User.ID, workspaceID)
	if err != nil {
		return nil, &WorkspaceRoleMutationError{Cause: err, WorkspaceID: workspaceID}
	}
	if !allowed {
		return nil, &WorkspaceMemberPermissionDeniedError{WorkspaceID: workspaceID}
	}
	if len(emails) > 512 {
		return nil, ErrWorkspaceInviteTooMany
	}
	workspace, err := dependencies.Repository.InvitationWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if invitelinks.ContainsURLOrDomain(workspace.Name) {
		return nil, ErrWorkspaceInviteLinkNameForbidden
	}

	results := make([]*model.InviteResult, len(emails))
	candidates := make([]inviteCandidate, 0, len(emails))
	seen := make(map[string]struct{}, len(emails))
	for index, email := range emails {
		result := &model.InviteResult{Email: email}
		results[index] = result
		normalized, valid := normalizeInviteEmail(email)
		if !valid {
			result.Error = map[string]any{
				"status": 400, "code": "Bad Request", "type": "INVALID_INPUT", "name": "INVALID_EMAIL",
				"message": "An invalid email provided: " + email, "data": map[string]any{"email": email},
			}
			continue
		}
		if _, duplicate := seen[normalized]; duplicate {
			result.Error = map[string]any{
				"status": 403, "code": "Forbidden", "type": "ACTION_FORBIDDEN", "name": "ACTION_FORBIDDEN",
				"message": "Duplicate invite email.", "data": nil,
			}
			continue
		}
		seen[normalized] = struct{}{}
		occupied, err := dependencies.Repository.EmailInvitationAlreadyExists(ctx, workspaceID, normalized)
		if err != nil {
			return nil, err
		}
		if occupied {
			result.Error = alreadyInWorkspaceError(workspaceID)
			continue
		}
		_, domain, _ := strings.Cut(normalized, "@")
		candidates = append(candidates, inviteCandidate{index: index, email: email, normalized: normalized, domain: domain})
	}
	if len(candidates) == 0 {
		return results, nil
	}

	domains := inviteTargetDomains(candidates)
	reservationID, err := dependencies.Admission.AdmitWorkspaceInvites(ctx, principal.User.ID, workspaceID, len(candidates), domains)
	if err != nil {
		return nil, err
	}
	settled := false
	defer func() {
		if !settled {
			if releaseErr := dependencies.Admission.ReleaseWorkspaceInviteQuota(ctx, reservationID); releaseErr != nil {
				// The admission remains fail-closed if the quota service cannot release it.
			}
		}
	}()

	requestedEmails := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		requestedEmails = append(requestedEmails, candidate.normalized)
	}
	reservations, err := dependencies.Repository.ReserveEmailInvitationsWithQuota(ctx, principal.User.ID, workspaceID, requestedEmails, reservationID, dependencies.Admission)
	if err != nil {
		if errors.Is(err, workspaces.ErrInvitationNoSeats) || errors.Is(err, workspaces.ErrInvitationAlreadyMember) {
			err = &workspaces.InvitationSpaceError{Cause: err, WorkspaceID: workspaceID}
		}
		return nil, workspaceRoleMutationResolverError(err, workspaceID)
	}
	settled = true
	for i, reservation := range reservations {
		if i >= len(candidates) {
			return nil, errors.New("workspace invitation repository returned extra reservations")
		}
		candidate := candidates[i]
		inviteID := reservation.ID
		results[candidate.index].InviteID = &inviteID
		if dependencies.Notifications != nil {
			notificationID, created, notificationErr := dependencies.Notifications.CreateInvitation(ctx, workspacesNotification(reservation.UserID, workspaceID, principal.User.ID, reservation.ID))
			if notificationErr != nil {
				slog.ErrorContext(ctx, "create workspace invitation notification failed", "workspace_id", workspaceID, "error", notificationErr)
			} else if created {
				if dependencies.NotificationMailer != nil {
					if mailErr := dependencies.NotificationMailer.QueueMemberInvitation(ctx, reservation.UserID, principal.User.ID, workspaceID, reservation.ID, notificationID); mailErr != nil {
						slog.ErrorContext(ctx, "queue workspace invitation mail failed", "workspace_id", workspaceID, "error", mailErr)
					}
				}
				if dependencies.CountPublisher != nil {
					if count, countErr := dependencies.Notifications.CountUnread(ctx, reservation.UserID); countErr != nil {
						slog.ErrorContext(ctx, "read workspace invitee notification count failed", "workspace_id", workspaceID, "error", countErr)
					} else {
						dependencies.CountPublisher.PublishNotificationCountChanged(reservation.UserID, count, "created")
					}
				}
			}
		}
	}
	if len(reservations) > 0 && dependencies.Publisher != nil {
		dependencies.Publisher.PublishWorkspaceMembersUpdated(workspaceID)
	}
	return results, nil
}

func workspacesNotification(userID, workspaceID, actorID, inviteID string) notifications.InvitationNotification {
	return notifications.InvitationNotification{
		UserID: userID, WorkspaceID: workspaceID, CreatedByUserID: actorID, InviteID: inviteID, Type: "Invitation",
	}
}

func normalizeInviteEmail(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || strings.ContainsAny(value, " \t\r\n") || strings.Count(value, "@") != 1 {
		return "", false
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value {
		return "", false
	}
	local, domain, found := strings.Cut(value, "@")
	if !found || local == "" || domain == "" || !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", false
	}
	return value, true
}

func alreadyInWorkspaceError(workspaceID string) model.JSONObject {
	return map[string]any{
		"status": 403, "code": "Forbidden", "type": "ACTION_FORBIDDEN", "name": "ALREADY_IN_SPACE",
		"message": "You have already joined in Space " + workspaceID + ".", "data": map[string]any{"spaceId": workspaceID},
	}
}

func inviteTargetDomains(candidates []inviteCandidate) []WorkspaceInviteQuotaTargetDomain {
	counts := make(map[string]int)
	for _, candidate := range candidates {
		counts[candidate.domain]++
	}
	domains := make([]WorkspaceInviteQuotaTargetDomain, 0, len(counts))
	for domain, count := range counts {
		domains = append(domains, WorkspaceInviteQuotaTargetDomain{Domain: domain, Count: count})
	}
	sort.Slice(domains, func(i, j int) bool { return domains[i].Domain < domains[j].Domain })
	return domains
}
