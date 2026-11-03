// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/appconfig"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/blobs"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/calendar"
	"github.com/fydeos/fydeos-notes-server/internal/comments"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/maildelivery"
	"github.com/fydeos/fydeos-notes-server/internal/mcpcredentials"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/serverconfig"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/upgrades"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

//go:generate go tool gqlgen generate

var ErrAuthenticationRequired = errors.New("You must sign in first to access this resource.")
var ErrUserQuotaUnavailable = errors.New("User quota is unavailable.")

type CalDAVAccountLinker interface {
	LinkCalDAVAccount(context.Context, string, calendar.LinkInput) (calendar.Account, error)
}

type GoogleCalendarAccountLinker interface {
	CreateAuthorizationURL(context.Context, string, *string) (string, error)
}

type CalendarProviderCatalog interface {
	AvailableCalendarProviders(context.Context) ([]string, error)
}

type WorkspaceRepository interface {
	CreateForUser(context.Context, string) (workspaces.Workspace, error)
	ListForUser(context.Context, string) ([]workspaces.Workspace, error)
	GetForUser(context.Context, string, string) (workspaces.Workspace, error)
	ReadPresentationMetadata(context.Context, string) (workspaces.PresentationMetadata, error)
	ReadQuota(context.Context, string) (workspaces.Quota, error)
	HasCommonActiveWorkspace(context.Context, string, string) (bool, error)
	UpsertRootDocument(context.Context, string, string, []byte) error
	UpdateForUser(context.Context, string, string, workspaces.SettingsUpdate) (workspaces.Workspace, error)
	DeleteForUser(context.Context, string, string) ([]string, error)
}

func (r *Resolver) workspacePresentationMetadata(ctx context.Context, workspaceID string) (workspaces.PresentationMetadata, error) {
	if r.WorkspaceRepo == nil {
		return workspaces.PresentationMetadata{}, ErrWorkspaceRepositoryUnavailable
	}
	return r.WorkspaceRepo.ReadPresentationMetadata(ctx, workspaceID)
}

type WorkspaceRolePermissionsReader interface {
	GetWorkspaceRolePermissions(context.Context, string, string) (workspaces.WorkspaceAccess, error)
}

type WorkspaceMemberReader interface {
	MemberCount(context.Context, string) (int, error)
	ListMembersForUser(context.Context, string, string, string, int, int) ([]workspaces.Member, error)
}

type WorkspaceQuotaReader interface {
	ReadQuota(context.Context, string) (workspaces.Quota, error)
}

type WorkspaceQuotaStateReader interface {
	ReadWorkspaceQuotaState(context.Context, string) (workspaces.WorkspaceQuotaState, error)
}

type UserQuotaStateReader interface {
	ReadUserQuotaState(context.Context, string) (workspaces.UserQuotaState, error)
}

type WorkspaceEntitlementChangePublisher interface {
	PublishWorkspaceEntitlementChanged(string)
}

type UserQuotaStateChangePublisher interface {
	PublishUserQuotaStateChanged(string, string)
}

type WorkspaceUsersManager interface {
	CanManageWorkspaceUsers(context.Context, string, string) (bool, error)
}

type WorkspaceInviteLinkReader interface {
	Get(context.Context, string) (*invitelinks.Link, error)
}

type WorkspaceInviteLinkRevoker interface {
	Revoke(context.Context, string) bool
}

type WorkspaceInviteLinkCreator interface {
	Create(context.Context, string, string, string) (*invitelinks.Link, error)
}

type WorkspaceInviteLinkPublisher interface {
	PublishWorkspaceInviteLinkChanged(string, string)
}

type WorkspaceInvitationInfoRepository interface {
	InvitationByID(context.Context, string) (workspaces.InvitationRecord, error)
	InvitationStatus(context.Context, string, string) (*string, error)
	InvitationWorkspace(context.Context, string) (workspaces.InvitationWorkspace, error)
}

type WorkspaceInviteLinkByIDReader interface {
	LookupByID(context.Context, string) (*invitelinks.InvitationTarget, error)
}

type WorkspaceAvatarReader interface {
	GetObject(context.Context, string, string) (io.ReadCloser, storage.ObjectMetadata, error)
}

type WorkspaceObjectCleaner interface {
	DeleteWorkspaceObjects(context.Context, string, []string) (int64, error)
}

type WorkspaceBlobManager interface {
	ListManaged(context.Context, string, string) ([]blobs.Blob, error)
	Delete(context.Context, string, string, string, bool) (bool, error)
	ReleaseDeletedBatch(context.Context, string, string, int) (int, bool, error)
}

type WorkspaceBlobUploader interface {
	CreateBlobUpload(context.Context, string, string, string, int64, string) (blobs.UploadInit, error)
	BlobUploadPartURL(context.Context, string, string, string, string, int32) (blobs.UploadPart, error)
	SetBlob(context.Context, string, string, string, string, io.Reader) (string, error)
	UploadCommentAttachment(context.Context, string, string, string, string, string, io.Reader) (string, error)
	CompleteBlobUpload(context.Context, string, string, string, *string, []storage.CompletedPart) (string, error)
	AbortBlobUpload(context.Context, string, string, string, string) (bool, error)
}

type WorkspaceBlobService interface {
	WorkspaceBlobManager
	WorkspaceBlobUploader
}

type UserService interface {
	Get(context.Context, string) (*users.User, error)
	GetPublic(context.Context, string) (*users.User, error)
	GetVisibleByEmail(context.Context, string, string) (*users.User, error)
	UpdateProfile(context.Context, string, *string) (*users.User, error)
	UpdateAvatar(context.Context, string, *string) (*users.User, error)
	GetSettings(context.Context, string) (users.UserSettings, error)
	UpdateSettings(context.Context, string, users.UserSettingsUpdate) (users.UserSettings, error)
}

type CopilotWorkspaceArtifactManager interface {
	EmbeddingAvailable(context.Context) bool
	Add(context.Context, string, string, string, io.Reader) (copilot.WorkspaceArtifact, error)
	List(context.Context, string, int, int) ([]copilot.WorkspaceArtifact, int, error)
	Remove(context.Context, string, string) (bool, error)
}

var ErrTranscriptTaskNotFound = errors.New("copilot_transcription_job_not_found")
var ErrTranscriptTaskFailed = errors.New("failed transcript tasks cannot be settled")

type MCPCredentialManager interface {
	CopilotEnabled(context.Context) (bool, error)
	ReadWriteAvailable() bool
	List(context.Context, string, string) ([]mcpcredentials.Credential, error)
	Create(context.Context, mcpcredentials.CreateInput) (mcpcredentials.Issued, error)
	Rotate(context.Context, string, string, string, int) (mcpcredentials.Issued, error)
	Revoke(context.Context, string, string, string) (bool, error)
}

type AvatarUploader interface {
	Upload(context.Context, string, string, io.Reader) (*users.User, error)
}

type UserManagementService interface {
	CreateAdminUser(context.Context, users.CreateInput) (*users.User, error)
	UpdateAdminUser(context.Context, string, users.AdminUpdateInput) (*users.User, bool, error)
	GetAdminByID(context.Context, string) (*users.User, error)
	GetAdminByEmail(context.Context, string) (*users.User, error)
	ListUsers(context.Context, users.UserListFilter) ([]*users.User, error)
	CountUsers(context.Context, users.UserListFilter) (int, error)
	UpdateUserFeatures(context.Context, string, []string) (bool, error)
	EnableUser(context.Context, string) (*users.User, error)
	DeleteAdminUser(context.Context, string) error
	BanAdminUser(context.Context, string) (*users.User, error)
}

type OwnAccountDeleter interface {
	DeleteOwnAccount(context.Context, string) error
}

type CommentReadRepository interface {
	Count(context.Context, string, string) (int, error)
	List(context.Context, string, string, *int64, int) ([]comments.Comment, error)
	ListChanges(context.Context, string, string, *time.Time, *time.Time, int) ([]comments.Change, error)
}

type CommentWriteRepository interface {
	CreateComment(context.Context, string, string, string, map[string]any, comments.NotificationInput) (comments.Comment, error)
	UpdateComment(context.Context, string, string, map[string]any) (comments.Comment, error)
	ResolveComment(context.Context, string, string, bool) (comments.Comment, error)
	DeleteComment(context.Context, string, string) (comments.Comment, error)
	CreateReply(context.Context, string, string, map[string]any, comments.NotificationInput) (comments.Reply, error)
	UpdateReply(context.Context, string, string, map[string]any) (comments.Reply, error)
	DeleteReply(context.Context, string, string) (comments.Reply, error)
}

type CommentChangePublisher interface {
	PublishCommentChanged(string, string)
}

type CommentDocumentAuthorizer interface {
	AuthorizeDocument(context.Context, string, string, string) (workspaces.DocumentAuthorization, error)
}

type CommentReadDependencies struct {
	Repository CommentReadRepository
	Writer     CommentWriteRepository
	Authorizer CommentDocumentAuthorizer
	Publisher  CommentChangePublisher
}

type UserNotificationRepository interface {
	CountUnread(context.Context, string) (int, error)
	ListUnread(context.Context, string, *time.Time, int, int) ([]notifications.Notification, error)
	CreateMention(context.Context, string, string, string, map[string]any) (string, error)
	MarkAsRead(context.Context, string, string) error
	MarkAllAsRead(context.Context, string) error
}

type NotificationCountPublisher interface {
	PublishNotificationCountChanged(string, int, string)
}

type NotificationDependencies struct {
	Repository UserNotificationRepository
	Authorizer CommentDocumentAuthorizer
	Publisher  NotificationCountPublisher
}

type WorkspaceInvitationAcceptanceRepository interface {
	CanManageWorkspaceUsers(context.Context, string, string) (bool, error)
	InvitationByID(context.Context, string) (workspaces.InvitationRecord, error)
	InvitationStatus(context.Context, string, string) (*string, error)
	ActivateEmailInvitation(context.Context, string, string, string) (workspaces.InvitationAcceptance, error)
	ActivatePendingInvitation(context.Context, string, string) (workspaces.InvitationAcceptance, error)
	ApproveReviewInvitation(context.Context, string, string, string) (workspaces.InvitationAcceptance, error)
	ReserveLinkReviewSeat(context.Context, string, string, string) (workspaces.InvitationAcceptance, error)
	InvitationReviewers(context.Context, string) ([]string, error)
}

type WorkspaceInvitationNotificationRepository interface {
	CreateInvitation(context.Context, notifications.InvitationNotification) (string, bool, error)
	CountUnread(context.Context, string) (int, error)
}

type WorkspaceMembersPublisher interface {
	PublishWorkspaceMembersUpdated(string)
}

type WorkspaceInvitationMailer interface {
	QueueInvitationReviewRequest(context.Context, string, string, string, string) error
	QueueInvitationReviewApproved(context.Context, string, string, string, string) error
	QueueInvitationReviewDeclined(context.Context, string, string, string, string) error
}

type WorkspaceRoleMutationRepository interface {
	TransitionWorkspaceRole(context.Context, string, string, string, string) (workspaces.WorkspaceRoleTransition, error)
	LeaveWorkspace(context.Context, string, string) error
}

type WorkspaceMemberRemovalMailer interface {
	QueueMemberRemoved(context.Context, string, string) error
}

type WorkspaceMemberLeaveMailer interface {
	QueueMemberLeave(context.Context, string, string) error
}

type WorkspaceConfigPublisher interface {
	PublishWorkspaceConfigUpdated(string)
}

type WorkspaceInvitationAcceptanceDependencies struct {
	Repository       WorkspaceInvitationAcceptanceRepository
	LinkLookup       WorkspaceInviteLinkByIDReader
	Notifications    WorkspaceInvitationNotificationRepository
	InvitationMailer WorkspaceInvitationMailer
	CountPublisher   NotificationCountPublisher
	MembersPublisher WorkspaceMembersPublisher
}

type WorkspaceInviteMutationRepository interface {
	CanManageWorkspaceUsers(context.Context, string, string) (bool, error)
	InvitationWorkspace(context.Context, string) (workspaces.InvitationWorkspace, error)
	EmailInvitationAlreadyExists(context.Context, string, string) (bool, error)
	ReserveEmailInvitationsWithQuota(context.Context, string, string, []string, string, workspaces.EmailInvitationQuotaSettlement) ([]workspaces.EmailInvitationReservation, error)
}

type WorkspaceInviteQuotaTargetDomain = invitelinks.WorkspaceInviteQuotaTargetDomain

type WorkspaceInviteQuotaAdmission interface {
	AdmitWorkspaceInvites(context.Context, string, string, int, []WorkspaceInviteQuotaTargetDomain) (string, error)
	ReleaseWorkspaceInviteQuota(context.Context, string) error
	workspaces.EmailInvitationQuotaSettlement
}

type WorkspaceInviteMutationDependencies struct {
	Repository         WorkspaceInviteMutationRepository
	Admission          WorkspaceInviteQuotaAdmission
	Notifications      WorkspaceInvitationNotificationRepository
	NotificationMailer WorkspaceMemberInvitationMailer
	CountPublisher     NotificationCountPublisher
	Publisher          WorkspaceMembersPublisher
}

type WorkspaceMemberInvitationMailer interface {
	QueueMemberInvitation(context.Context, string, string, string, string, string) error
}

type WorkspaceRoleMutationDependencies struct {
	Repository       WorkspaceRoleMutationRepository
	Notifications    WorkspaceInvitationNotificationRepository
	InvitationMailer WorkspaceInvitationMailer
	RemovalMailer    WorkspaceMemberRemovalMailer
	LeaveMailer      WorkspaceMemberLeaveMailer
	CountPublisher   NotificationCountPublisher
	Publisher        WorkspaceMembersPublisher
}

type HandlerDependencies struct {
	CalDAVAccountLinker         CalDAVAccountLinker
	GoogleCalendarAccountLinker GoogleCalendarAccountLinker
	CalendarProviderCatalog     CalendarProviderCatalog
	CopilotSessions             *copilot.Store
	CopilotAttachments          *copilot.SessionAttachments
	CopilotWorkspaceArtifacts   CopilotWorkspaceArtifactManager
	CopilotTranscriptTasks      *copilot.TranscriptTasks
	TranscriptTaskPublisher     TranscriptTaskPublisher
	SearchDocs                  *search.DocsService
	SearchProjection            *search.ProjectionService
	BYOKProfiles                *byok.ProfileStore
	AdminEntitlements           *entitlements.AdminGrantStore
	ServerConfig                *serverconfig.Service
	LicenseWorkspacePublisher   WorkspaceEntitlementChangePublisher
	LicenseQuotaPublisher       UserQuotaStateChangePublisher
	CopilotQuotaPublisher       UserQuotaStateChangePublisher
	MCPCredentials              MCPCredentialManager
	OAuthProviders              OAuthProviderCatalog
	DocumentReader              DocumentReader
	DocumentAnalytics           DocumentAnalyticsReader
	DocumentHistory             DocumentHistoryDependencies
	DocumentPublications        DocumentPublicationDependencies
	DocumentRoles               DocumentRoleDependencies
	UpgradeChecker              upgrades.Checker
	AvatarUploader              AvatarUploader
	UserSettings                UserSettingsPublisher
	Comments                    CommentReadDependencies
	WorkspaceConfig             WorkspaceConfigPublisher
	WorkspacePermissionReader   WorkspaceRolePermissionsReader
	Notifications               NotificationDependencies
	InviteLinks                 WorkspaceInviteLinkDependencies
	InviteInfo                  WorkspaceInvitationInfoDependencies
	InvitationAcceptance        WorkspaceInvitationAcceptanceDependencies
	WorkspaceInvitations        WorkspaceInviteMutationDependencies
	WorkspaceRoles              WorkspaceRoleMutationDependencies
	SecurityChallenges          SecurityChallengeDependencies
	AuthSigningKeys             AuthSigningKeyManager
	AppConfig                   AppConfigAdminManager
	SMTPTestEmailSender         SMTPTestEmailSender
	MailDeliveryAnalytics       MailDeliveryAnalyticsReader
}

type SecurityChallengeService interface {
	PrepareSecurityChallenge(context.Context, *http.Request, string, string, string) (bool, error)
	CreateSecurityURL(context.Context, *http.Request, string, string) (string, error)
	PrepareVerifyChangeEmail(context.Context, *http.Request, string, string, string, string) (bool, error)
	CompletePasswordChallenge(context.Context, string, string, string) (bool, error)
	CompleteEmailChallenge(context.Context, string, string, string) (bool, error)
	CompleteVerifyEmailChallenge(context.Context, string, string) (bool, error)
}

type SecurityChallengeDependencies struct {
	Service SecurityChallengeService
}

type WorkspaceInviteLinkDependencies struct {
	Reader     WorkspaceInviteLinkReader
	Creator    WorkspaceInviteLinkCreator
	Revoker    WorkspaceInviteLinkRevoker
	Authorizer WorkspaceUsersManager
	Publisher  WorkspaceInviteLinkPublisher
}

type WorkspaceInvitationInfoDependencies struct {
	Repository   WorkspaceInvitationInfoRepository
	LinkLookup   WorkspaceInviteLinkByIDReader
	AvatarReader WorkspaceAvatarReader
}

type RequestAuthenticator interface {
	FromRequest(context.Context, *http.Request) (*auth.Principal, error)
}

type ProfileChangePublisher interface {
	PublishUserProfileChanged(string)
}

type userUpdatePublisher interface {
	PublishUserUpdated(context.Context, string)
}

func publishUserUpdated(ctx context.Context, publisher ProfileChangePublisher, userID string) {
	if publisher == nil {
		return
	}
	if updater, ok := publisher.(userUpdatePublisher); ok {
		updater.PublishUserUpdated(ctx, userID)
		return
	}
	publisher.PublishUserProfileChanged(userID)
}

type UserSettingsPublisher interface {
	PublishUserSettingsChanged(string)
}

type TranscriptTaskPublisher interface {
	PublishTranscriptTaskChanged(string, string, string, string)
}

type AuthSigningKeyManager interface {
	Metadata(context.Context) ([]auth.AuthSigningKeyMetadata, error)
	Rotate(context.Context, string, string) ([]auth.AuthSigningKeyMetadata, error)
	Delete(context.Context, string, string) ([]auth.AuthSigningKeyMetadata, error)
}

type Resolver struct {
	CalDAVAccountLinker         CalDAVAccountLinker
	GoogleCalendarAccountLinker GoogleCalendarAccountLinker
	CalendarProviderCatalog     CalendarProviderCatalog
	CopilotSessions             *copilot.Store
	CopilotAttachments          *copilot.SessionAttachments
	CopilotWorkspaceArtifacts   CopilotWorkspaceArtifactManager
	CopilotTranscriptTasks      *copilot.TranscriptTasks
	TranscriptTaskPublisher     TranscriptTaskPublisher
	SearchDocs                  *search.DocsService
	SearchProjection            *search.ProjectionService
	BYOKProfiles                *byok.ProfileStore
	AdminEntitlements           *entitlements.AdminGrantStore
	MailDeliveryAnalytics       MailDeliveryAnalyticsReader
	ServerConfig                *serverconfig.Service
	LicenseWorkspacePublisher   WorkspaceEntitlementChangePublisher
	LicenseQuotaPublisher       UserQuotaStateChangePublisher
	CopilotQuotaPublisher       UserQuotaStateChangePublisher
	MCPCredentials              MCPCredentialManager
	OAuthProviders              OAuthProviderCatalog
	DocumentReader              DocumentReader
	DocumentAnalytics           DocumentAnalyticsReader
	DocumentHistory             DocumentHistoryDependencies
	DocumentPublications        DocumentPublicationDependencies
	DocumentRoles               DocumentRoleDependencies
	Config                      config.Config
	UpgradeChecker              upgrades.Checker
	UserService                 UserService
	AvatarUploader              AvatarUploader
	UserManagement              UserManagementService
	OwnAccountDeleter           OwnAccountDeleter
	WorkspaceRepo               WorkspaceRepository
	WorkspacePermissionReader   WorkspaceRolePermissionsReader
	WorkspaceMembers            WorkspaceMemberReader
	WorkspaceQuotaReader        WorkspaceQuotaReader
	WorkspaceQuotaStateReader   WorkspaceQuotaStateReader
	UserQuotaStateReader        UserQuotaStateReader
	WorkspaceConfigPublisher    WorkspaceConfigPublisher
	InviteLinkReader            WorkspaceInviteLinkReader
	InviteLinkCreator           WorkspaceInviteLinkCreator
	InviteLinkRevoker           WorkspaceInviteLinkRevoker
	InviteLinkAuthorizer        WorkspaceUsersManager
	InviteLinkPublisher         WorkspaceInviteLinkPublisher
	InviteInfoRepository        WorkspaceInvitationInfoRepository
	InviteLinkLookup            WorkspaceInviteLinkByIDReader
	InvitationAcceptance        WorkspaceInvitationAcceptanceDependencies
	WorkspaceInvitations        WorkspaceInviteMutationDependencies
	WorkspaceRoles              WorkspaceRoleMutationDependencies
	WorkspaceAvatarReader       WorkspaceAvatarReader
	WorkspaceCleaner            WorkspaceObjectCleaner
	BlobManager                 WorkspaceBlobManager
	BlobUploader                WorkspaceBlobUploader
	ProfilePublisher            ProfileChangePublisher
	UserSettingsPublisher       UserSettingsPublisher
	CommentReader               CommentReadRepository
	CommentWriter               CommentWriteRepository
	CommentAuth                 CommentDocumentAuthorizer
	CommentPublisher            CommentChangePublisher
	Notifications               UserNotificationRepository
	NotificationAuth            CommentDocumentAuthorizer
	NotificationPublisher       NotificationCountPublisher
	SecurityChallenges          SecurityChallengeService
	AuthSigningKeys             AuthSigningKeyManager
	AppConfig                   AppConfigAdminManager
	SMTPTestEmailSender         SMTPTestEmailSender
}

type AppConfigAdminManager interface {
	Read(context.Context) (appconfig.Snapshot, error)
	Validate([]appconfig.Update) []appconfig.Validation
	Update(context.Context, string, []appconfig.Update) (map[string]any, error)
}

type SMTPTestEmailSender interface {
	SendTestEmail(context.Context, maildelivery.SMTPConfig, string) error
}

type MailDeliveryAnalyticsReader interface {
	AdminAggregate(context.Context, time.Time, time.Time, string) ([]maildelivery.AdminAggregate, error)
}

type OAuthProviderCatalog interface {
	AvailableProviders(context.Context) ([]string, error)
}

var ErrWorkspaceStorageCleanupUnavailable = errors.New("workspace storage cleanup is unavailable")
var ErrAdminAccessDenied = errors.New("admin access forbidden")
var ErrWorkspaceMembersUnavailable = errors.New("workspace member service is unavailable")
var ErrWorkspaceQuotaUnavailable = errors.New("workspace quota service is unavailable")
var ErrWorkspaceRepositoryUnavailable = errors.New("workspace repository is unavailable")
var ErrWorkspaceInvitationInfoUnavailable = errors.New("workspace invitation info service is unavailable")
var ErrWorkspaceRolePermissionsUnavailable = errors.New("workspace role permissions are unavailable")
var ErrWorkspacePermissionsDenied = errors.New("workspace permissions denied")
var ErrWorkspacePermissionProjectionIncomplete = errors.New("workspace permission projection is incomplete")
var ErrWorkspaceRoleMutationUnavailable = errors.New("workspace role mutation service is unavailable")
var ErrWorkspaceInviteMutationUnavailable = errors.New("workspace invitation service is unavailable")
var ErrWorkspaceInviteTooMany = errors.New("too many workspace invitation targets")

type WorkspaceRoleMutationError struct {
	Cause       error
	WorkspaceID string
}

type WorkspacePermissionsDeniedError struct {
	WorkspaceID string
}

func (e *WorkspacePermissionsDeniedError) Error() string {
	return ErrWorkspacePermissionsDenied.Error()
}

func (e *WorkspacePermissionsDeniedError) Unwrap() error {
	return ErrWorkspacePermissionsDenied
}

func (e *WorkspaceRoleMutationError) Error() string {
	return e.Cause.Error()
}

func (e *WorkspaceRoleMutationError) Unwrap() error {
	return e.Cause
}

var ErrWorkspaceInviteLinkUnavailable = errors.New("workspace invite link service is unavailable")
var ErrWorkspaceInviteLinkForbidden = errors.New("workspace invite link creation is forbidden")
var ErrWorkspaceInviteLinkNameForbidden = errors.New("workspace name contains a URL or domain")
var ErrWorkspaceMemberQueryTooLong = errors.New("workspace member query is too long")
var ErrWorkspaceMemberPermissionDenied = errors.New("workspace member access denied")
var ErrBlobManagementUnavailable = errors.New("workspace blob management is unavailable")
var ErrBlobUploadUnavailable = errors.New("workspace blob upload is unavailable")
var ErrAvatarUploadUnavailable = errors.New("avatar upload is unavailable")
var ErrPublicUserUnavailable = errors.New("public user service is unavailable")
var ErrCommentReadUnavailable = errors.New("comment read service is unavailable")
var ErrCommentReadForbidden = errors.New("document_comments_read_forbidden")
var ErrCommentWriteUnavailable = errors.New("comment write service is unavailable")
var ErrNotificationUnavailable = errors.New("notification service is unavailable")
var ErrInvalidNotificationPagination = errors.New("invalid notification pagination")
var ErrInvalidMentionInput = errors.New("invalid mention input")
var ErrMentionUserOneselfDenied = errors.New("mention_user_oneself_denied")
var ErrMentionUserDocAccessDenied = errors.New("mention_user_doc_access_denied")
var ErrMentionDocumentUpdateDenied = errors.New("mention_document_update_denied")

type WorkspaceMemberPermissionDeniedError struct {
	WorkspaceID string
}

func (e *WorkspaceMemberPermissionDeniedError) Error() string {
	return ErrWorkspaceMemberPermissionDenied.Error()
}

func (e *WorkspaceMemberPermissionDeniedError) Unwrap() error {
	return ErrWorkspaceMemberPermissionDenied
}

type MentionDocumentPermissionDeniedError struct {
	Action      string
	WorkspaceID string
	DocID       string
}

func (e *MentionDocumentPermissionDeniedError) Error() string {
	return "document action denied"
}

func (e *MentionDocumentPermissionDeniedError) Unwrap() error {
	return ErrMentionDocumentUpdateDenied
}

type MentionUserDocAccessDeniedError struct{ DocID string }

func (e *MentionUserDocAccessDeniedError) Error() string {
	return "mentioned user cannot access document"
}

func (e *MentionUserDocAccessDeniedError) Unwrap() error {
	return ErrMentionUserDocAccessDenied
}

func (r *Resolver) deleteWorkspace(ctx context.Context, workspaceID, userID string) (bool, error) {
	if r.WorkspaceCleaner == nil {
		return false, ErrWorkspaceStorageCleanupUnavailable
	}
	userIDs, err := r.WorkspaceRepo.DeleteForUser(ctx, workspaceID, userID)
	if err != nil {
		return false, err
	}
	if _, err := r.WorkspaceCleaner.DeleteWorkspaceObjects(ctx, workspaceID, userIDs); err != nil {
		slog.WarnContext(ctx, "workspace object cleanup failed after relational deletion", "workspace_id", workspaceID)
	}
	return true, nil
}

func userModel(user *users.User) *model.UserType {
	hasPassword := user.HasPassword
	features := make([]model.FeatureType, 0, len(user.Features))
	for _, feature := range user.Features {
		if graphQLFeature, ok := graphQLUserFeature(feature); ok {
			features = append(features, graphQLFeature)
		}
	}
	return &model.UserType{
		ID:            user.ID,
		Name:          user.Name,
		Email:         user.Email,
		AvatarURL:     user.AvatarURL,
		EmailVerified: user.EmailVerified,
		HasPassword:   &hasPassword,
		Disabled:      user.Disabled,
		CreatedAt:     createdAt(user.CreatedAt),
		Features:      features,
	}
}

func createdAt(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func workspaceModel(workspace workspaces.Workspace) *model.WorkspaceType {
	return &model.WorkspaceType{
		ID:          workspace.ID,
		Initialized: workspace.Initialized,
		Team:        workspace.Team,
		Owner:       &model.WorkspaceUserType{ID: workspace.OwnerID},
	}
}
