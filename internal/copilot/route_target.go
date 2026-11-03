// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"strconv"
	"strings"
)

func parseBYOKRouteTarget(target, leaseID string) (profileID, modelID string, local, ok bool) {
	const prefix = "fydeos-byok:"
	if !strings.HasPrefix(target, prefix) {
		return "", "", false, false
	}
	raw := strings.TrimPrefix(target, prefix)
	if leaseID != "" && strings.HasPrefix(raw, leaseID+":") {
		localTarget := strings.TrimPrefix(raw, leaseID+":")
		separator := strings.IndexByte(localTarget, ':')
		if separator <= 0 || separator == len(localTarget)-1 {
			return "", "", false, false
		}
		if _, err := strconv.ParseUint(localTarget[:separator], 10, 32); err != nil {
			return "", "", false, false
		}
		return leaseID + ":" + localTarget[:separator], localTarget[separator+1:], true, true
	}
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false, false
	}
	return parts[0], parts[1], false, true
}
