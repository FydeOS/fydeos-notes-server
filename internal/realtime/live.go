// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/zishang520/socket.io/v2/socket"
	"golang.org/x/mod/semver"
)

var realtimeCanaryVersionPattern = regexp.MustCompile(`(?i)^v?(\d{4})\.(\d{1,2})\.(\d{1,2})-canary\.(\d+)(?:\+.*)?$`)

func registerRealtimeHandlers(client *socket.Socket, authenticator SessionAuthenticator, canaryEnabled bool, server *Server) {
	_ = client.On("realtime:request", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			respond(ack, nil, syncError("BAD_REQUEST", "Invalid realtime request."))
			return
		}
		if !assertRealtimeVersion(client, payload, ack, canaryEnabled) {
			return
		}
		request := client.Request().Request()
		ctx, cancel := realtimeOperationContext()
		defer cancel()
		op := stringField(payload, "op")
		if op == "notification.count.get" {
			if !emptyRealtimeInput(payload["input"]) {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid notification count request."))
				return
			}
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			count, err := server.notificationCount(ctx, state.userID)
			if err != nil {
				respond(ack, nil, syncError("INTERNAL_SERVER_ERROR", "The request could not be completed."))
				return
			}
			respond(ack, map[string]any{"count": count}, nil)
			return
		}
		if op == "user.quota-state.get" {
			if !emptyRealtimeInput(payload["input"]) {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid user quota state request."))
				return
			}
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			result, err := server.userQuotaState(ctx, state.userID)
			if err != nil {
				respond(ack, nil, quotaStateError(err))
				return
			}
			respond(ack, result, nil)
			return
		}
		if op == "workspace.quota-state.get" {
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			input, ok := payload["input"].(map[string]any)
			if !ok {
				respond(ack, nil, quotaStateError(errInvalidQuotaStateRequest))
				return
			}
			result, err := server.workspaceQuotaState(ctx, state.userID, input)
			if err != nil {
				respond(ack, nil, quotaStateError(err))
				return
			}
			respond(ack, result, nil)
			return
		}
		if op == "user.settings.get" {
			if !emptyRealtimeInput(payload["input"]) {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid user settings request."))
				return
			}
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			settings, err := server.userSettings(ctx, state.userID)
			if err != nil {
				respond(ack, nil, syncError("INTERNAL_SERVER_ERROR", "The request could not be completed."))
				return
			}
			respond(ack, map[string]any{"settings": settings}, nil)
			return
		}
		if op == "comment.changes.get" {
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			input, ok := payload["input"].(map[string]any)
			if !ok {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid comment changes request."))
				return
			}
			result, err := server.commentChanges(ctx, state.userID, input)
			if err != nil {
				code, message := commentRealtimeError(err)
				respond(ack, nil, syncError(code, message))
				return
			}
			respond(ack, result, nil)
			return
		}
		if op == "workspace.invite-link.get" {
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			workspaceID, valid := workspaceInviteLinkInput(payload["input"])
			if !valid {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid workspace invite link request."))
				return
			}
			link, err := server.workspaceInviteLink(ctx, state.userID, workspaceID)
			if err != nil {
				respond(ack, nil, workspaceInviteLinkError(err))
				return
			}
			respond(ack, map[string]any{"inviteLink": inviteLinkPayload(link)}, nil)
			return
		}
		if op == "workspace.access.get" {
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			input, ok := payload["input"].(map[string]any)
			if !ok {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid workspace access request."))
				return
			}
			result, err := server.workspaceAccess(ctx, state.userID, input)
			if err != nil {
				respond(ack, nil, workspaceAccessError(err))
				return
			}
			respond(ack, result, nil)
			return
		}
		if op == "workspace.config.get" {
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			input, ok := payload["input"].(map[string]any)
			if !ok {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid workspace config request."))
				return
			}
			result, err := server.workspaceConfig(ctx, state.userID, input)
			if err != nil {
				respond(ack, nil, workspaceConfigError(err))
				return
			}
			respond(ack, result, nil)
			return
		}
		if op == "workspace.embedding.progress.get" {
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			input, ok := payload["input"].(map[string]any)
			if !ok {
				respond(ack, nil, workspaceEmbeddingProgressError(errInvalidWorkspaceEmbeddingProgressRequest))
				return
			}
			result, err := server.workspaceEmbeddingProgress(ctx, state.userID, input)
			if err != nil {
				respond(ack, nil, workspaceEmbeddingProgressError(err))
				return
			}
			respond(ack, result, nil)
			return
		}
		if op == "workspace.members.get" {
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			input, ok := payload["input"].(map[string]any)
			if !ok {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid workspace members request."))
				return
			}
			result, err := server.workspaceMembers(ctx, state.userID, input)
			if err != nil {
				respond(ack, nil, workspaceMembersError(err))
				return
			}
			respond(ack, result, nil)
			return
		}
		if op == "doc.share-state.get" {
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			result, err := server.docShareState(ctx, state.userID, payload["input"])
			respond(ack, result, err)
			return
		}
		if op == "doc.grants.get" {
			state := clientSyncState(client, authenticator)
			if state == nil {
				rejectSyncClient(client, ack)
				return
			}
			input, ok := payload["input"].(map[string]any)
			if !ok {
				respond(ack, nil, docGrantsError(errInvalidDocGrants))
				return
			}
			result, err := server.docGrants(ctx, state.userID, input)
			if err != nil {
				respond(ack, nil, docGrantsError(err))
				return
			}
			respond(ack, result, nil)
			return
		}
		if op != "user.profile.get" || !emptyRealtimeInput(payload["input"]) {
			respond(ack, nil, syncError("BAD_REQUEST", "Unsupported realtime request."))
			return
		}
		principal, err := authenticator.FromRequest(ctx, request)
		if errors.Is(err, auth.ErrNoSession) {
			respond(ack, map[string]any{"user": nil}, nil)
			return
		}
		if err != nil || principal == nil {
			respond(ack, nil, syncError("INTERNAL_SERVER_ERROR", "The request could not be completed."))
			return
		}
		respond(ack, map[string]any{"user": realtimeUserProfile(principal.User)}, nil)
	})

	_ = client.On("realtime:subscribe", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			respond(ack, nil, syncError("BAD_REQUEST", "Invalid realtime subscription."))
			return
		}
		if !assertRealtimeVersion(client, payload, ack, canaryEnabled) {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		topic := stringField(payload, "topic")
		input, inputOK := payload["input"].(map[string]any)
		room := ""
		inputKey := "{}"
		if topic == "user.profile.changed" && emptyRealtimeInput(payload["input"]) {
			room = "user:" + state.userID + ":profile"
		} else if topic == "user.quota-state.changed" && emptyRealtimeInput(payload["input"]) {
			if server.workspace.Quota == nil {
				respond(ack, nil, quotaStateError(errQuotaStateUnavailable))
				return
			}
			room = quotaStateRoom(state.userID, "")
		} else if topic == "user.settings.changed" && emptyRealtimeInput(payload["input"]) {
			room = "user:" + state.userID + ":settings"
		} else if topic == "notification.count.changed" && emptyRealtimeInput(payload["input"]) {
			room = "user:" + state.userID + ":notification"
		} else if topic == "workspace.quota-state.changed" && inputOK {
			workspaceID, valid := workspaceInviteLinkInput(payload["input"])
			if !valid {
				respond(ack, nil, quotaStateError(errInvalidQuotaStateRequest))
				return
			}
			ctx, cancel := realtimeOperationContext()
			err := workspaceQuotaStateAccess(ctx, server.workspace.QuotaAccess, state.userID, workspaceID)
			cancel()
			if err != nil {
				respond(ack, nil, quotaStateError(err))
				return
			}
			if server.workspace.Quota == nil {
				respond(ack, nil, quotaStateError(errQuotaStateUnavailable))
				return
			}
			room = quotaStateRoom("", workspaceID)
			encodedInput, err := json.Marshal(map[string]string{"workspaceId": workspaceID})
			if err != nil {
				respond(ack, nil, syncError("INTERNAL_SERVER_ERROR", "The request could not be completed."))
				return
			}
			inputKey = string(encodedInput)
		} else if topic == "comment.changed" && inputOK {
			workspaceID := stringField(input, "workspaceId")
			docID := stringField(input, "docId")
			if workspaceID == "" || docID == "" {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid comment subscription."))
				return
			}
			ctx, cancel := realtimeOperationContext()
			err := server.authorizeCommentRead(ctx, state.userID, workspaceID, docID)
			cancel()
			if err != nil {
				code, message := commentRealtimeError(err)
				respond(ack, nil, syncError(code, message))
				return
			}
			room = commentRealtimeRoom(workspaceID, docID)
			encodedInput, err := json.Marshal(map[string]string{"workspaceId": workspaceID, "docId": docID})
			if err != nil {
				respond(ack, nil, syncError("INTERNAL_SERVER_ERROR", "The request could not be completed."))
				return
			}
			inputKey = string(encodedInput)
		} else if topic == "doc.grants.changed" || topic == "doc.share-state.changed" {
			workspaceID, docID, valid := docGrantsSubscriptionInput(payload["input"])
			if !valid {
				respond(ack, nil, docGrantsError(errInvalidDocGrants))
				return
			}
			if server.workspace.GrantsAuth == nil || topic == "doc.grants.changed" && server.workspace.UserUpdates == nil || topic == "doc.share-state.changed" && server.workspace.ShareState == nil {
				respond(ack, nil, docGrantsError(errDocGrantsUnavailable))
				return
			}
			ctx, cancel := realtimeOperationContext()
			authorization, err := server.workspace.GrantsAuth.AuthorizeDocument(ctx, state.userID, workspaceID, docID)
			cancel()
			if err == nil && (topic == "doc.grants.changed" && !authorization.UsersRead || topic == "doc.share-state.changed" && !authorization.Read) {
				err = errDocGrantsDenied
			}
			if err != nil {
				respond(ack, nil, docGrantsError(err))
				return
			}
			room = docGrantsRoom(workspaceID, docID)
			if topic == "doc.share-state.changed" {
				room = docShareStateRoom(workspaceID, docID)
			}
			encodedInput, err := marshalDocGrantsInput(workspaceID, docID)
			if err != nil {
				respond(ack, nil, syncError("INTERNAL_SERVER_ERROR", "The request could not be completed."))
				return
			}
			inputKey = string(encodedInput)
		} else if topic == "workspace.invite-link.changed" {
			workspaceID, valid := workspaceInviteLinkInput(payload["input"])
			if !valid {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid workspace invite link subscription."))
				return
			}
			ctx, cancel := realtimeOperationContext()
			err := server.authorizeWorkspaceInviteLink(ctx, state.userID, workspaceID)
			cancel()
			if err != nil {
				respond(ack, nil, workspaceInviteLinkError(err))
				return
			}
			room = workspaceInviteLinkRoom(workspaceID)
			encodedInput, err := json.Marshal(map[string]string{"workspaceId": workspaceID})
			if err != nil {
				respond(ack, nil, syncError("INTERNAL_SERVER_ERROR", "The request could not be completed."))
				return
			}
			inputKey = string(encodedInput)
		} else if topic == "workspace.access.changed" || topic == "workspace.config.changed" || topic == "workspace.members.changed" || topic == "workspace.embedding.progress.changed" {
			workspaceID, valid := workspaceInviteLinkInput(payload["input"])
			if !valid {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid workspace subscription."))
				return
			}
			ctx, cancel := realtimeOperationContext()
			var err error
			if topic == "workspace.embedding.progress.changed" {
				err = server.authorizeWorkspaceCopilot(ctx, state.userID, workspaceID)
			} else if topic == "workspace.members.changed" {
				if server.workspace.Members == nil {
					err = errWorkspaceMembersUnavailable
				} else {
					_, err = server.workspace.Members.ListMembersForUser(ctx, state.userID, workspaceID, "", 0, 1)
				}
			} else if topic == "workspace.config.changed" && server.workspace.Config != nil {
				_, err = server.workspace.Config.GetWorkspaceConfig(ctx, state.userID, workspaceID)
			} else if topic == "workspace.access.changed" && server.workspace.Access != nil {
				_, err = server.workspace.Access.GetWorkspaceAccess(ctx, state.userID, workspaceID)
			} else if server.workspace.Authorizer == nil {
				err = errWorkspaceMembersUnavailable
			} else {
				var access workspaces.WorkspaceSyncAccess
				access, err = server.workspace.Authorizer.AuthorizeWorkspaceSync(ctx, state.userID, workspaceID, nil)
				if err == nil && !access.CanSync {
					err = workspaces.ErrAccessDenied
				}
			}
			cancel()
			if err != nil {
				respond(ack, nil, workspaceMembersError(err))
				return
			}
			room = workspaceRealtimeRoom(workspaceID, topic)
			encodedInput, err := json.Marshal(map[string]string{"workspaceId": workspaceID})
			if err != nil {
				respond(ack, nil, syncError("INTERNAL_SERVER_ERROR", "The request could not be completed."))
				return
			}
			inputKey = string(encodedInput)
		} else {
			respond(ack, nil, syncError("BAD_REQUEST", "Unsupported realtime topic."))
			return
		}

		subscriptionID := fmt.Sprintf("%s:%s:%s", client.Id(), topic, inputKey)
		client.Join(socket.Room(room))
		state.mu.Lock()
		if state.realtimeTopics == nil {
			state.realtimeTopics = make(map[string]string)
		}
		state.realtimeTopics[subscriptionID] = room
		state.mu.Unlock()
		respond(ack, map[string]any{"subscriptionId": subscriptionID}, nil)
	})

	_ = client.On("realtime:unsubscribe", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			respond(ack, nil, syncError("BAD_REQUEST", "Invalid realtime unsubscription."))
			return
		}
		if !assertRealtimeVersion(client, payload, ack, canaryEnabled) {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		subscriptionID := stringField(payload, "subscriptionId")
		if subscriptionID != "" {
			state.mu.Lock()
			room, exists := state.realtimeTopics[subscriptionID]
			delete(state.realtimeTopics, subscriptionID)
			state.mu.Unlock()
			if exists {
				client.Leave(socket.Room(room))
			}
		}
		respond(ack, map[string]any{"ok": true}, nil)
	})
}

func workspaceInviteLinkInput(value any) (string, bool) {
	input, ok := value.(map[string]any)
	if !ok || len(input) != 1 {
		return "", false
	}
	workspaceID, ok := input["workspaceId"].(string)
	return workspaceID, ok && strings.TrimSpace(workspaceID) != ""
}

func inviteLinkPayload(link *invitelinks.Link) any {
	if link == nil {
		return nil
	}
	return map[string]any{
		"link": link.URL, "expireTime": link.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func workspaceInviteLinkError(err error) error {
	switch {
	case errors.Is(err, errWorkspaceInviteLinkDenied):
		return syncError("SPACE_ACCESS_DENIED", "Workspace user management access is denied.")
	case errors.Is(err, workspaces.ErrNotFound):
		return syncError("SPACE_NOT_FOUND", "Workspace not found.")
	default:
		return syncError("INTERNAL_SERVER_ERROR", "The request could not be completed.")
	}
}

func commentRealtimeError(err error) (string, string) {
	switch {
	case errors.Is(err, errInvalidCommentChangesRequest):
		return "BAD_REQUEST", "Invalid comment changes request."
	case errors.Is(err, errCommentReadDenied):
		return "DOC_ACTION_DENIED", "Comment document read access is denied."
	default:
		return "INTERNAL_SERVER_ERROR", "The request could not be completed."
	}
}

func assertRealtimeVersion(client *socket.Socket, payload map[string]any, ack socket.Ack, canaryEnabled bool) bool {
	if supportedRealtimeVersion(stringField(payload, "clientVersion"), canaryEnabled, time.Now()) {
		return true
	}
	respond(ack, nil, syncError("UNSUPPORTED_CLIENT_VERSION", "Client version is not supported."))
	client.Disconnect(false)
	return false
}

func supportedRealtimeVersion(version string, canaryEnabled bool, now time.Time) bool {
	version = strings.TrimSpace(version)
	if match := realtimeCanaryVersionPattern.FindStringSubmatch(version); match != nil {
		if !canaryEnabled {
			return false
		}
		year, _ := strconv.Atoi(match[1])
		month, _ := strconv.Atoi(match[2])
		day, _ := strconv.Atoi(match[3])
		date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
		if date.Year() != year || int(date.Month()) != month || date.Day() != day {
			return false
		}
		age := now.UTC().Truncate(24 * time.Hour).Sub(date)
		if age < -2*24*time.Hour || age > 62*24*time.Hour {
			return false
		}
		version = fmt.Sprintf("%d.%d.%d-canary.%s", year, month, day, strings.TrimLeft(match[4], "0"))
		if strings.HasSuffix(version, "canary.") {
			version += "0"
		}
	} else {
		version = strings.TrimPrefix(strings.ToLower(version), "v")
	}
	return semver.IsValid("v"+version) && semver.Compare("v"+version, "v0.26.0-0") >= 0
}

func emptyRealtimeInput(value any) bool {
	input, ok := value.(map[string]any)
	return ok && len(input) == 0
}

func realtimeUserProfile(user auth.User) map[string]any {
	features := make([]string, 0, len(user.Features))
	for _, feature := range user.Features {
		if feature == "administrator" {
			features = append(features, "Admin")
		} else {
			features = append(features, feature)
		}
	}
	return map[string]any{
		"id":            user.ID,
		"name":          user.Name,
		"email":         user.Email,
		"emailVerified": user.EmailVerified,
		"hasPassword":   user.HasPassword,
		"avatarUrl":     user.AvatarURL,
		"features":      features,
	}
}
