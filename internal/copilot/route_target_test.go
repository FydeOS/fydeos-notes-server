// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
)

type localChatResolverStub struct {
	leaseID, profileID, modelID string
	target                      byok.ChatTarget
}

type localImageResolverStub struct {
	leaseID, profileID, modelID, operation string
	target                                 byok.ChatTarget
}

func (s *localImageResolverStub) ResolveImageTarget(context.Context, string, string, string, string, string) (byok.ChatTarget, error) {
	return byok.ChatTarget{}, byok.ErrTargetUnavailable
}

func (s *localImageResolverStub) ResolveLocalImageTarget(_ context.Context, _, _, leaseID, profileID, modelID, operation string) (byok.ChatTarget, error) {
	s.leaseID, s.profileID, s.modelID, s.operation = leaseID, profileID, modelID, operation
	return s.target, nil
}

func (s *localChatResolverStub) ResolveChatTarget(context.Context, string, string, string, string) (byok.ChatTarget, error) {
	return byok.ChatTarget{}, byok.ErrTargetUnavailable
}

func (s *localChatResolverStub) ResolveLocalChatTarget(_ context.Context, _, _, leaseID, profileID, modelID string) (byok.ChatTarget, error) {
	s.leaseID, s.profileID, s.modelID = leaseID, profileID, modelID
	return s.target, nil
}

func TestParseBYOKRouteTargetSupportsLeaseProfileIDs(t *testing.T) {
	profile, model, local, ok := parseBYOKRouteTarget("fydeos-byok:lease-1:12:gpt-4.1", "lease-1")
	if !ok || !local || profile != "lease-1:12" || model != "gpt-4.1" {
		t.Fatalf("local target = %q %q %v %v", profile, model, local, ok)
	}
	profile, model, local, ok = parseBYOKRouteTarget("fydeos-byok:profile-1:model:variant", "lease-1")
	if !ok || local || profile != "profile-1" || model != "model:variant" {
		t.Fatalf("server target = %q %q %v %v", profile, model, local, ok)
	}
	if _, _, _, ok := parseBYOKRouteTarget("fydeos-byok:lease-1:not-an-index:model", "lease-1"); ok {
		t.Fatal("invalid local route index was accepted")
	}
}

func TestResolveRoutePassesLeaseBoundLocalTarget(t *testing.T) {
	resolver := &localChatResolverStub{target: byok.ChatTarget{
		Source: "byok_local", Provider: "openai", ModelID: "gpt-4.1", BaseURL: "https://api.openai.com/v1",
		Endpoint: byok.Endpoint{Kind: "provider_default"}, Credential: byok.NewSensitiveCredential([]byte("local-key")),
	}}
	handler := NewChatHTTPHandler(nil, config.Config{}, nil, nil, nil, nil)
	handler.SetBYOKProfileStore(resolver)
	request := httptest.NewRequest(http.MethodGet, "/?byokLeaseId=lease-1&routeTargetId=fydeos-byok%3Alease-1%3A0%3Agpt-4.1", nil)
	route, err := handler.resolveRoute(request, "user-1", "workspace-1")
	if err != nil {
		t.Fatal(err)
	}
	defer route.credential.Destroy()
	if resolver.leaseID != "lease-1" || resolver.profileID != "lease-1:0" || resolver.modelID != "gpt-4.1" || route.provider != "openai" || route.model != "gpt-4.1" {
		t.Fatalf("resolved local route=%+v resolver=%+v", route, resolver)
	}
}

func TestResolveImageTargetFromRequestUsesLocalLease(t *testing.T) {
	resolver := &localImageResolverStub{target: byok.ChatTarget{Source: "byok_local", Provider: "openai", ModelID: "gpt-image-1"}}
	request := httptest.NewRequest(http.MethodGet, "/?byokLeaseId=lease-1&routeTargetId=fydeos-byok%3Alease-1%3A4%3Agpt-image-1", nil)
	target, err := resolveImageTargetFromRequest(request.Context(), resolver, request, "workspace-1", "user-1", "image_edit")
	if err != nil {
		t.Fatal(err)
	}
	if resolver.leaseID != "lease-1" || resolver.profileID != "lease-1:4" || resolver.modelID != "gpt-image-1" || resolver.operation != "image_edit" || target.Source != "byok_local" {
		t.Fatalf("resolved image target=%+v resolver=%+v", target, resolver)
	}
}
