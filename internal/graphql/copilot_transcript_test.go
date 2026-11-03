// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

func TestTranscriptTaskModelSettledProjection(t *testing.T) {
	task := &copilot.TranscriptTask{
		ID:              "task-1",
		Status:          "settled",
		ProtectedResult: []byte(`{"sourceAudio":{"blobId":"blob-1","mimeType":"audio/webm","durationMs":12000},"quality":{"degraded":false,"overflowCount":0},"sliceManifest":[{"index":0,"fileName":"part.webm","mimeType":"audio/webm","startSec":0,"durationSec":12,"byteSize":128}],"normalizedSegments":[{"speaker":"A","startSec":0,"endSec":1.5,"start":"00:00:00","end":"00:00:01","text":"Hello"}],"normalizedTranscript":"00:00:00 A: Hello","summaryJson":{"title":"Standup","durationMinutes":1.5,"attendees":["A"],"keyPoints":["Ship it"],"actionItems":[{"description":"Review","owner":"A","deadline":"Friday"}],"decisions":[],"openQuestions":[],"blockers":[]},"version":"v2"}`),
	}
	result, err := transcriptTaskModel(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "task-1" || result.Status != model.AiJobStatusFinished || result.SourceAudio == nil || result.SourceAudio.BlobID == nil || *result.SourceAudio.BlobID != "blob-1" {
		t.Fatalf("result header = %#v", result)
	}
	if result.Title == nil || *result.Title != "Standup" || result.Summary == nil || *result.Summary != "## Key Points\n- Ship it" || result.Actions == nil || *result.Actions != "- [ ] Review (A · Friday)" {
		t.Fatalf("legacy projection = title %v summary %v actions %v", result.Title, result.Summary, result.Actions)
	}
	if len(result.Transcription) != 1 || result.Transcription[0].Transcription != "Hello" || result.NormalizedTranscript == nil || *result.NormalizedTranscript != "00:00:00 A: Hello" {
		t.Fatalf("transcript projection = %#v", result)
	}
}

func TestTranscriptTaskModelHidesResultUntilSettledAndIgnoresMalformedPayload(t *testing.T) {
	for _, task := range []*copilot.TranscriptTask{
		{ID: "pending", Status: "pending", ProtectedResult: []byte(`{"version":"v2"}`)},
		{ID: "bad-json", Status: "settled", ProtectedResult: []byte(`{"summaryJson":`)},
	} {
		result, err := transcriptTaskModel(task)
		if err != nil {
			t.Fatalf("task %s: %v", task.ID, err)
		}
		if result.ID != task.ID || result.Title != nil || result.NormalizedSegments != nil || result.Transcription != nil {
			t.Fatalf("task %s exposed partial transcript: %#v", task.ID, result)
		}
	}
	ready, err := transcriptTaskModel(&copilot.TranscriptTask{ID: "ready", Status: "ready", ProtectedResult: []byte(`{"sourceAudio":{"blobId":"blob-1"},"normalizedTranscript":"not-yet-settled"}`)})
	if err != nil || ready.Status != model.AiJobStatusFinished || ready.SourceAudio == nil || ready.NormalizedTranscript != nil {
		t.Fatalf("ready task projection = %#v, err=%v", ready, err)
	}
}
