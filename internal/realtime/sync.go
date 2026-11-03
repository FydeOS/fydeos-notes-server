// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5"
	"github.com/zishang520/socket.io/v2/socket"
	"golang.org/x/mod/semver"
)

const maxJoinBatchSize = 100

var canaryVersionPattern = regexp.MustCompile(`(?i)^v?(\d{4})\.(\d{1,2})\.(\d{1,2})-canary\.(\d+)(?:\+.*)?$`)

type syncClientState struct {
	mu              sync.RWMutex
	userID          string
	sessionID       string
	userSessionID   string
	docs            map[string]struct{}
	spaces          map[string]struct{}
	workspaceDocs   map[string]workspaceDocSubscription
	workspaceSpaces map[string]int64
	realtimeTopics  map[string]string
}

type joinEntry struct {
	spaceType string
	spaceID   string
	docID     string
}

func registerSyncHandlers(client *socket.Socket, authenticator SessionAuthenticator, docs UserDocumentSync, workspace WorkspaceDependencies, subscriptions *workspaceSubscriptionRegistry, server *Server, canaryEnabled bool) {
	if err := client.On("space:join-batch", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		entries, err := parseJoinBatch(payload, state.userID, canaryEnabled)
		if err != nil {
			if syncErr, ok := err.(syncProtocolError); ok && syncErr.name == "UNSUPPORTED_CLIENT_VERSION" {
				respond(ack, map[string]any{"clientId": client.Id(), "success": false}, nil)
				client.Disconnect(false)
				return
			}
			respond(ack, nil, err)
			return
		}
		first := entries[0]
		var access workspaces.WorkspaceSyncAccess
		if first.spaceType == "workspace" {
			if workspace.Authorizer == nil || workspace.Documents == nil {
				respond(ack, nil, syncError("INTERNAL_SERVER_ERROR", "Workspace sync is unavailable."))
				return
			}
			docIDs := make([]string, 0, len(entries))
			for _, entry := range entries {
				if entry.docID != "" {
					docIDs = append(docIDs, entry.docID)
				}
			}
			ctx, cancel := syncOperationContext()
			access, err = workspace.Authorizer.AuthorizeWorkspaceSync(ctx, state.userID, first.spaceID, docIDs)
			if err == nil && !access.CanSync {
				err = syncError("SPACE_ACCESS_DENIED", "Workspace is not accessible.")
			}
			if err == nil {
				for _, docID := range docIDs {
					if !access.Documents[docID].Read {
						err = syncError("DOC_ACTION_DENIED", "Document read access is denied.")
						break
					}
				}
			}
			if err == nil {
				var generation int64
				generation, err = workspace.Documents.CurrentPermissionGeneration(ctx, first.spaceID)
				if err == nil && generation != access.Generation {
					err = workspaces.ErrPermissionGenerationChanged
				}
			}
			cancel()
			if err != nil {
				respond(ack, nil, workspaceSyncError(err))
				return
			}
		}
		client.Join(socket.Room(spaceRoomFor(first.spaceType, first.spaceID, "sync")), socket.Room(spaceRoomFor(first.spaceType, first.spaceID, "sync-027")))
		state.mu.Lock()
		for _, entry := range entries {
			if entry.spaceType == "userspace" {
				state.spaces[entry.spaceID] = struct{}{}
				if entry.docID != "" {
					state.docs[docKey(entry.spaceID, entry.docID)] = struct{}{}
					client.Join(socket.Room(docRoom(entry.spaceID, entry.docID)))
				}
				continue
			}
			state.workspaceSpaces[entry.spaceID] = access.Generation
			if entry.docID != "" {
				docAccess := access.Documents[entry.docID]
				subscription := workspaceDocSubscription{Read: docAccess.Read, Update: docAccess.Update, Generation: access.Generation}
				state.workspaceDocs[workspaceDocKey(entry.spaceID, entry.docID)] = subscription
				subscriptions.add(client, state, entry.spaceID, entry.docID, subscription)
			}
		}
		state.mu.Unlock()
		respond(ack, map[string]any{"clientId": client.Id(), "success": true}, nil)
	}); err != nil {
		return
	}
	_ = client.On("space:leave-batch", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		spaceType, spaceID, err := spacePayload(payload, state.userID)
		if err != nil {
			respond(ack, nil, err)
			return
		}
		docIDs, ok := stringArray(payload["docIds"])
		if !ok {
			respond(ack, nil, syncError("BAD_REQUEST", "Invalid space leave batch payload."))
			return
		}
		canonicalIDs := make([]string, len(docIDs))
		for i, rawID := range docIDs {
			canonicalIDs[i], err = canonicalDocID(rawID)
			if err != nil {
				respond(ack, nil, err)
				return
			}
		}
		for _, docID := range canonicalIDs {
			if spaceType == "workspace" {
				removeWorkspaceSubscription(client, state, subscriptions, spaceID, docID)
				continue
			}
			state.mu.Lock()
			delete(state.docs, docKey(spaceID, docID))
			state.mu.Unlock()
			client.Leave(socket.Room(docRoom(spaceID, docID)))
		}
		if len(docIDs) == 0 {
			if spaceType == "workspace" {
				if !hasWorkspaceDocs(state, spaceID) {
					state.mu.Lock()
					delete(state.workspaceSpaces, spaceID)
					state.mu.Unlock()
					client.Leave(socket.Room(spaceRoomFor(spaceType, spaceID, "sync")))
					client.Leave(socket.Room(spaceRoomFor(spaceType, spaceID, "sync-027")))
				}
			} else if !hasUserspaceDocs(state, spaceID) {
				state.mu.Lock()
				delete(state.spaces, spaceID)
				state.mu.Unlock()
				client.Leave(socket.Room(spaceRoomFor(spaceType, spaceID, "sync")))
				client.Leave(socket.Room(spaceRoomFor(spaceType, spaceID, "sync-027")))
			}
		}
		respond(ack, map[string]any{"clientId": client.Id(), "success": true}, nil)
	})
	_ = client.On("space:leave", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		spaceType, spaceID, err := spacePayload(payload, state.userID)
		if err != nil {
			respond(ack, nil, err)
			return
		}
		if spaceType == "workspace" {
			state.mu.Lock()
			keys := make([]string, 0)
			for key := range state.workspaceDocs {
				if strings.HasPrefix(key, spaceID+"\x00") {
					keys = append(keys, key)
				}
			}
			delete(state.workspaceSpaces, spaceID)
			state.mu.Unlock()
			for _, key := range keys {
				docID := strings.TrimPrefix(key, spaceID+"\x00")
				removeWorkspaceSubscription(client, state, subscriptions, spaceID, docID)
			}
		} else {
			state.mu.Lock()
			for key := range state.docs {
				if strings.HasPrefix(key, spaceID+"\x00") {
					docID := strings.TrimPrefix(key, spaceID+"\x00")
					client.Leave(socket.Room(docRoom(spaceID, docID)))
					delete(state.docs, key)
				}
			}
			delete(state.spaces, spaceID)
			state.mu.Unlock()
		}
		client.Leave(socket.Room(spaceRoomFor(spaceType, spaceID, "sync")))
		client.Leave(socket.Room(spaceRoomFor(spaceType, spaceID, "sync-027")))
		respond(ack, map[string]any{"clientId": client.Id(), "success": true}, nil)
	})
	_ = client.On("space:load-doc", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		spaceType, spaceID, err := spacePayload(payload, state.userID)
		if err != nil {
			respond(ack, nil, err)
			return
		}
		docID, err := canonicalDocID(stringField(payload, "docId"))
		if err != nil {
			respond(ack, nil, err)
			return
		}
		var workspaceSubscription workspaceDocSubscription
		if spaceType == "workspace" {
			var subscribed bool
			workspaceSubscription, subscribed = activeWorkspaceDoc(state, spaceID, docID)
			if !subscribed || !workspaceSubscription.Read {
				respond(ack, nil, syncError("NOT_IN_SPACE", "Document subscription is required."))
				return
			}
			ctx, cancel := syncOperationContext()
			generation, generationErr := workspace.Documents.CurrentPermissionGeneration(ctx, spaceID)
			cancel()
			if generationErr != nil {
				respond(ack, nil, workspaceSyncError(generationErr))
				return
			}
			if generation != workspaceSubscription.Generation {
				removeWorkspaceSubscription(client, state, subscriptions, spaceID, docID)
				respond(ack, nil, syncError("NOT_IN_SPACE", "Document subscription is no longer valid."))
				return
			}
		} else if !activeDoc(state, spaceID, docID) {
			respond(ack, nil, syncError("NOT_IN_SPACE", "Document subscription is required."))
			return
		}
		var stateVector []byte
		if raw := stringField(payload, "stateVector"); raw != "" {
			stateVector, err = base64.StdEncoding.Strict().DecodeString(raw)
			if err != nil {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid state vector."))
				return
			}
		}
		ctx, cancel := syncOperationContext()
		var missing, vector []byte
		var timestamp int64
		if spaceType == "workspace" {
			missing, vector, timestamp, err = workspace.Documents.DiffWorkspace(ctx, spaceID, docID, stateVector)
		} else {
			missing, vector, timestamp, err = docs.Diff(ctx, state.userID, docID, stateVector)
		}
		cancel()
		if err != nil {
			respond(ack, nil, err)
			return
		}
		respond(ack, map[string]any{"missing": base64.StdEncoding.EncodeToString(missing), "state": base64.StdEncoding.EncodeToString(vector), "timestamp": timestamp}, nil)
	})
	_ = client.On("space:push-doc-update", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		spaceType, spaceID, err := spacePayload(payload, state.userID)
		if err != nil {
			respond(ack, nil, err)
			return
		}
		docID, err := canonicalDocID(stringField(payload, "docId"))
		if err != nil {
			respond(ack, nil, err)
			return
		}
		var workspaceSubscription workspaceDocSubscription
		if spaceType == "workspace" {
			var subscribed bool
			workspaceSubscription, subscribed = activeWorkspaceDoc(state, spaceID, docID)
			if !subscribed {
				respond(ack, nil, syncError("NOT_IN_SPACE", "Document subscription is required."))
				return
			}
			if !workspaceSubscription.Update {
				respond(ack, nil, syncError("DOC_ACTION_DENIED", "Document update access is denied."))
				return
			}
		} else if !activeDoc(state, spaceID, docID) {
			respond(ack, nil, syncError("NOT_IN_SPACE", "Document subscription is required."))
			return
		}
		update, err := base64.StdEncoding.Strict().DecodeString(stringField(payload, "update"))
		if err != nil {
			respond(ack, nil, syncError("BAD_REQUEST", "Invalid document update encoding."))
			return
		}
		ctx, cancel := syncOperationContext()
		var timestamp int64
		var workspacePush documents.WorkspacePushResult
		if spaceType == "workspace" {
			workspacePush, err = workspace.Documents.PushWorkspace(ctx, state.userID, spaceID, docID, update, workspaceSubscription.Generation)
			timestamp = workspacePush.Timestamp
		} else {
			timestamp, err = docs.Push(ctx, state.userID, docID, update)
		}
		if err != nil {
			cancel()
			if spaceType == "workspace" && errors.Is(err, documents.ErrWorkspacePermissionGenerationChanged) {
				removeWorkspaceSubscription(client, state, subscriptions, spaceID, docID)
				err = syncError("SYNC_PERMISSION_GENERATION_CHANGED", "Workspace permissions changed during document update.")
			}
			respond(ack, nil, err)
			return
		}
		broadcast := map[string]any{"spaceType": spaceType, "spaceId": spaceID, "docId": docID, "updates": []string{base64.StdEncoding.EncodeToString(update)}, "timestamp": timestamp, "editor": state.userID}
		server.broadcastRoom(spaceRoomFor(spaceType, spaceID, "sync-027"), "space:broadcast-doc-invalidation", map[string]any{"spaceType": spaceType, "spaceId": spaceID, "timestamp": timestamp}, string(client.Id()))
		if spaceType == "workspace" {
			server.broadcastWorkspaceDocument(spaceID, docID, string(client.Id()), "space:broadcast-doc-updates", broadcast, false)
			if workspacePush.WorkspaceMetadataChanged {
				server.PublishWorkspaceConfigUpdated(spaceID)
			}
		} else {
			server.broadcastRoom(docRoom(spaceID, docID), "space:broadcast-doc-updates", broadcast, string(client.Id()))
		}
		cancel()
		respond(ack, map[string]any{"accepted": true, "timestamp": timestamp}, nil)
	})
	_ = client.On("space:load-doc-timestamps", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		spaceType, spaceID, err := spacePayload(payload, state.userID)
		if err != nil {
			respond(ack, nil, err)
			return
		}
		if spaceType == "workspace" {
			generation, subscribed := activeWorkspaceSpace(state, spaceID)
			if !subscribed {
				respond(ack, nil, syncError("NOT_IN_SPACE", "Space subscription is required."))
				return
			}
			ctx, cancel := syncOperationContext()
			current, generationErr := workspace.Documents.CurrentPermissionGeneration(ctx, spaceID)
			cancel()
			if generationErr != nil {
				respond(ack, nil, workspaceSyncError(generationErr))
				return
			}
			if current != generation {
				removeWorkspaceSpace(client, state, subscriptions, spaceID)
				respond(ack, nil, syncError("NOT_IN_SPACE", "Space subscription is no longer valid."))
				return
			}
		} else if !activeSpace(state, spaceID) {
			respond(ack, nil, syncError("NOT_IN_SPACE", "Space subscription is required."))
			return
		}
		var after *int64
		if raw, exists := payload["timestamp"]; exists && raw != nil {
			value, ok := numericInt64(raw)
			if !ok {
				respond(ack, nil, syncError("BAD_REQUEST", "Invalid timestamp."))
				return
			}
			after = &value
		}
		ctx, cancel := syncOperationContext()
		var result map[string]int64
		if spaceType == "workspace" {
			result, err = workspace.Documents.WorkspaceTimestamps(ctx, spaceID, after)
			if err == nil {
				docIDs := make([]string, 0, len(result))
				for docID := range result {
					docIDs = append(docIDs, docID)
				}
				var access workspaces.WorkspaceSyncAccess
				access, err = workspace.Authorizer.AuthorizeWorkspaceSync(ctx, state.userID, spaceID, docIDs)
				generation, subscribed := activeWorkspaceSpace(state, spaceID)
				if err == nil && (!subscribed || !access.CanSync || access.Generation != generation) {
					removeWorkspaceSpace(client, state, subscriptions, spaceID)
					err = syncError("NOT_IN_SPACE", "Space subscription is no longer valid.")
				}
				if err == nil {
					for docID := range result {
						if !access.Documents[docID].Read {
							delete(result, docID)
						}
					}
				}
			}
		} else {
			result, err = docs.Timestamps(ctx, state.userID, after)
		}
		cancel()
		if err != nil {
			respond(ack, nil, err)
			return
		}
		respond(ack, result, nil)
	})
	_ = client.On("space:delete-doc", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		spaceType, spaceID, err := spacePayload(payload, state.userID)
		if err != nil {
			respond(ack, nil, err)
			return
		}
		if spaceType == "workspace" {
			docID, err := canonicalDocID(stringField(payload, "docId"))
			if err != nil {
				respond(ack, nil, err)
				return
			}
			ctx, cancel := syncOperationContext()
			_, err = applyWorkspaceDocumentLifecycle(ctx, client, state, workspace, server, spaceID, docID, documents.WorkspaceDocDelete)
			cancel()
			if err != nil {
				respond(ack, nil, err)
				return
			}
			respond(ack, map[string]any{"success": true}, nil)
			return
		}
		docID, err := canonicalDocID(stringField(payload, "docId"))
		if err != nil {
			respond(ack, nil, err)
			return
		}
		ctx, cancel := syncOperationContext()
		err = docs.Delete(ctx, state.userID, docID)
		cancel()
		if err != nil {
			respond(ack, nil, err)
			return
		}
		respond(ack, map[string]any{"success": true}, nil)
	})
	_ = client.On("space:doc-lifecycle", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		spaceType, spaceID, err := spacePayload(payload, state.userID)
		if err != nil {
			respond(ack, nil, err)
			return
		}
		if spaceType != "workspace" {
			respond(ack, nil, syncError("DOC_ACTION_DENIED", "Document lifecycle commands are only available in workspaces."))
			return
		}
		lifecycle := documents.WorkspaceDocLifecycle(stringField(payload, "lifecycle"))
		if lifecycle != documents.WorkspaceDocTrash && lifecycle != documents.WorkspaceDocRestore && lifecycle != documents.WorkspaceDocDelete {
			respond(ack, nil, syncError("BAD_REQUEST", "Invalid document lifecycle."))
			return
		}
		docID, err := canonicalDocID(stringField(payload, "docId"))
		if err != nil {
			respond(ack, nil, err)
			return
		}
		ctx, cancel := syncOperationContext()
		result, err := applyWorkspaceDocumentLifecycle(ctx, client, state, workspace, server, spaceID, docID, lifecycle)
		cancel()
		if err != nil {
			respond(ack, nil, err)
			return
		}
		respond(ack, result, nil)
	})
	_ = client.On("space:load-awarenesses", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		spaceType, spaceID, err := spacePayload(payload, state.userID)
		if err != nil {
			respond(ack, nil, err)
			return
		}
		docID, err := canonicalDocID(stringField(payload, "docId"))
		if err != nil {
			respond(ack, nil, syncError("NOT_IN_SPACE", "Document subscription is required."))
			return
		}
		collect := map[string]any{"spaceType": spaceType, "spaceId": spaceID, "docId": docID, "sourceSocketId": client.Id()}
		if spaceType == "workspace" {
			if _, err := requireWorkspaceSubscription(client, state, subscriptions, workspace.Documents, spaceID, docID, false); err != nil {
				respond(ack, nil, err)
				return
			}
			server.broadcastWorkspaceDocument(spaceID, docID, string(client.Id()), "space:collect-awareness", collect, false)
		} else {
			if !activeDoc(state, spaceID, docID) {
				respond(ack, nil, syncError("NOT_IN_SPACE", "Document subscription is required."))
				return
			}
			server.broadcastRoom(docRoom(spaceID, docID), "space:collect-awareness", collect, string(client.Id()))
		}
		respond(ack, map[string]any{"clientId": client.Id()}, nil)
	})
	_ = client.On("space:update-awareness", func(args ...any) {
		payload, ack, ok := eventPayload(args)
		if !ok {
			return
		}
		state := clientSyncState(client, authenticator)
		if state == nil {
			rejectSyncClient(client, ack)
			return
		}
		spaceType, spaceID, err := spacePayload(payload, state.userID)
		if err != nil {
			respond(ack, nil, err)
			return
		}
		docID, err := canonicalDocID(stringField(payload, "docId"))
		if err != nil {
			respond(ack, nil, syncError("NOT_IN_SPACE", "Document subscription is required."))
			return
		}
		awareness := stringField(payload, "awarenessUpdate")
		if awareness == "" {
			respond(ack, nil, syncError("BAD_REQUEST", "Invalid awareness update."))
			return
		}
		broadcast := map[string]any{"spaceType": spaceType, "spaceId": spaceID, "docId": docID, "awarenessUpdate": awareness}
		if spaceType == "workspace" {
			if _, err := requireWorkspaceSubscription(client, state, subscriptions, workspace.Documents, spaceID, docID, false); err != nil {
				respond(ack, nil, err)
				return
			}
			server.broadcastWorkspaceDocument(spaceID, docID, string(client.Id()), "space:broadcast-awareness-update", broadcast, false)
		} else {
			if !activeDoc(state, spaceID, docID) {
				respond(ack, nil, syncError("NOT_IN_SPACE", "Document subscription is required."))
				return
			}
			server.broadcastRoom(docRoom(spaceID, docID), "space:broadcast-awareness-update", broadcast, string(client.Id()))
		}
		respond(ack, map[string]any{}, nil)
	})
}

func applyWorkspaceDocumentLifecycle(ctx context.Context, client *socket.Socket, state *syncClientState, workspace WorkspaceDependencies, server *Server, workspaceID, docID string, lifecycle documents.WorkspaceDocLifecycle) (map[string]any, error) {
	if workspace.Authorizer == nil || workspace.Documents == nil {
		return nil, syncError("INTERNAL_SERVER_ERROR", "Workspace sync is unavailable.")
	}
	access, err := workspace.Authorizer.AuthorizeWorkspaceSync(ctx, state.userID, workspaceID, []string{docID})
	if err != nil {
		return nil, workspaceSyncError(err)
	}
	if !access.CanSync {
		return nil, syncError("SPACE_ACCESS_DENIED", "Workspace is not accessible.")
	}
	if !access.Documents[docID].Manage {
		return nil, syncError("DOC_ACTION_DENIED", "Document lifecycle permission is denied.")
	}
	result, err := workspace.Documents.ApplyWorkspaceDocLifecycle(ctx, state.userID, workspaceID, docID, lifecycle, access.Generation)
	if err != nil {
		return nil, workspaceSyncError(err)
	}
	rootDocID := workspaceID
	encodedUpdate := base64.StdEncoding.EncodeToString(result.RootUpdate)
	invalidation := map[string]any{"spaceType": "workspace", "spaceId": workspaceID, "timestamp": result.Timestamp}
	server.broadcastRoom(spaceRoomFor("workspace", workspaceID, "sync-027"), "space:broadcast-doc-invalidation", invalidation, string(client.Id()))
	broadcast := map[string]any{
		"spaceType": "workspace", "spaceId": workspaceID, "docId": rootDocID,
		"updates": []string{encodedUpdate}, "timestamp": result.Timestamp, "editor": state.userID,
	}
	server.broadcastWorkspaceDocument(workspaceID, rootDocID, string(client.Id()), "space:broadcast-doc-updates", broadcast, true)
	return map[string]any{"rootUpdate": encodedUpdate, "timestamp": result.Timestamp}, nil
}

func parseJoinBatch(payload map[string]any, userID string, canaryEnabled bool) ([]joinEntry, error) {
	spaces, ok := payload["spaces"].([]any)
	if !ok || len(spaces) == 0 || len(spaces) > maxJoinBatchSize {
		return nil, syncError("BAD_REQUEST", "Invalid space join batch.")
	}
	version := stringField(payload, "clientVersion")
	if !supportedSyncVersion(version, canaryEnabled, time.Now()) {
		return nil, syncError("UNSUPPORTED_CLIENT_VERSION", "Client version is not supported.")
	}
	entries := make([]joinEntry, 0, len(spaces))
	seen := make(map[string]struct{}, len(spaces))
	for _, raw := range spaces {
		space, ok := raw.(map[string]any)
		if !ok {
			return nil, syncError("BAD_REQUEST", "Invalid space join batch entry.")
		}
		spaceType := stringField(space, "spaceType")
		spaceID := stringField(space, "spaceId")
		if (spaceType != "userspace" && spaceType != "workspace") || strings.TrimSpace(spaceID) == "" {
			return nil, syncError("BAD_REQUEST", "Invalid space join batch entry.")
		}
		if spaceType == "userspace" && spaceID != userID {
			return nil, syncError("SPACE_ACCESS_DENIED", "Userspace is not accessible.")
		}
		docID := ""
		if value, exists := space["docId"]; exists && value != nil {
			rawID, ok := value.(string)
			if !ok || strings.TrimSpace(rawID) == "" {
				return nil, syncError("BAD_REQUEST", "Invalid document identifier.")
			}
			var err error
			docID, err = canonicalDocID(rawID)
			if err != nil {
				return nil, err
			}
		}
		if len(entries) > 0 && (entries[0].spaceType != spaceType || entries[0].spaceID != spaceID) {
			return nil, syncError("BAD_REQUEST", "Space join batch entries must belong to one space.")
		}
		key := spaceType + "\x00" + docKey(spaceID, docID)
		if _, exists := seen[key]; exists {
			return nil, syncError("BAD_REQUEST", "Space join batch contains duplicate entries.")
		}
		seen[key] = struct{}{}
		entries = append(entries, joinEntry{spaceType: spaceType, spaceID: spaceID, docID: docID})
	}
	return entries, nil
}

func supportedSyncVersion(version string, canaryEnabled bool, now time.Time) bool {
	if match := canaryVersionPattern.FindStringSubmatch(strings.TrimSpace(version)); match != nil {
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
	} else if strings.HasPrefix(strings.ToLower(strings.TrimSpace(version)), "v") {
		version = strings.TrimSpace(version)[1:]
	}
	if !semver.IsValid("v" + version) {
		return false
	}
	return semver.Compare("v"+version, "v0.27.5-0") >= 0
}

func userspacePayload(payload map[string]any, userID string) (string, error) {
	if stringField(payload, "spaceType") != "userspace" {
		return "", syncError("BAD_REQUEST", "Unsupported space type.")
	}
	spaceID := stringField(payload, "spaceId")
	if spaceID == "" || spaceID != userID {
		return "", syncError("SPACE_ACCESS_DENIED", "Userspace is not accessible.")
	}
	return spaceID, nil
}

func spacePayload(payload map[string]any, userID string) (string, string, error) {
	spaceType := stringField(payload, "spaceType")
	spaceID := stringField(payload, "spaceId")
	if (spaceType != "userspace" && spaceType != "workspace") || strings.TrimSpace(spaceID) == "" {
		return "", "", syncError("BAD_REQUEST", "Invalid space identifier.")
	}
	if spaceType == "userspace" && spaceID != userID {
		return "", "", syncError("SPACE_ACCESS_DENIED", "Userspace is not accessible.")
	}
	return spaceType, spaceID, nil
}

func canonicalDocID(raw string) (string, error) {
	if raw == "" {
		return "", syncError("BAD_REQUEST", "Invalid document identifier.")
	}
	parts := strings.Split(raw, ":")
	switch len(parts) {
	case 1:
		return parts[0], nil
	case 2:
		if !validDocumentVariant(parts[0]) || parts[1] == "" {
			return "", syncError("BAD_REQUEST", "Invalid document identifier.")
		}
		return parts[1], nil
	case 3:
		if !validDocumentVariant(parts[1]) || parts[2] == "" {
			return "", syncError("BAD_REQUEST", "Invalid document identifier.")
		}
		return parts[2], nil
	case 4:
		if parts[1] != "space" || parts[2] != "page" || parts[3] == "" {
			return "", syncError("BAD_REQUEST", "Invalid document identifier.")
		}
		return parts[3], nil
	default:
		return "", syncError("BAD_REQUEST", "Invalid document identifier.")
	}
}

func validDocumentVariant(value string) bool {
	return value == "page" || value == "space" || value == "settings"
}

func eventPayload(args []any) (map[string]any, socket.Ack, bool) {
	if len(args) == 0 {
		return nil, nil, false
	}
	var ack socket.Ack
	if candidate, ok := args[len(args)-1].(socket.Ack); ok {
		ack = candidate
		args = args[:len(args)-1]
	}
	if len(args) != 1 {
		return nil, ack, false
	}
	payload, ok := args[0].(map[string]any)
	return payload, ack, ok
}

func respond(ack socket.Ack, data any, err error) {
	if ack == nil {
		return
	}
	if err != nil {
		name, message := "INTERNAL_SERVER_ERROR", "The request could not be completed."
		switch {
		case errors.Is(err, pgx.ErrNoRows), errors.Is(err, documents.ErrDocNotFound):
			name, message = "DOC_NOT_FOUND", "Document not found."
		case errors.Is(err, documents.ErrWorkspaceDocNotFound):
			name, message = "DOC_NOT_FOUND", "Document not found."
		case errors.Is(err, documents.ErrWorkspaceNotFound), errors.Is(err, workspaces.ErrNotFound):
			name, message = "SPACE_NOT_FOUND", "Workspace not found."
		case errors.Is(err, documents.ErrWorkspaceRootCommandRequired):
			name, message = "DOC_ACTION_DENIED", "Workspace document lifecycle commands are required."
		case errors.Is(err, documents.ErrInvalidDoc):
			name, message = "BAD_REQUEST", "Invalid document update."
		case errors.Is(err, workspaces.ErrNotFound):
			name, message = "SPACE_NOT_FOUND", "Workspace not found."
		case errors.Is(err, workspaces.ErrAccessDenied):
			name, message = "SPACE_ACCESS_DENIED", "Workspace is not accessible."
		}
		if syncErr, ok := err.(syncProtocolError); ok {
			name, message = syncErr.name, syncErr.message
		}
		ack([]any{map[string]any{"error": map[string]string{"name": name, "message": message}}}, nil)
		return
	}
	ack([]any{map[string]any{"data": data}}, nil)
}

type syncProtocolError struct {
	name    string
	message string
}

func (e syncProtocolError) Error() string { return e.message }

func syncError(name, message string) error { return syncProtocolError{name: name, message: message} }

func clientSyncState(client *socket.Socket, authenticator SessionAuthenticator) *syncClientState {
	state, _ := client.Data().(*syncClientState)
	if state == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	principal, err := authenticator.FromRequest(ctx, client.Request().Request())
	if err != nil || principal == nil || principal.User.ID != state.userID || principal.SessionID != state.sessionID || principal.UserSessionID != state.userSessionID {
		return nil
	}
	return state
}

func rejectSyncClient(client *socket.Socket, ack socket.Ack) {
	respond(ack, nil, syncError("UNAUTHORIZED", "Authentication required."))
	client.Disconnect(false)
}

func syncOperationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func activeDoc(state *syncClientState, spaceID, docID string) bool {
	state.mu.RLock()
	defer state.mu.RUnlock()
	_, ok := state.docs[docKey(spaceID, docID)]
	return ok
}

func activeSpace(state *syncClientState, spaceID string) bool {
	state.mu.RLock()
	defer state.mu.RUnlock()
	_, ok := state.spaces[spaceID]
	return ok
}

func activeWorkspaceDoc(state *syncClientState, workspaceID, docID string) (workspaceDocSubscription, bool) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	subscription, ok := state.workspaceDocs[workspaceDocKey(workspaceID, docID)]
	return subscription, ok
}

func activeWorkspaceSpace(state *syncClientState, workspaceID string) (int64, bool) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	generation, ok := state.workspaceSpaces[workspaceID]
	return generation, ok
}

func removeWorkspaceSubscription(client *socket.Socket, state *syncClientState, subscriptions *workspaceSubscriptionRegistry, workspaceID, docID string) {
	state.mu.Lock()
	delete(state.workspaceDocs, workspaceDocKey(workspaceID, docID))
	state.mu.Unlock()
	subscriptions.remove(string(client.Id()), workspaceID, docID)
}

func removeWorkspaceSpace(client *socket.Socket, state *syncClientState, subscriptions *workspaceSubscriptionRegistry, workspaceID string) {
	state.mu.Lock()
	docIDs := make([]string, 0)
	for key := range state.workspaceDocs {
		if strings.HasPrefix(key, workspaceID+"\x00") {
			docIDs = append(docIDs, strings.TrimPrefix(key, workspaceID+"\x00"))
			delete(state.workspaceDocs, key)
		}
	}
	delete(state.workspaceSpaces, workspaceID)
	state.mu.Unlock()
	for _, docID := range docIDs {
		subscriptions.remove(string(client.Id()), workspaceID, docID)
	}
	client.Leave(socket.Room(spaceRoomFor("workspace", workspaceID, "sync")))
	client.Leave(socket.Room(spaceRoomFor("workspace", workspaceID, "sync-027")))
}

func hasUserspaceDocs(state *syncClientState, spaceID string) bool {
	state.mu.RLock()
	defer state.mu.RUnlock()
	for key := range state.docs {
		if strings.HasPrefix(key, spaceID+"\x00") {
			return true
		}
	}
	return false
}

func hasWorkspaceDocs(state *syncClientState, workspaceID string) bool {
	state.mu.RLock()
	defer state.mu.RUnlock()
	for key := range state.workspaceDocs {
		if strings.HasPrefix(key, workspaceID+"\x00") {
			return true
		}
	}
	return false
}

func requireWorkspaceSubscription(client *socket.Socket, state *syncClientState, subscriptions *workspaceSubscriptionRegistry, documents WorkspaceDocumentSync, workspaceID, docID string, requireUpdate bool) (workspaceDocSubscription, error) {
	subscription, ok := activeWorkspaceDoc(state, workspaceID, docID)
	if !ok || !subscription.Read {
		return workspaceDocSubscription{}, syncError("NOT_IN_SPACE", "Document subscription is required.")
	}
	if requireUpdate && !subscription.Update {
		return workspaceDocSubscription{}, syncError("DOC_ACTION_DENIED", "Document update access is denied.")
	}
	if documents == nil {
		return workspaceDocSubscription{}, syncError("INTERNAL_SERVER_ERROR", "Workspace sync is unavailable.")
	}
	ctx, cancel := syncOperationContext()
	generation, err := documents.CurrentPermissionGeneration(ctx, workspaceID)
	cancel()
	if err != nil {
		return workspaceDocSubscription{}, workspaceSyncError(err)
	}
	if generation != subscription.Generation {
		removeWorkspaceSubscription(client, state, subscriptions, workspaceID, docID)
		return workspaceDocSubscription{}, syncError("NOT_IN_SPACE", "Document subscription is no longer valid.")
	}
	return subscription, nil
}

func workspaceSyncError(err error) error {
	switch {
	case errors.Is(err, documents.ErrWorkspacePermissionGenerationChanged), errors.Is(err, workspaces.ErrPermissionGenerationChanged):
		return syncError("SYNC_PERMISSION_GENERATION_CHANGED", "Workspace permissions changed during synchronization.")
	case errors.Is(err, documents.ErrWorkspaceDocNotFound):
		return syncError("DOC_NOT_FOUND", "Workspace document not found.")
	case errors.Is(err, documents.ErrWorkspaceRootNotFound), errors.Is(err, workspaces.ErrNotFound):
		return syncError("SPACE_NOT_FOUND", "Workspace not found.")
	case errors.Is(err, documents.ErrWorkspaceQuotaReadonly):
		return syncError("WORKSPACE_READONLY", "Workspace is read-only due to quota limits.")
	case errors.Is(err, documents.ErrWorkspaceQuotaStateUnknown):
		return syncError("QUOTA_STATE_UNAVAILABLE", "Workspace quota state is unavailable.")
	case errors.Is(err, workspaces.ErrAccessDenied):
		return syncError("SPACE_ACCESS_DENIED", "Workspace is not accessible.")
	default:
		return err
	}
}

func spaceRoom(spaceID, channel string) string { return "userspace:" + spaceID + ":" + channel }

func spaceRoomFor(spaceType, spaceID, channel string) string {
	if spaceType == "userspace" {
		return spaceRoom(spaceID, channel)
	}
	return spaceType + ":" + spaceID + ":" + channel
}

func docRoom(spaceID, docID string) string { return spaceRoom(spaceID, "doc:"+docID) }

func docKey(spaceID, docID string) string { return spaceID + "\x00" + docID }

func workspaceDocKey(workspaceID, docID string) string { return docKey(workspaceID, docID) }

func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func stringArray(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok {
			return nil, false
		}
		result = append(result, value)
	}
	return result, true
}

func numericInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case int64:
		return number, true
	case int:
		return int64(number), true
	case float64:
		if number != float64(int64(number)) {
			return 0, false
		}
		return int64(number), true
	default:
		return 0, false
	}
}
