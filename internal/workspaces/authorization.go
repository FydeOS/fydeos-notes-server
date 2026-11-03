// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type MembershipReader interface {
	HasCommonActiveWorkspace(context.Context, string, string) (bool, error)
}

type syncSnapshotReader interface {
	ReadWorkspaceSyncSnapshot(ctx context.Context, userID, workspaceID string, docIDs []string) (WorkspaceSyncSnapshot, error)
}

type syncSnapshotTxReader interface {
	ReadWorkspaceSyncSnapshotInTx(ctx context.Context, tx pgx.Tx, userID, workspaceID string, docIDs []string) (WorkspaceSyncSnapshot, error)
}

type workspaceTxReader interface {
	CanReadWorkspaceInTx(ctx context.Context, tx pgx.Tx, userID, workspaceID string) (bool, error)
}

type userManager interface {
	CanManageWorkspaceUsers(ctx context.Context, userID, workspaceID string) (bool, error)
}

type accessFactsReader interface {
	ReadWorkspaceAccessFacts(ctx context.Context, userID, workspaceID string) (WorkspaceAccessFacts, error)
}

type commercialReader interface {
	WorkspaceCommercialEntitlement(ctx context.Context, workspaceID string) (bool, error)
}

type configReader interface {
	ReadWorkspaceConfig(ctx context.Context, workspaceID string) (WorkspaceConfig, error)
}

// AuthorizationService answers workspace and document authorization
// questions from whatever capabilities its reader offers.
type AuthorizationService struct {
	memberships MembershipReader
}

func NewAuthorizationService(memberships MembershipReader) *AuthorizationService {
	return &AuthorizationService{memberships: memberships}
}

func capability[T any](s *AuthorizationService) (T, bool) {
	var zero T
	if s == nil || s.memberships == nil {
		return zero, false
	}
	v, ok := s.memberships.(T)
	return v, ok
}

func (s *AuthorizationService) CanManageWorkspaceUsers(ctx context.Context, userID, workspaceID string) (bool, error) {
	m, ok := capability[userManager](s)
	if !ok {
		return false, ErrAuthorizationUnavailable
	}
	return m.CanManageWorkspaceUsers(ctx, userID, workspaceID)
}

func (s *AuthorizationService) CanViewUser(ctx context.Context, viewerID, targetID string) (bool, error) {
	if viewerID != "" && viewerID == targetID {
		return true, nil
	}
	if s == nil || s.memberships == nil {
		return false, ErrAuthorizationUnavailable
	}
	if viewerID == "" || targetID == "" {
		return false, nil
	}
	return s.memberships.HasCommonActiveWorkspace(ctx, viewerID, targetID)
}

// commercialFor upgrades a snapshot's commercial flag through the resolver
// when the flag matters (an active admin) and the shortcut said no.
func (s *AuthorizationService) commercialFor(ctx context.Context, snap WorkspaceSyncSnapshot, workspaceID string) (bool, error) {
	if snap.CommercialEntitlement || !snap.MemberActive || snap.WorkspaceRole != "admin" {
		return snap.CommercialEntitlement, nil
	}
	c, ok := capability[commercialReader](s)
	if !ok {
		return false, nil
	}
	return c.WorkspaceCommercialEntitlement(ctx, workspaceID)
}

func syncAccessFrom(snap WorkspaceSyncSnapshot, commercial bool, docIDs []string) WorkspaceSyncAccess {
	b := factsBundle{snap: snap}
	out := WorkspaceSyncAccess{
		CanSync:    snap.MemberActive && isMemberRole(snap.WorkspaceRole),
		Generation: snap.Generation,
		Documents:  make(map[string]DocumentSyncAccess, len(docIDs)),
	}
	for _, id := range docIDs {
		out.Documents[id] = b.doc(id, commercial).syncAccess()
	}
	return out
}

func (s *AuthorizationService) AuthorizeWorkspaceSync(ctx context.Context, userID, workspaceID string, docIDs []string) (WorkspaceSyncAccess, error) {
	reader, ok := capability[syncSnapshotReader](s)
	if !ok {
		return WorkspaceSyncAccess{}, ErrAuthorizationUnavailable
	}
	snap, err := reader.ReadWorkspaceSyncSnapshot(ctx, userID, workspaceID, docIDs)
	if err != nil {
		return WorkspaceSyncAccess{}, err
	}
	commercial, err := s.commercialFor(ctx, snap, workspaceID)
	if err != nil {
		return WorkspaceSyncAccess{}, err
	}
	return syncAccessFrom(snap, commercial, docIDs), nil
}

func (s *AuthorizationService) AuthorizeWorkspaceSyncInTx(ctx context.Context, tx pgx.Tx, userID, workspaceID string, docIDs []string) (WorkspaceSyncAccess, error) {
	reader, ok := capability[syncSnapshotTxReader](s)
	if !ok {
		return WorkspaceSyncAccess{}, ErrAuthorizationUnavailable
	}
	snap, err := reader.ReadWorkspaceSyncSnapshotInTx(ctx, tx, userID, workspaceID, docIDs)
	if err != nil {
		return WorkspaceSyncAccess{}, err
	}
	return syncAccessFrom(snap, snap.CommercialEntitlement, docIDs), nil
}

func (s *AuthorizationService) AuthorizeWorkspaceReadInTx(ctx context.Context, tx pgx.Tx, userID, workspaceID string) (bool, error) {
	reader, ok := capability[workspaceTxReader](s)
	if !ok {
		return false, ErrAuthorizationUnavailable
	}
	return reader.CanReadWorkspaceInTx(ctx, tx, userID, workspaceID)
}

func (s *AuthorizationService) AuthorizeDocument(ctx context.Context, userID, workspaceID, docID string) (DocumentAuthorization, error) {
	reader, ok := capability[syncSnapshotReader](s)
	if !ok {
		return DocumentAuthorization{}, ErrAuthorizationUnavailable
	}
	snap, err := reader.ReadWorkspaceSyncSnapshot(ctx, userID, workspaceID, []string{docID})
	if err != nil {
		return DocumentAuthorization{}, err
	}
	commercial, err := s.commercialFor(ctx, snap, workspaceID)
	if err != nil {
		return DocumentAuthorization{}, err
	}
	return factsBundle{snap: snap}.doc(docID, commercial).authorization(), nil
}

func (s *AuthorizationService) AuthorizeDocumentInTx(ctx context.Context, tx pgx.Tx, userID, workspaceID, docID string) (DocumentAuthorization, error) {
	reader, ok := capability[syncSnapshotTxReader](s)
	if !ok {
		return DocumentAuthorization{}, ErrAuthorizationUnavailable
	}
	snap, err := reader.ReadWorkspaceSyncSnapshotInTx(ctx, tx, userID, workspaceID, []string{docID})
	if err != nil {
		return DocumentAuthorization{}, err
	}
	return factsBundle{snap: snap}.doc(docID, snap.CommercialEntitlement).authorization(), nil
}

func (s *AuthorizationService) workspaceAccess(ctx context.Context, userID, workspaceID string, requireTeam bool) (WorkspaceAccess, error) {
	reader, ok := capability[accessFactsReader](s)
	if !ok {
		return WorkspaceAccess{}, ErrAuthorizationUnavailable
	}
	facts, err := reader.ReadWorkspaceAccessFacts(ctx, userID, workspaceID)
	if err != nil {
		return WorkspaceAccess{}, err
	}
	if !facts.WorkspaceExists {
		return WorkspaceAccess{}, ErrNotFound
	}
	access, ok := workspaceAccessFromFacts(facts)
	if !ok {
		return WorkspaceAccess{}, ErrAccessDenied
	}
	if requireTeam && !facts.TeamKnown {
		return WorkspaceAccess{}, ErrQuotaStateUnknown
	}
	return access, nil
}

func (s *AuthorizationService) GetWorkspaceAccess(ctx context.Context, userID, workspaceID string) (WorkspaceAccess, error) {
	return s.workspaceAccess(ctx, userID, workspaceID, true)
}

func (s *AuthorizationService) GetWorkspaceRolePermissions(ctx context.Context, userID, workspaceID string) (WorkspaceAccess, error) {
	return s.workspaceAccess(ctx, userID, workspaceID, false)
}

func (s *AuthorizationService) GetWorkspaceConfig(ctx context.Context, userID, workspaceID string) (WorkspaceConfig, error) {
	facts, ok := capability[accessFactsReader](s)
	cfg, ok2 := capability[configReader](s)
	if !ok || !ok2 {
		return WorkspaceConfig{}, ErrAuthorizationUnavailable
	}
	f, err := facts.ReadWorkspaceAccessFacts(ctx, userID, workspaceID)
	if err != nil {
		return WorkspaceConfig{}, err
	}
	if !f.WorkspaceExists {
		return WorkspaceConfig{}, ErrNotFound
	}
	if !f.MemberActive || !isMemberRole(f.WorkspaceRole) {
		return WorkspaceConfig{}, ErrAccessDenied
	}
	return cfg.ReadWorkspaceConfig(ctx, workspaceID)
}
