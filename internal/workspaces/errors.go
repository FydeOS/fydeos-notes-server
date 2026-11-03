// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import "errors"

var (
	ErrNotFound                       = errors.New("workspace not found")
	ErrAccessDenied                   = errors.New("workspace access denied")
	ErrOwnerMissing                   = errors.New("workspace owner is missing")
	ErrQuotaStateUnknown              = errors.New("workspace quota state is unavailable")
	ErrPermissionGenerationChanged    = errors.New("workspace permission generation changed during authorization")
	ErrStorageLockTimeout             = errors.New("workspace storage lock timed out")
	ErrInvalidCalendarRefreshInterval = errors.New("invalid calendar refresh interval")
	ErrAuthorizationUnavailable       = errors.New("workspace authorization is unavailable")
	ErrMemberQueryTooLong             = errors.New("workspace member query is too long")
	ErrWorkspaceMemberNotFound        = errors.New("workspace member not found")
	ErrWorkspaceRolePermissionDenied  = errors.New("workspace role mutation denied")
	ErrWorkspaceOwnerMustTransfer     = errors.New("workspace owner must transfer ownership")
	ErrWorkspaceOwnerCannotLeave      = errors.New("workspace owner cannot leave")
	ErrWorkspaceTargetMustBeActive    = errors.New("workspace role target must be an active member")
	ErrWorkspaceMemberCannotRevoke    = errors.New("cannot revoke yourself")
	ErrInvitationNotFound             = errors.New("invitation not found")
	ErrInvitationAccountMismatch      = errors.New("invitation belongs to another account")
	ErrInvitationOwnerNotFound        = errors.New("invitation workspace owner not found")
	ErrInvitationInvalid              = errors.New("invitation is invalid or no longer activatable")
	ErrInvitationAlreadyMember        = errors.New("user is already in workspace")
	ErrInvitationNoSeats              = errors.New("workspace has no available seats")
	ErrDocRoleDenied                  = errors.New("document role mutation denied")
	ErrDocRoleInvalid                 = errors.New("invalid document role mutation")
	ErrDocOwnerMustTransfer           = errors.New("doc_owner_must_transfer")
	ErrDocOwnerTargetInactive         = errors.New("target_member_not_active")
	ErrDocQuotaUnavailable            = errors.New("document role quota guard unavailable")
	ErrDocRoleInputLimit              = errors.New("document role input exceeds resource limits")
	ErrDocumentNotFound               = errors.New("document not found")
	ErrDocumentNotPublic              = errors.New("document is not public")
	ErrPreviewYjsHistoryTooLarge      = errors.New("renderer preview Yjs history exceeds resource limits")
	ErrPreviewProjectionBusy          = errors.New("renderer preview projection capacity is exhausted")
	ErrInvalidDocAnalyticsCursor      = errors.New("invalid pagination cursor")
	ErrDocAnalyticsQueryTooLong       = errors.New("document member query is too long")
	ErrInvalidAdminSharedLinksCursor  = errors.New("invalid admin shared links pagination cursor")
	ErrAdminSharedLinksQueryTooLong   = errors.New("admin shared links query is too long")
)

// InvitationSpaceError ties an invitation failure to the workspace it
// happened in.
type InvitationSpaceError struct {
	Cause       error
	WorkspaceID string
}

func (e *InvitationSpaceError) Error() string { return e.Cause.Error() }

func (e *InvitationSpaceError) Unwrap() error { return e.Cause }
