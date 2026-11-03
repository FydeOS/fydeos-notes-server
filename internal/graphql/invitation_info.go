// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

const defaultWorkspaceAvatarBase64 = "iVBORw0KGgoAAAANSUhEUgAAABgAAAAYCAYAAADgdz34AAAACXBIWXMAAAsTAAALEwEAmpwYAAAAAXNSR0IArs4c6QAAAARnQU1BAACxjwv8YQUAAAQtSURBVHgBfVa9jhxFEK6q7rkf+4T2AgdIIC0ZoXkBuNQJtngBuIzs1hIRye1FhL438D0CRgKRGUeE6wwkhHYlkE2AtGdkbN/MdJe/qu7Z27PWnnG5Znq7v/rqd47pHddkNh/918tR1/FBamXc9zxOPVFKfJ4yP86qD1LD3/986/3F2zB40+LXv83HrHq/6+gAoNS1kF4odUz2nhJRTkI5E6mD6Bk1crLJkLy5cHc+P4ohzxLng8RKLqKUq6hkUtBSe8Zvdmfir7TT2a0fnkzeaeCbv/44ztSfZskjP2ygVRM0mbYTpgHMMMS8CsIIj/c+//Hp8UYD3z758whQUwdeEwPjAZQLqJhI0VxB2MVco+kXP/0zuZKD6dP5uM397ELzqEtMba/UJ4t7iXeq8U94z52Q+js09qjlIXMxAEsRDJpI59dVPzlDTooHko7BdlR2FcYmAtbGMmAt2mFI4yDQkIjtEQkxUAMKAPD9SiOK4b578N0S7Nt+fqFKbTbmRD1YGXurEmdtnjjz4kFuIV0gtWewV62hMHBY2gpEOw3Rnmztx9jnO72xzTV/YkzgNmgkiypeYJdCLjonqyAAg7VCshVpjTbD08HbxrySdhKxcDvoJTA5gLvpeXVQ+K340WKea9UkNeZVqGSba/IbF6athj+LUeRmRCyiAVnlAKhJJQfmugGZ28ZWna24RGzwNUNUqpWGf6HkajvAgNA4NsSjHgcb9obx+k5c3DUttcwd3NcHxpVurXQ2d4MZACGw9TwEHsdtbEwytL1xywAGcxavjoH1quLVywuGi+aBhFWexRilFSwK0QzgdUdkkVMeKw4wijrgxjzz2CefCRZn+21ViOWW4Ym9nNnyFLMbMS8ivNhGP8RdlgUojBkuBLDpEPi+5LpWiDURgFkKOIIckJTgN/sZ84KtKkKpDnsOZiTQ47jD4ZGwHghbw6AXIL3lo5Zg6Tp2AwIAyYJ8BRzGfmfPl6kI7HOLUdN2LIg+4IfL5SiFdvkK4blI6h50qda7jQI0CUMLdEhFIkqtQciMvXsgpaZ1pWtVUfrIa+TX5/8+RBcftAhTa91r8ycXA5ZxBqhAh2zgVagUAddxMkxfF/JxfvbpB+8d2jhBtsPhtuqsE0HJlhxYeHKdkCU8xUCos8dmkDdnGaOlJ1yy9dM52J2spqldvz9fTgB4z+aQd2kqjUY2KU2s4dTT7ezD0AqDAbvZiKF/VO9+fGPv9IoBu+b/P5ti6djDY+JlSg4ug1jc6fJbMAx9/3b4CNGTD/evT698D9avv188m4gKvko8MiMeJC3jmOvU9MSuHXZohAVpOrmxd+10HW/jR3/58uU45TRFt35ZR2XpY61DzW+tH3z/7xdM8sP93d3Fm1gbDawbEtU7CMtt/JVxEw01Kh7RAmoBE4+u7eycYv38bRivAZbdHBtPrwOHAAAAAElFTkSuQmCC"

const maxWorkspaceAvatarBytes int64 = 10 << 20

func (r *queryResolver) invitationInfo(ctx context.Context, inviteID string) (*model.InvitationType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	repository := r.InviteInfoRepository
	if repository == nil || r.UserService == nil {
		return nil, ErrWorkspaceInvitationInfoUnavailable
	}

	var record workspaces.InvitationRecord
	var status *string
	isLink := false
	if r.InviteLinkLookup != nil {
		target, err := r.InviteLinkLookup.LookupByID(ctx, inviteID)
		if err != nil {
			return nil, err
		}
		if target != nil {
			isLink = true
			record = workspaces.InvitationRecord{WorkspaceID: target.WorkspaceID, InviteeID: principal.User.ID}
			status, err = repository.InvitationStatus(ctx, record.WorkspaceID, record.InviteeID)
			if err != nil {
				return nil, err
			}
		}
	}
	if !isLink {
		var err error
		record, err = repository.InvitationByID(ctx, inviteID)
		if err != nil {
			return nil, err
		}
		if record.InviteeID != principal.User.ID {
			return nil, workspaces.ErrInvitationAccountMismatch
		}
		status = record.Status
	}

	workspace, err := repository.InvitationWorkspace(ctx, record.WorkspaceID)
	if err != nil {
		return nil, err
	}
	owner, err := r.UserService.Get(ctx, workspace.OwnerID)
	if errors.Is(err, users.ErrNotFound) || err == nil && owner == nil {
		return nil, users.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	invitee, err := r.UserService.Get(ctx, record.InviteeID)
	if errors.Is(err, users.ErrNotFound) || err == nil && invitee == nil {
		return nil, users.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	var invitationStatus *model.WorkspaceMemberStatus
	if status != nil {
		mapped := model.WorkspaceMemberStatus(*status)
		invitationStatus = &mapped
	}
	return &model.InvitationType{
		Invitee:   invitationWorkspaceUser(invitee),
		Status:    invitationStatus,
		User:      invitationWorkspaceUser(owner),
		Workspace: &model.InvitationWorkspaceType{ID: workspace.ID, Name: workspace.Name, Avatar: r.invitationWorkspaceAvatar(ctx, workspace)},
	}, nil
}

func invitationWorkspaceUser(user *users.User) *model.WorkspaceUserType {
	return &model.WorkspaceUserType{ID: user.ID, Name: user.Name, Email: user.Email, AvatarURL: user.AvatarURL}
}

func (r *queryResolver) invitationWorkspaceAvatar(ctx context.Context, workspace workspaces.InvitationWorkspace) string {
	if workspace.AvatarKey == nil || *workspace.AvatarKey == "" || r.WorkspaceAvatarReader == nil {
		return defaultWorkspaceAvatarBase64
	}
	objectKey := fmt.Sprintf("%s/%s", workspace.ID, *workspace.AvatarKey)
	if err := storage.ValidateObjectKey(objectKey); err != nil {
		return defaultWorkspaceAvatarBase64
	}
	body, _, err := r.WorkspaceAvatarReader.GetObject(ctx, storage.BlobScope, objectKey)
	if err != nil || body == nil {
		return defaultWorkspaceAvatarBase64
	}
	data, readErr := io.ReadAll(io.LimitReader(body, maxWorkspaceAvatarBytes+1))
	closeErr := body.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) > maxWorkspaceAvatarBytes {
		return defaultWorkspaceAvatarBase64
	}
	return base64.StdEncoding.EncodeToString(data)
}
