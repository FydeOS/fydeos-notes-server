// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	gqlgraphql "github.com/99designs/gqlgen/graphql"
	gqlhandler "github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/fydeos/fydeos-notes-server/internal/appconfig"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/avatars"
	"github.com/fydeos/fydeos-notes-server/internal/blobs"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/clientip"
	"github.com/fydeos/fydeos-notes-server/internal/comments"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/maildelivery"
	"github.com/fydeos/fydeos-notes-server/internal/mcpcredentials"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/serverconfig"
	"github.com/fydeos/fydeos-notes-server/internal/upgrades"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	gqlast "github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

const maxGraphQLRequestBodyBytes int64 = 10 << 20
const maxGraphQLMultipartBodyBytes int64 = (100 << 20) + (1 << 20)
const graphQLBodyReadTimeout = 30 * time.Second
const graphQLMultipartBodyReadTimeout = 5 * time.Minute
const maxWorkspaceInitialDocumentBytes int64 = 100 << 20

type websocketUpgradeRequestKey struct{}
type graphQLHTTPRequestKey struct{}

func RequestFromContext(ctx context.Context) *http.Request {
	request, _ := ctx.Value(graphQLHTTPRequestKey{}).(*http.Request)
	return request
}

var ErrInitialWorkspaceDocumentTooLarge = errors.New("initial workspace document is too large")

func NewHandler(cfg config.Config, authenticator RequestAuthenticator, userService UserService, workspaceRepository WorkspaceRepository) http.Handler {
	return NewHandlerWithReadLimiter(cfg, authenticator, userService, workspaceRepository, auth.NewReadLimiter(auth.DefaultReadConcurrency))
}

func NewHandlerWithReadLimiter(cfg config.Config, authenticator RequestAuthenticator, userService UserService, workspaceRepository WorkspaceRepository, limiter *auth.ReadLimiter) http.Handler {
	return NewHandlerWithProfilePublisher(cfg, authenticator, userService, workspaceRepository, limiter, nil)
}

func NewHandlerWithProfilePublisher(cfg config.Config, authenticator RequestAuthenticator, userService UserService, workspaceRepository WorkspaceRepository, limiter *auth.ReadLimiter, publisher ProfileChangePublisher) http.Handler {
	return NewHandlerWithProfilePublisherAndWorkspaceCleaner(cfg, authenticator, userService, workspaceRepository, limiter, publisher, nil)
}

func NewHandlerWithProfilePublisherAndWorkspaceCleaner(cfg config.Config, authenticator RequestAuthenticator, userService UserService, workspaceRepository WorkspaceRepository, limiter *auth.ReadLimiter, publisher ProfileChangePublisher, cleaner WorkspaceObjectCleaner) http.Handler {
	return NewHandlerWithBlobService(cfg, authenticator, userService, workspaceRepository, limiter, publisher, cleaner, nil)
}

func NewHandlerWithBlobService(cfg config.Config, authenticator RequestAuthenticator, userService UserService, workspaceRepository WorkspaceRepository, limiter *auth.ReadLimiter, publisher ProfileChangePublisher, cleaner WorkspaceObjectCleaner, blobService WorkspaceBlobService, commentRead ...CommentReadDependencies) http.Handler {
	dependencies := HandlerDependencies{}
	if len(commentRead) > 0 {
		dependencies.Comments = commentRead[0]
	}
	return NewHandlerWithServices(cfg, authenticator, userService, workspaceRepository, limiter, publisher, cleaner, blobService, dependencies)
}

func NewHandlerWithServices(cfg config.Config, authenticator RequestAuthenticator, userService UserService, workspaceRepository WorkspaceRepository, limiter *auth.ReadLimiter, publisher ProfileChangePublisher, cleaner WorkspaceObjectCleaner, blobService WorkspaceBlobService, dependencies HandlerDependencies) http.Handler {
	if dependencies.ServerConfig == nil {
		dependencies.ServerConfig = serverconfig.New(cfg, nil)
	}
	if limiter == nil {
		limiter = auth.NewReadLimiter(auth.DefaultReadConcurrency)
	}
	upgradeChecker := dependencies.UpgradeChecker
	if upgradeChecker == nil {
		upgradeChecker = upgrades.NewChecker(nil, "")
	}
	resolver := &Resolver{
		CalDAVAccountLinker:         dependencies.CalDAVAccountLinker,
		GoogleCalendarAccountLinker: dependencies.GoogleCalendarAccountLinker,
		CalendarProviderCatalog:     dependencies.CalendarProviderCatalog,
		CopilotSessions:             dependencies.CopilotSessions,
		CopilotAttachments:          dependencies.CopilotAttachments,
		CopilotWorkspaceArtifacts:   dependencies.CopilotWorkspaceArtifacts,
		CopilotTranscriptTasks:      dependencies.CopilotTranscriptTasks,
		TranscriptTaskPublisher:     dependencies.TranscriptTaskPublisher,
		SearchDocs:                  dependencies.SearchDocs,
		SearchProjection:            dependencies.SearchProjection,
		BYOKProfiles:                dependencies.BYOKProfiles,
		AdminEntitlements:           dependencies.AdminEntitlements,
		ServerConfig:                dependencies.ServerConfig,
		LicenseWorkspacePublisher:   dependencies.LicenseWorkspacePublisher,
		LicenseQuotaPublisher:       dependencies.LicenseQuotaPublisher,
		CopilotQuotaPublisher:       dependencies.CopilotQuotaPublisher,
		MCPCredentials:              dependencies.MCPCredentials,
		OAuthProviders:              dependencies.OAuthProviders,
		DocumentReader:              dependencies.DocumentReader,
		DocumentAnalytics:           dependencies.DocumentAnalytics,
		DocumentHistory:             dependencies.DocumentHistory,
		DocumentPublications:        dependencies.DocumentPublications,
		DocumentRoles:               dependencies.DocumentRoles,
		Config:                      cfg,
		UpgradeChecker:              upgradeChecker,
		UserService:                 userService,
		AvatarUploader:              dependencies.AvatarUploader,
		WorkspaceRepo:               workspaceRepository,
		WorkspacePermissionReader:   dependencies.WorkspacePermissionReader,
		WorkspaceConfigPublisher:    dependencies.WorkspaceConfig,
		WorkspaceCleaner:            cleaner,
		BlobManager:                 blobService,
		BlobUploader:                blobService,
		ProfilePublisher:            publisher,
		UserSettingsPublisher:       dependencies.UserSettings,
		Notifications:               dependencies.Notifications.Repository,
		NotificationAuth:            dependencies.Notifications.Authorizer,
		NotificationPublisher:       dependencies.Notifications.Publisher,
		SecurityChallenges:          dependencies.SecurityChallenges.Service,
		AuthSigningKeys:             dependencies.AuthSigningKeys,
		AppConfig:                   dependencies.AppConfig,
		SMTPTestEmailSender:         dependencies.SMTPTestEmailSender,
		MailDeliveryAnalytics:       dependencies.MailDeliveryAnalytics,
		InviteLinkReader:            dependencies.InviteLinks.Reader,
		InviteLinkCreator:           dependencies.InviteLinks.Creator,
		InviteLinkRevoker:           dependencies.InviteLinks.Revoker,
		InviteLinkAuthorizer:        dependencies.InviteLinks.Authorizer,
		InviteLinkPublisher:         dependencies.InviteLinks.Publisher,
		InviteInfoRepository:        dependencies.InviteInfo.Repository,
		InviteLinkLookup:            dependencies.InviteInfo.LinkLookup,
		InvitationAcceptance:        dependencies.InvitationAcceptance,
		WorkspaceInvitations:        dependencies.WorkspaceInvitations,
		WorkspaceRoles:              dependencies.WorkspaceRoles,
		WorkspaceAvatarReader:       dependencies.InviteInfo.AvatarReader,
	}
	if accountDeleter, ok := userService.(OwnAccountDeleter); ok {
		resolver.OwnAccountDeleter = accountDeleter
	}
	if managementService, ok := userService.(UserManagementService); ok {
		resolver.UserManagement = managementService
	}
	if memberReader, ok := workspaceRepository.(WorkspaceMemberReader); ok {
		resolver.WorkspaceMembers = memberReader
	}
	resolver.WorkspaceQuotaReader = workspaceRepository
	if quotaStateReader, ok := workspaceRepository.(WorkspaceQuotaStateReader); ok {
		resolver.WorkspaceQuotaStateReader = quotaStateReader
	}
	if quotaStateReader, ok := workspaceRepository.(UserQuotaStateReader); ok {
		resolver.UserQuotaStateReader = quotaStateReader
	}
	resolver.CommentReader = dependencies.Comments.Repository
	resolver.CommentWriter = dependencies.Comments.Writer
	resolver.CommentAuth = dependencies.Comments.Authorizer
	resolver.CommentPublisher = dependencies.Comments.Publisher
	server := gqlhandler.New(NewExecutableSchema(Config{Resolvers: resolver}))
	server.SetErrorPresenter(presentGraphQLError)
	server.AddTransport(transport.Websocket{KeepAlivePingInterval: 10 * time.Second})
	server.AroundOperations(websocketSessionMiddleware(authenticator))
	server.AroundOperations(serverConfigOAuthSnapshotMiddleware())
	responseHeaders := map[string][]string{"Cache-Control": {"no-store"}}
	server.AddTransport(transport.GET{ResponseHeaders: responseHeaders})
	server.AddTransport(transport.POST{ResponseHeaders: responseHeaders})
	server.AddTransport(transport.MultipartForm{
		MaxUploadSize:   maxGraphQLMultipartBodyBytes,
		MaxMemory:       32 << 20,
		ResponseHeaders: responseHeaders,
	})
	server.SetQueryCache(lru.New[*gqlast.QueryDocument](1000))
	server.Use(extension.Introspection{})
	server.Use(extension.AutomaticPersistedQuery{Cache: lru.New[string](100)})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			writeUnsupportedGraphQLMethod(w)
			return
		}
		release, ok := limiter.TryAcquire()
		if !ok {
			w.Header().Set("Retry-After", "1")
			writeGraphQLHTTPError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		defer release()

		var principal *auth.Principal
		authenticatedMultipart := false
		if r.Method == http.MethodPost && r.Body != nil {
			mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mediaType == "multipart/form-data" && strings.TrimSpace(r.Header.Get("X-Operation-Name")) == "" {
				writeGraphQLHTTPError(w, http.StatusBadRequest, "multipart GraphQL requests require x-operation-name")
				return
			}
			if mediaType == "multipart/form-data" {
				var err error
				if authenticator != nil {
					principal, err = auth.AuthenticateRequest(r.Context(), w, r, authenticator)
				}
				if err != nil && !errors.Is(err, auth.ErrNoSession) {
					if auth.WriteAccessTokenError(w, err) {
						return
					}
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusServiceUnavailable)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"errors": []map[string]string{{"message": "authentication service unavailable"}},
					})
					return
				}
				if principal == nil {
					writeGraphQLHTTPError(w, http.StatusOK, ErrAuthenticationRequired.Error())
					return
				}
				authenticatedMultipart = true
			}
			maxBodyBytes := maxGraphQLRequestBodyBytes
			readTimeout := graphQLBodyReadTimeout
			if mediaType == "multipart/form-data" {
				maxBodyBytes = maxGraphQLMultipartBodyBytes
				readTimeout = graphQLMultipartBodyReadTimeout
			}
			if r.ContentLength > maxBodyBytes {
				writeGraphQLHTTPError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			controller := http.NewResponseController(w)
			if err := controller.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
				writeGraphQLHTTPError(w, http.StatusInternalServerError, "request body timeout unavailable")
				return
			}
			if mediaType == "multipart/form-data" {
				defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
				r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
			} else {
				limitedBody := http.MaxBytesReader(w, r.Body, maxBodyBytes)
				body, err := io.ReadAll(limitedBody)
				_ = limitedBody.Close()
				if clearErr := controller.SetReadDeadline(time.Time{}); clearErr != nil {
					writeGraphQLHTTPError(w, http.StatusInternalServerError, "unable to clear request body timeout")
					return
				}
				if err != nil {
					var maxBytesError *http.MaxBytesError
					if errors.As(err, &maxBytesError) {
						writeGraphQLHTTPError(w, http.StatusRequestEntityTooLarge, "request body too large")
						return
					}
					writeGraphQLHTTPError(w, http.StatusBadRequest, "unable to read request body")
					return
				}
				if mediaType == "application/json" {
					var params struct {
						Query         *string                    `json:"query"`
						OperationName *string                    `json:"operationName"`
						Variables     map[string]json.RawMessage `json:"variables"`
						Extensions    map[string]json.RawMessage `json:"extensions"`
					}
					if err := json.Unmarshal(body, &params); err != nil {
						writeGraphQLHTTPError(w, http.StatusBadRequest, "invalid request body")
						return
					}
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
		if !authenticatedMultipart {
			var err error
			principal, err = auth.AuthenticateRequest(r.Context(), w, r, authenticator)
			if err != nil && !errors.Is(err, auth.ErrNoSession) {
				if auth.WriteAccessTokenError(w, err) {
					return
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errors": []map[string]string{{"message": "authentication service unavailable"}},
				})
				return
			}
		}
		ctx := auth.WithPrincipal(r.Context(), principal)
		ctx = context.WithValue(ctx, graphQLHTTPRequestKey{}, r)
		ctx = invitelinks.WithRequestFacts(ctx, workspaceInviteRequestFacts(r, principal, cfg))
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
			ctx = context.WithValue(ctx, websocketUpgradeRequestKey{}, r)
		}
		if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
			w = &jsonResponseWriter{ResponseWriter: w}
		}
		server.ServeHTTP(w, r.WithContext(ctx))
	})
}

func workspaceInviteRequestFacts(r *http.Request, principal *auth.Principal, cfg config.Config) invitelinks.RequestFacts {
	clientIP := clientip.RequestClientIP(r, cfg.TrustedProxyCIDRs, cfg.TrustedCloudflareCIDRs, cfg.TrustedXRealIPCIDRs)
	requestID := ""
	if principal != nil {
		requestID = principal.SessionID
	}
	trustedPeer := clientip.IsTrustedPeer(r, cfg.TrustedProxyCIDRs, cfg.TrustedCloudflareCIDRs)
	if requestID == "" && trustedPeer {
		requestID = firstRequestHeader(r, "CF-Ray")
	}
	if requestID == "" {
		requestID = clientIP
	}
	asn := ""
	if trustedPeer {
		asn = firstRequestHeader(r, "x-affine-cf-asn")
	}
	return invitelinks.RequestFacts{RequestID: requestID, ClientIP: clientIP, ASN: asn}
}

func firstRequestHeader(r *http.Request, name string) string {
	for _, value := range r.Header.Values(name) {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func presentGraphQLError(ctx context.Context, err error) *gqlerror.Error {
	presented := gqlgraphql.DefaultErrorPresenter(ctx, err)
	var smtpTestError *maildelivery.SMTPTestError
	var appConfigInputError *appconfig.InvalidInputError
	var adminGrantError *entitlements.AdminGrantError
	switch {
	case errors.Is(err, ErrAuthenticationRequired):
		presented.Message = ErrAuthenticationRequired.Error()
		presented.Extensions = map[string]any{
			"status":  http.StatusUnauthorized,
			"code":    http.StatusText(http.StatusUnauthorized),
			"type":    "AUTHENTICATION_REQUIRED",
			"name":    "AUTHENTICATION_REQUIRED",
			"message": presented.Message,
		}
	case errors.Is(err, auth.ErrActionForbidden):
		presented.Message = "You are not allowed to perform this action."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "action_forbidden", presented.Message, nil)
	case errors.Is(err, byok.ErrEntitlementRequired):
		presented.Message = byok.ErrEntitlementRequired.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "action_forbidden", presented.Message, nil)
	case errors.As(err, &adminGrantError):
		presented.Message = adminGrantError.Reason
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "bad_request", "bad_request", presented.Message, nil)
	case errors.Is(err, entitlements.ErrAdminGrantUnavailable):
		presented.Message = "Admin entitlement management is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "internal_server_error", "admin_entitlement_unavailable", presented.Message, nil)
	case errors.Is(err, byok.ErrInvalidInput):
		presented.Message = strings.TrimPrefix(err.Error(), byok.ErrInvalidInput.Error()+": ")
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "validation_error", presented.Message, map[string]string{"errors": presented.Message})
	case errors.Is(err, ErrBYOKDescriptionRequired):
		presented.Message = ErrBYOKDescriptionRequired.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "validation_error", presented.Message, nil)
	case errors.Is(err, byok.ErrTargetUnavailable), errors.Is(err, byok.ErrProfileNotFound), errors.Is(err, byok.ErrProfileNameConflict), errors.Is(err, byok.ErrRevisionConflict):
		presented.Message = err.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "bad_request", "bad_request", presented.Message, nil)
	case errors.Is(err, byok.ErrCredentialUnavailable):
		presented.Message = byok.ErrCredentialUnavailable.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusInternalServerError, "internal_server_error", "credential_unavailable", presented.Message, nil)
	case errors.Is(err, ErrBYOKProfilesUnavailable):
		presented.Message = "BYOK profile service is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "service_unavailable", "byok_profiles_unavailable", presented.Message, nil)
	case errors.Is(err, mcpcredentials.ErrCopilotDisabled):
		presented.Message = mcpcredentials.ErrCopilotDisabled.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "action_forbidden", presented.Message, nil)
	case errors.Is(err, ErrCopilotEmbeddingUnavailable):
		presented.Message = "Copilot embedding is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "COPILOT_EMBEDDING_UNAVAILABLE", presented.Message, nil)
	case errors.Is(err, copilot.ErrManagedQuotaExceeded):
		presented.Message = "You have reached the limit of actions in this workspace, please upgrade your plan."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusPaymentRequired, "QUOTA_EXCEEDED", "COPILOT_QUOTA_EXCEEDED", presented.Message, nil)
	case errors.Is(err, copilot.ErrCopilotActionPromptInvalid):
		presented.Message = "Action prompts are only allowed in document sessions."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "bad_request", presented.Message, nil)
	case errors.Is(err, ErrTranscriptTaskNotFound):
		presented.Message = ErrTranscriptTaskNotFound.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "copilot_transcription_job_not_found", presented.Message, nil)
	case errors.Is(err, ErrTranscriptTaskFailed):
		presented.Message = ErrTranscriptTaskFailed.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "bad_request", presented.Message, nil)
	case errors.Is(err, copilot.ErrTranscriptAudioNotProvided):
		presented.Message = "Transcription audio is required."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "copilot_transcription_audio_not_provided", presented.Message, nil)
	case errors.Is(err, copilot.ErrTranscriptJobExists):
		presented.Message = "A transcript task for this audio is already running."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "copilot_transcription_job_exists", presented.Message, nil)
	case errors.Is(err, copilot.ErrTranscriptJobNotFound):
		presented.Message = copilot.ErrTranscriptJobNotFound.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "copilot_transcription_job_not_found", presented.Message, nil)
	case errors.Is(err, copilot.ErrTranscriptRetryNotAllowed):
		presented.Message = copilot.ErrTranscriptRetryNotAllowed.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "bad_request", presented.Message, nil)
	case errors.Is(err, mcpcredentials.ErrWriteToolsUnavailable), errors.Is(err, mcpcredentials.ErrCredentialNotFound), errors.Is(err, mcpcredentials.ErrInvalidExpiration), errors.Is(err, mcpcredentials.ErrInvalidName), errors.Is(err, mcpcredentials.ErrInvalidAccessMode):
		presented.Message = mcpCredentialPublicErrorMessage(err)
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "bad_request", "bad_request", presented.Message, nil)
	case errors.Is(err, mcpcredentials.ErrUnavailable):
		presented.Message = "MCP credential service is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "service_unavailable", "service_unavailable", presented.Message, nil)
	case errors.Is(err, ErrFeatureNotAvailable):
		presented.Message = "This feature is not available on this server."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "feature_not_available", presented.Message, nil)
	case errors.Is(err, workspaces.ErrInvitationNotFound):
		presented.Message = "Invitation not found"
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusNotFound, "RESOURCE_NOT_FOUND", "NOT_FOUND", presented.Message, nil)
	case errors.Is(err, workspaces.ErrInvitationAccountMismatch):
		presented.Message = "This invitation belongs to another account."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "ACTION_FORBIDDEN", "INVITATION_ACCOUNT_MISMATCH", presented.Message, nil)
	case errors.Is(err, workspaces.ErrInvitationInvalid):
		presented.Message = "Invalid invitation provided."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "INVALID_INPUT", "INVALID_INVITATION", presented.Message, nil)
	case errors.Is(err, workspaces.ErrInvitationNoSeats):
		spaceID := invitationErrorSpaceID(err)
		presented.Message = fmt.Sprintf("No more seat available in the Space %s.", spaceID)
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "NO_MORE_SEAT", presented.Message, map[string]string{"spaceId": spaceID})
	case errors.Is(err, workspaces.ErrInvitationAlreadyMember):
		spaceID := invitationErrorSpaceID(err)
		presented.Message = fmt.Sprintf("You have already joined in Space %s.", spaceID)
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "ACTION_FORBIDDEN", "ALREADY_IN_SPACE", presented.Message, map[string]string{"spaceId": spaceID})
	case errors.Is(err, users.ErrNotFound):
		presented.Message = "User not found"
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusNotFound, "RESOURCE_NOT_FOUND", "USER_NOT_FOUND", presented.Message, nil)
	case errors.Is(err, users.ErrCannotDeleteOwnAccount):
		presented.Message = "Cannot delete own account."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "cannot_delete_own_account", presented.Message, nil)
	case errors.Is(err, users.ErrCannotDeleteTeamWorkspace):
		presented.Message = "Cannot delete account. You are the owner of one or more team workspaces. Please transfer ownership or delete them first."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "cannot_delete_account_with_owned_team_workspace", presented.Message, nil)
	case errors.Is(err, users.ErrAccountLifecycleUnavailable):
		presented.Message = "Account lifecycle management is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "internal_server_error", "account_lifecycle_unavailable", presented.Message, nil)
	case errors.Is(err, users.ErrEmailAlreadyUsed):
		presented.Message = "This email has already been registered."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "RESOURCE_ALREADY_EXISTS", "email_already_used", presented.Message, nil)
	case errors.Is(err, auth.ErrInvalidEmailToken):
		presented.Message = "An invalid email token provided."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "invalid_email_token", presented.Message, nil)
	case errors.Is(err, auth.ErrSecurityLinkExpired):
		presented.Message = "The link has expired."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "bad_request", "link_expired", presented.Message, nil)
	case errors.Is(err, auth.ErrSecurityEmailVerificationNeeded):
		presented.Message = "You must verify your email before accessing this resource."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "email_verification_required", presented.Message, nil)
	case errors.Is(err, auth.ErrSecurityEmailAlreadyUsed):
		presented.Message = "This email has already been registered."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "resource_already_exists", "email_already_used", presented.Message, nil)
	case errors.Is(err, auth.ErrSecuritySameEmail):
		presented.Message = "You are trying to update your account email to the same as the old one."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "same_email_provided", presented.Message, nil)
	case errors.Is(err, auth.ErrMagicLinkQuotaDenied):
		presented.Message = "Too many requests."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusTooManyRequests, "too_many_requests", "too_many_request", presented.Message, nil)
	case errors.Is(err, auth.ErrEmailServiceNotConfigured):
		presented.Message = "Email service is not configured."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusInternalServerError, "internal_server_error", "email_service_not_configured", presented.Message, nil)
	case errors.Is(err, auth.ErrUnsafeMagicCallback):
		presented.Message = "You are not allowed to perform this action."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "action_forbidden", presented.Message, nil)
	case errors.Is(err, auth.ErrInvalidEmail):
		presented.Message = "Invalid email."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "invalid_email", presented.Message, nil)
	case errors.Is(err, auth.ErrWrongSignInCredentials):
		presented.Message = "Wrong user email or password."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "wrong_sign_in_credentials", presented.Message, nil)
	case errors.Is(err, auth.ErrSecurityPasswordRequired):
		presented.Message = "Password is required."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "validation_error", presented.Message, nil)
	case errors.Is(err, ErrSecurityChallengeUnavailable):
		presented.Message = "Security challenge service is temporarily unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "internal_server_error", "security_challenge_unavailable", presented.Message, nil)
	case errors.Is(err, ErrSecurityChallengeInternal):
		presented.Message = "An internal error occurred."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusInternalServerError, "internal_server_error", "internal_server_error", presented.Message, nil)
	case errors.Is(err, users.ErrInvalidEmail):
		var invalidEmail *users.InvalidEmailError
		errors.As(err, &invalidEmail)
		presented.Message = "An invalid email provided: " + invalidEmail.Email
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "invalid_email", presented.Message, map[string]string{"email": invalidEmail.Email})
	case isAvatarFormatError(err):
		var formatError *avatars.FormatError
		errors.As(err, &formatError)
		presented.Message = formatError.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "image_format_not_supported", presented.Message, map[string]string{"format": formatError.Format})
	case errors.Is(err, avatars.ErrUploadTooLarge):
		presented.Message = "You have exceeded your blob size quota."
		presented.Extensions = map[string]any{
			"status":  http.StatusPaymentRequired,
			"code":    http.StatusText(http.StatusPaymentRequired),
			"type":    "QUOTA_EXCEEDED",
			"name":    "BLOB_QUOTA_EXCEEDED",
			"message": presented.Message,
		}
	case errors.Is(err, avatars.ErrAvatarOperationFailed):
		presented.Message = "Avatar operation failed."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusInternalServerError, "internal_server_error", "internal_server_error", presented.Message, nil)
	case errors.Is(err, ErrAvatarUploadUnavailable):
		presented.Message = "Avatar upload is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "internal_server_error", "avatar_upload_unavailable", presented.Message, nil)
	case errors.Is(err, users.ErrUserFeatureNotConfigurable):
		presented.Message = err.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "BAD_REQUEST", presented.Message, nil)
	case errors.Is(err, users.ErrUserStatusUnavailable):
		presented.Message = "User status management is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "INTERNAL_SERVER_ERROR", "USER_STATUS_UNAVAILABLE", presented.Message, nil)
	case errors.Is(err, ErrAdminAccessDenied):
		presented.Message = "You are not allowed to perform this action."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "action_forbidden", presented.Message, nil)
	case errors.As(err, &appConfigInputError):
		presented.Message = appConfigInputError.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "invalid_app_config_input", presented.Message, map[string]string{"message": presented.Message})
	case errors.Is(err, ErrAppConfigUnavailable):
		presented.Message = "App config administration is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "internal_server_error", "app_config_unavailable", presented.Message, nil)
	case errors.Is(err, ErrMailDeliveryAnalyticsUnavailable):
		presented.Message = "internal_server_error"
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusInternalServerError, "internal_server_error", "internal_server_error", presented.Message, nil)
	case errors.As(err, &smtpTestError):
		if smtpTestError.Phase == "send" {
			presented.Message = "Failed to send test email. Cause: " + smtpTestError.Error()
		} else {
			presented.Message = "Failed to verify your SMTP configuration. Cause: " + smtpTestError.Error()
		}
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "bad_request", "bad_request", presented.Message, nil)
	case errors.Is(err, ErrSMTPTestEmailUnavailable):
		presented.Message = "SMTP test email service is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "service_unavailable", "service_unavailable", presented.Message, nil)
	case errors.Is(err, auth.ErrSigningKeyChanged):
		presented.Message = "The active signing key changed. Reload and try again."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "invalid_app_config_input", presented.Message, map[string]string{"message": presented.Message})
	case errors.Is(err, auth.ErrSigningKeyNotFound):
		presented.Message = "Signing key does not exist."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "invalid_app_config_input", presented.Message, map[string]string{"message": presented.Message})
	case errors.Is(err, auth.ErrSigningKeyCannotDelete):
		presented.Message = "You are not allowed to perform this action."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "action_forbidden", presented.Message, nil)
	case errors.Is(err, ErrAuthSigningKeysUnavailable):
		presented.Message = "Auth signing key management is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "internal_server_error", "auth_signing_keys_unavailable", presented.Message, nil)
	case errors.Is(err, ErrWorkspacePermissionsDenied):
		var denied *WorkspacePermissionsDeniedError
		if errors.As(err, &denied) {
			presented.Message = fmt.Sprintf("You do not have permission to access Space %s.", denied.WorkspaceID)
			presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "no_permission", "space_access_denied", presented.Message, map[string]string{"spaceId": denied.WorkspaceID})
		}
	case errors.Is(err, ErrWorkspaceRolePermissionsUnavailable):
		presented.Message = "Workspace role permissions are unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "internal_server_error", "workspace_role_permissions_unavailable", presented.Message, nil)
	case errors.Is(err, copilot.ErrMessageNotFound):
		presented.Message = copilot.ErrMessageNotFound.Error()
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusNotFound, "RESOURCE_NOT_FOUND", "COPILOT_MESSAGE_NOT_FOUND", presented.Message, nil)
	case errors.Is(err, copilot.ErrSessionNotFound):
		presented.Message = "Copilot session not found."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusNotFound, "RESOURCE_NOT_FOUND", "COPILOT_SESSION_NOT_FOUND", presented.Message, nil)
	case errors.Is(err, ErrWorkspacePermissionProjectionIncomplete):
		presented.Message = "Workspace role permissions are unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusInternalServerError, "internal_server_error", "workspace_role_permissions_unavailable", presented.Message, nil)
	case errors.Is(err, blobs.ErrCommentAttachmentQuotaExceeded):
		presented.Message = "You have exceeded the comment attachment size quota."
		presented.Extensions = map[string]any{
			"status":  http.StatusPaymentRequired,
			"code":    http.StatusText(http.StatusPaymentRequired),
			"type":    "QUOTA_EXCEEDED",
			"name":    "COMMENT_ATTACHMENT_QUOTA_EXCEEDED",
			"message": presented.Message,
		}
	case errors.Is(err, comments.ErrNotFound):
		presented.Message = "Comment not found."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusNotFound, "RESOURCE_NOT_FOUND", "COMMENT_NOT_FOUND", presented.Message, nil)
	case errors.Is(err, comments.ErrReplyNotFound):
		presented.Message = "Reply not found."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusNotFound, "RESOURCE_NOT_FOUND", "REPLY_NOT_FOUND", presented.Message, nil)
	case errors.Is(err, notifications.ErrNotFound):
		presented.Message = "Notification not found."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusNotFound, "RESOURCE_NOT_FOUND", "NOTIFICATION_NOT_FOUND", presented.Message, nil)
	case errors.Is(err, ErrWorkspaceMemberQueryTooLong):
		presented.Message = "Query is too long, max length is 255."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "INVALID_INPUT", "QUERY_TOO_LONG", presented.Message, map[string]int{"max": 255})
	case errors.Is(err, ErrWorkspaceMemberPermissionDenied):
		var denied *WorkspaceMemberPermissionDeniedError
		if errors.As(err, &denied) {
			presented.Message = fmt.Sprintf("You do not have permission to access Space %s.", denied.WorkspaceID)
			presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "NO_PERMISSION", "SPACE_ACCESS_DENIED", presented.Message, map[string]string{"spaceId": denied.WorkspaceID})
		}
	case errors.Is(err, workspaces.ErrWorkspaceRolePermissionDenied):
		workspaceID := workspaceRoleMutationWorkspaceID(err)
		presented.Message = fmt.Sprintf("You do not have permission to access Space %s.", workspaceID)
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "no_permission", "space_access_denied", presented.Message, map[string]string{"spaceId": workspaceID})
	case errors.Is(err, workspaces.ErrNotFound) && isWorkspaceRoleMutationError(err):
		presented.Message = "Space not found."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusNotFound, "resource_not_found", "space_not_found", presented.Message, map[string]string{"spaceId": workspaceRoleMutationWorkspaceID(err)})
	case errors.Is(err, workspaces.ErrWorkspaceMemberNotFound):
		presented.Message = "Member not found in this Space."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "member_not_found_in_space", presented.Message, map[string]string{"spaceId": workspaceRoleMutationWorkspaceID(err)})
	case errors.Is(err, workspaces.ErrWorkspaceMemberCannotRevoke):
		presented.Message = "You cannot revoke yourself."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "can_not_revoke_yourself", presented.Message, nil)
	case errors.Is(err, workspaces.ErrWorkspaceOwnerMustTransfer):
		presented.Message = "The workspace owner must transfer ownership before leaving or being removed."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "workspace_owner_must_transfer", presented.Message, nil)
	case errors.Is(err, workspaces.ErrWorkspaceOwnerCannotLeave):
		presented.Message = "Owner can not leave the workspace."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "owner_can_not_leave_workspace", presented.Message, nil)
	case errors.Is(err, workspaces.ErrWorkspaceTargetMustBeActive):
		presented.Message = "Only an active workspace member can become the owner."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "action_forbidden", "target_member_not_active", presented.Message, nil)
	case errors.Is(err, ErrWorkspaceInviteLinkForbidden):
		presented.Message = "This feature is temporarily unavailable for you."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "ACTION_FORBIDDEN", "ACTION_FORBIDDEN", presented.Message, nil)
	case errors.Is(err, ErrWorkspaceInviteLinkNameForbidden):
		presented.Message = "Workspace names containing links or domains cannot be used to invite members."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "ACTION_FORBIDDEN", "ACTION_FORBIDDEN", presented.Message, nil)
	case errors.Is(err, ErrWorkspaceInviteTooMany):
		presented.Message = "Too many requests."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusTooManyRequests, "TOO_MANY_REQUESTS", "TOO_MANY_REQUEST", presented.Message, nil)
	case errors.Is(err, invitelinks.ErrWorkspaceInviteForbidden):
		presented.Message = "This feature is temporarily unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "ACTION_FORBIDDEN", "ACTION_FORBIDDEN", presented.Message, nil)
	case errors.Is(err, invitelinks.ErrWorkspaceInviteQuotaExceeded):
		presented.Message = "Too many requests."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusTooManyRequests, "TOO_MANY_REQUESTS", "TOO_MANY_REQUEST", presented.Message, nil)
	case errors.Is(err, ErrWorkspaceInviteMutationUnavailable):
		presented.Message = "Workspace invitation service is unavailable."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "SERVICE_UNAVAILABLE", presented.Message, nil)
	case errors.Is(err, ErrInvalidMentionInput):
		presented.Message = "Validation error, errors: invalid mention input"
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusBadRequest, "INVALID_INPUT", "VALIDATION_ERROR", presented.Message, map[string]string{"errors": "invalid mention input"})
	case errors.Is(err, ErrMentionUserOneselfDenied):
		presented.Message = "You can not mention yourself."
		presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "ACTION_FORBIDDEN", "MENTION_USER_ONESELF_DENIED", presented.Message, nil)
	case errors.Is(err, ErrMentionUserDocAccessDenied):
		var denied *MentionUserDocAccessDeniedError
		if errors.As(err, &denied) {
			presented.Message = fmt.Sprintf("Mentioned user can not access doc %s.", denied.DocID)
			presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "NO_PERMISSION", "MENTION_USER_DOC_ACCESS_DENIED", presented.Message, map[string]string{"docId": denied.DocID})
		}
	case errors.Is(err, ErrMentionDocumentUpdateDenied):
		var denied *MentionDocumentPermissionDeniedError
		if errors.As(err, &denied) {
			presented.Message = fmt.Sprintf("You do not have permission to perform %s action on doc %s.", denied.Action, denied.DocID)
			presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "NO_PERMISSION", "DOC_ACTION_DENIED", presented.Message, map[string]string{
				"spaceId": denied.WorkspaceID,
				"docId":   denied.DocID,
				"action":  denied.Action,
			})
		}
	case errors.Is(err, comments.ErrCommentsCreateForbidden), errors.Is(err, comments.ErrCommentsModerateForbidden):
		var denied *comments.PermissionDeniedError
		if errors.As(err, &denied) {
			presented.Message = fmt.Sprintf("You do not have permission to perform %s action on doc %s.", denied.Action, denied.DocID)
			presented.Extensions = userFriendlyGraphQLExtensions(http.StatusForbidden, "NO_PERMISSION", "DOC_ACTION_DENIED", presented.Message, map[string]string{
				"spaceId": denied.WorkspaceID,
				"docId":   denied.DocID,
				"action":  denied.Action,
			})
		}
	default:
		return presented
	}
	return presented
}

func mcpCredentialPublicErrorMessage(err error) string {
	for _, candidate := range []error{
		mcpcredentials.ErrWriteToolsUnavailable,
		mcpcredentials.ErrCredentialNotFound,
		mcpcredentials.ErrInvalidExpiration,
		mcpcredentials.ErrInvalidName,
		mcpcredentials.ErrInvalidAccessMode,
	} {
		if errors.Is(err, candidate) {
			return candidate.Error()
		}
	}
	return "MCP credential request failed."
}

func isAvatarFormatError(err error) bool {
	var formatError *avatars.FormatError
	return errors.As(err, &formatError)
}

func workspaceRoleMutationWorkspaceID(err error) string {
	var mutationError *WorkspaceRoleMutationError
	if errors.As(err, &mutationError) {
		return mutationError.WorkspaceID
	}
	return ""
}

func isWorkspaceRoleMutationError(err error) bool {
	var mutationError *WorkspaceRoleMutationError
	return errors.As(err, &mutationError)
}

func invitationErrorSpaceID(err error) string {
	var spaceError *workspaces.InvitationSpaceError
	if errors.As(err, &spaceError) {
		return spaceError.WorkspaceID
	}
	return ""
}

func userFriendlyGraphQLExtensions(status int, errorType, name, message string, data any) map[string]any {
	return map[string]any{
		"status":  status,
		"code":    http.StatusText(status),
		"type":    errorType,
		"name":    name,
		"message": message,
		"data":    data,
	}
}

func websocketSessionMiddleware(authenticator RequestAuthenticator) gqlgraphql.OperationMiddleware {
	return func(ctx context.Context, next gqlgraphql.OperationHandler) gqlgraphql.ResponseHandler {
		request, ok := ctx.Value(websocketUpgradeRequestKey{}).(*http.Request)
		if !ok {
			return next(ctx)
		}

		principal, err := authenticator.FromRequest(ctx, request.WithContext(ctx))
		if errors.Is(err, auth.ErrNoSession) || err == nil && principal == nil {
			return gqlgraphql.OneShot(gqlgraphql.ErrorResponse(ctx, "%s", ErrAuthenticationRequired.Error()))
		}
		if err != nil {
			return gqlgraphql.OneShot(gqlgraphql.ErrorResponse(ctx, "%s", "authentication service unavailable"))
		}
		return next(auth.WithPrincipal(ctx, principal))
	}
}

func writeGraphQLHTTPError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"message": message}},
	})
}
