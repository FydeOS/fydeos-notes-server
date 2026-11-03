// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/cors"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zishang520/engine.io/v2/types"
	"github.com/zishang520/socket.io/v2/socket"
)

const socketPath = "/socket.io"
const anonymousRealtimeMaxPayloadSize int64 = 1 << 20

type Server struct {
	io                          *socket.Server
	handler                     http.Handler
	authenticator               SessionAuthenticator
	maxHttpBufferSize           int64
	authenticatedClients        sync.Map
	workspace                   WorkspaceDependencies
	commentReader               CommentChangeRepository
	commentAuthorizer           WorkspaceAuthorizer
	inviteLinks                 WorkspaceInviteLinkReader
	inviteLinkAuthorizer        WorkspaceUserManager
	notificationCountRepository NotificationCountRepository
	userSettingsReader          UserSettingsReader
	subscriptions               *workspaceSubscriptionRegistry
	fanout                      *realtimeFanout
	userUpdates                 *userUpdateQueue
}

func New(cfg config.Config) (*Server, error) {
	return NewWithDependencies(cfg, nil, nil)
}

type SessionAuthenticator interface {
	FromRequest(ctx context.Context, request *http.Request) (*auth.Principal, error)
}

type UserDocumentSync interface {
	Push(ctx context.Context, userID, docID string, update []byte) (int64, error)
	Diff(ctx context.Context, userID, docID string, stateVector []byte) ([]byte, []byte, int64, error)
	Timestamps(ctx context.Context, userID string, after *int64) (map[string]int64, error)
	Delete(ctx context.Context, userID, docID string) error
}

type WorkspaceAuthorizer interface {
	AuthorizeWorkspaceSync(context.Context, string, string, []string) (workspaces.WorkspaceSyncAccess, error)
}

type WorkspaceMemberReader interface {
	MemberCount(context.Context, string) (int, error)
	ListMembersForUser(context.Context, string, string, string, int, int) ([]workspaces.Member, error)
}

type UserUpdateReader interface {
	UserWorkspacePage(context.Context, string, string, int) ([]string, error)
	DirectGrantDocPage(context.Context, string, *workspaces.DocGrantRef, int) ([]workspaces.DocGrantRef, error)
	ListDirectDocGrantUsers(context.Context, string, string, int, int, *time.Time) (workspaces.DocGrantPage, error)
}

type WorkspaceDocumentAuthorizer interface {
	AuthorizeDocument(context.Context, string, string, string) (workspaces.DocumentAuthorization, error)
}

type WorkspaceAccessProvider interface {
	GetWorkspaceAccess(context.Context, string, string) (workspaces.WorkspaceAccess, error)
}

type WorkspaceRolePermissionsProvider interface {
	GetWorkspaceRolePermissions(context.Context, string, string) (workspaces.WorkspaceAccess, error)
}

type WorkspaceConfigProvider interface {
	GetWorkspaceConfig(context.Context, string, string) (workspaces.WorkspaceConfig, error)
}

type WorkspaceEmbeddingProgressProvider interface {
	ReadEmbeddingProgress(context.Context, string) (int64, int64, error)
}

type QuotaStateReader interface {
	ReadUserQuotaState(context.Context, string) (workspaces.UserQuotaState, error)
	ReadWorkspaceQuotaState(context.Context, string) (workspaces.WorkspaceQuotaState, error)
}

type WorkspaceUserManager interface {
	CanManageWorkspaceUsers(context.Context, string, string) (bool, error)
}

type UserSettingsReader interface {
	GetSettings(context.Context, string) (users.UserSettings, error)
}

type WorkspaceInviteLinkReader interface {
	Get(context.Context, string) (*invitelinks.Link, error)
}

type WorkspaceDocumentSync interface {
	CurrentPermissionGeneration(context.Context, string) (int64, error)
	PushWorkspace(context.Context, string, string, string, []byte, int64) (documents.WorkspacePushResult, error)
	ApplyWorkspaceDocLifecycle(context.Context, string, string, string, documents.WorkspaceDocLifecycle, int64) (documents.WorkspaceDocLifecycleResult, error)
	DiffWorkspace(context.Context, string, string, []byte) ([]byte, []byte, int64, error)
	WorkspaceTimestamps(context.Context, string, *int64) (map[string]int64, error)
}

type WorkspaceDependencies struct {
	ShareState     DocShareStateReader
	Authorizer     WorkspaceAuthorizer
	Documents      WorkspaceDocumentSync
	Access         WorkspaceAccessProvider
	QuotaAccess    WorkspaceRolePermissionsProvider
	Config         WorkspaceConfigProvider
	Embedding      WorkspaceEmbeddingProgressProvider
	Quota          QuotaStateReader
	Members        WorkspaceMemberReader
	UserUpdates    UserUpdateReader
	UserUpdateJobs UserUpdateRefreshJobStore
	GrantsAuth     WorkspaceDocumentAuthorizer
	InviteLinks    WorkspaceInviteLinkReader
	UserManager    WorkspaceUserManager
}

func NewWithDependencies(cfg config.Config, authenticator SessionAuthenticator, documents UserDocumentSync, workspaceDependencies ...WorkspaceDependencies) (*Server, error) {
	return newWithDependencies(cfg, authenticator, documents, nil, workspaceDependencies...)
}

func NewWithRedis(cfg config.Config, authenticator SessionAuthenticator, documents UserDocumentSync, redisClient redis.UniversalClient, workspaceDependencies ...WorkspaceDependencies) (*Server, error) {
	return newWithDependencies(cfg, authenticator, documents, redisClient, workspaceDependencies...)
}

func newWithDependencies(cfg config.Config, authenticator SessionAuthenticator, documents UserDocumentSync, redisClient redis.UniversalClient, workspaceDependencies ...WorkspaceDependencies) (*Server, error) {
	maxHttpBufferSize := cfg.WebsocketMaxHttpBufferSize
	if maxHttpBufferSize == 0 {
		maxHttpBufferSize = config.DefaultWebsocketMaxHttpBufferSize
	}
	if maxHttpBufferSize < 1 {
		return nil, errors.New("websocket.maxHttpBufferSize must be positive")
	}
	if (authenticator == nil) != (documents == nil) {
		return nil, errors.New("realtime authentication and document sync must be configured together")
	}
	if len(workspaceDependencies) > 1 {
		return nil, errors.New("realtime accepts at most one workspace sync dependency set")
	}
	var workspace WorkspaceDependencies
	if len(workspaceDependencies) == 1 {
		workspace = workspaceDependencies[0]
		if workspace.UserUpdateJobs != nil && workspace.UserUpdates == nil {
			return nil, errors.New("durable user update refresh requires a user update reader")
		}
		if (workspace.Authorizer == nil) != (workspace.Documents == nil) {
			return nil, errors.New("workspace authorization and document sync must be configured together")
		}
		if workspace.Authorizer != nil && authenticator == nil {
			return nil, errors.New("workspace sync requires realtime authentication")
		}
		if (workspace.InviteLinks == nil) != (workspace.UserManager == nil) {
			return nil, errors.New("workspace invite link reader and user manager must be configured together")
		}
		if workspace.InviteLinks != nil && authenticator == nil {
			return nil, errors.New("workspace invite links require realtime authentication")
		}
		if workspace.Members != nil && authenticator == nil {
			return nil, errors.New("workspace member realtime requires authentication")
		}
		if workspace.UserUpdates != nil && authenticator == nil {
			return nil, errors.New("user update realtime requires authentication")
		}
		if workspace.GrantsAuth != nil && authenticator == nil {
			return nil, errors.New("document grants realtime requires authentication")
		}
		if workspace.Access != nil && authenticator == nil {
			return nil, errors.New("workspace access realtime requires authentication")
		}
		if workspace.Config != nil && authenticator == nil {
			return nil, errors.New("workspace config realtime requires authentication")
		}
		if workspace.Embedding != nil && authenticator == nil {
			return nil, errors.New("workspace embedding progress requires realtime authentication")
		}
		if workspace.Quota != nil && authenticator == nil {
			return nil, errors.New("quota state realtime requires authentication")
		}
		if workspace.QuotaAccess != nil && authenticator == nil {
			return nil, errors.New("quota state authorization requires authentication")
		}
	}
	options := socket.DefaultServerOptions()
	options.SetPath(socketPath)
	options.SetTransports(types.NewSet("polling", "websocket"))
	options.SetMaxHttpBufferSize(maxHttpBufferSize)

	io := socket.NewServer(nil, options)
	subscriptions := newWorkspaceSubscriptionRegistry()
	server := &Server{
		io: io, workspace: workspace, subscriptions: subscriptions,
		authenticator: authenticator, maxHttpBufferSize: maxHttpBufferSize,
		inviteLinks: workspace.InviteLinks, inviteLinkAuthorizer: workspace.UserManager,
	}
	server.handler = cors.Realtime(cfg, io.ServeHandler(options))
	io.Engine().Use(server.guardIncomingPayloads)
	if workspace.UserUpdates != nil {
		if workspace.UserUpdateJobs != nil {
			server.userUpdates = newDurableUserUpdateQueue(server, workspace.UserUpdates, workspace.UserUpdateJobs)
		} else {
			server.userUpdates = newUserUpdateQueue(server, workspace.UserUpdates)
		}
	}
	if authenticator != nil && redisClient != nil {
		server.fanout = newRealtimeFanout(redisClient, uuid.NewString(), server.dispatchFanoutMessage)
	}
	if authenticator != nil {
		io.Use(func(client *socket.Socket, next func(*socket.ExtendedError)) {
			request := client.Request().Request()
			ctx, cancel := realtimeOperationContext()
			defer cancel()
			principal, err := authenticator.FromRequest(ctx, request)
			if err != nil && !errors.Is(err, auth.ErrNoSession) {
				next(socket.NewExtendedError("unauthorized", map[string]string{"code": "UNAUTHORIZED"}))
				return
			}
			authenticated := err == nil && principal != nil
			limit := anonymousRealtimeMaxPayloadSize
			if authenticated {
				limit = server.maxHttpBufferSize
			}
			server.authenticatedClients.Store(client.Conn().Id(), authenticated)
			client.Conn().Transport().SetMaxHttpBufferSize(limit)
			state := &syncClientState{
				docs: make(map[string]struct{}), spaces: make(map[string]struct{}),
				workspaceDocs: make(map[string]workspaceDocSubscription), workspaceSpaces: make(map[string]int64),
			}
			if principal != nil {
				state.userID = principal.User.ID
				state.sessionID = principal.SessionID
				state.userSessionID = principal.UserSessionID
			}
			client.SetData(state)
			next(nil)
		})
		_ = io.On("connection", func(args ...any) {
			if len(args) == 0 {
				return
			}
			client, ok := args[0].(*socket.Socket)
			if !ok {
				return
			}
			registerRealtimeHandlers(client, authenticator, cfg.Namespace == "dev", server)
			registerSyncHandlers(client, authenticator, documents, workspace, subscriptions, server, cfg.Namespace == "dev")
			_ = client.On("disconnect", func(...any) {
				server.authenticatedClients.Delete(client.Conn().Id())
				subscriptions.removeClient(string(client.Id()))
			})
		})
	}
	return server, nil
}

func (s *Server) guardIncomingPayloads(ctx *types.HttpContext, next func(error)) {
	request := ctx.Request()
	transport := ctx.Query().Peek("transport")
	sid := ctx.Query().Peek("sid")
	if transport == "websocket" {
		if sid != "" {
			authenticated, ok := s.authenticatedClients.Load(sid)
			if (ok && authenticated != true) || (!ok && !s.requestAuthenticated(request)) {
				ctx.SetStatusCode(http.StatusUnauthorized)
				ctx.Write(nil)
				return
			}
		} else if !s.requestAuthenticated(request) {
			ctx.SetStatusCode(http.StatusUnauthorized)
			ctx.Write(nil)
			return
		}
	}
	if transport == "polling" && request.Method == http.MethodPost {
		limit := anonymousRealtimeMaxPayloadSize
		if authenticated, ok := s.authenticatedClients.Load(sid); ok && authenticated == true {
			limit = s.maxHttpBufferSize
		}
		if !boundRealtimeRequestBody(ctx, request, limit) {
			return
		}
	}
	next(nil)
}

func (s *Server) requestAuthenticated(request *http.Request) bool {
	if s.authenticator == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	principal, err := s.authenticator.FromRequest(ctx, request)
	return err == nil && principal != nil
}

func realtimeOperationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func boundRealtimeRequestBody(ctx *types.HttpContext, request *http.Request, limit int64) bool {
	if request.ContentLength > limit {
		ctx.SetStatusCode(http.StatusRequestEntityTooLarge)
		ctx.Write(nil)
		return false
	}
	if request.ContentLength >= 0 || request.Body == nil {
		return true
	}
	if limit > anonymousRealtimeMaxPayloadSize {
		request.Body = http.MaxBytesReader(ctx.Response(), request.Body, limit)
		return true
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	_ = request.Body.Close()
	if err != nil {
		ctx.SetStatusCode(http.StatusBadRequest)
		ctx.Write(nil)
		return false
	}
	if int64(len(body)) > limit {
		ctx.SetStatusCode(http.StatusRequestEntityTooLarge)
		ctx.Write(nil)
		return false
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	return true
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) Close() {
	if s == nil {
		return
	}
	if s.userUpdates != nil {
		s.userUpdates.Close()
	}
	if s.fanout != nil {
		s.fanout.Close()
	}
	if s.io != nil {
		s.io.Close(nil)
	}
}

func (s *Server) PublishUserProfileChanged(userID string) {
	if s == nil || s.io == nil || userID == "" {
		return
	}
	s.broadcastRoom("user:"+userID+":profile", "realtime:event", map[string]any{
		"topic":    "user.profile.changed",
		"inputKey": "{}",
		"sentAt":   time.Now().UnixMilli(),
		"event":    map[string]any{"changed": true, "reason": "user-updated"},
	}, "")
}

func (s *Server) PublishUserUpdated(ctx context.Context, userID string) {
	s.PublishUserProfileChanged(userID)
	if s == nil || s.userUpdates == nil || userID == "" {
		return
	}
	if err := s.userUpdates.Enqueue(ctx, userID); err != nil {
		slog.Warn("user update realtime queue admission canceled", "user_id", userID, "error", err)
	}
}

func (s *Server) PublishDocGrantsChanged(workspaceID, docID, reason string) {
	s.publishDocGrantsChangedContext(context.Background(), workspaceID, docID, reason)
}

// PublishDocumentRoleChanged is called only after the ACL transaction commits.
func (s *Server) PublishDocumentRoleChanged(workspaceID, docID, reason string) {
	if s == nil || workspaceID == "" || docID == "" {
		return
	}
	s.publishWorkspacePermissionChange(workspaceID)
	if reason == "default-role-changed" {
		s.publishDocShareStateChanged(workspaceID, docID, reason)
	} else {
		s.PublishDocGrantsChanged(workspaceID, docID, reason)
	}
}

// PublishDocumentPublicStateChanged runs after a publication transaction commits.
func (s *Server) PublishDocumentPublicStateChanged(workspaceID, docID string) {
	if s == nil || workspaceID == "" || docID == "" {
		return
	}
	s.publishWorkspacePermissionChange(workspaceID)
	s.publishDocShareStateChanged(workspaceID, docID, "public-state-changed")
}

func (s *Server) publishDocShareStateChanged(workspaceID, docID, reason string) {
	inputKey, err := marshalDocGrantsInput(workspaceID, docID)
	if err != nil {
		return
	}
	s.broadcastRoom(docShareStateRoom(workspaceID, docID), "realtime:event", map[string]any{
		"topic": "doc.share-state.changed", "inputKey": string(inputKey), "sentAt": time.Now().UnixMilli(),
		"event": map[string]any{"changed": true, "reason": reason},
	}, "")
}

func (s *Server) publishDocGrantsChangedContext(ctx context.Context, workspaceID, docID, reason string) error {
	if s == nil || workspaceID == "" || docID == "" {
		return nil
	}
	inputKey, err := marshalDocGrantsInput(workspaceID, docID)
	if err != nil {
		return err
	}
	return s.broadcastRoomContext(ctx, docGrantsRoom(workspaceID, docID), "realtime:event", map[string]any{
		"topic": "doc.grants.changed", "inputKey": string(inputKey), "sentAt": time.Now().UnixMilli(),
		"event": map[string]any{"changed": true, "reason": reason},
	}, "")
}

func (s *Server) PublishUserSettingsChanged(userID string) {
	if s == nil || s.io == nil || userID == "" {
		return
	}
	s.broadcastRoom("user:"+userID+":settings", "realtime:event", map[string]any{
		"topic":    "user.settings.changed",
		"inputKey": "{}",
		"sentAt":   time.Now().UnixMilli(),
		"event":    map[string]any{"changed": true, "reason": "settings-updated"},
	}, "")
}

func (s *Server) broadcastRoom(room, event string, payload any, excludeSocketID string) {
	s.broadcastRoomContext(context.Background(), room, event, payload, excludeSocketID)
}

func (s *Server) broadcastRoomContext(ctx context.Context, room, event string, payload any, excludeSocketID string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	message := realtimeFanoutMessage{
		Kind: "room", Room: room, Event: event, Payload: payload, ExcludeSocketID: excludeSocketID,
	}
	s.dispatchFanoutMessage(message)
	return s.publishFanoutMessageContext(ctx, message)
}

func (s *Server) broadcastWorkspaceDocument(workspaceID, docID, excludeSocketID, event string, payload any, afterPermissionChange bool) {
	message := realtimeFanoutMessage{
		Kind: "workspace-document", WorkspaceID: workspaceID, DocID: docID,
		ExcludeSocketID: excludeSocketID, Event: event, Payload: payload,
		AfterPermissionChange: afterPermissionChange,
	}
	s.dispatchFanoutMessage(message)
	s.publishFanoutMessage(message)
}

func (s *Server) publishFanoutMessage(message realtimeFanoutMessage) {
	s.publishFanoutMessageContext(context.Background(), message)
}

func (s *Server) publishFanoutMessageContext(ctx context.Context, message realtimeFanoutMessage) error {
	if s == nil || s.fanout == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := s.fanout.PublishContext(ctx, message); err != nil {
		slog.Warn("realtime cluster fanout failed", "error", err)
		return err
	}
	return nil
}

func (s *Server) dispatchFanoutMessage(message realtimeFanoutMessage) {
	if s == nil || s.io == nil {
		return
	}
	switch message.Kind {
	case "room":
		operator := s.io.To(socket.Room(message.Room))
		if message.ExcludeSocketID != "" {
			operator = operator.Except(socket.Room(message.ExcludeSocketID))
		}
		if err := operator.Emit(message.Event, message.Payload); err != nil {
			slog.Warn("realtime room emit failed", "event", message.Event, "error", err)
		}
	case "workspace-document":
		if s.subscriptions == nil || s.workspace.Authorizer == nil || s.workspace.Documents == nil {
			return
		}
		ctx, cancel := syncOperationContext()
		if message.AfterPermissionChange {
			s.subscriptions.emitAfterPermissionChange(ctx, s.workspace.Authorizer, s.workspace.Documents, message.WorkspaceID, message.DocID, message.ExcludeSocketID, message.Event, message.Payload)
		} else {
			s.subscriptions.emit(ctx, s.workspace.Documents, message.WorkspaceID, message.DocID, message.ExcludeSocketID, message.Event, message.Payload)
		}
		cancel()
	case "workspace-permissions":
		if s.subscriptions == nil || s.workspace.Authorizer == nil || s.workspace.Documents == nil {
			return
		}
		ctx, cancel := syncOperationContext()
		s.subscriptions.revalidateWorkspace(ctx, s.workspace.Authorizer, s.workspace.Documents, message.WorkspaceID)
		cancel()
	}
}
