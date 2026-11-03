// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDocVisitorID(t *testing.T) {
	sum := sha256.Sum256([]byte("w:d:t"))
	if got := DocVisitorID("w", "d", "t"); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("DocVisitorID = %q", got)
	}
}

func TestHumanReadableQuota(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1536: "1.5 KB", 10 << 30: "10 GB", 1023: "1023 B"} {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
	limit := int32(10)
	q := FormatUserQuota(UserQuotaState{Plan: "pro", SeatLimit: 3, HistoryPeriodSeconds: 7 * 86400, CopilotActionLimit: &limit})
	if q.Name != "Pro" || q.HumanReadable.HistoryPeriod != "7 days" || q.HumanReadable.CopilotActionLimit != "10 times" || q.HumanReadable.MemberLimit != "3" {
		t.Fatalf("formatted quota = %+v", q)
	}
	q = FormatUserQuota(UserQuotaState{Plan: "other", CopilotActionLimit: &limit, UnlimitedCopilot: true})
	if q.Name != "Free" || q.CopilotActionLimit != nil || q.HumanReadable.CopilotActionLimit != "Unlimited" {
		t.Fatalf("unlimited quota = %+v", q)
	}
	for plan, want := range map[string]string{"selfhost_free": "Pro", "lifetime_pro": "Lifetime Pro", "ai": "AI", "selfhost_team": "Team", "": "Free"} {
		if got := planDisplayName(plan); got != want {
			t.Errorf("plan %q = %q", plan, got)
		}
	}
}

func memberFacts(role string) docFacts {
	return docFacts{workspaceExists: true, memberRole: role, memberActive: true, sharing: true, wsDefault: "manager", present: true}
}

func TestDocumentRoleResolution(t *testing.T) {
	f := memberFacts("member")
	f.acl.MemberDefaultRole = "reader"
	a := f.authorization()
	if !a.Read || !a.HistoryRead || !a.UsersRead || a.Update {
		t.Fatalf("reader default = %+v", a)
	}
	f.acl.ExplicitUserRole = "editor"
	if !f.authorization().Update {
		t.Fatal("explicit editor grant did not override default")
	}

	ext := docFacts{workspaceExists: true, sharing: true, present: true, acl: DocumentACL{ExplicitUserRole: "manager"}}
	if a := ext.authorization(); !a.Update || a.Manage || a.HistoryRead {
		t.Fatalf("external manager grant must cap at editor: %+v", a)
	}
	ext.sharing = false
	if ext.authorization().Read {
		t.Fatal("grant counted with sharing disabled")
	}

	public := docFacts{workspaceExists: true, sharing: true, present: true, acl: DocumentACL{VisibilityPublic: true, Published: true, PublicRole: "external"}}
	if !public.authorization().Read {
		t.Fatal("anonymous cannot read published doc")
	}
	public.acl.Published = false
	if public.authorization().Read {
		t.Fatal("unpublished page readable")
	}

	missing := memberFacts("owner")
	missing.present = false
	if missing.authorization() != (DocumentAuthorization{}) {
		t.Fatal("doc outside snapshot must be all-false")
	}

	admin := memberFacts("admin")
	admin.acl.MemberDefaultRole = "none"
	if admin.effectiveRole() != roleNone {
		t.Fatal("non-commercial admin got an implicit role")
	}
	admin.commercial = true
	if admin.effectiveRole() != roleManager {
		t.Fatal("commercial admin is not manager")
	}
}

func TestDocumentPermissionMap(t *testing.T) {
	f := memberFacts("owner")
	p := f.permissions(true)
	if len(p) != 21 {
		t.Fatalf("permission keys = %d", len(p))
	}
	if p["Doc.Analytics.Read"] || p["Doc.Analytics.Viewers.Read"] || !p["Doc.TransferOwner"] || !p["Doc.Publish"] {
		t.Fatalf("owner without commercial = %v", p)
	}
	f.commercial = true
	if p := f.permissions(true); !p["Doc.Analytics.Read"] || !p["Doc.Analytics.Viewers.Read"] {
		t.Fatalf("owner with commercial = %v", p)
	}
	f.sharing = false
	if p := f.permissions(true); p["Doc.Publish"] || p["Doc.Preview"] {
		t.Fatalf("sharing disabled = %v", p)
	}
	f.sharing, f.wsURLPreview = true, true
	if !f.permissions(true)["Doc.Preview"] || f.permissions(false)["Doc.Preview"] {
		t.Fatal("preview flag")
	}
}

func TestWorkspaceAccessMatrix(t *testing.T) {
	owner, ok := workspaceAccessFromFacts(WorkspaceAccessFacts{WorkspaceExists: true, WorkspaceRole: "owner", MemberActive: true, SharingEnabled: true})
	if !ok || owner.Role != "Owner" || len(owner.Permissions) != 20 {
		t.Fatalf("owner access = %+v", owner)
	}
	for k, v := range owner.Permissions {
		want := k != "Workspace_Administrators_Manage" && k != "Workspace_Preview"
		if v != want {
			t.Errorf("owner %s = %v", k, v)
		}
	}
	admin, _ := workspaceAccessFromFacts(WorkspaceAccessFacts{WorkspaceExists: true, WorkspaceRole: "admin", MemberActive: true})
	if admin.Role != "Collaborator" || admin.Permissions["Workspace_Users_Manage"] {
		t.Fatalf("non-commercial admin = %+v", admin)
	}
	admin, _ = workspaceAccessFromFacts(WorkspaceAccessFacts{WorkspaceExists: true, WorkspaceRole: "admin", MemberActive: true, CommercialEntitlement: true})
	if admin.Role != "Admin" || !admin.Permissions["Workspace_Administrators_Manage"] {
		t.Fatalf("commercial admin = %+v", admin)
	}
	ext, ok := workspaceAccessFromFacts(WorkspaceAccessFacts{WorkspaceExists: true, WorkspacePublic: true, SharingEnabled: true})
	if !ok || ext.Role != "External" || !ext.Permissions["Workspace_Read"] || ext.Permissions["Workspace_Sync"] || !ext.Permissions["Workspace_Preview"] {
		t.Fatalf("external = %+v", ext)
	}
	if _, ok := workspaceAccessFromFacts(WorkspaceAccessFacts{WorkspaceExists: true}); ok {
		t.Fatal("private workspace visible to stranger")
	}
}

type factsOnly struct {
	facts WorkspaceAccessFacts
}

func (f factsOnly) HasCommonActiveWorkspace(context.Context, string, string) (bool, error) {
	return false, nil
}

func (f factsOnly) ReadWorkspaceAccessFacts(context.Context, string, string) (WorkspaceAccessFacts, error) {
	return f.facts, nil
}

func TestAuthorizationServiceCapabilities(t *testing.T) {
	ctx := context.Background()
	svc := NewAuthorizationService(factsOnly{facts: WorkspaceAccessFacts{WorkspaceExists: true, WorkspaceRole: "member", MemberActive: true}})
	if _, err := svc.AuthorizeWorkspaceSync(ctx, "u", "w", nil); !errors.Is(err, ErrAuthorizationUnavailable) {
		t.Fatalf("missing capability = %v", err)
	}
	if ok, err := svc.CanViewUser(ctx, "x", "x"); !ok || err != nil {
		t.Fatalf("self view = %v %v", ok, err)
	}
	if _, err := svc.GetWorkspaceAccess(ctx, "u", "w"); !errors.Is(err, ErrQuotaStateUnknown) {
		t.Fatalf("unknown team = %v", err)
	}
	if a, err := svc.GetWorkspaceRolePermissions(ctx, "u", "w"); err != nil || a.Role != "Collaborator" {
		t.Fatalf("role permissions = %+v %v", a, err)
	}
	var nilSvc *AuthorizationService
	if _, err := nilSvc.AuthorizeDocument(ctx, "u", "w", "d"); !errors.Is(err, ErrAuthorizationUnavailable) {
		t.Fatalf("nil service = %v", err)
	}
}

func TestCursors(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 6e6, time.UTC)
	enc := encodeGrantCursor(at)
	raw, _ := base64.StdEncoding.DecodeString(enc)
	if string(raw) != "2026-01-02T03:04:05.006Z" {
		t.Fatalf("grant cursor = %s", raw)
	}
	if got, err := DecodeDocGrantCursor(enc); err != nil || !got.Equal(at) {
		t.Fatalf("decode grant cursor = %v %v", got, err)
	}
	if got, err := DecodeDocGrantCursor(""); got != nil || err != nil {
		t.Fatal("empty grant cursor")
	}
	if _, err := DecodeDocGrantCursor("%%%"); err == nil {
		t.Fatal("bad grant cursor accepted")
	}

	c, _ := EncodeDocMemberLastAccessCursor(DocMemberLastAccess{UserID: "u1", LastAccessedAt: at})
	d, err := decodeMemberCursor(&c)
	if err != nil || d.userID != "u1" || !d.at.Equal(at) {
		t.Fatalf("member cursor = %+v %v", d, err)
	}
	rawJSON := `{"lastAccessedAt":1767323045006,"userId":"u2"}`
	if d, err := decodeMemberCursor(&rawJSON); err != nil || !d.at.Equal(at) {
		t.Fatalf("ms cursor = %+v %v", d, err)
	}
	urlForm := base64.RawURLEncoding.EncodeToString([]byte(rawJSON))
	if _, err := decodeMemberCursor(&urlForm); err != nil {
		t.Fatalf("url base64 cursor: %v", err)
	}
	for _, bad := range []string{"garbage", `{"lastAccessedAt":"x","userId":"u"}`, `{"lastAccessedAt":1,"userId":""}`} {
		if _, err := decodeMemberCursor(&bad); !errors.Is(err, ErrInvalidDocAnalyticsCursor) {
			t.Errorf("cursor %q = %v", bad, err)
		}
	}
	blank := "  "
	if d, err := decodeMemberCursor(&blank); d != nil || err != nil {
		t.Fatal("blank cursor")
	}

	link := AdminSharedLink{WorkspaceID: "w", DocID: "d", SortValueNumber: 42}
	views := encodeSharedLinkCursor(AdminSharedLinksViewsDesc, link)
	raw, _ = base64.StdEncoding.DecodeString(views)
	if !strings.Contains(string(raw), `"sortValue":42`) {
		t.Fatalf("views cursor = %s", raw)
	}
	if sc, err := decodeSharedLinkCursor(&views, AdminSharedLinksViewsDesc); err != nil || sc.number != 42 {
		t.Fatalf("decode views cursor = %+v %v", sc, err)
	}
	if sc, err := decodeSharedLinkCursor(&views, AdminSharedLinksUpdatedAtDesc); sc != nil || err != nil {
		t.Fatal("cursor of another order must be ignored")
	}
	str := `{"orderBy":"ViewsDesc","sortValue":"17","workspaceId":"w","docId":"d"}`
	if sc, err := decodeSharedLinkCursor(&str, AdminSharedLinksViewsDesc); err != nil || sc.number != 17 {
		t.Fatalf("numeric string = %+v %v", sc, err)
	}
	noID := `{"orderBy":"ViewsDesc","sortValue":1,"workspaceId":"w"}`
	if _, err := decodeSharedLinkCursor(&noID, AdminSharedLinksViewsDesc); !errors.Is(err, ErrInvalidAdminSharedLinksCursor) {
		t.Fatalf("missing doc id = %v", err)
	}
	ms := `{"orderBy":"UpdatedAtDesc","sortValue":1767323045006,"workspaceId":"w","docId":"d"}`
	if sc, err := decodeSharedLinkCursor(&ms, AdminSharedLinksUpdatedAtDesc); err != nil || !sc.date.Equal(at) {
		t.Fatalf("ms date cursor = %+v %v", sc, err)
	}
}

func TestDocRoleInputLimits(t *testing.T) {
	ctx := context.Background()
	svc := NewDocumentRoleService(&Repository{}, nil)
	if err := svc.GrantDocRoles(ctx, "a", "w", "d", make([]string, MaxDocRoleTargets+1), "reader"); !errors.Is(err, ErrDocRoleInputLimit) {
		t.Fatalf("too many targets = %v", err)
	}
	if err := svc.GrantDocRoles(ctx, "a", "w", "d", []string{strings.Repeat("x", MaxDocRoleIdentifierBytes+1)}, "reader"); !errors.Is(err, ErrDocRoleInputLimit) {
		t.Fatalf("long id = %v", err)
	}
	if err := svc.GrantDocRoles(ctx, "a", "w", "d", []string{"b"}, "owner"); !errors.Is(err, ErrDocRoleInvalid) {
		t.Fatalf("owner grant = %v", err)
	}
	if err := svc.GrantDocRoles(ctx, "a", "w", "w", []string{"b"}, "reader"); !errors.Is(err, ErrDocRoleInvalid) {
		t.Fatalf("doc == workspace = %v", err)
	}
	if err := svc.SetDocDefaultRole(ctx, "a", "w", "d", "owner"); !errors.Is(err, ErrDocRoleInvalid) {
		t.Fatalf("owner default = %v", err)
	}
	pub := NewDocumentPublicationService(&Repository{}, nil)
	if _, err := pub.SetDocPublished(ctx, "a", "w", "d", 2, true); !errors.Is(err, ErrDocRoleInvalid) {
		t.Fatalf("mode 2 = %v", err)
	}
}

func TestInvitationSpaceError(t *testing.T) {
	err := error(&InvitationSpaceError{Cause: ErrInvitationAlreadyMember, WorkspaceID: "w"})
	if !errors.Is(err, ErrInvitationAlreadyMember) || err.Error() != ErrInvitationAlreadyMember.Error() {
		t.Fatal("InvitationSpaceError does not wrap its cause")
	}
}
