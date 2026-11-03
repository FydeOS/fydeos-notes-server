// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"fmt"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type documentRolePublisherFake struct{ events []string }

func (p *documentRolePublisherFake) PublishDocumentRoleChanged(workspaceID, docID, reason string) {
	p.events = append(p.events, workspaceID+"/"+docID+"/"+reason)
}

func TestDocumentRoleGraphQLContractsAndCommitEvents(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	_, err := pool.Exec(ctx, `
		ALTER TABLE workspace_access_policies ADD COLUMN member_default_doc_role text NOT NULL DEFAULT 'reader';
		CREATE TABLE doc_grants(workspace_id text,doc_id text,principal_type text,principal_id text,role text,granted_by text,created_at timestamptz DEFAULT now(),updated_at timestamptz DEFAULT now(),PRIMARY KEY(workspace_id,doc_id,principal_type,principal_id));
		CREATE TABLE doc_access_policies(workspace_id text,doc_id text,visibility text DEFAULT 'private',public_role text,member_default_role text,published_at timestamptz,updated_at timestamptz DEFAULT now(),PRIMARY KEY(workspace_id,doc_id));
		CREATE TABLE workspace_pages(workspace_id text,page_id text,mode smallint DEFAULT 0,published_at timestamptz);
		CREATE TABLE workspace_sync_permission_generations(workspace_id text PRIMARY KEY,generation bigint NOT NULL DEFAULT 0);
		CREATE FUNCTION bump_doc_test_generation() RETURNS trigger AS $$ BEGIN
		  INSERT INTO workspace_sync_permission_generations VALUES(COALESCE(NEW.workspace_id,OLD.workspace_id),1) ON CONFLICT(workspace_id) DO UPDATE SET generation=workspace_sync_permission_generations.generation+1;
		  RETURN COALESCE(NEW,OLD); END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER grant_generation AFTER INSERT OR UPDATE OR DELETE ON doc_grants FOR EACH ROW EXECUTE FUNCTION bump_doc_test_generation();
		CREATE TRIGGER policy_generation AFTER INSERT OR UPDATE OR DELETE ON doc_access_policies FOR EACH ROW EXECUTE FUNCTION bump_doc_test_generation();
		CREATE TABLE blobs(workspace_id text,size bigint,status text,deleted_at timestamptz,reservation_expires_at timestamptz);
		CREATE TABLE comment_attachments(LIKE blobs);`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,quantity,status) VALUES('workspace',$1,'cloud_subscription','team',10,'active')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	publisher := &documentRolePublisherFake{}
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud", Host: "localhost", Port: 3010}, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{DocumentRoles: DocumentRoleDependencies{
		Service: workspaces.NewDocumentRoleService(repository, documents.NewWorkspaceRepositoryWithEntitlementConfig(pool, "cloud")), Publisher: publisher,
	}})
	input := map[string]any{"workspaceId": fixture.workspaceID, "docId": "doc", "userIds": []string{fixture.sharedUserID, fixture.sharedUserID}, "role": "Reader"}
	request := func(op, inputType string, in map[string]any, session, user string) map[string]any {
		return graphRequest(t, handler, fmt.Sprintf(`mutation($input:%s!){%s(input:$input)}`, inputType, op), map[string]any{"input": in}, session, user)
	}
	assertSuccess := func(op, inputType, reason string, in map[string]any) {
		t.Helper()
		before := len(publisher.events)
		result := request(op, inputType, in, fixture.sessionID, fixture.userID)
		if result["errors"] != nil || result["data"].(map[string]any)[op] != true {
			t.Fatalf("%s = %#v", op, result)
		}
		if len(publisher.events) != before+1 || publisher.events[before] != fixture.workspaceID+"/doc/"+reason {
			t.Fatalf("events=%#v", publisher.events)
		}
	}
	assertSuccess("grantDocUserRoles", "GrantDocUserRolesInput", "grants-changed", input)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM doc_grants`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("dedup grant count=%d: %v", count, err)
	}
	roleInput := map[string]any{"workspaceId": fixture.workspaceID, "docId": "doc", "userId": fixture.sharedUserID, "role": "Editor"}
	assertSuccess("updateDocUserRole", "UpdateDocUserRoleInput", "grants-changed", roleInput)
	defaultInput := map[string]any{"workspaceId": fixture.workspaceID, "docId": "doc", "role": "None"}
	assertSuccess("updateDocDefaultRole", "UpdateDocDefaultRoleInput", "default-role-changed", defaultInput)
	revokeInput := map[string]any{"workspaceId": fixture.workspaceID, "docId": "doc", "userId": fixture.sharedUserID}
	assertSuccess("revokeDocUserRoles", "RevokeDocUserRoleInput", "grants-changed", revokeInput)
	roleInput["role"] = "Owner"
	assertSuccess("updateDocUserRole", "UpdateDocUserRoleInput", "owner-changed", roleInput)
	before := len(publisher.events)

	// The native domain owner invariant is a generic internal error on the wire,
	// not a raw SQL/runtime string. Failed commands must not publish an event.
	failed := request("revokeDocUserRoles", "RevokeDocUserRoleInput", revokeInput, fixture.sessionID, fixture.userID)
	if failed["errors"] == nil {
		t.Fatal("owner revoke succeeded")
	}
	failure := failed["errors"].([]any)[0].(map[string]any)
	if failure["message"] != "An internal error occurred." || failure["extensions"].(map[string]any)["name"] != "internal_server_error" {
		t.Fatalf("domain failure contract=%#v", failure)
	}
	oversized := map[string]any{"workspaceId": fixture.workspaceID, "docId": "doc", "userIds": make([]string, workspaces.MaxDocRoleTargets+1), "role": "Reader"}
	limited := request("grantDocUserRoles", "GrantDocUserRolesInput", oversized, fixture.sessionID, fixture.userID)
	if limited["errors"] == nil {
		t.Fatal("oversized grant batch accepted")
	}
	if ext := limited["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any); ext["type"] != "invalid_input" {
		t.Fatalf("limit error=%#v", ext)
	}

	for _, tc := range []struct {
		op, inputType, name string
		input               map[string]any
	}{
		{"grantDocUserRoles", "GrantDocUserRolesInput", "expect_to_grant_doc_user_roles", map[string]any{"workspaceId": fixture.workspaceID, "docId": "doc", "role": "Owner", "userIds": []string{fixture.sharedUserID}}},
		{"updateDocUserRole", "UpdateDocUserRoleInput", "expect_to_update_doc_user_role", map[string]any{"workspaceId": fixture.workspaceID, "docId": "doc", "role": "None", "userId": fixture.sharedUserID}},
		{"updateDocDefaultRole", "UpdateDocDefaultRoleInput", "doc_default_role_can_not_be_owner", map[string]any{"workspaceId": fixture.workspaceID, "docId": "doc", "role": "Owner"}},
		{"revokeDocUserRoles", "RevokeDocUserRoleInput", "expect_to_revoke_doc_user_roles", map[string]any{"workspaceId": fixture.workspaceID, "docId": fixture.workspaceID, "userId": fixture.sharedUserID}},
	} {
		result := request(tc.op, tc.inputType, tc.input, fixture.sessionID, fixture.userID)
		if result["errors"] == nil {
			t.Fatalf("invalid input accepted: %#v", tc)
		}
		ext := result["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
		if ext["type"] != "invalid_input" || ext["name"] != tc.name {
			t.Fatalf("input error=%#v", ext)
		}
	}
	if result := request("revokeDocUserRoles", "RevokeDocUserRoleInput", revokeInput, "", ""); result["errors"] == nil {
		t.Fatal("anonymous mutation succeeded")
	}
	for _, tc := range []struct {
		op, inputType string
		input         map[string]any
	}{
		{"grantDocUserRoles", "GrantDocUserRolesInput", input},
		{"revokeDocUserRoles", "RevokeDocUserRoleInput", revokeInput},
		{"updateDocUserRole", "UpdateDocUserRoleInput", roleInput},
		{"updateDocDefaultRole", "UpdateDocDefaultRoleInput", defaultInput},
	} {
		missing := make(map[string]any, len(tc.input))
		for key, value := range tc.input {
			missing[key] = value
		}
		missing["workspaceId"] = "missing-workspace"
		result := request(tc.op, tc.inputType, missing, fixture.sessionID, fixture.userID)
		if result["errors"] == nil {
			t.Fatalf("%s missing workspace accepted", tc.op)
		}
		ext := result["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
		// Unlike publish/unpublish, legacy role resolvers do not translate the
		// native workspace_not_found error into a permission denial.
		if ext["name"] != "internal_server_error" {
			t.Fatalf("%s missing workspace contract=%#v", tc.op, ext)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role='member' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	result := request("updateDocDefaultRole", "UpdateDocDefaultRoleInput", defaultInput, fixture.sessionID, fixture.userID)
	if result["errors"] == nil {
		t.Fatal("unauthorized update succeeded")
	}
	ext := result["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
	if ext["name"] != "doc_action_denied" || ext["data"].(map[string]any)["action"] != "Doc.Properties.Update" {
		t.Fatalf("denied error=%#v", ext)
	}
	if len(publisher.events) != before {
		t.Fatalf("failed mutation published: %#v", publisher.events)
	}
}
