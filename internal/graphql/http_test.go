// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	gqlupload "github.com/99designs/gqlgen/graphql"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/blobs"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
)

type noSessionAuthenticator struct{}
type requestAuthenticatorFunc func(context.Context, *http.Request) (*auth.Principal, error)

func (f requestAuthenticatorFunc) FromRequest(ctx context.Context, r *http.Request) (*auth.Principal, error) {
	return f(ctx, r)
}

type deadlineTestResponseWriter struct {
	*httptest.ResponseRecorder
	readDeadlines []time.Time
}

type observedBody struct {
	read bool
}

type fakeWorkspaceBlobService struct {
	*fakeBlobManager
	*fakeBlobUploader
}

func TestPresentGraphQLErrorCopilotNotFound(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{name: "message", err: copilot.ErrMessageNotFound, code: "COPILOT_MESSAGE_NOT_FOUND"},
		{name: "session", err: copilot.ErrSessionNotFound, code: "COPILOT_SESSION_NOT_FOUND"},
		{name: "quota", err: copilot.ErrManagedQuotaExceeded, code: "COPILOT_QUOTA_EXCEEDED"},
		{name: "invalid action session", err: copilot.ErrCopilotActionPromptInvalid, code: "bad_request"},
	} {
		t.Run(test.name, func(t *testing.T) {
			presented := presentGraphQLError(context.Background(), test.err)
			if presented.Extensions["name"] != test.code {
				t.Fatalf("error name = %#v, want %q", presented.Extensions["name"], test.code)
			}
			wantStatus := http.StatusNotFound
			if test.name == "quota" {
				wantStatus = http.StatusPaymentRequired
			}
			if test.name == "invalid action session" {
				wantStatus = http.StatusBadRequest
			}
			if presented.Extensions["status"] != wantStatus {
				t.Fatalf("error status = %#v, want %d", presented.Extensions["status"], wantStatus)
			}
		})
	}
}

func (b *observedBody) Read([]byte) (int, error) {
	b.read = true
	return 0, io.EOF
}

func (*observedBody) Close() error { return nil }

func newDeadlineTestResponseWriter() *deadlineTestResponseWriter {
	return &deadlineTestResponseWriter{ResponseRecorder: httptest.NewRecorder()}
}

func (w *deadlineTestResponseWriter) SetReadDeadline(deadline time.Time) error {
	w.readDeadlines = append(w.readDeadlines, deadline)
	return nil
}

func (noSessionAuthenticator) FromRequest(context.Context, *http.Request) (*auth.Principal, error) {
	return nil, auth.ErrNoSession
}

func TestGraphQLHTTPPreservesNativeAccessTokenErrorCodes(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{auth.ErrAccessTokenExpired, "ACCESS_TOKEN_EXPIRED"},
		{auth.ErrAccessTokenInvalid, "ACCESS_TOKEN_INVALID"},
		{auth.ErrAuthSessionExpired, "AUTH_SESSION_EXPIRED"},
		{auth.ErrAuthSessionRevoked, "AUTH_SESSION_REVOKED"},
	} {
		t.Run(test.code, func(t *testing.T) {
			handler := NewHandler(config.Config{Version: "0.27.5"}, requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
				return nil, test.err
			}), nil, nil)
			request := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"query { currentUser { id } }"}`))
			request.Header.Set("Content-Type", "application/json")
			response := newDeadlineTestResponseWriter()
			handler.ServeHTTP(response, request)

			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode auth error response: %v: %s", err, response.Body.String())
			}
			if response.Code != http.StatusUnauthorized || body["code"] != test.code {
				t.Fatalf("response = %d %#v, want 401 with code %s", response.Code, body, test.code)
			}
		})
	}
}

func TestGraphQLAnonymousWorkspaceErrorMatchesLegacy(t *testing.T) {
	handler := NewHandler(config.Config{Version: "0.27.5"}, noSessionAuthenticator{}, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"query getWorkspaces {\n  workspaces { id }\n}"}`))
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	var got, want map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"errors":[{"message":"You must sign in first to access this resource.","locations":[{"line":2,"column":3}],"path":["workspaces"],"extensions":{"status":401,"code":"Unauthorized","type":"AUTHENTICATION_REQUIRED","name":"AUTHENTICATION_REQUIRED","message":"You must sign in first to access this resource."}}],"data":null}`), &want); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("anonymous GraphQL response = %d %s, want 200 %#v", response.Code, response.Body.String(), want)
	}
}

func TestGraphQLErrorRegistrationQueryMatchesLegacy(t *testing.T) {
	for _, name := range []string{"ACCESS_DENIED", "WRONG_SIGN_IN_CREDENTIALS"} {
		t.Run(name, func(t *testing.T) {
			handler := NewHandler(config.Config{Version: "0.27.5"}, noSessionAuthenticator{}, nil, nil)
			request := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"query { error(name: `+name+`) { __typename } }"}`))
			request.Header.Set("Content-Type", "application/json")
			response := newDeadlineTestResponseWriter()
			handler.ServeHTTP(response, request)

			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode GraphQL error response: %v: %s", err, response.Body.String())
			}
			errors, ok := body["errors"].([]any)
			if !ok || len(errors) != 1 {
				t.Fatalf("GraphQL errors = %#v; want one forbidden error", body["errors"])
			}
			graphqlError, ok := errors[0].(map[string]any)
			if !ok {
				t.Fatalf("GraphQL error = %#v; want object", errors[0])
			}
			if response.Code != http.StatusOK || graphqlError["message"] != "You are not allowed to perform this action." {
				t.Fatalf("response = %d %s; want legacy ActionForbidden GraphQL error", response.Code, response.Body.String())
			}
			extensions, ok := graphqlError["extensions"].(map[string]any)
			if !ok {
				t.Fatalf("GraphQL error extensions = %#v; want object", graphqlError["extensions"])
			}
			wantExtensions := map[string]any{
				"status":  float64(http.StatusForbidden),
				"code":    http.StatusText(http.StatusForbidden),
				"type":    "action_forbidden",
				"name":    "action_forbidden",
				"message": "You are not allowed to perform this action.",
				"data":    nil,
			}
			if !reflect.DeepEqual(extensions, wantExtensions) {
				t.Fatalf("GraphQL extensions = %#v, want %#v", extensions, wantExtensions)
			}
		})
	}
}

func TestGraphQLHTTPRejectsUnauthenticatedMultipartBeforeReadingBody(t *testing.T) {
	handler := NewHandler(config.Config{Version: "0.27.5"}, noSessionAuthenticator{}, nil, nil)
	body := &observedBody{}
	request := httptest.NewRequest(http.MethodPost, "/graphql", body)
	request.Header.Set("Content-Type", "multipart/form-data; boundary=test-boundary")
	request.Header.Set("X-Operation-Name", "uploadAvatar")
	request.ContentLength = maxGraphQLMultipartBodyBytes
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), ErrAuthenticationRequired.Error()) {
		t.Fatalf("unauthenticated multipart response = %d %s", response.Code, response.Body.String())
	}
	if body.read || len(response.readDeadlines) != 0 {
		t.Fatalf("unauthenticated multipart body read=%t read deadlines=%v", body.read, response.readDeadlines)
	}
}

func TestWebsocketSessionIsRevalidatedForEveryOperation(t *testing.T) {
	active := true
	authCalls := 0
	authenticator := requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
		authCalls++
		if !active {
			return nil, auth.ErrNoSession
		}
		return &auth.Principal{User: auth.User{ID: "user-1"}}, nil
	})
	middleware := websocketSessionMiddleware(authenticator)
	request := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	ctx := context.WithValue(context.Background(), websocketUpgradeRequestKey{}, request)
	nextCalls := 0
	next := func(ctx context.Context) gqlupload.ResponseHandler {
		nextCalls++
		if auth.PrincipalFromContext(ctx) == nil {
			t.Fatal("authenticated principal was not attached to the operation context")
		}
		return gqlupload.OneShot(&gqlupload.Response{Data: []byte(`{"ok":true}`)})
	}

	response := middleware(ctx, next)(ctx)
	if response == nil || string(response.Data) != `{"ok":true}` {
		t.Fatalf("active session response = %#v", response)
	}
	active = false
	response = middleware(ctx, next)(ctx)
	if response == nil || len(response.Errors) == 0 {
		t.Fatalf("revoked session response = %#v", response)
	}
	if nextCalls != 1 || authCalls != 2 {
		t.Fatalf("resolver calls = %d, authentication calls = %d; want 1 and 2", nextCalls, authCalls)
	}
}

func TestUnauthenticatedClientBootstrapQueries(t *testing.T) {
	handler := NewHandler(config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010}, noSessionAuthenticator{}, nil, nil)
	queries := []struct {
		name  string
		query string
		data  string
	}{
		{
			name:  "current user",
			query: `query getCurrentUser { currentUser { id name email emailVerified avatarUrl hasPassword features } }`,
			data:  `{"currentUser":null}`,
		},
		{
			name:  "user lookup prevents enumeration",
			query: `query getUser($email: String!) { user(email: $email) { __typename ... on UserType { id name email hasPassword } ... on LimitedUserType { email hasPassword } } }`,
			data:  `{"user":null}`,
		},
	}
	for _, test := range queries {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"query":     test.query,
				"variables": map[string]any{"email": "person@example.test"},
			})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := newDeadlineTestResponseWriter()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var result struct {
				Data   json.RawMessage `json:"data"`
				Errors []any           `json:"errors"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if len(result.Errors) != 0 {
				t.Fatalf("unexpected GraphQL errors: %#v", result.Errors)
			}
			var data map[string]any
			if err := json.Unmarshal(result.Data, &data); err != nil {
				t.Fatalf("decode data: %v", err)
			}
			if got, err := json.Marshal(data); err != nil || string(got) != test.data {
				t.Fatalf("data = %s, want %s", got, test.data)
			}
		})
	}
}

func TestServerConfigBootstrapMatchesWebClientContract(t *testing.T) {
	handler := NewHandler(config.Config{
		Version:     "0.27.5",
		Deployment:  "affine",
		Host:        "notes.example.test",
		Port:        443,
		HTTPS:       true,
		Path:        "notes/v1",
		ExternalURL: "https://notes.example.test/notes/v1/",
	}, noSessionAuthenticator{}, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewBufferString(`{"query":"query serverConfig { serverConfig { availableUserFeatures version baseUrl name features type calendarProviders oauthProviders credentialsRequirement { password { minLength maxLength } } } }"}`))
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Data   map[string]map[string]any `json:"data"`
		Errors []any                     `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected GraphQL errors: %#v", result.Errors)
	}
	config := result.Data["serverConfig"]
	if config["version"] != "0.27.5" || config["baseUrl"] != "https://notes.example.test/notes/v1" || config["name"] != "FydeOS Notes Cloud" || config["type"] != "Affine" {
		t.Fatalf("serverConfig = %#v", config)
	}
	if !reflect.DeepEqual(config["availableUserFeatures"], []any{"Admin"}) {
		t.Fatalf("available user features = %#v, want Admin", config["availableUserFeatures"])
	}
	if !reflect.DeepEqual(config["features"], []any{"LocalWorkspace"}) || len(config["calendarProviders"].([]any)) != 0 || len(config["oauthProviders"].([]any)) != 0 {
		t.Fatalf("serverConfig advertises unsupported capabilities: %#v", config)
	}
	password := config["credentialsRequirement"].(map[string]any)["password"].(map[string]any)
	if password["minLength"] != float64(8) || password["maxLength"] != float64(32) {
		t.Fatalf("password limits = %#v", password)
	}
	if len(response.readDeadlines) != 2 || response.readDeadlines[0].IsZero() || !response.readDeadlines[1].IsZero() {
		t.Fatalf("request body read deadlines = %#v, want a deadline followed by a cleared deadline", response.readDeadlines)
	}
}

func TestServerBaseURLUsesExistingHostAndListenerConfig(t *testing.T) {
	got := config.PublicBaseURL(config.Config{
		Host:  "notes.example.test",
		Port:  443,
		HTTPS: true,
		Path:  "/notes/v1/",
	})
	if got != "https://notes.example.test/notes/v1" {
		t.Fatalf("server base URL = %q", got)
	}
}

func TestGraphQLRejectsOversizedPostBody(t *testing.T) {
	handler := NewHandler(config.Config{Version: "0.27.5"}, noSessionAuthenticator{}, nil, nil)
	body := bytes.NewReader([]byte(strings.Repeat("x", int(maxGraphQLRequestBodyBytes+1))))
	request := httptest.NewRequest(http.MethodPost, "/graphql", body)
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), strings.Repeat("x", 64)) {
		t.Fatalf("oversized request content was reflected: %s", response.Body.String())
	}
}

func TestGraphQLMultipartBodyUsesUploadLimit(t *testing.T) {
	handler := NewHandler(config.Config{Version: "0.27.5"}, noSessionAuthenticator{}, nil, nil)
	body := bytes.NewReader([]byte(strings.Repeat("x", int(maxGraphQLRequestBodyBytes+1))))
	request := httptest.NewRequest(http.MethodPost, "/graphql", body)
	request.Header.Set("Content-Type", "multipart/form-data; boundary=test-boundary")
	request.Header.Set("x-operation-name", "CreateWorkspace")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("multipart request above JSON limit was rejected: %s", response.Body.String())
	}
}

func TestGraphQLMultipartTransportAllowsFilesAboveDefault32MiBLimit(t *testing.T) {
	handler := NewHandler(config.Config{Version: "0.27.5"}, noSessionAuthenticator{}, nil, nil)
	body := &bytes.Buffer{}
	multipartWriter := multipart.NewWriter(body)
	operations, err := json.Marshal(map[string]any{
		"query":     `mutation CreateWorkspace($init: Upload) { createWorkspace(init: $init) { id } }`,
		"variables": map[string]any{"init": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.WriteField("operations", string(operations)); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.WriteField("map", `{"0":["variables.init"]}`); err != nil {
		t.Fatal(err)
	}
	file, err := multipartWriter.CreateFormFile("0", "initial.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(bytes.Repeat([]byte{1}, (32<<20)+1)); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", body)
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	request.Header.Set("x-operation-name", "CreateWorkspace")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code == http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), ErrAuthenticationRequired.Error()) {
		t.Fatalf("multipart parser did not reach resolver above 32 MiB: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCommentAttachmentQuotaErrorPreservesLegacyGraphQLExtensions(t *testing.T) {
	service := &fakeWorkspaceBlobService{
		fakeBlobManager:  &fakeBlobManager{},
		fakeBlobUploader: &fakeBlobUploader{commentErr: blobs.ErrCommentAttachmentQuotaExceeded},
	}
	handler := NewHandlerWithBlobService(
		config.Config{Version: "0.27.5"},
		requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
			return &auth.Principal{User: auth.User{ID: "commenter-1"}}, nil
		}),
		nil,
		nil,
		auth.NewReadLimiter(auth.DefaultReadConcurrency),
		nil,
		nil,
		service,
	)

	body := &bytes.Buffer{}
	multipartWriter := multipart.NewWriter(body)
	operations, err := json.Marshal(map[string]any{
		"query":     `mutation UploadCommentAttachment($attachment: Upload!, $docId: String!, $workspaceId: String!) { uploadCommentAttachment(attachment: $attachment, docId: $docId, workspaceId: $workspaceId) }`,
		"variables": map[string]any{"attachment": nil, "docId": "doc-1", "workspaceId": "workspace-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.WriteField("operations", string(operations)); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.WriteField("map", `{"0":["variables.attachment"]}`); err != nil {
		t.Fatal(err)
	}
	file, err := multipartWriter.CreateFormFile("0", "attachment.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("attachment")); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/graphql", body)
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	request.Header.Set("x-operation-name", "UploadCommentAttachment")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var result struct {
		Errors []struct {
			Message    string         `json:"message"`
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v: %s", err, response.Body.String())
	}
	if len(result.Errors) != 1 {
		t.Fatalf("errors = %#v, want exactly one quota error", result.Errors)
	}
	got := result.Errors[0]
	if got.Message != "You have exceeded the comment attachment size quota." {
		t.Fatalf("error message = %q", got.Message)
	}
	want := map[string]any{
		"status":  float64(http.StatusPaymentRequired),
		"code":    http.StatusText(http.StatusPaymentRequired),
		"type":    "QUOTA_EXCEEDED",
		"name":    "COMMENT_ATTACHMENT_QUOTA_EXCEEDED",
		"message": got.Message,
	}
	if len(got.Extensions) != len(want) {
		t.Fatalf("extensions = %#v, want %#v", got.Extensions, want)
	}
	for key, value := range want {
		if got.Extensions[key] != value {
			t.Errorf("extensions[%q] = %#v, want %#v", key, got.Extensions[key], value)
		}
	}
}

func TestGraphQLRejectsSimpleMultipartRequestsBeforeReadingBody(t *testing.T) {
	handler := NewHandler(config.Config{Version: "0.27.5"}, noSessionAuthenticator{}, nil, nil)
	body := &observedBody{}
	request := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	request.Header.Set("Content-Type", "multipart/form-data; boundary=test-boundary")
	request.Body = body
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || body.read {
		t.Fatalf("simple multipart status=%d read=%t body=%s", response.Code, body.read, response.Body.String())
	}
}

func TestCreateWorkspaceRejectsOversizedInitialDocumentBeforeRepositoryWrite(t *testing.T) {
	resolver := &mutationResolver{Resolver: &Resolver{}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "user-1"}})
	_, err := resolver.CreateWorkspace(ctx, &gqlupload.Upload{
		File: bytes.NewReader([]byte("too small to reach the size limit")),
		Size: maxWorkspaceInitialDocumentBytes + 1,
	})
	if err != ErrInitialWorkspaceDocumentTooLarge {
		t.Fatalf("oversized initial document error = %v", err)
	}
}

func TestGraphQLDoesNotReflectMalformedJSONBody(t *testing.T) {
	handler := NewHandler(config.Config{Version: "0.27.5"}, noSessionAuthenticator{}, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":true,"secret":"do-not-reflect"}`))
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "do-not-reflect") {
		t.Fatalf("request body was reflected: %s", response.Body.String())
	}
}

func TestGraphQLRejectsWhenSharedReadCapacityIsFull(t *testing.T) {
	limiter := auth.NewReadLimiter(1)
	release, ok := limiter.TryAcquire()
	if !ok {
		t.Fatal("failed to acquire initial read slot")
	}
	defer release()
	authenticator := requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
		t.Fatal("authenticator called while shared read capacity is full")
		return nil, nil
	})
	handler := NewHandlerWithReadLimiter(config.Config{}, authenticator, nil, nil, limiter)
	response := httptest.NewRecorder()
	body := &observedBody{}
	request := httptest.NewRequest(http.MethodPost, "/graphql", body)
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("status/retry-after = %d/%q, want 429/1", response.Code, response.Header().Get("Retry-After"))
	}
	if body.read {
		t.Fatal("GraphQL request body was read while shared capacity was full")
	}
}
