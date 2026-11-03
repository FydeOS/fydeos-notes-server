// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5/pgxpool"
)

type publicationReadbackPublisher struct {
	t      *testing.T
	pool   *pgxpool.Pool
	states []bool
}

func (p *publicationReadbackPublisher) PublishDocumentPublicStateChanged(workspaceID, docID string) {
	p.t.Helper()
	var public, coherent bool
	err := p.pool.QueryRow(context.Background(), `SELECT policy.visibility='public',
		CASE WHEN policy.visibility='public' THEN policy.public_role='external' AND policy.published_at IS NOT NULL AND policy.published_at=page.published_at
		ELSE policy.public_role IS NULL AND policy.published_at IS NULL AND page.published_at IS NULL END
		FROM workspace_pages page JOIN doc_access_policies policy ON page.workspace_id=policy.workspace_id AND page.page_id=policy.doc_id
		WHERE page.workspace_id=$1 AND page.page_id=$2`, workspaceID, docID).Scan(&public, &coherent)
	if err != nil || !coherent {
		p.t.Fatalf("event published without committed state: public=%t coherent=%t error=%v", public, coherent, err)
	}
	p.states = append(p.states, public)
}

func TestDocumentPublicationGraphQLContractsAndCommitEvents(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	_, err := pool.Exec(ctx, `
		ALTER TABLE workspace_access_policies ADD COLUMN member_default_doc_role text NOT NULL DEFAULT 'reader';
		ALTER TABLE updates ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
		CREATE TABLE doc_grants(workspace_id text,doc_id text,principal_type text,principal_id text,role text);
		CREATE TABLE doc_access_policies(workspace_id text,doc_id text,visibility text DEFAULT 'private',public_role text,member_default_role text,published_at timestamptz,updated_at timestamptz DEFAULT now(),PRIMARY KEY(workspace_id,doc_id));
		CREATE TABLE workspace_pages(workspace_id text,page_id text,mode smallint DEFAULT 0,title text,summary text,published_at timestamptz,PRIMARY KEY(workspace_id,page_id));
		CREATE TABLE workspace_sync_permission_generations(workspace_id text PRIMARY KEY,generation bigint NOT NULL DEFAULT 0);
		CREATE FUNCTION bump_publish_test_generation() RETURNS trigger AS $$ BEGIN
		  INSERT INTO workspace_sync_permission_generations VALUES(COALESCE(NEW.workspace_id,OLD.workspace_id),1) ON CONFLICT(workspace_id) DO UPDATE SET generation=workspace_sync_permission_generations.generation+1;
		  RETURN COALESCE(NEW,OLD); END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER policy_generation AFTER INSERT OR UPDATE OR DELETE ON doc_access_policies FOR EACH ROW EXECUTE FUNCTION bump_publish_test_generation();
		CREATE TABLE blobs(workspace_id text,size bigint,status text,deleted_at timestamptz,reservation_expires_at timestamptz);
		CREATE TABLE comment_attachments(LIKE blobs);`)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO workspace_access_policies(workspace_id,visibility,sharing_enabled,url_preview_enabled,member_default_doc_role) VALUES($1,'private',true,false,'reader')`,
		`INSERT INTO entitlements(target_type,target_id,source,plan,quantity,status) VALUES('workspace',$1,'cloud_subscription','team',10,'active')`,
		`INSERT INTO snapshots(workspace_id,guid,blob) VALUES($1,'doc','\x0000')`,
		`INSERT INTO workspace_pages(workspace_id,page_id,title,summary) VALUES($1,'doc','Page title','Page summary')`,
		`INSERT INTO doc_access_policies(workspace_id,doc_id,member_default_role) VALUES($1,'doc','none')`,
	} {
		if _, err := pool.Exec(ctx, statement, fixture.workspaceID); err != nil {
			t.Fatal(err)
		}
	}
	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	publisher := &publicationReadbackPublisher{t: t, pool: pool}
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud", Host: "localhost", Port: 3010}, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{DocumentPublications: DocumentPublicationDependencies{
		Service: workspaces.NewDocumentPublicationService(repository, documents.NewWorkspaceRepositoryWithEntitlementConfig(pool, "cloud")), Publisher: publisher,
	}})
	query := `mutation($workspace:String!,$doc:String!,$mode:PublicDocMode=Page){publishDoc(workspaceId:$workspace,docId:$doc,mode:$mode){id workspaceId mode public defaultRole title summary createdAt updatedAt createdBy {id} lastUpdatedBy {id}}}`
	variables := map[string]any{"workspace": fixture.workspaceID, "doc": "doc"}
	assertDoc := func(result map[string]any, field, mode string, public bool) {
		t.Helper()
		if result["errors"] != nil {
			t.Fatalf("%s response=%#v", field, result)
		}
		doc := result["data"].(map[string]any)[field].(map[string]any)
		if doc["id"] != "doc" || doc["mode"] != mode || doc["public"] != public {
			t.Fatalf("%s document=%#v", field, doc)
		}
	}
	result := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	assertDoc(result, "publishDoc", "Page", true)
	doc := result["data"].(map[string]any)["publishDoc"].(map[string]any)
	if doc["defaultRole"] != "None" || doc["title"] != "Page title" || doc["summary"] != "Page summary" || doc["createdAt"] != nil || doc["createdBy"] != nil {
		t.Fatalf("metadata projection=%#v", doc)
	}
	variables["mode"] = "Edgeless"
	assertDoc(graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID), "publishDoc", "Edgeless", true)
	revoke := `mutation($workspace:String!,$doc:String!){revokePublicDoc(workspaceId:$workspace,docId:$doc){id mode public}}`
	assertDoc(graphRequest(t, handler, revoke, variables, fixture.sessionID, fixture.userID), "revokePublicDoc", "Edgeless", false)
	if fmt.Sprint(publisher.states) != "[true true false]" {
		t.Fatalf("committed events=%v", publisher.states)
	}

	assertError := func(query string, vars map[string]any, session, user, name string) {
		t.Helper()
		before := len(publisher.states)
		result := graphRequest(t, handler, query, vars, session, user)
		if result["errors"] == nil {
			t.Fatalf("expected %s, got %#v", name, result)
		}
		err := result["errors"].([]any)[0].(map[string]any)
		if name != "" && err["extensions"].(map[string]any)["name"] != name {
			t.Fatalf("expected %s, got %#v", name, err)
		}
		if strings.Contains(fmt.Sprint(err), "SQLSTATE") || strings.Contains(fmt.Sprint(err), "reject_publish") {
			t.Fatalf("database details leaked: %#v", err)
		}
		if len(publisher.states) != before {
			t.Fatalf("failure published an event: %#v", result)
		}
	}
	assertError(revoke, variables, fixture.sessionID, fixture.userID, "doc_is_not_public")
	assertError(query, variables, "", "", "")
	for _, op := range []string{query, revoke} {
		assertError(op, map[string]any{"workspace": "missing-workspace", "doc": "doc"}, fixture.sessionID, fixture.userID, "doc_action_denied")
	}
	assertError(query, map[string]any{"workspace": fixture.workspaceID, "doc": "missing"}, fixture.sessionID, fixture.userID, "doc_not_found")
	assertError(query, map[string]any{"workspace": fixture.workspaceID, "doc": fixture.workspaceID}, fixture.sessionID, fixture.userID, "expect_to_publish_doc")
	assertError(revoke, map[string]any{"workspace": fixture.workspaceID, "doc": fixture.workspaceID}, fixture.sessionID, fixture.userID, "expect_to_revoke_public_doc")
	assertError(query, map[string]any{"workspace": fixture.workspaceID, "doc": "doc", "mode": nil}, fixture.sessionID, fixture.userID, "internal_server_error")
	assertError(query, map[string]any{"workspace": fixture.workspaceID, "doc": strings.Repeat("x", 1025)}, fixture.sessionID, fixture.userID, "expect_to_publish_doc")
	if _, err := pool.Exec(ctx, `ALTER TABLE workspace_pages ADD CONSTRAINT reject_publish CHECK(published_at IS NULL)`); err != nil {
		t.Fatal(err)
	}
	assertError(query, variables, fixture.sessionID, fixture.userID, "internal_server_error")
	state, err := repository.ReadDocShareState(ctx, fixture.workspaceID, "doc")
	if err != nil || state == nil || state.Public || state.Mode != "Edgeless" {
		t.Fatalf("failed publish mutated state=%+v: %v", state, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE workspace_pages DROP CONSTRAINT reject_publish`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role=CASE WHEN user_id=$2 THEN 'owner' ELSE 'member' END WHERE workspace_id=$1`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	for _, docID := range []string{"doc", "missing"} {
		assertError(query, map[string]any{"workspace": fixture.workspaceID, "doc": docID}, fixture.sessionID, fixture.userID, "doc_action_denied")
	}
}
