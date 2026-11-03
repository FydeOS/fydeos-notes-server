// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

// docRole is a position on the document role ladder.
type docRole int

const (
	roleNone docRole = iota
	roleExternal
	roleReader
	roleCommenter
	roleEditor
	roleManager
	roleOwner
)

var docRoleNames = [...]string{"none", "external", "reader", "commenter", "editor", "manager", "owner"}

func (r docRole) String() string { return docRoleNames[r] }

// parseDocRole maps a stored role name; unknown names are "none".
func parseDocRole(s string) docRole {
	for i, name := range docRoleNames {
		if name == s {
			return docRole(i)
		}
	}
	return roleNone
}

func validDocRoleName(s string) bool {
	for _, name := range docRoleNames {
		if name == s {
			return true
		}
	}
	return false
}

func maxRole(a, b docRole) docRole {
	if a > b {
		return a
	}
	return b
}

func displayDocRole(stored *string) string {
	if stored == nil {
		return "Manager"
	}
	switch *stored {
	case "none":
		return "None"
	case "reader":
		return "Reader"
	case "commenter":
		return "Commenter"
	case "editor":
		return "Editor"
	case "manager":
		return "Manager"
	case "owner":
		return "Owner"
	}
	return "Manager"
}

func workspaceRank(role string) int {
	switch role {
	case "owner":
		return 3
	case "admin":
		return 2
	case "member":
		return 1
	}
	return 0
}

func isMemberRole(role string) bool { return workspaceRank(role) > 0 }

// docFacts is everything needed to evaluate one user's access to one doc.
type docFacts struct {
	workspaceExists bool
	memberRole      string // stored role when the membership is active
	memberActive    bool
	sharing         bool
	wsDefault       string
	wsURLPreview    bool
	commercial      bool
	present         bool // the doc is part of the evaluated snapshot
	acl             DocumentACL
	docURLPreview   bool
	pageExists      bool
}

func (f docFacts) activeMember() bool { return f.memberActive && isMemberRole(f.memberRole) }

func (f docFacts) externalRole() docRole {
	if !f.sharing {
		return roleNone
	}
	role := roleNone
	if explicit := parseDocRole(f.acl.ExplicitUserRole); explicit > roleNone {
		if explicit > roleEditor {
			explicit = roleEditor
		}
		role = explicit
	}
	if f.acl.VisibilityPublic && f.acl.Published {
		role = maxRole(role, parseDocRole(f.acl.PublicRole))
	}
	return role
}

func (f docFacts) effectiveRole() docRole {
	if !f.present || !f.workspaceExists {
		return roleNone
	}
	external := f.externalRole()
	if !f.activeMember() {
		return external
	}
	role := roleNone
	switch {
	case f.memberRole == "owner":
		role = roleOwner
	case f.memberRole == "admin" && f.commercial:
		role = roleManager
	}
	if explicit := parseDocRole(f.acl.ExplicitUserRole); explicit > roleNone {
		role = maxRole(role, explicit)
	} else {
		def := f.acl.MemberDefaultRole
		if def == "" {
			def = f.wsDefault
		}
		if def == "" {
			def = "manager"
		}
		role = maxRole(role, parseDocRole(def))
	}
	return maxRole(role, external)
}

func (f docFacts) authorization() DocumentAuthorization {
	role := f.effectiveRole()
	return DocumentAuthorization{
		Read:             role >= roleExternal,
		HistoryRead:      role >= roleReader && f.activeMember(),
		UsersRead:        role >= roleReader,
		CommentsCreate:   role >= roleCommenter,
		CommentsModerate: role >= roleEditor,
		Update:           role >= roleEditor,
		Manage:           role >= roleManager,
	}
}

func (f docFacts) syncAccess() DocumentSyncAccess {
	role := f.effectiveRole()
	return DocumentSyncAccess{Read: role >= roleExternal, Update: role >= roleEditor, Manage: role >= roleManager}
}

var docPermissionLadder = []struct {
	key  string
	role docRole
}{
	{"Doc.Read", roleExternal}, {"Doc.Copy", roleExternal}, {"Doc.Properties.Read", roleExternal}, {"Doc.Comments.Read", roleExternal},
	{"Doc.Users.Read", roleReader}, {"Doc.Duplicate", roleReader}, {"Doc.History.Read", roleReader},
	{"Doc.Comments.Create", roleCommenter},
	{"Doc.Properties.Update", roleEditor}, {"Doc.Update", roleEditor}, {"Doc.Analytics.Read", roleEditor}, {"Doc.Comments.Moderate", roleEditor},
	{"Doc.Publish", roleManager}, {"Doc.Unpublish", roleManager}, {"Doc.Trash", roleManager}, {"Doc.Restore", roleManager}, {"Doc.Delete", roleManager}, {"Doc.Users.Manage", roleManager},
	{"Doc.TransferOwner", roleOwner},
}

// permissions renders the 21-key document permission map.
func (f docFacts) permissions(withPreview bool) map[string]bool {
	role := f.effectiveRole()
	out := make(map[string]bool, 21)
	for _, p := range docPermissionLadder {
		out[p.key] = role >= p.role
	}
	active := f.activeMember()
	out["Doc.History.Read"] = out["Doc.History.Read"] && active
	out["Doc.Analytics.Read"] = out["Doc.Analytics.Read"] && active && f.commercial
	out["Doc.Analytics.Viewers.Read"] = active && (f.memberRole == "owner" || f.memberRole == "admin") && f.commercial && out["Doc.Read"]
	out["Doc.Publish"] = out["Doc.Publish"] && f.sharing
	preview := false
	if withPreview && f.present && f.workspaceExists && f.sharing {
		preview = f.wsURLPreview || f.docURLPreview ||
			(f.acl.VisibilityPublic && f.acl.Published && f.acl.PublicRole == "external")
	}
	out["Doc.Preview"] = preview
	return out
}

var workspacePermissionLadder = []struct {
	key  string
	rank int
}{
	{"Workspace_Read", 0}, {"Workspace_Organize_Read", 0}, {"Workspace_Properties_Read", 0},
	{"Workspace_Sync", 1}, {"Workspace_CreateDoc", 1}, {"Workspace_Users_Read", 1}, {"Workspace_Settings_Read", 1}, {"Workspace_Blobs_Upload", 1}, {"Workspace_Copilot", 1},
	{"Workspace_Users_Manage", 2}, {"Workspace_Administrators_Manage", 2}, {"Workspace_Settings_Update", 2},
	{"Workspace_Properties_Create", 2}, {"Workspace_Properties_Update", 2}, {"Workspace_Properties_Delete", 2}, {"Workspace_Blobs_Manage", 2},
	{"Workspace_Delete", 3}, {"Workspace_TransferOwner", 3}, {"Workspace_Payment_Manage", 3},
}

// workspaceAccessFromFacts computes the role name and permission map; ok is
// false when the user may not see the workspace at all.
func workspaceAccessFromFacts(f WorkspaceAccessFacts) (WorkspaceAccess, bool) {
	member := f.MemberActive && isMemberRole(f.WorkspaceRole)
	publicShared := f.WorkspacePublic && f.SharingEnabled
	if !member && !publicShared {
		return WorkspaceAccess{}, false
	}
	rank, name := 0, "External"
	if member {
		switch f.WorkspaceRole {
		case "owner":
			rank, name = 3, "Owner"
		case "admin":
			if f.CommercialEntitlement {
				rank, name = 2, "Admin"
			} else {
				rank, name = 1, "Collaborator"
			}
		default:
			rank, name = 1, "Collaborator"
		}
	}
	perms := make(map[string]bool, 20)
	for _, p := range workspacePermissionLadder {
		perms[p.key] = rank >= p.rank
	}
	if !f.CommercialEntitlement {
		perms["Workspace_Administrators_Manage"] = false
	}
	perms["Workspace_Preview"] = f.SharingEnabled && (f.WorkspacePublic || f.URLPreviewEnabled)
	return WorkspaceAccess{Role: name, Permissions: perms, Team: f.Team}, perms["Workspace_Read"]
}
