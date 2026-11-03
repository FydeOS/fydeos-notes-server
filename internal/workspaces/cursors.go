// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// decodeFlexibleJSON accepts raw JSON or any common base64 spelling of it.
func decodeFlexibleJSON(cursor string) ([]byte, bool) {
	trimmed := strings.TrimSpace(cursor)
	if strings.HasPrefix(trimmed, "{") {
		return []byte(trimmed), true
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err := enc.DecodeString(trimmed); err == nil {
			raw = bytes.TrimSpace(raw)
			if len(raw) > 0 && raw[0] == '{' {
				return raw, true
			}
		}
	}
	return nil, false
}

// parseFlexibleTime reads an RFC3339 string or Unix milliseconds.
func parseFlexibleTime(raw json.RawMessage) (time.Time, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		t, err := time.Parse(time.RFC3339Nano, s)
		return t, err == nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > 8.64e15 {
			return time.Time{}, false
		}
		return time.UnixMilli(int64(f)).UTC(), true
	}
	return time.Time{}, false
}

type memberCursor struct {
	LastAccessedAt string `json:"lastAccessedAt"`
	UserID         string `json:"userId"`
}

// EncodeDocMemberLastAccessCursor encodes a last-access page cursor.
func EncodeDocMemberLastAccessCursor(member DocMemberLastAccess) (string, error) {
	raw, err := json.Marshal(memberCursor{LastAccessedAt: member.LastAccessedAt.UTC().Format(time.RFC3339Nano), UserID: member.UserID})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

type decodedMemberCursor struct {
	at     time.Time
	userID string
}

func decodeMemberCursor(cursor *string) (*decodedMemberCursor, error) {
	if cursor == nil || strings.TrimSpace(*cursor) == "" {
		return nil, nil
	}
	raw, ok := decodeFlexibleJSON(*cursor)
	if !ok {
		return nil, ErrInvalidDocAnalyticsCursor
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, ErrInvalidDocAnalyticsCursor
	}
	var user string
	if err := json.Unmarshal(fields["userId"], &user); err != nil || user == "" {
		return nil, ErrInvalidDocAnalyticsCursor
	}
	at, ok := parseFlexibleTime(fields["lastAccessedAt"])
	if !ok {
		return nil, ErrInvalidDocAnalyticsCursor
	}
	return &decodedMemberCursor{at: at, userID: user}, nil
}

type sharedLinkCursor struct {
	order       AdminSharedLinksOrder
	date        time.Time
	number      int64
	workspaceID string
	docID       string
}

func encodeSharedLinkCursor(order AdminSharedLinksOrder, link AdminSharedLink) string {
	var sortValue any = link.SortValueDate.UTC().Format(time.RFC3339Nano)
	if order == AdminSharedLinksViewsDesc {
		sortValue = link.SortValueNumber
	}
	raw, _ := json.Marshal(map[string]any{"orderBy": order, "sortValue": sortValue, "workspaceId": link.WorkspaceID, "docId": link.DocID})
	return base64.StdEncoding.EncodeToString(raw)
}

// decodeSharedLinkCursor returns nil (no error) for a cursor of another
// order.
func decodeSharedLinkCursor(cursor *string, order AdminSharedLinksOrder) (*sharedLinkCursor, error) {
	if cursor == nil || strings.TrimSpace(*cursor) == "" {
		return nil, nil
	}
	raw, ok := decodeFlexibleJSON(*cursor)
	if !ok {
		return nil, ErrInvalidAdminSharedLinksCursor
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, ErrInvalidAdminSharedLinksCursor
	}
	var c sharedLinkCursor
	var orderName string
	if err := json.Unmarshal(fields["orderBy"], &orderName); err != nil {
		return nil, ErrInvalidAdminSharedLinksCursor
	}
	if AdminSharedLinksOrder(orderName) != order {
		return nil, nil
	}
	c.order = order
	if json.Unmarshal(fields["workspaceId"], &c.workspaceID) != nil || json.Unmarshal(fields["docId"], &c.docID) != nil || c.workspaceID == "" || c.docID == "" {
		return nil, ErrInvalidAdminSharedLinksCursor
	}
	sv := fields["sortValue"]
	if order == AdminSharedLinksViewsDesc {
		var f float64
		if err := json.Unmarshal(sv, &f); err != nil {
			var s string
			if err := json.Unmarshal(sv, &s); err != nil {
				return nil, ErrInvalidAdminSharedLinksCursor
			}
			if f, err = strconv.ParseFloat(strings.TrimSpace(s), 64); err != nil {
				return nil, ErrInvalidAdminSharedLinksCursor
			}
		}
		if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > 9e15 {
			return nil, ErrInvalidAdminSharedLinksCursor
		}
		c.number = int64(f)
		return &c, nil
	}
	t, ok := parseFlexibleTime(sv)
	if !ok {
		return nil, ErrInvalidAdminSharedLinksCursor
	}
	c.date = t
	return &c, nil
}
