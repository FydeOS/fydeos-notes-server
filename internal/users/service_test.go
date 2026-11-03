// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package users

import (
	"context"
	"errors"
	"testing"
)

type readerStub struct {
	user *User
	err  error
}

func (s readerStub) Get(context.Context, string) (*User, error) { return s.user, s.err }
func (s readerStub) GetPublic(context.Context, string) (*User, error) {
	return s.user, s.err
}
func (s readerStub) GetByEmail(context.Context, string) (*User, error) {
	return s.user, s.err
}

type visibilityStub struct {
	allowed bool
	err     error
	viewer  string
	target  string
}

func (s *visibilityStub) CanViewUser(_ context.Context, viewerID, targetID string) (bool, error) {
	s.viewer, s.target = viewerID, targetID
	return s.allowed, s.err
}

func TestGetVisibleByEmailComposesLookupAndAuthorization(t *testing.T) {
	target := &User{ID: "target-1", Email: "target@example.test"}
	visibility := &visibilityStub{allowed: true}
	service := NewService(readerStub{user: target}, visibility)

	got, err := service.GetVisibleByEmail(context.Background(), target.Email, "viewer-1")
	if err != nil || got != target {
		t.Fatalf("GetVisibleByEmail() = (%v, %v), want target", got, err)
	}
	if visibility.viewer != "viewer-1" || visibility.target != target.ID {
		t.Fatalf("authorization args = %q/%q", visibility.viewer, visibility.target)
	}
}

func TestValidateAdminEmailMatchesLegacyZodEmailExamples(t *testing.T) {
	for _, value := range []string{
		"valid+tag@sub.example.test",
		"FOO@EXAMPLE.COM",
		"a_b@example.com",
		"a'b@example.com",
		"a@example-.test",
	} {
		if err := validateAdminEmail(value); err != nil {
			t.Errorf("validateAdminEmail(%q) = %v, want valid", value, err)
		}
	}
	for _, value := range []string{
		"x@y",
		"x@y.c",
		".lead@example.test",
		"a..b@example.test",
		"foo@[127.0.0.1]",
		`"foo bar"@example.test`,
		"a.@example.test",
		"a@-example.test",
		"a@example.c0m",
		"foo@example.中国",
	} {
		if err := validateAdminEmail(value); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("validateAdminEmail(%q) = %v, want invalid email", value, err)
		}
	}
}

func TestGetVisibleByEmailHidesUnauthorizedAndMissingUsers(t *testing.T) {
	visibility := &visibilityStub{}
	service := NewService(readerStub{user: &User{ID: "target-1"}}, visibility)
	got, err := service.GetVisibleByEmail(context.Background(), "target@example.test", "viewer-1")
	if err != nil || got != nil {
		t.Fatalf("unauthorized lookup = (%v, %v), want nil", got, err)
	}

	service = NewService(readerStub{err: ErrNotFound}, visibility)
	got, err = service.GetVisibleByEmail(context.Background(), "missing@example.test", "viewer-1")
	if err != nil || got != nil {
		t.Fatalf("missing lookup = (%v, %v), want nil", got, err)
	}
}

func TestGetVisibleByEmailPropagatesAuthorizationFailure(t *testing.T) {
	wantErr := errors.New("authorization store unavailable")
	service := NewService(readerStub{user: &User{ID: "target-1"}}, &visibilityStub{err: wantErr})
	if _, err := service.GetVisibleByEmail(context.Background(), "target@example.test", "viewer-1"); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}
