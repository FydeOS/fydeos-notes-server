// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type inviteLinkReaderFake struct {
	link         *invitelinks.Link
	workspaceID  string
	err          error
	revokeResult bool
	revokeCalls  int
}

func (f *inviteLinkReaderFake) Get(_ context.Context, workspaceID string) (*invitelinks.Link, error) {
	f.workspaceID = workspaceID
	return f.link, f.err
}

func (f *inviteLinkReaderFake) Revoke(_ context.Context, workspaceID string) bool {
	f.workspaceID = workspaceID
	f.revokeCalls++
	return f.revokeResult
}

type inviteLinkPublisherFake struct {
	workspaceID string
	reason      string
	calls       int
}

type inviteLinkCreatorFake struct {
	link        *invitelinks.Link
	err         error
	actorID     string
	workspaceID string
	expireTime  string
	calls       int
}

func (f *inviteLinkCreatorFake) Create(_ context.Context, actorID, workspaceID, expireTime string) (*invitelinks.Link, error) {
	f.actorID, f.workspaceID, f.expireTime = actorID, workspaceID, expireTime
	f.calls++
	return f.link, f.err
}

func (f *inviteLinkPublisherFake) PublishWorkspaceInviteLinkChanged(workspaceID, reason string) {
	f.workspaceID, f.reason = workspaceID, reason
	f.calls++
}

type workspaceUsersManagerFake struct {
	allowed     bool
	userID      string
	workspaceID string
	err         error
}

func (f *workspaceUsersManagerFake) CanManageWorkspaceUsers(_ context.Context, userID, workspaceID string) (bool, error) {
	f.userID, f.workspaceID = userID, workspaceID
	return f.allowed, f.err
}

func TestWorkspaceInviteLinkRequiresManagePermission(t *testing.T) {
	expiresAt := time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)
	reader := &inviteLinkReaderFake{link: &invitelinks.Link{URL: "https://notes.example.test/invite/abc123", ExpiresAt: expiresAt}}
	manager := &workspaceUsersManagerFake{allowed: true}
	resolver := &workspaceTypeResolver{Resolver: &Resolver{InviteLinkReader: reader, InviteLinkAuthorizer: manager}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "admin-1"}})

	got, err := resolver.InviteLink(ctx, &model.WorkspaceType{ID: "workspace-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Link != reader.link.URL || got.ExpireTime != expiresAt.Format(time.RFC3339Nano) {
		t.Fatalf("inviteLink = %#v", got)
	}
	if manager.userID != "admin-1" || manager.workspaceID != "workspace-1" || reader.workspaceID != "workspace-1" {
		t.Fatalf("authorization/read inputs manager=%+v reader=%+v", manager, reader)
	}

	manager.allowed = false
	reader.workspaceID = ""
	if _, err := resolver.InviteLink(ctx, &model.WorkspaceType{ID: "workspace-2"}); !errors.Is(err, ErrWorkspaceMemberPermissionDenied) {
		t.Fatalf("denied inviteLink error = %v", err)
	}
	if reader.workspaceID != "" {
		t.Fatal("reader was called for an unauthorized user")
	}
}

func TestWorkspaceInviteLinkRequiresAuthenticationAndPropagatesDependencies(t *testing.T) {
	resolver := &workspaceTypeResolver{Resolver: &Resolver{InviteLinkReader: &inviteLinkReaderFake{}, InviteLinkAuthorizer: &workspaceUsersManagerFake{allowed: true}}}
	if _, err := resolver.InviteLink(context.Background(), &model.WorkspaceType{ID: "workspace-1"}); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("anonymous inviteLink error = %v", err)
	}

	manager := &workspaceUsersManagerFake{allowed: true, err: errors.New("authorization database unavailable")}
	resolver.InviteLinkAuthorizer = manager
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "admin-1"}})
	if _, err := resolver.InviteLink(ctx, &model.WorkspaceType{ID: "workspace-1"}); !errors.Is(err, manager.err) {
		t.Fatalf("authorization failure = %v", err)
	}
}

func TestWorkspaceInviteLinkGraphQLHTTPContract(t *testing.T) {
	expiresAt := time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)
	reader := &inviteLinkReaderFake{link: &invitelinks.Link{URL: "https://notes.example.test/invite/abc123", ExpiresAt: expiresAt}}
	manager := &workspaceUsersManagerFake{allowed: true}
	repository := commentWorkspaceRepositoryFake{workspace: workspaces.Workspace{ID: "workspace-1", OwnerID: "admin-1"}}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5"},
		requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
			return &auth.Principal{User: auth.User{ID: "admin-1"}}, nil
		}), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{InviteLinks: WorkspaceInviteLinkDependencies{Reader: reader, Authorizer: manager}},
	)
	query := `query { workspace(id: "workspace-1") { inviteLink { link expireTime } } }`
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL status = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"inviteLink":{"link":"https://notes.example.test/invite/abc123","expireTime":"2026-10-18T12:30:00Z"}`) {
		t.Fatalf("GraphQL invite-link response = %s", response.Body.String())
	}

	manager.allowed = false
	response = newDeadlineTestResponseWriter()
	request = httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"name":"SPACE_ACCESS_DENIED"`) {
		t.Fatalf("GraphQL denied invite-link response = %s", response.Body.String())
	}
}

func TestRevokeWorkspaceInviteLinkGraphQLHTTPContract(t *testing.T) {
	reader := &inviteLinkReaderFake{}
	manager := &workspaceUsersManagerFake{allowed: true}
	publisher := &inviteLinkPublisherFake{}
	repository := commentWorkspaceRepositoryFake{workspace: workspaces.Workspace{ID: "workspace-1", OwnerID: "admin-1"}}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5"},
		requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
			return &auth.Principal{User: auth.User{ID: "admin-1"}}, nil
		}), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{InviteLinks: WorkspaceInviteLinkDependencies{
			Reader: reader, Revoker: reader, Authorizer: manager, Publisher: publisher,
		}},
	)
	query := `mutation { revokeInviteLink(workspaceId: "workspace-1") }`
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	newRequest := func() *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		return request
	}
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, newRequest())
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revokeInviteLink":false`) {
		t.Fatalf("missing-link revoke response = %d %s", response.Code, response.Body.String())
	}
	if reader.revokeCalls != 1 || manager.userID != "admin-1" || manager.workspaceID != "workspace-1" {
		t.Fatalf("revoke authorization/store = manager:%+v store:%+v", manager, reader)
	}
	if publisher.calls != 1 || publisher.workspaceID != "workspace-1" || publisher.reason != "invite-link-revoked" {
		t.Fatalf("revoke publisher = %+v", publisher)
	}

	manager.allowed = false
	response = newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, newRequest())
	if !strings.Contains(response.Body.String(), `"name":"SPACE_ACCESS_DENIED"`) {
		t.Fatalf("unauthorized revoke response = %s", response.Body.String())
	}
	if reader.revokeCalls != 1 || publisher.calls != 1 {
		t.Fatalf("unauthorized revoke reached side effects: store calls=%d publisher calls=%d", reader.revokeCalls, publisher.calls)
	}
}

func TestCreateWorkspaceInviteLinkGraphQLHTTPContract(t *testing.T) {
	expiresAt := time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC)
	creator := &inviteLinkCreatorFake{link: &invitelinks.Link{
		URL: "https://notes.example.test/invite/new-link", ExpiresAt: expiresAt, Created: true,
	}}
	manager := &workspaceUsersManagerFake{allowed: true}
	publisher := &inviteLinkPublisherFake{}
	repository := commentWorkspaceRepositoryFake{workspace: workspaces.Workspace{ID: "workspace-1", OwnerID: "admin-1"}}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5"},
		requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
			return &auth.Principal{User: auth.User{ID: "admin-1"}}, nil
		}), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{InviteLinks: WorkspaceInviteLinkDependencies{
			Creator: creator, Authorizer: manager, Publisher: publisher,
		}},
	)
	query := `mutation { createInviteLink(expireTime: OneDay, workspaceId: "workspace-1") { link expireTime } }`
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	newRequest := func() *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		return request
	}
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, newRequest())
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"createInviteLink":{"link":"https://notes.example.test/invite/new-link","expireTime":"2026-10-20T12:30:00Z"}`) {
		t.Fatalf("create invite-link response = %d %s", response.Code, response.Body.String())
	}
	if creator.calls != 1 || creator.actorID != "admin-1" || creator.workspaceID != "workspace-1" || creator.expireTime != "OneDay" {
		t.Fatalf("creator input = %+v", creator)
	}
	if publisher.calls != 1 || publisher.reason != "invite-link-created" || publisher.workspaceID != "workspace-1" {
		t.Fatalf("created event = %+v", publisher)
	}

	creator.link.Created = false
	response = newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, newRequest())
	if response.Code != http.StatusOK || publisher.calls != 1 {
		t.Fatalf("reused link should not publish a create event: %d %s publisher=%+v", response.Code, response.Body.String(), publisher)
	}

	manager.allowed = false
	response = newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, newRequest())
	if !strings.Contains(response.Body.String(), `"name":"SPACE_ACCESS_DENIED"`) || creator.calls != 2 || publisher.calls != 1 {
		t.Fatalf("unauthorized create side effects: %s creator=%+v publisher=%+v", response.Body.String(), creator, publisher)
	}

	manager.allowed = true
	creator.err = invitelinks.ErrPolicyDenied
	response = newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, newRequest())
	if !strings.Contains(response.Body.String(), `"name":"ACTION_FORBIDDEN"`) || publisher.calls != 1 {
		t.Fatalf("policy denial response or event = %s publisher=%+v", response.Body.String(), publisher)
	}
}
