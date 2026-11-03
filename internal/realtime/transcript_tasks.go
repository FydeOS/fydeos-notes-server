// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"encoding/json"
	"time"
)

func (s *Server) PublishTranscriptTaskChanged(workspaceID, taskID, status, errorCode string) {
	if s == nil || workspaceID == "" || taskID == "" {
		return
	}
	inputKey, err := json.Marshal(map[string]string{"workspaceId": workspaceID, "taskId": taskID})
	if err != nil {
		return
	}
	event := map[string]any{"taskId": taskID, "status": status}
	if errorCode != "" {
		event["error"] = errorCode
	}
	s.broadcastRoom("copilot:transcript:"+workspaceID+":"+taskID, "realtime:event", map[string]any{
		"topic": "copilot.transcript.task.changed", "inputKey": string(inputKey),
		"sentAt": time.Now().UnixMilli(), "event": event,
	}, "")
}
