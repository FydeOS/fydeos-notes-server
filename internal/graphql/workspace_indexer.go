// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type batchDocumentPermissionReader interface {
	ReadDocumentPermissionsBatch(context.Context, string, string, []string) (map[string]map[string]bool, error)
}

func (r *Resolver) workspaceSearch(ctx context.Context, workspace *model.WorkspaceType, input model.SearchInput) (*model.SearchResultObjectType, error) {
	principal, err := r.authorizeSearchWorkspace(ctx, workspace.ID)
	if err != nil {
		return nil, err
	}
	if input.Options == nil || input.Query == nil || r.SearchProjection == nil {
		if r.SearchProjection == nil {
			return nil, searchIndexNotReadyError(workspace.ID)
		}
		return nil, documentListBadRequest("Invalid indexer input")
	}
	request, err := searchRequestFromModel(input)
	if err != nil {
		return nil, documentListBadRequest("Invalid indexer input")
	}
	hits, err := r.SearchProjection.Search(ctx, workspace.ID, principal.User.ID, request)
	if errors.Is(err, search.ErrInvalidQuery) {
		return nil, documentListBadRequest("Invalid indexer input")
	}
	if err != nil {
		return nil, err
	}
	visible, err := r.filterSearchHits(ctx, principal.User.ID, workspace.ID, hits)
	if err != nil {
		return nil, err
	}
	page, hasMore, err := search.ApplySearchPagination(visible, request.Options.Pagination)
	if err != nil {
		return nil, documentListBadRequest("Invalid indexer pagination")
	}
	result := &model.SearchResultObjectType{
		Nodes:      make([]*model.SearchNodeObjectType, 0, len(page)),
		Pagination: &model.SearchResultPagination{Count: len(page), HasMore: hasMore},
	}
	for _, hit := range page {
		result.Nodes = append(result.Nodes, searchNode(hit, request.Options.Fields))
	}
	if hasMore {
		offset := pageOffset(request.Options.Pagination, len(page))
		cursor := fmt.Sprintf(`{"offset":%d}`, offset)
		result.Pagination.NextCursor = &cursor
	}
	return result, nil
}

func (r *Resolver) workspaceAggregate(ctx context.Context, workspace *model.WorkspaceType, input model.AggregateInput) (*model.AggregateResultObjectType, error) {
	principal, err := r.authorizeSearchWorkspace(ctx, workspace.ID)
	if err != nil {
		return nil, err
	}
	if input.Options == nil || input.Options.Hits == nil || input.Query == nil || r.SearchProjection == nil {
		if r.SearchProjection == nil {
			return nil, searchIndexNotReadyError(workspace.ID)
		}
		return nil, documentListBadRequest("Invalid indexer input")
	}
	request, err := aggregateRequestFromModel(input)
	if err != nil {
		return nil, documentListBadRequest("Invalid indexer input")
	}
	if err := search.ValidateProjectionAggregate(request, search.PermissionScope{WorkspaceID: workspace.ID, ActorUserID: principal.User.ID}); errors.Is(err, search.ErrInvalidQuery) {
		return nil, documentListBadRequest("Invalid indexer input")
	} else if err != nil {
		return nil, err
	}
	// Grouping needs the bucket field even when it is not requested in hit output.
	projectionOptions := request.Options.Hits
	projectionOptions.Fields = append(append([]string(nil), projectionOptions.Fields...), request.Field)
	hits, err := r.SearchProjection.Search(ctx, workspace.ID, principal.User.ID, search.SearchRequest{
		Table: request.Table, Query: request.Query, Options: projectionOptions,
	})
	if errors.Is(err, search.ErrInvalidQuery) {
		return nil, documentListBadRequest("Invalid indexer input")
	}
	if err != nil {
		return nil, err
	}
	visible, err := r.filterSearchHits(ctx, principal.User.ID, workspace.ID, hits)
	if err != nil {
		return nil, err
	}
	bucketOffset := 0
	bucketLimit := 10
	if request.Options.Pagination.Limit != nil {
		bucketLimit = int(*request.Options.Pagination.Limit)
	}
	if request.Options.Pagination.Skip != nil {
		bucketOffset = int(*request.Options.Pagination.Skip)
	}
	hitOffset := 0
	hitLimit := 10
	if request.Options.Hits.Pagination.Limit != nil {
		hitLimit = int(*request.Options.Hits.Pagination.Limit)
	}
	if request.Options.Hits.Pagination.Skip != nil {
		hitOffset = int(*request.Options.Hits.Pagination.Skip)
	}
	buckets, total := search.AggregateHits(visible, request.Field, bucketOffset, bucketLimit, hitOffset, hitLimit)
	result := &model.AggregateResultObjectType{Buckets: make([]*model.AggregateBucketObjectType, 0, len(buckets))}
	for _, bucket := range buckets {
		item := &model.AggregateBucketObjectType{
			Key: bucket.Key, Count: bucket.Count,
			Hits: &model.AggregateBucketHitsObjectType{Nodes: make([]*model.SearchNodeObjectType, 0, len(bucket.Hits))},
		}
		for _, hit := range bucket.Hits {
			item.Hits.Nodes = append(item.Hits.Nodes, searchNode(hit, request.Options.Hits.Fields))
		}
		result.Buckets = append(result.Buckets, item)
	}
	result.Pagination = &model.SearchResultPagination{Count: len(buckets), HasMore: bucketOffset+len(buckets) < total}
	return result, nil
}

func (r *Resolver) authorizeSearchWorkspace(ctx context.Context, workspaceID string) (*auth.Principal, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.WorkspacePermissionReader == nil {
		return nil, documentWorkspaceReadError(workspaces.ErrAuthorizationUnavailable, workspaceID)
	}
	access, err := r.WorkspacePermissionReader.GetWorkspaceRolePermissions(ctx, principal.User.ID, workspaceID)
	if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: workspaceID}
	}
	if err != nil {
		return nil, err
	}
	if !access.Permissions["Workspace_Read"] {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: workspaceID}
	}
	return principal, nil
}

func (r *Resolver) filterSearchHits(ctx context.Context, userID, workspaceID string, hits []search.ProjectionHit) ([]search.ProjectionHit, error) {
	if r.DocumentReader == nil {
		return nil, documentReadError(workspaces.ErrAuthorizationUnavailable, workspaceID, "", "Doc.Read")
	}
	if batchReader, ok := r.DocumentReader.(batchDocumentPermissionReader); ok {
		docIDs := make([]string, 0, len(hits))
		seen := make(map[string]struct{}, len(hits))
		for _, hit := range hits {
			if _, ok := seen[hit.DocID]; ok {
				continue
			}
			seen[hit.DocID] = struct{}{}
			docIDs = append(docIDs, hit.DocID)
		}
		permissionsByDoc, err := batchReader.ReadDocumentPermissionsBatch(ctx, userID, workspaceID, docIDs)
		if errors.Is(err, workspaces.ErrDocRoleDenied) || errors.Is(err, workspaces.ErrDocumentNotFound) {
			return []search.ProjectionHit{}, nil
		}
		if err != nil {
			return nil, documentReadError(err, workspaceID, "", "Doc.Read")
		}
		visible := make([]search.ProjectionHit, 0, len(hits))
		for _, hit := range hits {
			if permissionsByDoc[hit.DocID]["Doc.Read"] {
				visible = append(visible, hit)
			}
		}
		return visible, nil
	}
	visible := make([]search.ProjectionHit, 0, len(hits))
	readable := make(map[string]bool)
	for _, hit := range hits {
		allowed, checked := readable[hit.DocID]
		if !checked {
			permissions, err := r.DocumentReader.ReadDocumentPermissions(ctx, userID, workspaceID, hit.DocID)
			if errors.Is(err, workspaces.ErrDocRoleDenied) || errors.Is(err, workspaces.ErrDocumentNotFound) {
				readable[hit.DocID] = false
				continue
			}
			if err != nil {
				return nil, documentReadError(err, workspaceID, hit.DocID, "Doc.Read")
			}
			allowed = permissions["Doc.Read"]
			readable[hit.DocID] = allowed
		}
		if allowed {
			visible = append(visible, hit)
		}
	}
	return visible, nil
}

func searchRequestFromModel(input model.SearchInput) (search.SearchRequest, error) {
	request := search.SearchRequest{Table: search.Table(input.Table), Query: searchQueryFromModel(input.Query)}
	options, err := searchOptionsFromModel(input.Options.Fields, input.Options.Highlights, input.Options.Pagination)
	if err != nil {
		return search.SearchRequest{}, err
	}
	request.Options = options
	return request, nil
}

func aggregateRequestFromModel(input model.AggregateInput) (search.AggregateRequest, error) {
	request := search.AggregateRequest{Table: search.Table(input.Table), Query: searchQueryFromModel(input.Query), Field: input.Field}
	hits, err := searchOptionsFromModel(input.Options.Hits.Fields, input.Options.Hits.Highlights, nil)
	if err != nil {
		return search.AggregateRequest{}, err
	}
	if input.Options.Hits.Pagination != nil {
		hits.Pagination.Limit, err = uint32Pointer(input.Options.Hits.Pagination.Limit)
		if err != nil {
			return search.AggregateRequest{}, err
		}
		hits.Pagination.Skip, err = uint32Pointer(input.Options.Hits.Pagination.Skip)
		if err != nil {
			return search.AggregateRequest{}, err
		}
	}
	pagination, err := searchOptionsFromModel(nil, nil, input.Options.Pagination)
	if err != nil {
		return search.AggregateRequest{}, err
	}
	request.Options = search.AggregateOptions{Hits: hits, Pagination: pagination.Pagination}
	return request, nil
}

func searchOptionsFromModel(fields []string, highlights []*model.SearchHighlight, pagination *model.SearchPagination) (search.SearchOptions, error) {
	options := search.SearchOptions{Fields: fields}
	for _, highlight := range highlights {
		if highlight != nil {
			options.Highlights = append(options.Highlights, search.Highlight{Field: highlight.Field, Before: highlight.Before, End: highlight.End})
		}
	}
	if pagination != nil {
		var err error
		options.Pagination.Limit, err = uint32Pointer(pagination.Limit)
		if err != nil {
			return search.SearchOptions{}, err
		}
		options.Pagination.Skip, err = uint32Pointer(pagination.Skip)
		if err != nil {
			return search.SearchOptions{}, err
		}
		options.Pagination.Cursor = pagination.Cursor
	}
	return options, nil
}

func uint32Pointer(value *int) (*uint32, error) {
	if value == nil {
		return nil, nil
	}
	if *value < 0 {
		return nil, search.ErrInvalidQuery
	}
	if uint64(*value) > uint64(^uint32(0)) {
		return nil, search.ErrInvalidQuery
	}
	converted := uint32(*value)
	return &converted, nil
}

func searchQueryFromModel(input *model.SearchQuery) *search.Query {
	if input == nil {
		return nil
	}
	query := &search.Query{Type: string(input.Type), Boost: input.Boost}
	if input.Field != nil {
		query.Field = *input.Field
	}
	if input.Match != nil {
		query.Match = *input.Match
	}
	if input.Occur != nil {
		query.Occur = string(*input.Occur)
	}
	if input.Query != nil {
		query.Query = searchQueryFromModel(input.Query)
	}
	for _, child := range input.Queries {
		query.Queries = append(query.Queries, searchQueryFromModel(child))
	}
	return query
}

func searchNode(hit search.ProjectionHit, fields []string) *model.SearchNodeObjectType {
	var highlights model.JSONObject
	if len(hit.Highlights) > 0 {
		highlights = make(model.JSONObject, len(hit.Highlights))
		for field, values := range hit.Highlights {
			highlights[field] = values
		}
	}
	return &model.SearchNodeObjectType{Fields: model.JSONObject(search.SelectFields(hit, fields)), Highlights: highlights}
}

func pageOffset(pagination search.Pagination, count int) int {
	if pagination.Cursor != nil {
		var value struct {
			Offset int `json:"offset"`
		}
		if json.Unmarshal([]byte(*pagination.Cursor), &value) == nil && value.Offset >= 0 {
			return value.Offset + count
		}
	}
	if pagination.Skip != nil {
		return int(*pagination.Skip) + count
	}
	return count
}
