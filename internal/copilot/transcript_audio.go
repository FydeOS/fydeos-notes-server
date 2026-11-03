// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
)

// Legacy uploads use a relative blob-ID/index key; Go uploads use a task-owned
// key. Resolve both from the task's database identity, never sourceAudio.blobId
// or an arbitrary remote URL. Expired signatures do not affect local reads.
func transcriptObjectKey(task TranscriptExecutionTask, info transcriptAudioInfo, position int) (string, error) {
	for _, part := range []string{task.UserID, task.WorkspaceID, task.ID} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\x00") {
			return "", errTranscriptInput
		}
	}
	scope := task.UserID + "/" + task.WorkspaceID + "/"
	current := scope + "transcripts/" + task.ID + "/" + strconv.Itoa(position)
	if info.Key == current {
		return current, nil
	}
	if task.BlobID == "" || strings.ContainsAny(task.BlobID, "/\\\x00") {
		return "", errTranscriptInput
	}
	legacy := task.BlobID + "-" + strconv.Itoa(position)
	if info.Key != "" {
		if info.Key != legacy {
			return "", errTranscriptInput
		}
		return scope + legacy, nil
	}
	if strings.HasPrefix(info.URL, "data:") {
		header, encoded, ok := strings.Cut(info.URL, ",")
		if !ok || len(header) > 256 || !strings.HasSuffix(header, ";base64") || int64(len(encoded)) > ((maxTranscriptUploadBytes+2)/3)*4 {
			return "", errTranscriptInput
		}
		return "", nil
	}
	parsed, err := url.Parse(info.URL)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Path != "/api/copilot/blob/"+scope+legacy {
		return "", errTranscriptInput
	}
	return scope + legacy, nil
}

func (e *TranscriptExecutor) openAudio(ctx context.Context, key string, info transcriptAudioInfo) (io.ReadCloser, error) {
	if key != "" {
		reader, _, err := e.objects.GetObject(ctx, storage.CopilotScope, key)
		return reader, err
	}
	_, encoded, ok := strings.Cut(info.URL, ",")
	if !ok {
		return nil, errTranscriptInput
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || int64(len(data)) > maxTranscriptUploadBytes {
		return nil, errTranscriptInput
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
