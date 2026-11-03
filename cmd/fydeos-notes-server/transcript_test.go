// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
)

type transcriptRouteSpy struct{ calls int }

func (s *transcriptRouteSpy) ResolveServerTarget(context.Context, string, string, string) (byok.ChatTarget, error) {
	s.calls++
	return byok.ChatTarget{Provider: "gemini", ModelID: "audio"}, nil
}
func TestTranscriptManagedSelfhostDoesNotSelectBYOKImplicitly(t *testing.T) {
	spy := &transcriptRouteSpy{}
	routes := copilot.NewTranscriptRoutes(nil, config.Config{Deployment: "selfhosted", FydeOSManagedCopilot: true}, spy)
	if _, err := routes.ResolveServerTarget(context.Background(), "workspace", "actor", "transcript"); !errors.Is(err, byok.ErrTargetUnavailable) || spy.calls != 0 {
		t.Fatalf("managed route err=%v BYOK calls=%d", err, spy.calls)
	}
	routes = copilot.NewTranscriptRoutes(nil, config.Config{Deployment: "selfhosted"}, spy)
	target, err := routes.ResolveServerTarget(context.Background(), "workspace", "actor", "transcript")
	if err != nil || spy.calls != 1 || target.Provider != "gemini" {
		t.Fatalf("BYOK route=%#v calls=%d err=%v", target, spy.calls, err)
	}
}
