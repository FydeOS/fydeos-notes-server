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
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
)

type ImageTargetResolver interface {
	ResolveImageTarget(context.Context, string, string, string, string, string) (byok.ChatTarget, error)
}

type ImageQuotaReserver interface {
	ReserveCloudImageQuota(context.Context, string, string, string, string, string, string) (string, error)
	ReleaseCloudImageQuota(context.Context, string) error
}

type ImageProfileResolver interface {
	ImageTargetResolver
	TranscriptTargetResolver
	ResolveLocalImageTarget(context.Context, string, string, string, string, string, string) (byok.ChatTarget, error)
}

type LocalImageTargetResolver interface {
	ResolveLocalImageTarget(context.Context, string, string, string, string, string, string) (byok.ChatTarget, error)
}

type ImageRoutes struct {
	profiles ImageProfileResolver
	defaults *TranscriptRoutes
}

func NewImageRoutes(db DB, cfg config.Config, profiles ImageProfileResolver) *ImageRoutes {
	return &ImageRoutes{profiles: profiles, defaults: NewTranscriptRoutes(db, cfg, profiles)}
}

func (r *ImageRoutes) ResolveImageTarget(ctx context.Context, workspace, actor, profile, model, operation string) (byok.ChatTarget, error) {
	if profile != "" {
		if r.profiles == nil {
			return byok.ChatTarget{}, byok.ErrTargetUnavailable
		}
		return r.profiles.ResolveImageTarget(ctx, workspace, actor, profile, model, operation)
	}
	return r.defaults.ResolveServerTarget(ctx, workspace, actor, operation)
}

func (r *ImageRoutes) ResolveLocalImageTarget(ctx context.Context, workspace, actor, lease, profile, model, operation string) (byok.ChatTarget, error) {
	if r.profiles == nil {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	return r.profiles.ResolveLocalImageTarget(ctx, workspace, actor, lease, profile, model, operation)
}

func resolveImageTargetFromRequest(ctx context.Context, resolver ImageTargetResolver, r *http.Request, workspace, actor, operation string) (byok.ChatTarget, error) {
	selection := r.URL.Query().Get("routeTargetId")
	if selection == "" {
		return resolver.ResolveImageTarget(ctx, workspace, actor, "", "", operation)
	}
	leaseID := r.URL.Query().Get("byokLeaseId")
	profileID, modelID, local, ok := parseBYOKRouteTarget(selection, leaseID)
	if !ok {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	if local {
		localResolver, ok := resolver.(LocalImageTargetResolver)
		if !ok {
			return byok.ChatTarget{}, byok.ErrTargetUnavailable
		}
		return localResolver.ResolveLocalImageTarget(ctx, workspace, actor, leaseID, profileID, modelID, operation)
	}
	return resolver.ResolveImageTarget(ctx, workspace, actor, profileID, modelID, operation)
}

func (r *ImageRoutes) ReserveCloudImageQuota(ctx context.Context, userID, workspaceID, sessionID, billingUnitID, provider, model string) (string, error) {
	return r.defaults.ReserveCloudImageQuota(ctx, userID, workspaceID, sessionID, billingUnitID, provider, model)
}

func (r *ImageRoutes) ReleaseCloudImageQuota(ctx context.Context, reservationID string) error {
	return r.defaults.ReleaseCloudImageQuota(ctx, reservationID)
}

func (h *ChatHTTPHandler) SetImageTargetResolver(targets ImageTargetResolver) {
	h.imageTargets = targets
}

func isImageFilterAction(action string) bool {
	switch action {
	case "image.filter.sketch", "image.filter.clay", "image.filter.anime", "image.filter.pixel":
		return true
	default:
		return false
	}
}

func (h *ChatHTTPHandler) serveImageFilterAction(w http.ResponseWriter, r *http.Request, actor, workspace, doc, session, message, action, version string, retry bool, params map[string]any) {
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	if h.imageTargets == nil || h.attachments == nil || h.imageGate == nil {
		h.writeActionError(w, http.StatusServiceUnavailable, "Image generation is unavailable.")
		return
	}
	select {
	case h.imageGate <- struct{}{}:
		defer func() { <-h.imageGate }()
	case <-ctx.Done():
		h.writeActionError(w, http.StatusServiceUnavailable, "Image generation is busy.")
		return
	}
	if err := h.reauthorizeImageAction(ctx, r, actor, workspace, doc, session); err != nil {
		h.writeActionError(w, http.StatusForbidden, "Copilot access is denied.")
		return
	}
	target, err := resolveImageTargetFromRequest(ctx, h.imageTargets, r, workspace, actor, "image_edit")
	if err != nil {
		h.writeActionError(w, http.StatusBadRequest, "The selected image route is unavailable.")
		return
	}
	if target.Credential != nil {
		defer target.Credential.Destroy()
	}
	if target.Provider != "openai" && target.Provider != "gemini" && target.Provider != "fal" {
		h.writeActionError(w, http.StatusBadRequest, "The selected model cannot generate images.")
		return
	}
	messages, err := h.actionPromptMessages(ctx, action, params)
	if err != nil {
		h.writeActionError(w, http.StatusInternalServerError, "The image prompt is unavailable.")
		return
	}
	var prompt strings.Builder
	for _, message := range messages {
		if prompt.Len() > 0 {
			prompt.WriteString("\n\n")
		}
		prompt.WriteString(providerText(message.Content))
	}
	var attachmentsRaw, configRaw []byte
	if err = h.db.QueryRow(ctx, `SELECT COALESCE(attachments,'[]'::json) FROM ai_sessions_messages WHERE id=$1 AND session_id=$2 AND role='user'`, message, session).Scan(&attachmentsRaw); err != nil {
		h.writeActionError(w, http.StatusBadRequest, "The image input is unavailable.")
		return
	}
	if err = h.db.QueryRow(ctx, `SELECT COALESCE(config::jsonb,'{}'::jsonb) FROM ai_prompts_metadata WHERE name=$1`, action).Scan(&configRaw); err != nil {
		h.writeActionError(w, http.StatusInternalServerError, "The image prompt configuration is unavailable.")
		return
	}
	var options map[string]any
	if json.Unmarshal(configRaw, &options) != nil {
		h.writeActionError(w, http.StatusInternalServerError, "The image prompt configuration is invalid.")
		return
	}
	locators, err := parseSessionAttachmentLocators(attachmentsRaw)
	if err != nil || len(locators) == 0 || len(locators) > maxCopilotAttachmentsPerMessage {
		h.writeActionError(w, http.StatusBadRequest, "An image attachment is required.")
		return
	}
	images, err := h.loadImageInputs(ctx, Session{ID: session, UserID: actor, WorkspaceID: workspace, DocID: &doc}, locators)
	if err != nil {
		h.writeActionError(w, http.StatusBadRequest, "The image attachments could not be read.")
		return
	}
	snapshot, err := json.Marshal(map[string]any{"params": params, "messageId": message})
	if err != nil {
		h.writeActionError(w, http.StatusBadRequest, "The image action input is invalid.")
		return
	}
	retryOf := ""
	if retry {
		retryOf = r.URL.Query().Get("runId")
		if retryOf == "" {
			h.writeActionError(w, http.StatusBadRequest, "An action run is required for retry.")
			return
		}
	}
	runID, attempt, err := h.createActionRun(ctx, actor, workspace, doc, session, message, action, version, retryOf, snapshot)
	if err != nil {
		h.writeActionError(w, http.StatusBadRequest, "The image action run could not be created.")
		return
	}
	quotaReservation := ""
	if target.Source == "affine_cloud" {
		reserver, ok := h.imageTargets.(ImageQuotaReserver)
		if !ok {
			h.failActionRun(ctx, runID, "managed_quota_unavailable")
			h.writeActionError(w, http.StatusServiceUnavailable, "Image quota is unavailable.")
			return
		}
		quotaReservation, err = reserver.ReserveCloudImageQuota(ctx, actor, workspace, session, runID, target.Provider, target.ModelID)
		if errors.Is(err, ErrManagedQuotaExceeded) {
			h.failActionRun(ctx, runID, "managed_quota_exceeded")
			h.writeActionError(w, http.StatusPaymentRequired, "The Copilot request limit has been reached.")
			return
		}
		if err != nil {
			h.failActionRun(ctx, runID, "managed_quota_unavailable")
			h.writeActionError(w, http.StatusInternalServerError, "Image quota is unavailable.")
			return
		}
		if quotaReservation != "" {
			defer func() {
				releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer releaseCancel()
				if releaseErr := reserver.ReleaseCloudImageQuota(releaseCtx, quotaReservation); releaseErr != nil {
					slog.ErrorContext(releaseCtx, "managed image action quota release failed", "error", releaseErr)
				}
			}()
		}
	}
	if err := h.reauthorizeImageAction(ctx, r, actor, workspace, doc, session); err != nil {
		h.failActionRun(ctx, runID, "action_access_denied")
		h.writeActionError(w, http.StatusForbidden, "Copilot access is denied.")
		return
	}
	h.writeActionStart(w, runID, action, version, message, attempt)
	data, media, err := h.runImageProvider(ctx, target, prompt.String(), images, options)
	if err != nil {
		code := "action_provider_error"
		if ctx.Err() != nil {
			code = "action_aborted"
		}
		h.failActionRun(ctx, runID, code)
		h.writeActionError(w, http.StatusBadGateway, "The image provider request failed.")
		return
	}
	attachmentURL, err := h.attachments.Upload(ctx, actor, session, "generated."+imageExtension(media), media, bytes.NewReader(data))
	if err != nil {
		h.failActionRun(ctx, runID, "action_artifact_storage_failed")
		h.writeActionError(w, http.StatusInternalServerError, "The generated image could not be saved.")
		return
	}
	artifact := map[string]any{"url": attachmentURL, "mimeType": media}
	if err = h.completeImageAction(ctx, runID, actor, workspace, session, target, quotaReservation, artifact); err != nil {
		h.failActionRun(ctx, runID, "action_output_projection_failed")
		h.writeActionError(w, http.StatusInternalServerError, "The image result could not be recorded.")
		return
	}
	// Image consumers pass attachment data directly to fetchImageToFile.
	// Structured metadata belongs in action_done and the persisted result.
	_, _ = fmt.Fprintf(w, "event: attachment\nid: %s\ndata: %s\n\n", message, attachmentURL)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	h.writeImageEvent(w, "event", message, map[string]any{"type": "action_done", "actionId": action, "actionVersion": version, "runId": runID, "status": "succeeded", "result": artifact})
}

// Queueing and loading inputs may outlive the initial request admission.
func (h *ChatHTTPHandler) reauthorizeImageAction(ctx context.Context, r *http.Request, actor, workspace, doc, session string) error {
	principal, err := h.auth.FromRequest(ctx, r)
	if err != nil || principal == nil || principal.User.ID != actor {
		return errors.New("image authentication changed")
	}
	if h.feature != nil {
		enabled, err := h.feature.CopilotEnabled(ctx)
		if err != nil {
			return err
		}
		if !enabled {
			return errors.New("Copilot is disabled")
		}
	}
	currentWorkspace, currentDoc, err := h.sessions.GetScope(ctx, actor, session)
	if err != nil {
		return err
	}
	if currentWorkspace != workspace || (currentDoc != nil && *currentDoc != doc) || (currentDoc == nil && doc != "") {
		return errors.New("image session scope changed")
	}
	if h.authorize == nil {
		return errors.New("image authorization unavailable")
	}
	return h.authorize(ctx, actor, workspace, currentDoc)
}

func (h *ChatHTTPHandler) completeImageAction(ctx context.Context, runID, actor, workspace, session string, target byok.ChatTarget, quotaReservation string, artifact map[string]any) error {
	result, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	artifacts, _ := json.Marshal([]any{artifact})
	attachments, _ := json.Marshal([]any{artifact["url"]})
	assistantID := uuid.NewString()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO ai_sessions_messages(id,session_id,role,content,attachments,"streamObjects",created_at,updated_at) VALUES($1,$2,'assistant',$3,$4,'[]',now(),now())`, assistantID, session, truncateActionSummary(string(result), 500), attachments); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE ai_action_runs SET status='succeeded',assistant_message_id=$2,result=$3,artifacts=$4,result_summary=$5,error_code=NULL,updated_at=now() WHERE id=$1 AND user_id=$6 AND status='running'`, runID, assistantID, result, artifacts, truncateActionSummary(string(result), 500), actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("image action is no longer running")
	}
	if tag, err = tx.Exec(ctx, `UPDATE ai_sessions_metadata SET updated_at=now() WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL`, session, actor); err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("image session is no longer active")
	}
	if target.Source == "affine_cloud" && quotaReservation != "" {
		reservation, deleteErr := tx.Exec(ctx, `DELETE FROM ai_usage_events WHERE id=$1 AND user_id=$2 AND billing_unit_id=$3 AND provider_source='affine_cloud_image_reserved'`, quotaReservation, actor, runID)
		if deleteErr != nil {
			return deleteErr
		}
		if reservation.RowsAffected() != 1 {
			return errors.New("managed image action quota reservation was lost")
		}
	}
	if target.Source == "byok_server" || target.Source == "byok_local" {
		if _, err = tx.Exec(ctx, `INSERT INTO ai_usage_events(id,workspace_id,user_id,provider,provider_source,billing_unit_id,feature_kind,model,session_id,created_at) VALUES($1,$2,$3,$4,$5,$6,'image',$7,$8,now())`, uuid.NewString(), workspace, actor, target.Provider, target.Source, runID, target.ModelID, session); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (h *ChatHTTPHandler) writeImageEvent(w http.ResponseWriter, event, message string, payload any) {
	encoded, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n", event, message, encoded)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (h *ChatHTTPHandler) runImageProvider(ctx context.Context, target byok.ChatTarget, prompt string, images []providerAttachment, options map[string]any) ([]byte, string, error) {
	endpoint, headers, body, err := buildImageProviderRequest(target, prompt, images, options)
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxCopilotRequestBytes {
		return nil, "", errors.New("image request exceeds byte limit")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	client := h.client
	if target.Transport != nil {
		client = &http.Client{Transport: target.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if target.Endpoint.Kind == "openai_compatible" {
			defer client.CloseIdleConnections()
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("image provider HTTP %d", response.StatusCode)
	}
	const maxImageResponseBytes = 30 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxImageResponseBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > maxImageResponseBytes {
		return nil, "", errors.New("image response exceeds byte limit")
	}
	data, locator, err := firstProviderImage(target.Provider, raw)
	if err != nil {
		return nil, "", err
	}
	if locator != "" {
		if strings.HasPrefix(locator, "data:") {
			data, _, err = h.loadMessageAttachment(ctx, Session{}, locator, maxSessionAttachmentBytes)
		} else {
			data, _, err = fetchRemoteAttachment(ctx, h.remoteAttachments, locator)
		}
		if err != nil {
			return nil, "", err
		}
	}
	media := http.DetectContentType(data)
	if len(data) == 0 || int64(len(data)) > maxSessionAttachmentBytes || !supportedGeneratedImageMIME(media) {
		return nil, "", errors.New("provider returned an invalid image")
	}
	return data, media, nil
}

func buildImageProviderRequest(target byok.ChatTarget, prompt string, images []providerAttachment, options map[string]any) (string, map[string]string, []byte, error) {
	if target.Credential == nil || target.ModelID == "" {
		return "", nil, nil, errors.New("image target or input missing")
	}
	base, err := url.Parse(target.BaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", nil, nil, errors.New("invalid image endpoint")
	}
	headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	var body []byte
	path := ""
	switch target.Provider {
	case "openai":
		if len(images) == 0 {
			payload := map[string]any{"model": target.ModelID, "prompt": prompt, "n": 1, "output_format": "webp"}
			if quality, ok := options["quality"].(string); ok && quality != "" {
				payload["quality"] = quality
			}
			body, err = json.Marshal(payload)
			headers["Authorization"], path = "Bearer "+string(target.Credential.Bytes()), "/images/generations"
			break
		}
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		for name, value := range map[string]string{"model": target.ModelID, "prompt": prompt, "n": "1", "output_format": "webp"} {
			if err = writer.WriteField(name, value); err != nil {
				return "", nil, nil, err
			}
		}
		if quality, ok := options["quality"].(string); ok && quality != "" {
			if err = writer.WriteField("quality", quality); err != nil {
				return "", nil, nil, err
			}
		}
		for i, image := range images {
			data, err := base64.StdEncoding.DecodeString(image.Data)
			if err != nil {
				return "", nil, nil, err
			}
			header := textproto.MIMEHeader{"Content-Disposition": {fmt.Sprintf(`form-data; name="image[]"; filename="input-%d.%s"`, i, imageExtension(image.MIMEType))}, "Content-Type": {image.MIMEType}}
			part, err := writer.CreatePart(header)
			if err != nil {
				return "", nil, nil, err
			}
			if _, err = part.Write(data); err != nil {
				return "", nil, nil, err
			}
		}
		if err = writer.Close(); err != nil {
			return "", nil, nil, err
		}
		headers["Content-Type"], headers["Authorization"] = writer.FormDataContentType(), "Bearer "+string(target.Credential.Bytes())
		body, path = buffer.Bytes(), "/images/edits"
	case "gemini":
		parts := []map[string]any{{"text": prompt}}
		for _, image := range images {
			parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": image.MIMEType, "data": image.Data}})
		}
		body, err = json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": parts}}, "generationConfig": map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}}})
		headers["x-goog-api-key"], path = string(target.Credential.Bytes()), "/models/"+url.PathEscape(target.ModelID)+":generateContent"
	case "fal":
		if len(images) > 1 {
			return "", nil, nil, errors.New("selected image route accepts at most one input image")
		}
		segments := strings.Split(target.ModelID, "/")
		for i, segment := range segments {
			if segment == "" || segment == "." || segment == ".." {
				return "", nil, nil, errors.New("invalid image model")
			}
			segments[i] = url.PathEscape(segment)
		}
		payload := map[string]any{"prompt": prompt, "sync_mode": true, "enable_safety_checker": true, "num_images": 1, "output_format": "webp"}
		if len(images) == 1 {
			payload["image_url"] = "data:" + images[0].MIMEType + ";base64," + images[0].Data
		}
		for from, to := range map[string]string{"modelName": "model_name", "loras": "loras", "quality": "quality", "seed": "seed"} {
			if value, ok := options[from]; ok {
				payload[to] = value
			}
		}
		body, err = json.Marshal(payload)
		headers["Authorization"], path = "Key "+string(target.Credential.Bytes()), "/fal-ai/"+strings.Join(segments, "/")
	default:
		return "", nil, nil, errors.New("unsupported image provider")
	}
	return strings.TrimRight(target.BaseURL, "/") + path, headers, body, err
}

func firstProviderImage(provider string, raw []byte) ([]byte, string, error) {
	var response struct {
		Data []struct {
			Base64 string `json:"b64_json"`
			URL    string `json:"url"`
		} `json:"data"`
		Images []struct {
			URL string `json:"url"`
		} `json:"images"`
		Image *struct {
			URL string `json:"url"`
		} `json:"image"`
		Candidates []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []struct {
					InlineData *struct {
						Data string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &response) != nil || (len(response.Error) > 0 && string(response.Error) != "null") {
		return nil, "", errors.New("invalid image response")
	}
	encoded, locator := "", ""
	switch provider {
	case "openai":
		if len(response.Data) > 0 {
			encoded, locator = response.Data[0].Base64, response.Data[0].URL
		}
	case "fal":
		if len(response.Images) > 0 {
			locator = response.Images[0].URL
		} else if response.Image != nil {
			locator = response.Image.URL
		}
	case "gemini":
		if len(response.Candidates) > 0 && response.Candidates[0].FinishReason == "STOP" {
			for _, part := range response.Candidates[0].Content.Parts {
				if part.InlineData != nil {
					encoded = part.InlineData.Data
					break
				}
			}
		}
	}
	if encoded != "" {
		if base64PayloadDecodedLength(encoded) > maxSessionAttachmentBytes {
			return nil, "", errors.New("generated image exceeds byte limit")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		return data, "", err
	}
	if locator != "" {
		return nil, locator, nil
	}
	return nil, "", errors.New("provider returned no image")
}

func supportedGeneratedImageMIME(media string) bool {
	return media == "image/png" || media == "image/jpeg" || media == "image/webp"
}
func imageExtension(media string) string {
	switch media {
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	default:
		return "png"
	}
}
