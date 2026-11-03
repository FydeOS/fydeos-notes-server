// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type profilePublisherRecorder struct {
	userIDs []string
}

type workspaceConfigPublisherRecorder struct {
	workspaceIDs []string
}

func (p *workspaceConfigPublisherRecorder) PublishWorkspaceConfigUpdated(workspaceID string) {
	p.workspaceIDs = append(p.workspaceIDs, workspaceID)
}

type workspaceCleanerRecorder struct {
	workspaceID string
	userIDs     []string
	count       int
	err         error
}

func (c *workspaceCleanerRecorder) DeleteWorkspaceObjects(_ context.Context, workspaceID string, userIDs []string) (int64, error) {
	c.workspaceID = workspaceID
	c.userIDs = append([]string(nil), userIDs...)
	c.count++
	return 0, c.err
}

func (p *profilePublisherRecorder) PublishUserProfileChanged(userID string) {
	p.userIDs = append(p.userIDs, userID)
}

func TestDatabaseBackedWebQueries(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	sessionResolver := auth.NewSessionResolver(pool)
	readLimiter := auth.NewReadLimiter(auth.DefaultReadConcurrency)
	userRepository := users.NewRepository(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	handler := NewHandlerWithReadLimiter(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		sessionResolver,
		userService,
		workspaceRepository,
		readLimiter,
	)
	authSessionHandler := auth.NewSessionHTTPHandler(sessionResolver, readLimiter)
	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	sessionRequest.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fixture.sessionID})
	sessionRequest.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: fixture.userID})
	sessionResponse := httptest.NewRecorder()
	authSessionHandler.ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK || sessionResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("auth session status/cache-control = %d/%q", sessionResponse.Code, sessionResponse.Header().Get("Cache-Control"))
	}
	var sessionBody struct {
		User map[string]any `json:"user"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &sessionBody); err != nil {
		t.Fatal(err)
	}
	if sessionBody.User["id"] != fixture.userID || sessionBody.User["email"] != "web@example.test" {
		t.Fatalf("auth session user = %#v", sessionBody.User)
	}
	methodsHandler := auth.NewBoundMethodsHTTPHandler(sessionResolver, auth.NewBoundMethodsRepository(pool), readLimiter)
	methodsRequest := httptest.NewRequest(http.MethodGet, "/api/auth/methods", nil)
	methodsRequest.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fixture.sessionID})
	methodsRequest.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: fixture.userID})
	methodsResponse := httptest.NewRecorder()
	methodsHandler.ServeHTTP(methodsResponse, methodsRequest)
	if methodsResponse.Code != http.StatusOK {
		t.Fatalf("auth methods status/body = %d/%s", methodsResponse.Code, methodsResponse.Body.String())
	}
	var methodsBody struct {
		Password struct {
			Bound bool `json:"bound"`
		} `json:"password"`
		OAuth struct {
			Bound     bool     `json:"bound"`
			Providers []string `json:"providers"`
		} `json:"oauth"`
		Passkey struct {
			Bound bool   `json:"bound"`
			Count uint32 `json:"count"`
		} `json:"passkey"`
	}
	if err := json.Unmarshal(methodsResponse.Body.Bytes(), &methodsBody); err != nil {
		t.Fatal(err)
	}
	if methodsBody.Password.Bound || !methodsBody.OAuth.Bound || len(methodsBody.OAuth.Providers) != 1 || methodsBody.OAuth.Providers[0] != "fydeos" || methodsBody.Passkey.Bound || methodsBody.Passkey.Count != 0 {
		t.Fatalf("bound methods = %#v", methodsBody)
	}

	currentUser := graphRequest(t, handler, `query getCurrentUser {
		currentUser { id name email emailVerified avatarUrl hasPassword createdAt features }
	}`, nil, fixture.sessionID, fixture.userID)
	userData := currentUser["data"].(map[string]any)["currentUser"].(map[string]any)
	if userData["id"] != fixture.userID || userData["name"] != "Web User" || userData["emailVerified"] != true {
		t.Fatalf("currentUser = %#v", userData)
	}
	if userData["features"].([]any)[0] != "Admin" {
		t.Fatalf("currentUser features = %#v", userData["features"])
	}
	if _, err := time.Parse(time.RFC3339Nano, userData["createdAt"].(string)); err != nil {
		t.Fatalf("createdAt is not an RFC3339 timestamp: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET avatar_url='https://example.test/public.png' WHERE id=$1`, fixture.userID); err != nil {
		t.Fatalf("set public user avatar fixture: %v", err)
	}
	publicUser := graphRequest(t, handler, `query getPublicUserById($id: String!) {
		publicUserById(id: $id) { id name avatarUrl }
	}`, map[string]any{"id": fixture.userID}, "", "")
	publicUserData := publicUser["data"].(map[string]any)["publicUserById"].(map[string]any)
	if publicUserData["id"] != fixture.userID || publicUserData["name"] != "Web User" || publicUserData["avatarUrl"] != "https://example.test/public.png" {
		t.Fatalf("anonymous public user query = %#v", publicUserData)
	}
	disabledPublicUser := graphRequest(t, handler, `query getDisabledPublicUser($id: String!) {
		publicUserById(id: $id) { id name avatarUrl }
	}`, map[string]any{"id": fixture.disabledUserID}, "", "")
	if disabledPublicUser["data"].(map[string]any)["publicUserById"] != nil {
		t.Fatalf("disabled user was publicly visible: %#v", disabledPublicUser)
	}

	workspaces := graphRequest(t, handler, `query getWorkspaces {
		workspaces { id initialized team owner { id } }
	}`, nil, fixture.sessionID, fixture.userID)
	workspaceData := workspaces["data"].(map[string]any)["workspaces"].([]any)
	if len(workspaceData) != 1 {
		t.Fatalf("workspaces = %#v, want only the current user's workspace", workspaceData)
	}
	workspace := workspaceData[0].(map[string]any)
	if workspace["id"] != fixture.workspaceID || workspace["initialized"] != true || workspace["team"] != true {
		t.Fatalf("workspace data = %#v", workspace)
	}
	if workspace["owner"].(map[string]any)["id"] != fixture.userID {
		t.Fatalf("workspace owner = %#v", workspace["owner"])
	}

	visibleUser := graphRequest(t, handler, `query getUser($email: String!) {
		user(email: $email) {
			__typename
			... on UserType { id name email hasPassword }
			... on LimitedUserType { email hasPassword }
		}
	}`, map[string]any{"email": fixture.sharedEmail}, fixture.sessionID, fixture.userID)
	visibleUserData := visibleUser["data"].(map[string]any)["user"].(map[string]any)
	if visibleUserData["id"] != fixture.sharedUserID || visibleUserData["__typename"] != "UserType" {
		t.Fatalf("visible user = %#v", visibleUserData)
	}

	hiddenUser := graphRequest(t, handler, `query getUser($email: String!) { user(email: $email) { __typename } }`, map[string]any{"email": fixture.hiddenEmail}, fixture.sessionID, fixture.userID)
	if hiddenUser["data"].(map[string]any)["user"] != nil {
		t.Fatalf("unshared user was visible: %#v", hiddenUser)
	}

	deniedWorkspace := graphRequest(t, handler, `query getWorkspace($id: String!) { workspace(id: $id) { id } }`, map[string]any{"id": fixture.hiddenWorkspaceID}, fixture.sessionID, fixture.userID)
	if len(deniedWorkspace["errors"].([]any)) == 0 {
		t.Fatalf("cross-workspace query was not denied: %#v", deniedWorkspace)
	}

	for name, sessionID := range map[string]string{
		"expired":  fixture.expiredSessionID,
		"revoked":  fixture.revokedSessionID,
		"disabled": fixture.disabledSessionID,
	} {
		t.Run(name+" session", func(t *testing.T) {
			response := graphRequest(t, handler, `query { currentUser { id } }`, nil, sessionID, fixture.disabledOrCurrentUser(name))
			if response["data"].(map[string]any)["currentUser"] != nil {
				t.Fatalf("invalid session resolved a user: %#v", response)
			}
		})
	}

	unauthenticated := graphRequest(t, handler, `query getWorkspaces { workspaces { id } }`, nil, "", "")
	if len(unauthenticated["errors"].([]any)) == 0 {
		t.Fatalf("unauthenticated workspace list did not fail: %#v", unauthenticated)
	}
}

func TestUpdateProfileMutationUsesAuthenticatedUserAndPersists(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	workspaceRepository := workspaces.NewRepository(pool)
	userRepository := users.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	publisher := &profilePublisherRecorder{}
	handler := NewHandlerWithProfilePublisher(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool),
		userService,
		workspaceRepository,
		auth.NewReadLimiter(auth.DefaultReadConcurrency),
		publisher,
	)
	mutation := `mutation UpdateProfile($input: UpdateUserInput!) { updateProfile(input: $input) { id name email } }`
	updated := graphRequest(t, handler, mutation, map[string]any{"input": map[string]any{"name": "Renamed User"}}, fixture.sessionID, fixture.userID)
	if failures, ok := updated["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("updateProfile errors = %#v", failures)
	}
	user := updated["data"].(map[string]any)["updateProfile"].(map[string]any)
	if user["id"] != fixture.userID || user["name"] != "Renamed User" || user["email"] != "web@example.test" {
		t.Fatalf("updated profile = %#v", user)
	}
	if len(publisher.userIDs) != 1 || publisher.userIDs[0] != fixture.userID {
		t.Fatalf("profile change publications = %#v", publisher.userIDs)
	}
	var currentName, otherName string
	if err := pool.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, fixture.userID).Scan(&currentName); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, fixture.sharedUserID).Scan(&otherName); err != nil {
		t.Fatal(err)
	}
	if currentName != "Renamed User" || otherName != "Shared User" {
		t.Fatalf("persisted names = current %q, other %q", currentName, otherName)
	}

	unchanged := graphRequest(t, handler, `mutation { updateProfile(input: {}) { id name } }`, nil, fixture.sessionID, fixture.userID)
	unchangedUser := unchanged["data"].(map[string]any)["updateProfile"].(map[string]any)
	if unchangedUser["name"] != "Renamed User" {
		t.Fatalf("empty updateProfile input changed the name: %#v", unchangedUser)
	}
	if len(publisher.userIDs) != 1 {
		t.Fatalf("empty profile update published a change: %#v", publisher.userIDs)
	}

	unauthenticated := graphRequest(t, handler, `mutation { updateProfile(input: { name: "Unauthorized" }) { id } }`, nil, "", "")
	if len(unauthenticated["errors"].([]any)) == 0 {
		t.Fatalf("unauthenticated profile update succeeded: %#v", unauthenticated)
	}
	if len(publisher.userIDs) != 1 {
		t.Fatalf("unauthenticated profile update published a change: %#v", publisher.userIDs)
	}
}

func TestCreateWorkspaceMutationPersistsOwnerPolicyAndInitialDocument(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	readLimiter := auth.NewReadLimiter(auth.DefaultReadConcurrency)
	workspaceRepository := workspaces.NewRepository(pool)
	userRepository := users.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	handler := NewHandlerWithReadLimiter(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool),
		userService,
		workspaceRepository,
		readLimiter,
	)

	content := []byte{0, 0, 1, 2, 255}
	requestBody := &bytes.Buffer{}
	multipartWriter := multipart.NewWriter(requestBody)
	operations := `{"query":"mutation CreateWorkspace($init: Upload) { createWorkspace(init: $init) { id initialized team public createdAt owner { id } } }","variables":{"init":null}}`
	if err := multipartWriter.WriteField("operations", operations); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.WriteField("map", `{"0":["variables.init"]}`); err != nil {
		t.Fatal(err)
	}
	file, err := multipartWriter.CreateFormFile("0", "initial.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", requestBody)
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	request.Header.Set("x-operation-name", "CreateWorkspace")
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fixture.sessionID})
	request.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: fixture.userID})
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create workspace status = %d body=%s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode create workspace response: %v body=%s", err, response.Body.String())
	}
	if errors := result["errors"]; errors != nil {
		t.Fatalf("create workspace errors = %#v", errors)
	}
	workspaceData := result["data"].(map[string]any)["createWorkspace"].(map[string]any)
	workspaceID := workspaceData["id"].(string)
	if workspaceData["initialized"] != true || workspaceData["team"] != false || workspaceData["public"] != false || workspaceData["createdAt"] == "" || workspaceData["owner"].(map[string]any)["id"] != fixture.userID {
		t.Fatalf("created workspace = %#v", workspaceData)
	}
	var visibility string
	var sharingEnabled, urlPreviewEnabled bool
	if err := pool.QueryRow(ctx, `SELECT visibility, sharing_enabled, url_preview_enabled FROM workspace_access_policies WHERE workspace_id = $1`, workspaceID).Scan(&visibility, &sharingEnabled, &urlPreviewEnabled); err != nil {
		t.Fatal(err)
	}
	if visibility != "private" || !sharingEnabled || urlPreviewEnabled {
		t.Fatalf("created workspace access policy = %q/%v/%v", visibility, sharingEnabled, urlPreviewEnabled)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_access_policies SET visibility='public' WHERE workspace_id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	publicWorkspace := graphRequest(t, handler, `query($id: String!) { workspace(id: $id) { public createdAt } }`, map[string]any{"id": workspaceID}, fixture.sessionID, fixture.userID)
	publicWorkspaceData := publicWorkspace["data"].(map[string]any)["workspace"].(map[string]any)
	if publicWorkspaceData["public"] != true || publicWorkspaceData["createdAt"] != workspaceData["createdAt"] {
		t.Fatalf("workspace presentation metadata = %#v, created response = %#v", publicWorkspaceData, workspaceData)
	}
	var role, state string
	if err := pool.QueryRow(ctx, `SELECT role, state FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`, workspaceID, fixture.userID).Scan(&role, &state); err != nil {
		t.Fatal(err)
	}
	if role != "owner" || state != "active" {
		t.Fatalf("created owner membership = %q/%q", role, state)
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT blob FROM snapshots WHERE workspace_id = $1 AND guid = $1`, workspaceID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("initial document = %v, want %v", stored, content)
	}
	withoutInit := graphRequest(t, handler, `mutation { createWorkspace { id initialized team owner { id } } }`, nil, fixture.sessionID, fixture.userID)
	if failures, ok := withoutInit["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("create workspace without init failed: %#v", withoutInit)
	}
	plainWorkspace := withoutInit["data"].(map[string]any)["createWorkspace"].(map[string]any)
	if plainWorkspace["initialized"] != false || plainWorkspace["team"] != false || plainWorkspace["owner"].(map[string]any)["id"] != fixture.userID {
		t.Fatalf("workspace without init = %#v", plainWorkspace)
	}
	listed := graphRequest(t, handler, `query { workspaces { id initialized team owner { id } } }`, nil, fixture.sessionID, fixture.userID)
	if failures, ok := listed["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("workspace list after creation failed: %#v", listed)
	}
	items := listed["data"].(map[string]any)["workspaces"].([]any)
	if len(items) != 3 {
		t.Fatalf("workspace list after creation has %d items, want 3", len(items))
	}
	unauthenticated := graphRequest(t, handler, `mutation { createWorkspace { id } }`, nil, "", "")
	if len(unauthenticated["errors"].([]any)) == 0 {
		t.Fatalf("unauthenticated workspace creation succeeded: %#v", unauthenticated)
	}
}

func TestUpdateWorkspaceMutationEnforcesAdminAndPersistsSettings(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	workspaceRepository := workspaces.NewRepository(pool)
	userRepository := users.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	configPublisher := &workspaceConfigPublisherRecorder{}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool),
		userService,
		workspaceRepository,
		nil, nil, nil, nil,
		HandlerDependencies{WorkspaceConfig: configPublisher},
	)
	query := `mutation UpdateWorkspace($input: UpdateWorkspaceInput!) { updateWorkspace(input: $input) { id } }`
	input := map[string]any{
		"id":                 fixture.workspaceID,
		"enableAi":           false,
		"enableDocEmbedding": false,
		"enableSharing":      false,
		"enableUrlPreview":   true,
		"public":             true,
	}
	updated := graphRequest(t, handler, query, map[string]any{"input": input}, fixture.sessionID, fixture.userID)
	if failures, ok := updated["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("owner workspace update failed: %#v", updated)
	}
	if !reflect.DeepEqual(configPublisher.workspaceIDs, []string{fixture.workspaceID}) {
		t.Fatalf("workspace config change publications = %v", configPublisher.workspaceIDs)
	}
	var enableAI, enableDocEmbedding bool
	if err := pool.QueryRow(ctx, `SELECT enable_ai, enable_doc_embedding FROM workspaces WHERE id = $1`, fixture.workspaceID).Scan(&enableAI, &enableDocEmbedding); err != nil {
		t.Fatal(err)
	}
	if enableAI || enableDocEmbedding {
		t.Fatalf("workspace feature flags = %v/%v, want false/false", enableAI, enableDocEmbedding)
	}
	var visibility string
	var sharingEnabled, urlPreviewEnabled bool
	if err := pool.QueryRow(ctx, `SELECT visibility, sharing_enabled, url_preview_enabled FROM workspace_access_policies WHERE workspace_id = $1`, fixture.workspaceID).Scan(&visibility, &sharingEnabled, &urlPreviewEnabled); err != nil {
		t.Fatal(err)
	}
	if visibility != "public" || sharingEnabled || !urlPreviewEnabled {
		t.Fatalf("workspace access policy = %q/%v/%v", visibility, sharingEnabled, urlPreviewEnabled)
	}
	denied := graphRequest(t, handler, query, map[string]any{"input": map[string]any{
		"id":     fixture.workspaceID,
		"public": false,
	}}, fixture.sessionID, fixture.sharedUserID)
	if failures, ok := denied["errors"].([]any); !ok || len(failures) == 0 {
		t.Fatalf("workspace collaborator updated settings: %#v", denied)
	}
	if !reflect.DeepEqual(configPublisher.workspaceIDs, []string{fixture.workspaceID}) {
		t.Fatalf("denied workspace update published config change: %v", configPublisher.workspaceIDs)
	}
	var unchangedVisibility string
	if err := pool.QueryRow(ctx, `SELECT visibility FROM workspace_access_policies WHERE workspace_id = $1`, fixture.workspaceID).Scan(&unchangedVisibility); err != nil {
		t.Fatal(err)
	}
	if unchangedVisibility != "public" {
		t.Fatalf("denied update changed visibility to %q", unchangedVisibility)
	}
}

func TestDeleteWorkspaceMutationRequiresOwnerAndCleansObjectsAfterCommit(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_metadata(workspace_id,user_id) VALUES($1,'ai-only-user')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	repository := workspaces.NewRepository(pool)
	userRepository := users.NewRepository(pool)
	cleaner := &workspaceCleanerRecorder{}
	handler := NewHandlerWithProfilePublisherAndWorkspaceCleaner(
		config.Config{Version: "0.27.5", Deployment: "selfhosted"},
		auth.NewSessionResolver(pool),
		users.NewService(userRepository, workspaces.NewAuthorizationService(repository)),
		repository,
		auth.NewReadLimiter(auth.DefaultReadConcurrency),
		nil,
		cleaner,
	)
	mutation := `mutation DeleteWorkspace($id: String!) { deleteWorkspace(id: $id) }`
	denied := graphRequest(t, handler, mutation, map[string]any{"id": fixture.workspaceID}, fixture.sessionID, fixture.sharedUserID)
	if len(denied["errors"].([]any)) == 0 || cleaner.count != 0 {
		t.Fatalf("non-owner workspace delete = %#v, cleanup calls=%d", denied, cleaner.count)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, fixture.workspaceID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("denied workspace deletion changed the database")
	}

	deleted := graphRequest(t, handler, mutation, map[string]any{"id": fixture.workspaceID}, fixture.sessionID, fixture.userID)
	if failures, ok := deleted["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("owner workspace delete errors = %#v", failures)
	}
	if deleted["data"].(map[string]any)["deleteWorkspace"] != true {
		t.Fatalf("owner workspace delete = %#v", deleted)
	}
	wantStorageUserIDs := []string{"ai-only-user", fixture.sharedUserID, fixture.userID}
	sort.Strings(wantStorageUserIDs)
	if cleaner.count != 1 || cleaner.workspaceID != fixture.workspaceID || !reflect.DeepEqual(cleaner.userIDs, wantStorageUserIDs) {
		t.Fatalf("storage cleanup invocation = %+v", cleaner)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, fixture.workspaceID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("workspace was not deleted")
	}
	for table := range map[string]bool{"snapshots": true, "updates": true, "snapshot_histories": true} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE workspace_id=$1`, fixture.workspaceID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows after delete = %d", table, count)
		}
	}
}

type graphqlFixture struct {
	userID            string
	sharedUserID      string
	sharedEmail       string
	hiddenEmail       string
	sessionID         string
	expiredSessionID  string
	revokedSessionID  string
	disabledSessionID string
	workspaceID       string
	hiddenWorkspaceID string
	disabledUserID    string
}

func (f graphqlFixture) disabledOrCurrentUser(name string) string {
	if name == "disabled" {
		return f.disabledUserID
	}
	return f.userID
}

func seedGraphQLFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) graphqlFixture {
	t.Helper()
	newID := func() string { return uuid.NewString() }
	fixture := graphqlFixture{
		userID:            newID(),
		sharedUserID:      newID(),
		sharedEmail:       "shared@example.test",
		hiddenEmail:       "hidden@example.test",
		sessionID:         newID(),
		expiredSessionID:  newID(),
		revokedSessionID:  newID(),
		disabledSessionID: newID(),
		workspaceID:       newID(),
		hiddenWorkspaceID: newID(),
		disabledUserID:    newID(),
	}

	for _, statement := range []string{
		`CREATE TABLE users (id text PRIMARY KEY, name text NOT NULL, email text NOT NULL, email_verified timestamptz, avatar_url text, created_at timestamptz NOT NULL, password text, disabled boolean NOT NULL, auth_epoch integer NOT NULL DEFAULT 0)`,
		`CREATE TABLE user_settings (user_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL, payload jsonb NOT NULL)`,
		`CREATE TABLE user_connected_accounts (user_id text NOT NULL, provider text NOT NULL)`,
		`CREATE TABLE multiple_users_sessions (id text PRIMARY KEY)`,
		`CREATE TABLE user_sessions (id text PRIMARY KEY, session_id text NOT NULL, user_id text NOT NULL, expires_at timestamptz, refresh_client_version text, created_at timestamptz NOT NULL)`,
		`CREATE TABLE app_configs (
			id text PRIMARY KEY, value jsonb NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
			last_updated_by text REFERENCES users(id) ON DELETE SET NULL
		)`,
		`CREATE TABLE auth_sessions (id text PRIMARY KEY, user_session_id text UNIQUE NOT NULL REFERENCES user_sessions(id) ON DELETE CASCADE, idle_expires_at timestamptz NOT NULL, absolute_expires_at timestamptz NOT NULL, revoked_at timestamptz, revoke_reason text)`,
		`CREATE TABLE auth_refresh_tokens (id text PRIMARY KEY, auth_session_id text NOT NULL REFERENCES auth_sessions(id) ON DELETE CASCADE, revoked_at timestamptz)`,
		`CREATE TABLE user_features (user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE, name text NOT NULL, type integer NOT NULL DEFAULT 0, reason text NOT NULL DEFAULT 'fixture', created_at timestamptz NOT NULL DEFAULT now(), activated boolean NOT NULL, expired_at timestamptz)`,
		`CREATE TABLE workspaces (id text PRIMARY KEY, name text, avatar_key text, created_at timestamptz NOT NULL DEFAULT now(), enable_ai boolean NOT NULL DEFAULT true, enable_doc_embedding boolean NOT NULL DEFAULT true)`,
		`CREATE TABLE workspace_members (id text PRIMARY KEY, workspace_id text NOT NULL, user_id text NOT NULL, role text NOT NULL, state text NOT NULL, source text NOT NULL DEFAULT 'legacy', created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE (workspace_id, user_id, state))`,
		`CREATE TABLE workspace_invitations (id text PRIMARY KEY, workspace_id text NOT NULL, invitee_user_id text, inviter_user_id text, requested_role text NOT NULL DEFAULT 'member', status text NOT NULL, kind text NOT NULL DEFAULT 'email', created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE effective_workspace_quota_states (workspace_id text PRIMARY KEY, owner_user_id text, uses_owner_quota boolean NOT NULL, known boolean NOT NULL, stale boolean NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE entitlements (target_type text NOT NULL, target_id text, source text NOT NULL DEFAULT 'admin_grant', plan text NOT NULL, status text NOT NULL, quantity integer, starts_at timestamptz, expires_at timestamptz, grace_until timestamptz, signed_payload bytea, updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE workspace_access_policies (workspace_id text PRIMARY KEY, visibility text NOT NULL, sharing_enabled boolean NOT NULL, url_preview_enabled boolean NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE snapshots (workspace_id text NOT NULL, guid text NOT NULL, blob bytea, size bigint, created_at timestamptz, updated_at timestamptz, created_by text, updated_by text, PRIMARY KEY (workspace_id, guid))`,
		`CREATE TABLE updates (workspace_id text NOT NULL, guid text NOT NULL, created_at timestamptz, created_by text)`,
		`CREATE TABLE snapshot_histories (workspace_id text NOT NULL, guid text NOT NULL)`,
		`CREATE TABLE ai_sessions_metadata (workspace_id text NOT NULL, user_id text NOT NULL)`,
		`CREATE TABLE workspace_storage_cleanup_jobs (
			workspace_id text PRIMARY KEY, user_ids text[] NOT NULL DEFAULT ARRAY[]::text[],
			attempt integer NOT NULL DEFAULT 0, available_at timestamptz NOT NULL DEFAULT now(),
			lease_token uuid, lease_until timestamptz, last_error text, updated_at timestamptz NOT NULL DEFAULT now()
		)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create integration schema: %v", err)
		}
	}

	now := time.Now().UTC()
	users := []struct {
		id, name, email string
		disabled        bool
	}{
		{fixture.userID, "Web User", "web@example.test", false},
		{fixture.sharedUserID, "Shared User", fixture.sharedEmail, false},
		{newID(), "Hidden User", fixture.hiddenEmail, false},
		{fixture.disabledUserID, "Disabled User", "disabled@example.test", true},
	}
	for _, user := range users {
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, email, email_verified, avatar_url, created_at, password, disabled) VALUES ($1, $2, $3, $4, NULL, $5, NULL, $6)`, user.id, user.name, user.email, now, now, user.disabled); err != nil {
			t.Fatalf("insert integration user: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_connected_accounts (user_id, provider) VALUES ($1, 'fydeos')`, fixture.userID); err != nil {
		t.Fatalf("insert integration OAuth account: %v", err)
	}

	for _, sessionID := range []string{fixture.sessionID, fixture.expiredSessionID, fixture.revokedSessionID, fixture.disabledSessionID} {
		if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions (id) VALUES ($1)`, sessionID); err != nil {
			t.Fatalf("insert integration session: %v", err)
		}
	}
	userSessions := []struct {
		id, sessionID, userID string
		expiresAt             *time.Time
	}{
		{newID(), fixture.sessionID, fixture.userID, nil},
		{newID(), fixture.sessionID, fixture.sharedUserID, nil},
		{newID(), fixture.expiredSessionID, fixture.userID, timePointer(now.Add(-time.Hour))},
		{newID(), fixture.revokedSessionID, fixture.userID, nil},
		{newID(), fixture.disabledSessionID, fixture.disabledUserID, nil},
	}
	var revokedUserSessionID string
	for _, session := range userSessions {
		if _, err := pool.Exec(ctx, `INSERT INTO user_sessions (id, session_id, user_id, expires_at, created_at) VALUES ($1, $2, $3, $4, $5)`, session.id, session.sessionID, session.userID, session.expiresAt, now); err != nil {
			t.Fatalf("insert integration user session: %v", err)
		}
		if session.sessionID == fixture.revokedSessionID {
			revokedUserSessionID = session.id
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions (id, user_session_id, idle_expires_at, absolute_expires_at, revoked_at) VALUES ($1, $2, $3, $4, $5)`, newID(), revokedUserSessionID, now.Add(time.Hour), now.Add(24*time.Hour), now); err != nil {
		t.Fatalf("insert revoked auth session: %v", err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO user_features (user_id, name, activated, expired_at) VALUES ($1, 'administrator', true, NULL)`, fixture.userID); err != nil {
		t.Fatalf("insert user feature: %v", err)
	}
	for _, workspaceID := range []string{fixture.workspaceID, fixture.hiddenWorkspaceID} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, created_at) VALUES ($1, $2)`, workspaceID, now); err != nil {
			t.Fatalf("insert workspace: %v", err)
		}
	}
	for _, member := range []struct{ workspaceID, userID, role string }{
		{fixture.workspaceID, fixture.userID, "owner"},
		{fixture.workspaceID, fixture.sharedUserID, "collaborator"},
		{fixture.hiddenWorkspaceID, users[2].id, "owner"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_members (id, workspace_id, user_id, role, state, created_at) VALUES ($1, $2, $3, $4, 'active', $5)`, newID(), member.workspaceID, member.userID, member.role, now); err != nil {
			t.Fatalf("insert workspace member: %v", err)
		}
	}
	for _, quotaState := range []struct{ workspaceID, ownerID string }{
		{fixture.workspaceID, fixture.userID}, {fixture.hiddenWorkspaceID, users[2].id},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO effective_workspace_quota_states (workspace_id, owner_user_id, uses_owner_quota, known, stale) VALUES ($1, $2, false, true, false)`, quotaState.workspaceID, quotaState.ownerID); err != nil {
			t.Fatalf("insert quota state: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots (workspace_id, guid) VALUES ($1, $1)`, fixture.workspaceID); err != nil {
		t.Fatalf("insert workspace snapshot: %v", err)
	}
	return fixture
}

func graphRequest(t *testing.T, handler http.Handler, query string, variables map[string]any, sessionID, userID string) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sessionID})
	}
	if userID != "" {
		request.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: userID})
	}
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL status = %d body=%s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode GraphQL response: %v; body=%s", err, response.Body.String())
	}
	return result
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsedURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse NOTES_TEST_DATABASE_URL: %v", err)
	}
	if parsedURL.Hostname() != "localhost" && parsedURL.Hostname() != "127.0.0.1" && parsedURL.Hostname() != "::1" {
		t.Fatal("integration tests require a loopback PostgreSQL host")
	}
	if strings.Trim(parsedURL.Path, "/") != "notes_test" {
		t.Fatal("integration tests require a disposable database named notes_test")
	}

	adminPool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect integration PostgreSQL: %v", err)
	}
	schema := "notes_go_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse integration pool config: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("open schema-scoped integration pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
	})
	return pool
}

func timePointer(value time.Time) *time.Time {
	return &value
}
