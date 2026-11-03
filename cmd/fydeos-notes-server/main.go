// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/appconfig"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/avatars"
	"github.com/fydeos/fydeos-notes-server/internal/blobmanifesthttp"
	"github.com/fydeos/fydeos-notes-server/internal/blobs"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/calendar"
	"github.com/fydeos/fydeos-notes-server/internal/commentattachments"
	"github.com/fydeos/fydeos-notes-server/internal/comments"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/database"
	dbmigrations "github.com/fydeos/fydeos-notes-server/internal/database/migrations"
	"github.com/fydeos/fydeos-notes-server/internal/documenthttp"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	graphserver "github.com/fydeos/fydeos-notes-server/internal/graphql"
	"github.com/fydeos/fydeos-notes-server/internal/httpserver"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/maildelivery"
	"github.com/fydeos/fydeos-notes-server/internal/mcpcredentials"
	"github.com/fydeos/fydeos-notes-server/internal/mcphttp"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/oauth"
	"github.com/fydeos/fydeos-notes-server/internal/realtime"
	"github.com/fydeos/fydeos-notes-server/internal/renderer"
	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/serverconfig"
	"github.com/fydeos/fydeos-notes-server/internal/setup"
	"github.com/fydeos/fydeos-notes-server/internal/staticfiles"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/worker"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	redisclient "github.com/redis/go-redis/v9"
)

var releaseVersion = "development"
var buildRevision = "unknown"

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 {
		if len(os.Args) == 2 && os.Args[1] == "version" {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{
				"backend": "go", "version": releaseVersion, "revision": buildRevision,
			})
		}
		if len(os.Args) == 2 && os.Args[1] == "ensure-private-key" {
			return runEnsurePrivateKey()
		}
		if len(os.Args) == 2 && os.Args[1] == "migrate" {
			return runMigrations()
		}
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
	return runServer()
}

func runMigrations() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := dbmigrations.Apply(ctx, db); err != nil {
		return err
	}
	slog.Info("Go backend database migrations are up to date")
	return nil
}

func runServer() (runErr error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := validateServerFlavor(cfg.Flavor); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 10*time.Second)
	db, err := database.Open(connectCtx, cfg.DatabaseURL)
	cancelConnect()
	if err != nil {
		return err
	}
	defer db.Close()
	if cfg.Flavor != "worker" {
		if err := dbmigrations.Check(ctx, db); err != nil {
			return fmt.Errorf("database migrations must be applied before starting the server: %w", err)
		}
	}
	cfg.WebsocketMaxHttpBufferSize, err = config.ReadWebsocketMaxHttpBufferSize(ctx, db, cfg.RuntimeConfigPath)
	if err != nil {
		return err
	}
	if cfg.Flavor == "worker" {
		return runWorker(ctx, db, cfg)
	}
	redisOptions, err := redisclient.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("invalid Redis configuration")
	}
	redisOptions.DialTimeout = time.Second
	redisOptions.ReadTimeout = time.Second
	redisOptions.WriteTimeout = time.Second
	redisOptions.MaxRetries = -1
	redisClient := redisclient.NewClient(redisOptions)
	defer redisClient.Close()
	socketRedisOptions := *redisOptions
	socketRedisOptions.DB += 3
	if socketRedisOptions.DB > 15 {
		return fmt.Errorf("Redis database index for Socket.IO exceeds the supported range")
	}
	socketRedisClient := redisclient.NewClient(&socketRedisOptions)
	defer socketRedisClient.Close()

	addr := net.JoinHostPort(cfg.ListenAddr, fmt.Sprint(cfg.Port))
	sessionResolver := auth.NewSessionResolverWithConfig(db, cfg)
	workspaceRepository := workspaces.NewRepositoryWithEntitlementConfig(db, cfg.Deployment)
	workspaceAuthorization := workspaces.NewAuthorizationService(workspaceRepository)
	inviteLinkStore := invitelinks.NewRedisStore(redisClient, config.PublicOrigin(cfg))
	realtimeServer, err := realtime.NewWithRedis(
		cfg,
		sessionResolver,
		documents.NewUserspaceRepository(db),
		socketRedisClient,
		realtime.WorkspaceDependencies{
			Authorizer:     workspaceAuthorization,
			Documents:      documents.NewWorkspaceRepositoryWithEntitlementConfig(db, cfg.Deployment),
			Access:         workspaceAuthorization,
			QuotaAccess:    workspaceAuthorization,
			Config:         workspaceAuthorization,
			Embedding:      search.NewEmbeddingProgressReader(db),
			Quota:          workspaceRepository,
			Members:        workspaceRepository,
			UserUpdates:    workspaceRepository,
			UserUpdateJobs: realtime.NewPostgresUserUpdateRefreshJobStore(db),
			GrantsAuth:     workspaceAuthorization,
			ShareState:     workspaceRepository,
			InviteLinks:    inviteLinkStore,
			UserManager:    workspaceAuthorization,
		},
	)
	if err != nil {
		return err
	}
	defer realtimeServer.Close()

	handlers := httpserver.ApplicationHandlers{SocketIO: realtimeServer.Handler()}
	publicServerConfig := serverconfig.New(cfg, db)
	authReadLimiter := auth.NewReadLimiter(auth.DefaultReadConcurrency)
	authLoginLimiter := auth.NewRedisPasswordLoginLimiter(redisClient)
	oauthProviderSource := oauth.NewRuntimeOAuthProviderSource(db, cfg)
	captchaService := auth.NewCaptchaService(db, cfg)
	handlers.AuthSession = auth.NewSessionHTTPHandler(sessionResolver, authReadLimiter)
	handlers.AuthPreflight = auth.NewLoginPreflightHTTPHandler(db, cfg, oauth.NewProviderCatalogWithOAuthProviderSource(oauthProviderSource), authLoginLimiter, authReadLimiter)
	handlers.AuthSignIn = auth.NewPasswordLoginHTTPHandlerWithDependencies(db, cfg, authLoginLimiter, captchaService)
	handlers.AuthCaptcha = auth.NewCaptchaHTTPHandler(captchaService, authLoginLimiter)
	handlers.AuthOpenApp = auth.NewOpenAppHTTPHandler(db, cfg, sessionResolver, authLoginLimiter)
	handlers.AuthMagicLink = auth.NewMagicLinkHTTPHandler(db, cfg, authLoginLimiter, os.Getenv("AFFINE_PRIVATE_KEY"))
	handlers.NativeAuthSession = auth.NewNativeSessionHTTPHandler(db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
	handlers.AuthMethods = auth.NewBoundMethodsHTTPHandler(sessionResolver, auth.NewBoundMethodsRepository(db), authReadLimiter)
	handlers.AuthSessions = auth.NewSessionManagementHTTPHandler(sessionResolver, auth.NewPostgresSessionManagementRepository(db), authReadLimiter)
	handlers.AuthSignOut = auth.NewSignOutHTTPHandler(sessionResolver, auth.NewPostgresCookieSessionRepository(db), authReadLimiter)
	if cfg.Deployment == "selfhosted" {
		handlers.Setup = setup.NewHTTPHandler(db, publicServerConfig.PasswordLimits)
	}
	if cfg.Flavor == "front" || (cfg.Flavor != "worker" && (cfg.Deployment == "selfhosted" || cfg.Namespace == "dev")) {
		handlers.Frontend = staticfiles.NewHandler(staticfiles.HandlerConfig{
			Root:        runtimeStaticRootFromProcess(),
			ServerPath:  cfg.Path,
			Namespace:   cfg.Namespace,
			Deployment:  cfg.Deployment,
			Initialized: publicServerConfig.Initialized,
		})
	}
	if cfg.Flavor == "renderer" {
		webAssets, mobileAssets, err := renderer.LoadAssets(runtimeStaticRootFromProcess(), cfg.Deployment == "selfhosted", cfg.NodeEnv == "production")
		if err != nil {
			return err
		}
		previewReader := workspaces.NewDocumentReadService(workspaceRepository, nil)
		previewReader.SetWorkspaceMetadataNotifier(realtimeServer)
		analytics := workspaces.NewDocumentAnalyticsService(workspaceRepository, redisClient)
		handlers.Renderer = renderer.NewHandler(previewReader, analytics, renderer.Config{
			ServerPath: cfg.Path, Deployment: cfg.Deployment, Namespace: cfg.Namespace,
			PublicBaseURL: config.PublicBaseURL(cfg), WebAssets: webAssets, MobileAssets: mobileAssets,
			TrustedProxyCIDRs: cfg.TrustedProxyCIDRs, TrustedCloudflareCIDRs: cfg.TrustedCloudflareCIDRs, TrustedXRealIPCIDRs: cfg.TrustedXRealIPCIDRs,
		})
	}
	if cfg.Flavor == "allinone" || cfg.Flavor == "graphql" {
		providerCtx, cancelProvider := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = oauthProviderSource.LoadProvider(providerCtx, "fydeos")
		cancelProvider()
		if err != nil {
			return err
		}
		storageCtx, cancelStorage := context.WithTimeout(context.Background(), 5*time.Second)
		storageRuntime, err := storage.NewRuntime(storageCtx, db, cfg.RuntimeConfigPath, os.LookupEnv)
		cancelStorage()
		if err != nil {
			return err
		}

		userRepository := users.NewRepository(db)
		realtimeServer.SetUserSettingsReader(userRepository)
		avatarConfigCtx, cancelAvatarConfig := context.WithTimeout(ctx, 5*time.Second)
		avatarPublicPath, err := loadAvatarPublicPath(avatarConfigCtx, db, cfg)
		cancelAvatarConfig()
		if err != nil {
			return err
		}
		avatarService := avatars.NewService(userRepository, storageRuntime, avatarPublicPath, config.PublicBaseURL(cfg))
		handlers.Avatars = avatars.NewPublicHandler(avatarService)
		blobService := blobs.NewService(db, storageRuntime, cfg.Deployment, config.PublicBaseURL(cfg))
		blobService.SetCommentDocumentAuthorizer(workspaceAuthorization)
		blobService.SetQuotaStateChangePublisher(realtimeServer)
		blobManifestService := blobs.NewManifestService(db, workspaceAuthorization, storageRuntime)
		handlers.BlobManifests = blobmanifesthttp.NewHandler(sessionResolver, blobManifestService)
		handlers.StorageProxy = blobService.ProxyUploadHandler()
		handlers.CommentAttachments = commentattachments.NewHandler(sessionResolver, workspaceAuthorization, blobService)
		commentRepository := comments.NewRepositoryWithAuthorizer(db, workspaceAuthorization)
		notificationRepository := notifications.NewRepository(db, config.PublicBaseURL(cfg))
		memberRemovalMailer := newMailDeliveryRepository(db, cfg)
		accountLifecycle := users.NewAccountLifecycleService(users.AccountLifecycleDependencies{
			Repository: userRepository,
			Workspaces: workspaceRepository,
			ListOwnedWorkspaces: func(ctx context.Context, userID string) ([]users.OwnedWorkspace, error) {
				owned, err := workspaceRepository.ListOwnedForUser(ctx, userID)
				if err != nil {
					return nil, err
				}
				result := make([]users.OwnedWorkspace, len(owned))
				for i, workspace := range owned {
					result[i] = users.OwnedWorkspace{ID: workspace.ID, Team: workspace.Team}
				}
				return result, nil
			},
			Objects: storageRuntime,
			Avatars: avatarService,
			Mail:    memberRemovalMailer,
		})
		userService := users.NewService(userRepository, workspaceAuthorization, accountLifecycle)
		inviteQuotaAdmission := invitelinks.NewWorkspaceInviteQuotaAdmission(db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
		documentRepository := documents.NewWorkspaceRepositoryWithEntitlementConfig(db, cfg.Deployment)
		mcpCredentialService := mcpcredentials.NewService(db, cfg)
		handlers.MCP = mcphttp.NewHandler(mcpCredentialService, workspaceAuthorization, documentRepository)
		documentReadService := workspaces.NewDocumentReadService(workspaceRepository, workspaces.NewRedisDocumentOwnerRepairGate(redisClient))
		documentReadService.SetDocumentOwnerRepairNotifier(realtimeServer)
		documentAnalytics := workspaces.NewDocumentAnalyticsService(workspaceRepository, redisClient)
		documentHTTP := documenthttp.NewHandler(sessionResolver, workspaceAuthorization, documentRepository, workspaceRepository, documentReadService, documentAnalytics)
		handlers.WorkspaceDocuments = documentHTTP
		handlers.WorkspacePublicRootDoc = documentHTTP.PublicRootDocHandler()
		go inviteQuotaAdmission.RunInviteAbuseWorker(ctx)
		if cfg.Flavor == "allinone" {
			projectionWorker := search.NewProjectionWorker(db, documentRepository, realtimeServer)
			go func() {
				if err := projectionWorker.Run(ctx); err != nil {
					slog.ErrorContext(ctx, "search projection worker stopped", "error", err)
				}
			}()
			embeddingTargets := newEmbeddingTargetResolver(db, cfg)
			embeddingCoordinator := search.NewEmbeddingCoordinator(db, embeddingTargets)
			go func() {
				if err := embeddingCoordinator.Run(ctx); err != nil {
					slog.Error("embedding coordinator stopped", "error", err)
				}
			}()
			embeddingWorker := search.NewEmbeddingWorker(db, embeddingTargets, realtimeServer, storageRuntime)
			go func() {
				if err := embeddingWorker.Run(ctx); err != nil {
					slog.Error("embedding worker stopped", "error", err)
				}
			}()
		}
		realtimeServer.SetCommentDependencies(commentRepository, workspaceAuthorization)
		realtimeServer.SetNotificationCountRepository(notificationRepository)
		byokProfiles := byok.NewProfileStore(db, cfg.RuntimeConfigPath, cfg.Deployment, func(ctx context.Context) (string, error) {
			return auth.LoadAppPrivateKey(ctx, db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
		})
		copilotStore := copilot.NewStore(db)
		copilotWorkspaceArtifacts := copilot.NewWorkspaceArtifacts(db, storageRuntime)
		copilotTranscriptTasks, transcriptWorker := newTranscriptRuntime(db, cfg, storageRuntime, realtimeServer)
		if cfg.Flavor == "allinone" {
			transcriptCtx, cancelTranscript := context.WithCancel(ctx)
			transcriptDone := make(chan error, 1)
			go func() { transcriptDone <- transcriptWorker.Run(transcriptCtx) }()
			defer func() {
				cancelTranscript()
				select {
				case err := <-transcriptDone:
					runErr = errors.Join(runErr, err)
				case <-time.After(10 * time.Second):
					runErr = errors.Join(runErr, errors.New("transcript worker shutdown timed out"))
				}
			}()
		}
		authorizeCopilotSession := func(ctx context.Context, userID, workspaceID string, docID *string) error {
			if docID != nil && *docID != "" {
				permissions, err := documentReadService.ReadDocumentPermissions(ctx, userID, workspaceID, *docID)
				if err != nil {
					return err
				}
				if !permissions["Doc.Update"] {
					return workspaces.ErrDocRoleDenied
				}
				return nil
			}
			access, err := workspaceAuthorization.GetWorkspaceRolePermissions(ctx, userID, workspaceID)
			if err != nil {
				return err
			}
			if !access.Permissions["Workspace.Copilot"] {
				return workspaces.ErrAccessDenied
			}
			return nil
		}
		copilotAttachments := copilot.NewSessionAttachments(storageRuntime, copilotStore, authorizeCopilotSession, sessionResolver, mcpCredentialService)
		handlers.CopilotAttachments = copilotAttachments
		copilotChatHandler := copilot.NewChatHTTPHandler(db, cfg, sessionResolver, copilotStore, authorizeCopilotSession, mcpCredentialService)
		copilotChatHandler.SetBYOKProfileStore(byokProfiles)
		copilotChatHandler.SetImageTargetResolver(copilot.NewImageRoutes(db, cfg, byokProfiles))
		copilotChatHandler.SetQuotaStatePublisher(realtimeServer)
		documentChatTools := copilot.NewDocumentChatTools(search.NewDocsService(db), documentReadService)
		documentChatTools.SetSemanticSearcher(search.NewEmbeddingCandidateSearcher(db, newEmbeddingTargetResolver(db, cfg)))
		documentChatTools.SetArtifactTools(search.NewEmbeddingCandidateSearcher(db, newEmbeddingTargetResolver(db, cfg)), copilotWorkspaceArtifacts, documentReadService)
		copilotChatHandler.SetToolExecutor(documentChatTools)
		copilotChatHandler.SetSessionAttachments(copilotAttachments)
		copilotChatHandler.SetDocumentBlobReader(func(ctx context.Context, actorID, workspaceID, docID, key string) (io.ReadCloser, string, int64, error) {
			body, metadata, err := blobManifestService.OpenBlobV1(ctx, actorID, blobs.BlobSource{
				Type: "currentDoc", WorkspaceID: workspaceID, DocID: docID,
			}, key)
			return body, metadata.MIME, metadata.Size, err
		})
		handlers.Copilot = copilotChatHandler
		handlers.CopilotActions = copilotChatHandler.ActionHandler()
		caldavService := calendar.NewService(db, cfg.RuntimeConfigPath, func(ctx context.Context) (string, error) {
			return auth.LoadAppPrivateKey(ctx, db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
		})
		googleCalendarLinker := calendar.NewGoogleAccountLinker(db, cfg.RuntimeConfigPath, func(ctx context.Context) (string, error) {
			return auth.LoadAppPrivateKey(ctx, db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
		}, config.PublicBaseURL(cfg))
		handlers.CalendarOAuth = googleCalendarLinker
		handlers.CalendarWebhook = googleCalendarLinker.WebhookHandler()
		if cfg.Flavor == "allinone" {
			calendarWorkerCtx, cancelCalendarWorker := context.WithCancel(ctx)
			calendarWorkerDone := make(chan error, 1)
			go func() {
				calendarWorkerDone <- worker.RunCalendarSync(calendarWorkerCtx, worker.CalendarSyncGroup{googleCalendarLinker, caldavService})
			}()
			defer func() {
				cancelCalendarWorker()
				select {
				case err := <-calendarWorkerDone:
					runErr = errors.Join(runErr, err)
				case <-time.After(10 * time.Second):
					runErr = errors.Join(runErr, errors.New("calendar worker shutdown timed out"))
				}
			}()
		}
		handlers.GraphQL = graphserver.NewHandlerWithServices(
			cfg,
			sessionResolver,
			userService,
			workspaceRepository,
			authReadLimiter,
			realtimeServer,
			storageRuntime,
			blobService,
			graphserver.HandlerDependencies{
				CalDAVAccountLinker:         caldavService,
				GoogleCalendarAccountLinker: googleCalendarLinker,
				CalendarProviderCatalog:     googleCalendarLinker,
				CopilotSessions:             copilotStore,
				CopilotAttachments:          copilotAttachments,
				CopilotWorkspaceArtifacts:   copilotWorkspaceArtifacts,
				CopilotTranscriptTasks:      copilotTranscriptTasks,
				TranscriptTaskPublisher:     realtimeServer,
				SearchDocs:                  search.NewDocsService(db),
				SearchProjection:            search.NewProjectionService(db),
				BYOKProfiles:                byokProfiles,
				AdminEntitlements:           entitlements.NewAdminGrantStore(db, cfg.Deployment),
				ServerConfig:                publicServerConfig,
				LicenseWorkspacePublisher:   realtimeServer,
				LicenseQuotaPublisher:       realtimeServer,
				CopilotQuotaPublisher:       realtimeServer,
				MCPCredentials:              mcpCredentialService,
				OAuthProviders:              oauth.NewProviderCatalogWithOAuthProviderSource(oauthProviderSource),
				WorkspacePermissionReader:   workspaceAuthorization,
				DocumentReader:              documentReadService,
				DocumentAnalytics:           documentAnalytics,
				DocumentHistory: graphserver.DocumentHistoryDependencies{
					Repository: documentRepository,
					Access:     documentReadService,
				},
				DocumentPublications: graphserver.DocumentPublicationDependencies{Service: workspaces.NewDocumentPublicationService(workspaceRepository, documentRepository), Publisher: realtimeServer},
				DocumentRoles:        graphserver.DocumentRoleDependencies{Service: workspaces.NewDocumentRoleService(workspaceRepository, documentRepository), Publisher: realtimeServer},
				WorkspaceConfig:      realtimeServer,
				UserSettings:         realtimeServer,
				AvatarUploader:       avatarService,
				Comments: graphserver.CommentReadDependencies{
					Repository: commentRepository,
					Writer:     commentRepository,
					Authorizer: workspaceAuthorization,
					Publisher:  realtimeServer,
				},
				Notifications: graphserver.NotificationDependencies{
					Repository: notificationRepository,
					Authorizer: workspaceAuthorization,
					Publisher:  realtimeServer,
				},
				SecurityChallenges: graphserver.SecurityChallengeDependencies{
					Service: auth.NewSecurityChallengeService(db, cfg, os.Getenv("AFFINE_PRIVATE_KEY")),
				},
				AuthSigningKeys:       auth.NewAuthSigningKeyRing(db, cfg),
				AppConfig:             appconfig.New(db, cfg.RuntimeConfigPath),
				SMTPTestEmailSender:   maildelivery.NewTestEmailSender(),
				MailDeliveryAnalytics: memberRemovalMailer,
				InviteLinks: graphserver.WorkspaceInviteLinkDependencies{
					Reader:     inviteLinkStore,
					Creator:    invitelinks.NewCreator(db, inviteLinkStore, cfg, os.Getenv("AFFINE_PRIVATE_KEY")),
					Revoker:    inviteLinkStore,
					Authorizer: workspaceAuthorization,
					Publisher:  realtimeServer,
				},
				InviteInfo: graphserver.WorkspaceInvitationInfoDependencies{
					Repository:   workspaceRepository,
					LinkLookup:   inviteLinkStore,
					AvatarReader: storageRuntime,
				},
				WorkspaceInvitations: graphserver.WorkspaceInviteMutationDependencies{
					Repository:         workspaceRepository,
					Admission:          inviteQuotaAdmission,
					Notifications:      notificationRepository,
					NotificationMailer: memberRemovalMailer,
					CountPublisher:     realtimeServer,
					Publisher:          realtimeServer,
				},
				InvitationAcceptance: graphserver.WorkspaceInvitationAcceptanceDependencies{
					Repository:       workspaceRepository,
					LinkLookup:       inviteLinkStore,
					Notifications:    notificationRepository,
					InvitationMailer: memberRemovalMailer,
					CountPublisher:   realtimeServer,
					MembersPublisher: realtimeServer,
				},
				WorkspaceRoles: graphserver.WorkspaceRoleMutationDependencies{
					Repository:       workspaceRepository,
					Notifications:    notificationRepository,
					InvitationMailer: memberRemovalMailer,
					RemovalMailer:    memberRemovalMailer,
					LeaveMailer:      memberRemovalMailer,
					CountPublisher:   realtimeServer,
					Publisher:        realtimeServer,
				},
			},
		)
		handlers.OAuthPreflight = oauth.NewPreflightHandlerWithLimiter(
			oauth.NewServiceWithOAuthProviderSource(oauthProviderSource, oauth.NewStateStore(db)),
			cfg,
			oauth.NewRedisStrictLimiter(redisClient),
		)
		handlers.OAuthCallback = oauth.NewCallbackHandler(oauth.NewCallbackServiceWithOAuthProviderSourceAndConfig(db, oauthProviderSource, cfg), cfg)
	}
	server := &http.Server{
		Addr: addr,
		Handler: httpserver.NewApplicationHandler(cfg, db.Ready, httpserver.ApplicationHandlers{
			GraphQL:                handlers.GraphQL,
			AuthSession:            handlers.AuthSession,
			AuthPreflight:          handlers.AuthPreflight,
			AuthSignIn:             handlers.AuthSignIn,
			AuthCaptcha:            handlers.AuthCaptcha,
			AuthOpenApp:            handlers.AuthOpenApp,
			AuthMagicLink:          handlers.AuthMagicLink,
			NativeAuthSession:      handlers.NativeAuthSession,
			AuthMethods:            handlers.AuthMethods,
			AuthSessions:           handlers.AuthSessions,
			AuthSignOut:            handlers.AuthSignOut,
			OAuthPreflight:         handlers.OAuthPreflight,
			OAuthCallback:          handlers.OAuthCallback,
			CalendarOAuth:          handlers.CalendarOAuth,
			CalendarWebhook:        handlers.CalendarWebhook,
			Copilot:                handlers.Copilot,
			SocketIO:               handlers.SocketIO,
			CopilotAttachments:     handlers.CopilotAttachments,
			CopilotActions:         handlers.CopilotActions,
			StorageProxy:           handlers.StorageProxy,
			CommentAttachments:     handlers.CommentAttachments,
			BlobManifests:          handlers.BlobManifests,
			MCP:                    handlers.MCP,
			WorkspaceDocuments:     handlers.WorkspaceDocuments,
			WorkspacePublicRootDoc: handlers.WorkspacePublicRootDoc,
			Avatars:                handlers.Avatars,
			Frontend:               handlers.Frontend,
			Renderer:               handlers.Renderer,
			Setup:                  handlers.Setup,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("Notes Go backend listening", "address", addr, "flavor", cfg.Flavor, "version", cfg.Version)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runWorker(ctx context.Context, db *database.Pool, cfg config.Config) error {
	storageCtx, cancelStorage := context.WithTimeout(ctx, 5*time.Second)
	storageRuntime, err := storage.NewRuntime(storageCtx, db, cfg.RuntimeConfigPath, os.LookupEnv)
	cancelStorage()
	if err != nil {
		return err
	}
	blobService := blobs.NewService(db, storageRuntime, cfg.Deployment, config.PublicBaseURL(cfg))
	redisOptions, err := redisclient.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("invalid Redis configuration")
	}
	redisOptions.DialTimeout = time.Second
	redisOptions.ReadTimeout = time.Second
	redisOptions.WriteTimeout = time.Second
	redisOptions.MaxRetries = -1
	socketRedisOptions := *redisOptions
	socketRedisOptions.DB += 3
	if socketRedisOptions.DB > 15 {
		return fmt.Errorf("Redis database index for Socket.IO exceeds the supported range")
	}
	socketRedisClient := redisclient.NewClient(&socketRedisOptions)
	defer socketRedisClient.Close()

	sessionResolver := auth.NewSessionResolverWithConfig(db, cfg)
	realtimeServer, err := realtime.NewWithRedis(cfg, sessionResolver, documents.NewUserspaceRepository(db), socketRedisClient)
	if err != nil {
		return err
	}
	defer realtimeServer.Close()
	blobService.SetQuotaStateChangePublisher(realtimeServer)
	realtimeServer.SetUserSettingsReader(users.NewRepository(db))
	notificationRepository := notifications.NewRepository(db, config.PublicBaseURL(cfg))
	realtimeServer.SetNotificationCountRepository(notificationRepository)
	mailSettings, err := maildelivery.LoadSettings(ctx, db, cfg, os.LookupEnv)
	if err != nil {
		return err
	}
	mailRepository := newMailDeliveryRepository(db, cfg)
	mailWorker, err := maildelivery.NewWorker(mailRepository, maildelivery.NewSMTPSender(mailSettings), mailSettings)
	if err != nil {
		return err
	}
	googleCalendarSync := calendar.NewGoogleAccountLinker(db, cfg.RuntimeConfigPath, func(ctx context.Context) (string, error) {
		return auth.LoadAppPrivateKey(ctx, db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
	}, config.PublicBaseURL(cfg))
	caldavCalendarSync := calendar.NewService(db, cfg.RuntimeConfigPath, func(ctx context.Context) (string, error) {
		return auth.LoadAppPrivateKey(ctx, db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
	})
	calendarSyncGroup := worker.CalendarSyncGroup{googleCalendarSync, caldavCalendarSync}
	slog.Info("worker flavor started", "enabled_tasks", []string{"expired_storage_reservation_cleanup", "workspace_storage_cleanup", "expired_notification_cleanup", "expired_rolling_quota_cleanup", "mail_delivery", "invite_abuse_actions", "calendar_sync", "search_projection", "embedding_route_reconciliation", "embedding_projection"})

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	const workerCount = 9
	results := make(chan error, workerCount)
	_, transcriptWorker := newTranscriptRuntime(db, cfg, storageRuntime, realtimeServer)
	go func() { results <- transcriptWorker.Run(workerCtx) }()
	inviteQuotaAdmission := invitelinks.NewWorkspaceInviteQuotaAdmission(db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
	go func() {
		results <- worker.RunDailyMaintenance(workerCtx, blobService, notificationRepository, notificationRepository, realtimeServer, mailRepository)
	}()
	go func() { results <- worker.RunWorkspaceStorageCleanup(workerCtx, storageRuntime) }()
	go func() { results <- mailWorker.Run(workerCtx) }()
	go func() { results <- worker.RunCalendarSync(workerCtx, calendarSyncGroup) }()
	go func() {
		results <- search.NewProjectionWorker(db, documents.NewWorkspaceRepositoryWithEntitlementConfig(db, cfg.Deployment), realtimeServer).Run(workerCtx)
	}()
	go func() { results <- newEmbeddingCoordinator(db, cfg).Run(workerCtx) }()
	embeddingTargets := newEmbeddingTargetResolver(db, cfg)
	go func() {
		results <- search.NewEmbeddingWorker(db, embeddingTargets, realtimeServer, storageRuntime).Run(workerCtx)
	}()
	go func() {
		inviteQuotaAdmission.RunInviteAbuseWorker(workerCtx)
		results <- nil
	}()
	first := <-results
	cancelWorkers()
	for remaining := 1; remaining < workerCount; remaining++ {
		first = errors.Join(first, <-results)
	}
	return first
}

func loadAvatarPublicPath(ctx context.Context, db config.RuntimeConfigReader, cfg config.Config) (string, error) {
	raw, found, err := config.ReadRuntimeValue(ctx, db, cfg.RuntimeConfigPath, "storages.avatar.publicPath")
	if err != nil {
		return "", err
	}
	if !found {
		return "/api/avatars/", nil
	}
	var publicPath string
	if err := json.Unmarshal(raw, &publicPath); err != nil || publicPath == "" {
		return "", errors.New("invalid avatar public path configuration")
	}
	return publicPath, nil
}

func newMailDeliveryRepository(db *database.Pool, cfg config.Config) *maildelivery.Repository {
	keyProvider := func(ctx context.Context) (string, error) {
		return auth.LoadAppPrivateKey(ctx, db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
	}
	return maildelivery.NewRepository(db, cfg, keyProvider, invitelinks.NewMemberLeaveMailPolicy(db))
}

func validateServerFlavor(flavor string) error {
	switch flavor {
	case "allinone", "graphql", "sync", "front", "worker", "renderer":
		return nil
	case "script":
		return fmt.Errorf("SERVER_FLAVOR=script is a command mode and cannot start the HTTP server")
	default:
		return fmt.Errorf("SERVER_FLAVOR=%q is not implemented by the Go backend", flavor)
	}
}

func runtimeStaticRootFromProcess() string {
	executable, _ := os.Executable()
	workingDirectory, _ := os.Getwd()
	return runtimeStaticRoot(executable, workingDirectory)
}

func runtimeStaticRoot(executablePath, workingDirectory string) string {
	executableRoot := filepath.Join(filepath.Dir(executablePath), "static")
	if isDirectory(executableRoot) {
		return executableRoot
	}
	workingRoot := filepath.Join(workingDirectory, "static")
	if isDirectory(workingRoot) {
		return workingRoot
	}
	return executableRoot
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
