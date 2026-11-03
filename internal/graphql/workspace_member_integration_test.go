// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestWorkspaceMembersGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	var invitedUserID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email=$1`, fixture.hiddenEmail).Scan(&invitedUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET created_at=$2 WHERE workspace_id=$1 AND user_id=$3`, fixture.workspaceID, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET created_at=$2 WHERE workspace_id=$1 AND user_id=$3`, fixture.workspaceID, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id, workspace_id, invitee_user_id, requested_role, status, kind, created_at) VALUES ('registered-invite', $1, $2, 'admin', 'waiting_review', 'email', $3), ('email-only-invite', $1, NULL, 'member', 'pending', 'email', $4)`, fixture.workspaceID, invitedUserID, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET avatar_url='https://example.test/invited.png' WHERE id=$1`, invitedUserID); err != nil {
		t.Fatal(err)
	}

	workspaceRepository := workspaces.NewRepository(pool)
	userRepository := users.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	handler := NewHandler(config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010}, auth.NewSessionResolver(pool), userService, workspaceRepository)
	query := `query workspaceMembers($id: String!, $skip: Int, $take: Int, $query: String) {
		workspace(id: $id) {
			memberCount
			members(skip: $skip, take: $take, query: $query) {
				id name email avatarUrl permission role inviteId status createdAt disabled emailVerified hasPassword
			}
		}
	}`
	result := graphRequest(t, handler, query, map[string]any{"id": fixture.workspaceID}, fixture.sessionID, fixture.userID)
	workspace := result["data"].(map[string]any)["workspace"].(map[string]any)
	if workspace["memberCount"] != float64(4) {
		t.Fatalf("memberCount = %#v, want 2 active members plus both invitation rows", workspace["memberCount"])
	}
	members := workspace["members"].([]any)
	if len(members) != 3 {
		t.Fatalf("members = %#v, want active members and only the invitation linked to a user", members)
	}
	first := members[0].(map[string]any)
	invited := members[2].(map[string]any)
	if first["id"] != fixture.userID || first["inviteId"] != fixture.userID || first["role"] != "Owner" || first["permission"] != "Owner" || first["status"] != "Accepted" {
		t.Fatalf("owner projection = %#v", first)
	}
	if invited["id"] != invitedUserID || invited["inviteId"] != invitedUserID || invited["name"] != "Hidden User" || invited["email"] != fixture.hiddenEmail || invited["avatarUrl"] != "https://example.test/invited.png" || invited["role"] != "Admin" || invited["permission"] != "Admin" || invited["status"] != "UnderReview" {
		t.Fatalf("registered invite projection = %#v", invited)
	}
	for _, field := range []string{"createdAt", "disabled", "emailVerified", "hasPassword"} {
		if invited[field] != nil {
			t.Fatalf("unselected user field %s = %#v, want null", field, invited[field])
		}
	}

	page := graphRequest(t, handler, query, map[string]any{"id": fixture.workspaceID, "skip": 1, "take": 1}, fixture.sessionID, fixture.userID)
	pageMembers := page["data"].(map[string]any)["workspace"].(map[string]any)["members"].([]any)
	if len(pageMembers) != 1 || pageMembers[0].(map[string]any)["id"] != fixture.sharedUserID {
		t.Fatalf("member offset page = %#v", pageMembers)
	}

	search := graphRequest(t, handler, query, map[string]any{"id": fixture.workspaceID, "query": "hidden@example.test"}, fixture.sessionID, fixture.userID)
	searchMembers := search["data"].(map[string]any)["workspace"].(map[string]any)["members"].([]any)
	if len(searchMembers) != 1 || searchMembers[0].(map[string]any)["id"] != invitedUserID {
		t.Fatalf("member search = %#v", searchMembers)
	}

	tooLong := graphRequest(t, handler, query, map[string]any{"id": fixture.workspaceID, "query": strings.Repeat("x", 256)}, fixture.sessionID, fixture.userID)
	errors := tooLong["errors"].([]any)
	if len(errors) != 1 {
		t.Fatalf("oversized member search errors = %#v", errors)
	}
	extensions := errors[0].(map[string]any)["extensions"].(map[string]any)
	if extensions["status"] != float64(http.StatusBadRequest) || extensions["type"] != "INVALID_INPUT" || extensions["name"] != "QUERY_TOO_LONG" || extensions["message"] != "Query is too long, max length is 255." {
		t.Fatalf("oversized member search extensions = %#v", extensions)
	}
	if extensions["data"].(map[string]any)["max"] != float64(255) {
		t.Fatalf("oversized member search data = %#v", extensions["data"])
	}
}
