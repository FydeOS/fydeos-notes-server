// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type inviteInfoLinkLookupFake struct {
	target *invitelinks.InvitationTarget
	err    error
	id     string
}

func (f *inviteInfoLinkLookupFake) LookupByID(_ context.Context, inviteID string) (*invitelinks.InvitationTarget, error) {
	f.id = inviteID
	return f.target, f.err
}

type inviteInfoAvatarReaderFake struct {
	scope string
	key   string
	body  string
	err   error
}

func (f *inviteInfoAvatarReaderFake) GetObject(_ context.Context, scope, key string) (io.ReadCloser, storage.ObjectMetadata, error) {
	f.scope, f.key = scope, key
	if f.err != nil {
		return nil, storage.ObjectMetadata{}, f.err
	}
	return io.NopCloser(strings.NewReader(f.body)), storage.ObjectMetadata{ContentLength: int64(len(f.body))}, nil
}

func TestGetInviteInfoGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	var hiddenUserID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email=$1`, fixture.hiddenEmail).Scan(&hiddenUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions (id, session_id, user_id, created_at) VALUES ('hidden-invite-session', $1, $2, now())`, fixture.sessionID, hiddenUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET name='Product Workspace', avatar_key='avatar-123' WHERE id=$1`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id, workspace_id, invitee_user_id, requested_role, status, kind) VALUES ('invite-hidden', $1, $2, 'member', 'waiting_review', 'email')`, fixture.workspaceID, hiddenUserID); err != nil {
		t.Fatal(err)
	}

	workspaceRepository := workspaces.NewRepository(pool)
	userRepository := users.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	avatarReader := &inviteInfoAvatarReaderFake{body: "custom-avatar-bytes"}
	linkLookup := &inviteInfoLinkLookupFake{}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool), userService, workspaceRepository,
		nil, nil, nil, nil,
		HandlerDependencies{InviteInfo: WorkspaceInvitationInfoDependencies{
			Repository: workspaceRepository, LinkLookup: linkLookup, AvatarReader: avatarReader,
		}},
	)
	query := `query getInviteInfo($inviteId: String!) {
		getInviteInfo(inviteId: $inviteId) {
			invitee { id name email avatarUrl }
			status
			user { id name email }
			workspace { id name avatar }
		}
	}`

	emailResult := graphRequest(t, handler, query, map[string]any{"inviteId": "invite-hidden"}, fixture.sessionID, hiddenUserID)
	if errors := emailResult["errors"]; errors != nil {
		t.Fatalf("email invite query errors = %#v", errors)
	}
	invitation := emailResult["data"].(map[string]any)["getInviteInfo"].(map[string]any)
	invitee := invitation["invitee"].(map[string]any)
	owner := invitation["user"].(map[string]any)
	workspace := invitation["workspace"].(map[string]any)
	if invitee["id"] != hiddenUserID || invitee["email"] != fixture.hiddenEmail || invitation["status"] != "UnderReview" {
		t.Fatalf("email invitation projection = %#v", invitation)
	}
	if owner["id"] != fixture.userID || workspace["id"] != fixture.workspaceID || workspace["name"] != "Product Workspace" || workspace["avatar"] != base64.StdEncoding.EncodeToString([]byte(avatarReader.body)) {
		t.Fatalf("workspace or owner projection = %#v", invitation)
	}
	if avatarReader.scope != storage.BlobScope || avatarReader.key != fixture.workspaceID+"/avatar-123" {
		t.Fatalf("avatar storage lookup = %s/%s", avatarReader.scope, avatarReader.key)
	}

	mismatchResult := graphRequest(t, handler, query, map[string]any{"inviteId": "invite-hidden"}, fixture.sessionID, fixture.userID)

	linkLookup.target = &invitelinks.InvitationTarget{WorkspaceID: fixture.workspaceID, InviterID: fixture.userID}
	linkResult := graphRequest(t, handler, query, map[string]any{"inviteId": "link-123"}, fixture.sessionID, hiddenUserID)
	if errors := linkResult["errors"]; errors != nil {
		t.Fatalf("link invite query errors = %#v", errors)
	}
	linkInvitation := linkResult["data"].(map[string]any)["getInviteInfo"].(map[string]any)
	if linkInvitation["status"] != "UnderReview" || linkInvitation["invitee"].(map[string]any)["id"] != hiddenUserID || linkLookup.id != "link-123" {
		t.Fatalf("link invitation projection = %#v; lookup id = %q", linkInvitation, linkLookup.id)
	}

	avatarReader.err = errors.New("storage unavailable")
	linkResult = graphRequest(t, handler, query, map[string]any{"inviteId": "link-123"}, fixture.sessionID, hiddenUserID)
	linkInvitation = linkResult["data"].(map[string]any)["getInviteInfo"].(map[string]any)
	if linkInvitation["workspace"].(map[string]any)["avatar"] != defaultWorkspaceAvatarBase64 {
		t.Fatal("avatar read failure did not return the legacy default avatar")
	}
	encodedMismatch, err := json.Marshal(mismatchResult)
	if err != nil {
		t.Fatal(err)
	}
	mismatchBody := string(encodedMismatch)
	if !strings.Contains(mismatchBody, "This invitation belongs to another account.") || !strings.Contains(mismatchBody, "INVITATION_ACCOUNT_MISMATCH") {
		t.Fatalf("account mismatch response = %s", mismatchBody)
	}
}
