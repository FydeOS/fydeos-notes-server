// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/jackc/pgx/v5"
)

const (
	MaxDocRoleTargets                 = 1000
	MaxDocRoleIdentifierBytes         = 1024
	MemberSearchMaxLength             = 255
	DefaultAdminSharedLinksWindowDays = 28
	MaxAdminSharedLinksWindowDays     = 90
	MaxAdminSharedLinksPageSize       = 100
)

type PreviewExposure string

const (
	PreviewDenied          PreviewExposure = "denied"
	PreviewMetadataNoIndex PreviewExposure = "metadata_no_index"
	PreviewPublicIndexable PreviewExposure = "public_indexable"
)

type AdminSharedLinksOrder string

const (
	AdminSharedLinksUpdatedAtDesc AdminSharedLinksOrder = "UpdatedAtDesc"
	AdminSharedLinksPublishedDesc AdminSharedLinksOrder = "PublishedAtDesc"
	AdminSharedLinksViewsDesc     AdminSharedLinksOrder = "ViewsDesc"
)

type Workspace struct {
	ID          string
	Initialized bool
	Team        bool
	OwnerID     string
}

type OwnedWorkspace struct {
	ID   string
	Team bool
}

type PresentationMetadata struct {
	Public    bool
	CreatedAt time.Time
}

type SettingsUpdate struct {
	EnableAI, EnableDocEmbedding, EnableSharing, EnableURLPreview, Public *bool
}

type WorkspaceSyncSnapshot struct {
	WorkspaceExists       bool
	WorkspaceRole         string
	MemberActive          bool
	SharingEnabled        bool
	MemberDefaultDocRole  string
	CommercialEntitlement bool
	Generation            int64
	Documents             map[string]DocumentACL
}

type DocumentACL struct {
	VisibilityPublic, Published                     bool
	PublicRole, MemberDefaultRole, ExplicitUserRole string
}

type WorkspaceAccessFacts struct {
	WorkspaceExists bool
	WorkspaceRole   string
	MemberActive, WorkspacePublic, SharingEnabled,
	URLPreviewEnabled, CommercialEntitlement, Team, TeamKnown bool
}

type WorkspaceAccess struct {
	Role        string
	Permissions map[string]bool
	Team        bool
}

type WorkspaceConfig struct {
	EnableAI, EnableSharing, EnableURLPreview, EnableDocEmbedding bool
}

type DocumentSyncAccess struct{ Read, Update, Manage bool }

type DocumentAuthorization struct {
	Read, HistoryRead, UsersRead, CommentsCreate, CommentsModerate, Update, Manage bool
}

type WorkspaceSyncAccess struct {
	CanSync    bool
	Generation int64
	Documents  map[string]DocumentSyncAccess
}

type Member struct {
	ID, InviteID, Name, Email string
	AvatarURL                 *string
	Role, Status              string
}

type WorkspaceRoleTransition struct {
	PreviousRole, Role, PreviousState     string
	Invitation, Removed, OwnerTransferred bool
}

type InvitationRecord struct {
	WorkspaceID, InviteeID, InviterID, Kind, RequestedRole string
	Status                                                 *string
}

type InvitationWorkspace struct {
	ID, Name  string
	AvatarKey *string
	OwnerID   string
}

type InvitationAcceptance struct {
	InviteID, WorkspaceID, InviteeID, InviterID, OwnerID string
}

type EmailInvitationReservation struct{ ID, UserID, Email string }

type EmailInvitationQuotaSettlement interface {
	CommitWorkspaceInviteQuotaTx(context.Context, pgx.Tx, string, int, []invitelinks.WorkspaceInviteQuotaTargetDomain) error
}

type DocGrantRef struct{ WorkspaceID, DocID string }

type DocGrantUser struct {
	UserID, Name, Email string
	AvatarURL           *string
	Role                string
	CreatedAt           time.Time
}

type DocGrantPage struct {
	TotalCount int
	HasNext    bool
	EndCursor  *string
	Users      []DocGrantUser
}

type DocShareState struct {
	Public      bool   `json:"public"`
	Mode        string `json:"mode"`
	DefaultRole string `json:"defaultRole"`
}

type PublishedDocumentMetadata struct {
	WorkspaceID, DocID string
	Mode               int
	Public             bool
	DefaultRole        string
	Title, Summary     *string
}

type DocumentInfo struct {
	PublishedDocumentMetadata
	CreatedAt, UpdatedAt     *time.Time
	CreatorID, LastUpdaterID *string
}

type DocumentListInput struct {
	First, Offset   int
	After           *time.Time
	RecentlyUpdated bool
}

type DocumentListPage struct {
	Documents  []DocumentInfo
	TotalCount int
}

type DocumentAuthor struct {
	ID, Name  string
	AvatarURL *string
}

type DocumentAuthors struct {
	CreatedAt, UpdatedAt time.Time
	CreatedBy, UpdatedBy *DocumentAuthor
}

type DocumentHistoryAccess struct {
	Read, Update bool
	Generation   int64
}

type PreviewResult struct {
	Exposure                          PreviewExposure
	Present                           bool
	Title, Summary, Avatar, AvatarKey string
}

type Quota struct {
	Name                                                     string
	BlobLimit, StorageQuota, UsedStorageQuota, HistoryPeriod int64
	MemberLimit, MemberCount, OvercapacityMemberCount        int32
	HumanReadable                                            QuotaHumanReadable
}

type QuotaHumanReadable struct {
	Name, BlobLimit, StorageQuota, StorageQuotaUsed, HistoryPeriod, MemberLimit, MemberCount, OvercapacityMemberCount string
}

type UserQuotaState struct {
	Plan                                                            string
	SeatLimit                                                       int32
	BlobLimit, StorageQuota, UsedStorageQuota, HistoryPeriodSeconds int64
	CopilotActionLimit                                              *int32
	UnlimitedCopilot                                                bool
}

type UserQuota struct {
	Name                                                     string
	BlobLimit, StorageQuota, UsedStorageQuota, HistoryPeriod int64
	MemberLimit                                              int32
	CopilotActionLimit                                       *int32
	HumanReadable                                            UserQuotaHumanReadable
}

type UserQuotaHumanReadable struct {
	Name, BlobLimit, StorageQuota, UsedStorageQuota, HistoryPeriod, MemberLimit, CopilotActionLimit string
}

type WorkspaceQuotaState struct {
	Plan, OwnerUserID                                               string
	UsesOwnerQuota                                                  bool
	SeatLimit, MemberCount, OvercapacityMemberCount                 int32
	BlobLimit, StorageQuota, UsedStorageQuota, HistoryPeriodSeconds int64
	Readonly                                                        bool
	ReadonlyReasons                                                 []string
	UnlimitedCopilot                                                bool
}

type IgnoredWorkspaceDoc struct {
	DocID                                        string
	CreatedAt                                    time.Time
	DocCreatedAt, DocUpdatedAt                   *time.Time
	Title, CreatedBy, CreatedByAvatar, UpdatedBy *string
}

type DocPageAnalyticsInput struct {
	WindowDays *int
	Timezone   *string
}

type DocPageAnalytics struct {
	Window      DocAnalyticsTimeWindow
	Series      []DocPageAnalyticsPoint
	Summary     DocPageAnalyticsSummary
	GeneratedAt time.Time
}

type DocAnalyticsTimeWindow struct {
	From, To                     time.Time
	Timezone, Bucket             string
	RequestedSize, EffectiveSize int
}

type DocPageAnalyticsPoint struct {
	Date                                time.Time
	TotalViews, UniqueViews, GuestViews int64
}

type DocPageAnalyticsSummary struct {
	TotalViews, UniqueViews, GuestViews int64
	LastAccessedAt                      *time.Time
}

type DocMemberLastAccessPagination struct {
	First, Offset int
	After         *string
}

type DocMemberLastAccess struct {
	UserID, Name   string
	AvatarURL      *string
	LastAccessedAt time.Time
	LastDocID      *string
}

type DocMemberLastAccessPage struct {
	Members                      []DocMemberLastAccess
	HasNextPage, HasPreviousPage bool
	StartCursor, EndCursor       *string
	TotalCount                   *int
}

type AdminWorkspaceFilter struct {
	First, Skip                                                           int
	Keyword                                                               *string
	OrderBy                                                               string
	Public, EnableAI, EnableSharing, EnableURLPreview, EnableDocEmbedding *bool
}

type AdminWorkspaceUpdate struct {
	Name                         *string
	NameSet                      bool
	AvatarKey                    *string
	AvatarKeySet                 bool
	EnableAI, EnableDocEmbedding *bool
	EnableSharing                *bool
	EnableSharingSet             bool
	EnableURLPreview             *bool
	EnableURLPreviewSet          bool
	Public                       *bool
}

type AdminWorkspaceOwner struct {
	ID, Name, Email string
	AvatarURL       *string
}

type AdminWorkspace struct {
	ID                                                                             string
	Public                                                                         bool
	CreatedAt                                                                      time.Time
	Name, AvatarKey                                                                *string
	EnableAI, EnableSharing, EnableURLPreview, EnableDocEmbedding                  bool
	Owner                                                                          *AdminWorkspaceOwner
	MemberCount, PublicPageCount, SnapshotCount, SnapshotSize, BlobCount, BlobSize int64
}

type AdminWorkspaceMember struct {
	ID, Name, Email string
	AvatarURL       *string
	Role, Status    string
}

type AdminWorkspaceSharedLink struct {
	DocID       string
	Title       *string
	PublishedAt *time.Time
}

type AdminSharedLinksInput struct {
	Keyword, WorkspaceID *string
	UpdatedAfter         *time.Time
	OrderBy              AdminSharedLinksOrder
	AnalyticsWindowDays  *int
	IncludeTotal         bool
	First, Offset        *int
	After                *string
}

type AdminSharedLink struct {
	WorkspaceID, DocID             string
	Title                          *string
	PublishedAt, DocUpdatedAt      *time.Time
	WorkspaceOwner, LastUpdaterID  *string
	ShareURL                       string
	Views, UniqueViews, GuestViews int64
	LastAccessedAt                 *time.Time
	SortValueDate                  time.Time
	SortValueNumber                int64
}

type AdminSharedLinkEdge struct {
	Cursor string
	Node   AdminSharedLink
}

type AdminSharedLinksPage struct {
	Edges                        []AdminSharedLinkEdge
	HasNextPage, HasPreviousPage bool
	StartCursor, EndCursor       *string
	TotalCount                   *int
	AnalyticsWindow              DocAnalyticsTimeWindow
}

type AdminDashboardInput struct {
	Timezone                                                                      *string
	StorageHistoryDays, SyncHistoryHours, SharedLinkWindowDays, CopilotWindowDays *int
	IncludeTopSharedLinks                                                         bool
}

type AdminDashboardTimeWindow struct {
	From, To                     time.Time
	Timezone, Bucket             string
	RequestedSize, EffectiveSize int
}

type AdminDashboardMinutePoint struct {
	Minute      time.Time
	ActiveUsers int
}

type AdminDashboardValueDayPoint struct {
	Date  time.Time
	Value int64
}

type AdminDashboardTopSharedLink struct {
	WorkspaceID, DocID             string
	Title                          *string
	PublishedAt                    *time.Time
	Views, UniqueViews, GuestViews int64
	LastAccessedAt                 *time.Time
}

type AdminDashboard struct {
	SyncActiveUsers                             int
	SyncActiveUsersTimeline                     []AdminDashboardMinutePoint
	SyncWindow                                  AdminDashboardTimeWindow
	CopilotConversations                        int64
	CopilotWindow                               AdminDashboardTimeWindow
	WorkspaceStorageBytes, BlobStorageBytes     int64
	WorkspaceStorageHistory, BlobStorageHistory []AdminDashboardValueDayPoint
	StorageWindow                               AdminDashboardTimeWindow
	TopSharedLinks                              []AdminDashboardTopSharedLink
	TopSharedLinksWindow                        AdminDashboardTimeWindow
	GeneratedAt                                 time.Time
}

type WorkspaceCalendar struct {
	ID, WorkspaceID, CreatedByUserID   string
	DisplayNameOverride, ColorOverride *string
	Enabled                            bool
	Items                              []WorkspaceCalendarItem
}

type WorkspaceCalendarItem struct {
	ID, SubscriptionID string
	SortOrder          *int
	ColorOverride      *string
	Enabled            bool
}

type WorkspaceCalendarItemInput struct {
	SubscriptionID string
	SortOrder      *int
	ColorOverride  *string
}

type CalendarEvent struct {
	ID, SubscriptionID, ExternalEventID                                  string
	RecurrenceID, Status, Title, Description, Location, OriginalTimezone *string
	StartAtUTC, EndAtUTC                                                 time.Time
	AllDay                                                               bool
}

type CalendarAccount struct {
	ID, Provider, ProviderAccountID string
	DisplayName, Email, LastError   *string
	Status                          string
	RefreshIntervalMinutes          int
	CreatedAt, UpdatedAt            time.Time
	CalendarsCount                  int
}

type CalendarSubscription struct {
	ID, AccountID, Provider, ExternalCalendarID string
	DisplayName, Timezone, Color                *string
	Enabled                                     bool
	LastSyncAt                                  *time.Time
}
