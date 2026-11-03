// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/google/uuid"
)

// serveImages implements the current client's ordinary generation/processing
// endpoint. Filter actions keep their separate action run/retry protocol.
func (h *ChatHTTPHandler) serveImages(w http.ResponseWriter, r *http.Request, actor, workspace string, docID *string, sessionID string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	fail := func(status int, message string) { h.writeStreamError(w, status, message) }
	if h.imageTargets == nil || h.attachments == nil || h.imageGate == nil {
		fail(http.StatusServiceUnavailable, "Image generation is unavailable.")
		return
	}
	select {
	case h.imageGate <- struct{}{}:
		defer func() { <-h.imageGate }()
	case <-ctx.Done():
		fail(http.StatusServiceUnavailable, "Image generation is busy.")
		return
	}
	doc := ""
	if docID != nil {
		doc = *docID
	}
	if err := h.reauthorizeImageAction(ctx, r, actor, workspace, doc, sessionID); err != nil {
		fail(http.StatusForbidden, "Copilot access is denied.")
		return
	}
	var promptName, messageID, content string
	var rawAttachments, rawParams, rawOptions []byte
	err := h.db.QueryRow(ctx, `SELECT session.prompt_name,message.id,message.content,
		COALESCE(message.attachments,'[]'::json),COALESCE(message.params,'{}'::json),COALESCE(prompt.config,'{}'::json)
		FROM ai_sessions_metadata session JOIN ai_sessions_messages message ON message.session_id=session.id
		JOIN ai_prompts_metadata prompt ON prompt.name=session.prompt_name
		WHERE session.id=$1 AND session.user_id=$2 AND session.deleted_at IS NULL AND message.role='user'
		AND ($3='' OR message.id=$3) ORDER BY message.created_at DESC,message.id DESC LIMIT 1`, sessionID, actor, r.URL.Query().Get("messageId")).Scan(&promptName, &messageID, &content, &rawAttachments, &rawParams, &rawOptions)
	if err != nil {
		fail(http.StatusBadRequest, "The image message or prompt was not found.")
		return
	}
	var params, options map[string]any
	if json.Unmarshal(rawParams, &params) != nil || json.Unmarshal(rawOptions, &options) != nil {
		fail(http.StatusBadRequest, "The image parameters are invalid.")
		return
	}
	if params == nil {
		params = map[string]any{}
	}
	if options == nil {
		options = map[string]any{}
	}
	params["content"] = content
	if quality, ok := params["quality"].(string); ok {
		options["quality"] = quality
	}
	seed := r.URL.Query().Get("seed")
	if seed == "" && params["seed"] != nil {
		seed = fmt.Sprint(params["seed"])
	}
	if seed != "" {
		if value, err := strconv.ParseInt(seed, 10, 64); err == nil {
			options["seed"] = value
		}
	}
	messages, err := h.actionPromptMessages(ctx, promptName, params)
	if err != nil {
		fail(http.StatusInternalServerError, "The image prompt is unavailable.")
		return
	}
	var prompt strings.Builder
	for _, message := range messages {
		if prompt.Len() > 0 {
			prompt.WriteString("\n\n")
		}
		prompt.WriteString(providerText(message.Content))
	}
	locators, err := parseSessionAttachmentLocators(rawAttachments)
	if err != nil {
		fail(http.StatusBadRequest, "The image attachments are invalid.")
		return
	}
	if (promptName == "Upscale image" || promptName == "Remove background" || promptName == "Convert to sticker") && len(locators) == 0 {
		fail(http.StatusBadRequest, "An image attachment is required.")
		return
	}
	images, err := h.loadImageInputs(ctx, Session{ID: sessionID, UserID: actor, WorkspaceID: workspace, DocID: docID}, locators)
	if err != nil {
		fail(http.StatusBadRequest, "The image attachments could not be read.")
		return
	}
	operation := "image_generate"
	if len(images) > 0 {
		operation = "image_process"
	}
	target, err := resolveImageTargetFromRequest(ctx, h.imageTargets, r, workspace, actor, operation)
	if err != nil {
		fail(http.StatusBadRequest, "The selected image route is unavailable.")
		return
	}
	if target.Credential != nil {
		defer target.Credential.Destroy()
	}
	assistantID := uuid.NewString()
	quotaReservation := ""
	if target.Source == "affine_cloud" {
		reserver, ok := h.imageTargets.(ImageQuotaReserver)
		if !ok {
			fail(http.StatusServiceUnavailable, "Image quota is unavailable.")
			return
		}
		quotaReservation, err = reserver.ReserveCloudImageQuota(ctx, actor, workspace, sessionID, assistantID, target.Provider, target.ModelID)
		if errors.Is(err, ErrManagedQuotaExceeded) {
			fail(http.StatusPaymentRequired, "The Copilot request limit has been reached.")
			return
		}
		if err != nil {
			fail(http.StatusInternalServerError, "Image quota is unavailable.")
			return
		}
		if quotaReservation != "" {
			defer func() {
				releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer releaseCancel()
				if releaseErr := reserver.ReleaseCloudImageQuota(releaseCtx, quotaReservation); releaseErr != nil {
					slog.ErrorContext(releaseCtx, "managed image quota release failed", "error", releaseErr)
				}
			}()
		}
	}
	if err := h.reauthorizeImageAction(ctx, r, actor, workspace, doc, sessionID); err != nil {
		fail(http.StatusForbidden, "Copilot access is denied.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	type result struct {
		data  []byte
		media string
		err   error
	}
	completed := make(chan result, 1)
	go func() {
		data, media, err := h.runImageProvider(ctx, target, prompt.String(), images, options)
		completed <- result{data, media, err}
	}()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	var output result
wait:
	for {
		select {
		case output = <-completed:
			break wait
		case <-ping.C:
			if _, err := fmt.Fprintf(w, "event: ping\nid: %s\ndata: \n\n", messageID); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case <-ctx.Done():
			fail(http.StatusBadGateway, "The image provider request was interrupted.")
			return
		}
	}
	if output.err != nil {
		fail(http.StatusBadGateway, "The image provider request failed.")
		return
	}
	if err := h.reauthorizeImageAction(ctx, r, actor, workspace, doc, sessionID); err != nil {
		fail(http.StatusForbidden, "Copilot access is denied.")
		return
	}
	locator, err := h.attachments.Upload(ctx, actor, sessionID, "generated."+imageExtension(output.media), output.media, bytes.NewReader(output.data))
	if err != nil {
		fail(http.StatusInternalServerError, "The generated image could not be saved.")
		return
	}
	if err := h.persistImageTurn(ctx, actor, workspace, sessionID, target, assistantID, quotaReservation, []string{locator}); err != nil {
		fail(http.StatusInternalServerError, "The image result could not be recorded.")
		return
	}
	_, _ = fmt.Fprintf(w, "event: attachment\nid: %s\ndata: %s\n\n", messageID, locator)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (h *ChatHTTPHandler) loadImageInputs(ctx context.Context, session Session, locators []string) ([]providerAttachment, error) {
	if len(locators) > maxCopilotAttachmentsPerMessage {
		return nil, errors.New("too many image attachments")
	}
	images := make([]providerAttachment, 0, len(locators))
	remaining := int64(maxCopilotAttachmentBytes)
	for _, locator := range locators {
		if remaining <= 0 {
			return nil, errors.New("image attachments exceed byte limit")
		}
		data, _, err := h.loadMessageAttachment(ctx, session, locator, remaining)
		if err != nil {
			return nil, err
		}
		media := http.DetectContentType(data)
		if !supportedGeneratedImageMIME(media) {
			return nil, errors.New("unsupported image input")
		}
		remaining -= int64(len(data))
		images = append(images, providerAttachment{FileName: fmt.Sprintf("input-%d.%s", len(images), imageExtension(media)), MIMEType: media, Data: base64.StdEncoding.EncodeToString(data)})
	}
	return images, nil
}

func (h *ChatHTTPHandler) persistImageTurn(ctx context.Context, actor, workspace, session string, target byok.ChatTarget, assistant, quotaReservation string, attachments []string) error {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var active string
	if err = tx.QueryRow(ctx, `SELECT id FROM ai_sessions_metadata WHERE id=$1 AND user_id=$2 AND workspace_id=$3 AND deleted_at IS NULL FOR UPDATE`, session, actor, workspace).Scan(&active); err != nil {
		return err
	}
	raw, err := json.Marshal(attachments)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ai_sessions_messages(id,session_id,role,content,attachments,"streamObjects",created_at,updated_at) VALUES($1,$2,'assistant','',$3,'[]',now(),now())`, assistant, session, raw); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE ai_sessions_metadata SET updated_at=now() WHERE id=$1`, session); err != nil {
		return err
	}
	if target.Source == "affine_cloud" && quotaReservation != "" {
		result, updateErr := tx.Exec(ctx, `UPDATE ai_usage_events SET provider_source='affine_cloud' WHERE id=$1 AND user_id=$2 AND billing_unit_id=$3 AND provider_source='affine_cloud_image_reserved'`, quotaReservation, actor, assistant)
		if updateErr != nil {
			return updateErr
		}
		if result.RowsAffected() != 1 {
			return errors.New("managed image quota reservation was lost")
		}
	} else if target.Source == "byok_server" || target.Source == "byok_local" || target.Source == "affine_cloud" {
		if _, err = tx.Exec(ctx, `INSERT INTO ai_usage_events(id,workspace_id,user_id,provider,provider_source,billing_unit_id,feature_kind,model,session_id,created_at) VALUES($1,$2,$3,$4,$5,$6,'image',$7,$8,now())`, uuid.NewString(), workspace, actor, target.Provider, target.Source, assistant, target.ModelID, session); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
