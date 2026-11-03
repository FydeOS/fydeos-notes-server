// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type analyticsDocumentReaderStub struct {
	DocumentReader
	permissions map[string]bool
	err         error
}

func (s analyticsDocumentReaderStub) ReadDocumentPermissions(context.Context, string, string, string) (map[string]bool, error) {
	return s.permissions, s.err
}

type documentAnalyticsStub struct {
	workspaces.DocumentAnalyticsService
	page workspaces.DocPageAnalytics
	last workspaces.DocMemberLastAccessPage
	err  error
}

func (s documentAnalyticsStub) GetDocPageAnalytics(context.Context, string, string, string, workspaces.DocPageAnalyticsInput) (workspaces.DocPageAnalytics, error) {
	return s.page, s.err
}

func (s documentAnalyticsStub) PaginateDocLastAccessedMembers(context.Context, string, string, string, workspaces.DocMemberLastAccessPagination, *string, bool) (workspaces.DocMemberLastAccessPage, error) {
	return s.last, s.err
}

func TestDocumentAnalyticsResolversCheckSpecificPermissionsAndMapResults(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	from, to := now.AddDate(0, 0, -1), now
	reader := analyticsDocumentReaderStub{permissions: map[string]bool{
		"Doc.Analytics.Read": true, "Doc.Analytics.Viewers.Read": true,
	}}
	memberCursor := "cursor"
	member := workspaces.DocMemberLastAccess{UserID: "user-1", Name: "User One", LastAccessedAt: now, LastDocID: analyticsStringPointer("doc")}
	analytics := documentAnalyticsStub{
		page: workspaces.DocPageAnalytics{
			Window:  workspaces.DocAnalyticsTimeWindow{From: from, To: to, Timezone: "UTC", Bucket: "Day", RequestedSize: 2, EffectiveSize: 2},
			Series:  []workspaces.DocPageAnalyticsPoint{{Date: to, TotalViews: 3, UniqueViews: 2, GuestViews: 1}},
			Summary: workspaces.DocPageAnalyticsSummary{TotalViews: 3, UniqueViews: 2, GuestViews: 1, LastAccessedAt: &to}, GeneratedAt: now,
		},
		last: workspaces.DocMemberLastAccessPage{
			Members: []workspaces.DocMemberLastAccess{member}, HasNextPage: true,
			StartCursor: &memberCursor, EndCursor: &memberCursor,
		},
	}
	resolver := &docTypeResolver{Resolver: &Resolver{DocumentReader: reader, DocumentAnalytics: analytics}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "admin"}})
	doc := &model.DocType{WorkspaceID: "space", ID: "doc"}

	page, err := resolver.Analytics(ctx, doc, nil)
	if err != nil || page == nil || page.Window.RequestedSize != 2 || page.Window.Bucket != model.TimeBucketDay || len(page.Series) != 1 || page.Series[0].TotalViews != 3 || page.Summary.LastAccessedAt == nil {
		t.Fatalf("analytics result=%+v err=%v", page, err)
	}
	viewers, err := resolver.LastAccessedMembers(ctx, doc, nil, model.PaginationInput{First: analyticsIntPointer(10)}, nil)
	if err != nil || viewers == nil || len(viewers.Edges) != 1 || viewers.Edges[0].Cursor != memberCursor || viewers.Edges[0].Node.User.ID != "user-1" || viewers.PageInfo.StartCursor == nil {
		t.Fatalf("last-access result=%+v err=%v", viewers, err)
	}

	reader.permissions["Doc.Analytics.Read"] = false
	if _, err := resolver.Analytics(ctx, doc, nil); err == nil {
		t.Fatal("analytics succeeded without Doc.Analytics.Read")
	} else {
		var graphErr *gqlerror.Error
		if !errors.As(err, &graphErr) || graphErr.Extensions["name"] != "doc_action_denied" {
			t.Fatalf("analytics denial error = %#v", err)
		}
	}
	reader.permissions["Doc.Analytics.Read"] = true
	reader.permissions["Doc.Analytics.Viewers.Read"] = false
	if _, err := resolver.LastAccessedMembers(ctx, doc, nil, model.PaginationInput{First: analyticsIntPointer(10)}, nil); err == nil {
		t.Fatal("viewer list succeeded without Doc.Analytics.Viewers.Read")
	}
	if _, err := resolver.Analytics(context.Background(), doc, nil); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("anonymous analytics error = %v", err)
	}
}

func TestDocumentAnalyticsResolverMapsQueryLimitError(t *testing.T) {
	reader := analyticsDocumentReaderStub{permissions: map[string]bool{"Doc.Analytics.Viewers.Read": true}}
	analytics := documentAnalyticsStub{err: workspaces.ErrDocAnalyticsQueryTooLong}
	resolver := &docTypeResolver{Resolver: &Resolver{DocumentReader: reader, DocumentAnalytics: analytics}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "admin"}})
	query := "too long"
	_, err := resolver.LastAccessedMembers(ctx, &model.DocType{WorkspaceID: "space", ID: "doc"}, nil, model.PaginationInput{First: analyticsIntPointer(10)}, &query)
	var graphErr *gqlerror.Error
	if !errors.As(err, &graphErr) || graphErr.Extensions["name"] != "query_too_long" || graphErr.Extensions["status"] != 400 {
		t.Fatalf("query limit GraphQL error = %#v", err)
	}
}

func analyticsStringPointer(value string) *string { return &value }
func analyticsIntPointer(value int) *int          { return &value }
