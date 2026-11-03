// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	gqlgraphql "github.com/99designs/gqlgen/graphql"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/avatars"
	"github.com/fydeos/fydeos-notes-server/internal/users"
)

func TestUploadAvatarUsesAuthenticatedUserAndPublishesProfileChange(t *testing.T) {
	uploader := &avatarUploaderFake{user: &users.User{ID: "user-1", AvatarURL: stringPointer("https://notes.example.test/avatar.webp")}}
	publisher := &avatarPublisherFake{}
	resolver := &mutationResolver{Resolver: &Resolver{AvatarUploader: uploader, ProfilePublisher: publisher}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "user-1"}})
	file := bytes.NewReader([]byte("png bytes"))
	got, err := resolver.UploadAvatar(ctx, gqlgraphql.Upload{Filename: "avatar.png", ContentType: "image/png", File: file})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "user-1" || got.AvatarURL == nil || *got.AvatarURL != "https://notes.example.test/avatar.webp" {
		t.Fatalf("upload result = %#v", got)
	}
	if uploader.userID != "user-1" || uploader.contentType != "image/png" || uploader.body != "png bytes" {
		t.Fatalf("upload input = user %q, content type %q, body %q", uploader.userID, uploader.contentType, uploader.body)
	}
	if !reflect.DeepEqual(publisher.userIDs, []string{"user-1"}) {
		t.Fatalf("profile publications = %#v", publisher.userIDs)
	}
}

func TestUploadAvatarRejectsAnonymousAndPreservesUploaderError(t *testing.T) {
	uploader := &avatarUploaderFake{err: &avatars.FormatError{Format: "image/svg+xml"}}
	publisher := &avatarPublisherFake{}
	resolver := &mutationResolver{Resolver: &Resolver{AvatarUploader: uploader, ProfilePublisher: publisher}}
	if _, err := resolver.UploadAvatar(context.Background(), gqlgraphql.Upload{}); err != ErrAuthenticationRequired {
		t.Fatalf("anonymous upload error = %v", err)
	}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "user-1"}})
	if _, err := resolver.UploadAvatar(ctx, gqlgraphql.Upload{File: bytes.NewReader(nil)}); err == nil || !errors.As(err, new(*avatars.FormatError)) {
		t.Fatalf("format error = %v, want propagated FormatError", err)
	}
	if len(publisher.userIDs) != 0 {
		t.Fatalf("failed or anonymous upload published profile change: %#v", publisher.userIDs)
	}
}

func TestAvatarFormatGraphQLErrorMatchesLegacyContract(t *testing.T) {
	presented := presentGraphQLError(context.Background(), &avatars.FormatError{Format: "image/svg+xml"})
	if presented.Message != "Image format not supported: image/svg+xml" {
		t.Fatalf("message = %q", presented.Message)
	}
	if presented.Extensions["type"] != "invalid_input" || presented.Extensions["name"] != "image_format_not_supported" || presented.Extensions["status"] != 400 {
		t.Fatalf("extensions = %#v", presented.Extensions)
	}
	data, ok := presented.Extensions["data"].(map[string]string)
	if !ok || data["format"] != "image/svg+xml" {
		t.Fatalf("error data = %#v", presented.Extensions["data"])
	}

	quota := presentGraphQLError(context.Background(), avatars.ErrUploadTooLarge)
	if quota.Message != "You have exceeded your blob size quota." || quota.Extensions["status"] != 402 || quota.Extensions["type"] != "QUOTA_EXCEEDED" || quota.Extensions["name"] != "BLOB_QUOTA_EXCEEDED" {
		t.Fatalf("oversized avatar error = %q %#v", quota.Message, quota.Extensions)
	}
}

type avatarUploaderFake struct {
	user        *users.User
	err         error
	userID      string
	contentType string
	body        string
}

func (f *avatarUploaderFake) Upload(_ context.Context, userID, contentType string, source io.Reader) (*users.User, error) {
	f.userID, f.contentType = userID, contentType
	if source != nil {
		body, err := io.ReadAll(source)
		if err != nil {
			return nil, err
		}
		f.body = string(body)
	}
	return f.user, f.err
}

type avatarPublisherFake struct{ userIDs []string }

func (f *avatarPublisherFake) PublishUserProfileChanged(userID string) {
	f.userIDs = append(f.userIDs, userID)
}
