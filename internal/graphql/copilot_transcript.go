// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/99designs/gqlgen/graphql"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

type transcriptPayload struct {
	SourceAudio          *model.TranscriptionSourceAudioType      `json:"sourceAudio"`
	Quality              *model.TranscriptionQualityType          `json:"quality"`
	SliceManifest        []*model.AudioSliceManifestItemType      `json:"sliceManifest"`
	NormalizedSegments   []*model.NormalizedTranscriptSegmentType `json:"normalizedSegments"`
	NormalizedTranscript *string                                  `json:"normalizedTranscript"`
	SummaryJSON          *model.MeetingSummaryV2Type              `json:"summaryJson"`
	Version              *string                                  `json:"version"`
}

func (r *copilotResolver) transcriptTask(ctx context.Context, parent *model.Copilot, blobID, taskID *string) (*model.TranscriptionResultType, error) {
	if parent == nil || parent.WorkspaceID == nil || (emptyTranscriptSelector(taskID) && emptyTranscriptSelector(blobID)) {
		return nil, nil
	}
	principal, personal, err := r.authorizeCopilotScope(ctx, *parent.WorkspaceID, nil, "")
	if err != nil {
		return nil, err
	}
	if r.CopilotTranscriptTasks == nil {
		return nil, ErrCopilotTranscriptTasksUnavailable
	}
	task, err := r.CopilotTranscriptTasks.GetForUser(ctx, principal.User.ID, *parent.WorkspaceID, taskID, blobID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		if personal {
			if _, err := r.authorizeWorkspaceEmbedding(ctx, *parent.WorkspaceID, "Workspace_Copilot"); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	return transcriptTaskModel(task)
}

func (r *Resolver) settleTranscriptTask(ctx context.Context, taskID, workspaceID string) (*model.TranscriptionResultType, error) {
	principal, personal, err := r.authorizeCopilotScope(ctx, workspaceID, nil, "")
	if err != nil {
		return nil, err
	}
	if r.CopilotTranscriptTasks == nil {
		return nil, ErrCopilotTranscriptTasksUnavailable
	}
	id := taskID
	task, err := r.CopilotTranscriptTasks.GetForUser(ctx, principal.User.ID, workspaceID, &id, nil)
	if err != nil {
		return nil, err
	}
	if task == nil {
		if personal {
			if _, err := r.authorizeWorkspaceEmbedding(ctx, workspaceID, "Workspace_Copilot"); err != nil {
				return nil, err
			}
		}
		return nil, ErrTranscriptTaskNotFound
	}
	if task.Status == "failed" {
		return nil, ErrTranscriptTaskFailed
	}
	if task.Status != "ready" && task.Status != "settled" {
		return nil, nil
	}
	if task.Status == "ready" {
		task, err = r.CopilotTranscriptTasks.SettleForUser(ctx, principal.User.ID, workspaceID, taskID)
		if err != nil || task == nil {
			return nil, err
		}
	}
	return transcriptTaskModel(task)
}

func emptyTranscriptSelector(value *string) bool {
	return value == nil || *value == ""
}

func transcriptTaskModel(task *copilot.TranscriptTask) (*model.TranscriptionResultType, error) {
	result := &model.TranscriptionResultType{ID: task.ID, Status: transcriptStatus(task.Status)}
	if task.Status != "ready" && task.Status != "settled" {
		return result, nil
	}
	if len(task.ProtectedResult) == 0 || string(task.ProtectedResult) == "null" {
		return result, nil
	}
	var payload transcriptPayload
	if err := json.Unmarshal(task.ProtectedResult, &payload); err != nil {
		return result, nil
	}
	result.SourceAudio = payload.SourceAudio
	result.Quality = payload.Quality
	result.SliceManifest = payload.SliceManifest
	if task.Status != "settled" {
		return result, nil
	}
	result.NormalizedSegments = payload.NormalizedSegments
	result.NormalizedTranscript = payload.NormalizedTranscript
	result.SummaryJSON = payload.SummaryJSON
	result.Version = payload.Version
	if payload.SummaryJSON != nil {
		result.Title = transcriptString(payload.SummaryJSON.Title)
		result.Summary = transcriptSummaryMarkdown(payload.SummaryJSON)
		result.Actions = transcriptActionMarkdown(payload.SummaryJSON)
	}
	for _, segment := range payload.NormalizedSegments {
		if segment != nil {
			result.Transcription = append(result.Transcription, &model.TranscriptionItemType{
				Speaker: segment.Speaker, Start: segment.Start, End: segment.End, Transcription: segment.Text,
			})
		}
	}
	return result, nil
}

func transcriptStatus(status string) model.AiJobStatus {
	switch status {
	case "pending":
		return model.AiJobStatusPending
	case "running":
		return model.AiJobStatusRunning
	case "ready", "settled":
		return model.AiJobStatusFinished
	default:
		return model.AiJobStatusFailed
	}
}

func transcriptString(value string) *string {
	return &value
}

func transcriptSummaryMarkdown(summary *model.MeetingSummaryV2Type) *string {
	sections := []struct {
		title string
		items []string
	}{
		{title: "Key Points", items: summary.KeyPoints},
		{title: "Decisions", items: summary.Decisions},
		{title: "Open Questions", items: summary.OpenQuestions},
		{title: "Blockers", items: summary.Blockers},
	}
	var lines []string
	for _, section := range sections {
		if len(section.items) == 0 {
			continue
		}
		lines = append(lines, "## "+section.title)
		for _, item := range section.items {
			lines = append(lines, "- "+item)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	value := strings.Join(lines, "\n")
	return &value
}

func transcriptActionMarkdown(summary *model.MeetingSummaryV2Type) *string {
	var lines []string
	for _, action := range summary.ActionItems {
		if action == nil {
			continue
		}
		suffix := []string{}
		if action.Owner != nil && *action.Owner != "" {
			suffix = append(suffix, *action.Owner)
		}
		if action.Deadline != nil && *action.Deadline != "" {
			suffix = append(suffix, *action.Deadline)
		}
		line := "- [ ] " + action.Description
		if len(suffix) > 0 {
			line += " (" + strings.Join(suffix, " · ") + ")"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil
	}
	value := strings.Join(lines, "\n")
	return &value
}

var ErrCopilotTranscriptTasksUnavailable = errors.New("copilot transcript task service is unavailable")

func (r *Resolver) submitTranscriptTask(ctx context.Context, blob *graphql.Upload, blobs []*graphql.Upload, blobID, workspaceID string, input *model.SubmitAudioTranscriptionInput) (*model.TranscriptionResultType, error) {
	principal, personal, err := r.authorizeCopilotScope(ctx, workspaceID, nil, "")
	if err != nil {
		return nil, err
	}
	if r.CopilotTranscriptTasks == nil {
		return nil, ErrCopilotTranscriptTasksUnavailable
	}
	uploads := make([]copilot.TranscriptUpload, 0, len(blobs)+1)
	appendUpload := func(upload *graphql.Upload) {
		if upload != nil {
			uploads = append(uploads, copilot.TranscriptUpload{FileName: upload.Filename, ContentType: upload.ContentType, Body: upload.File})
		}
	}
	appendUpload(blob)
	for _, upload := range blobs {
		appendUpload(upload)
	}
	var submit copilot.TranscriptSubmitInput
	if input != nil {
		var marshalErr error
		if input.SourceAudio != nil {
			submit.SourceAudio, marshalErr = json.Marshal(input.SourceAudio)
		}
		if marshalErr == nil && input.Quality != nil {
			submit.Quality, marshalErr = json.Marshal(input.Quality)
		}
		if marshalErr == nil && input.SliceManifest != nil {
			submit.SliceManifest, marshalErr = json.Marshal(input.SliceManifest)
		}
		if marshalErr != nil {
			return nil, marshalErr
		}
	}
	task, err := r.CopilotTranscriptTasks.SubmitForUser(ctx, principal.User.ID, workspaceID, blobID, uploads, submit, personal)
	if err != nil {
		return nil, err
	}
	if r.TranscriptTaskPublisher != nil {
		r.TranscriptTaskPublisher.PublishTranscriptTaskChanged(workspaceID, task.ID, task.Status, "")
	}
	return transcriptTaskModel(task)
}

func (r *Resolver) retryTranscriptTask(ctx context.Context, taskID, workspaceID string) (*model.TranscriptionResultType, error) {
	principal, _, err := r.authorizeCopilotScope(ctx, workspaceID, nil, "")
	if err != nil {
		return nil, err
	}
	if r.CopilotTranscriptTasks == nil {
		return nil, ErrCopilotTranscriptTasksUnavailable
	}
	task, err := r.CopilotTranscriptTasks.RetryForUser(ctx, principal.User.ID, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	if r.TranscriptTaskPublisher != nil {
		r.TranscriptTaskPublisher.PublishTranscriptTaskChanged(workspaceID, task.ID, task.Status, "")
	}
	return transcriptTaskModel(task)
}
