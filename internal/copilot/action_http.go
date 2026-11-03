// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type actionPromptMessage struct {
	Role    string
	Content string
}

type textActionDefinition struct {
	promptRef string
}

var textActionDefinitions = map[string]textActionDefinition{
	"mindmap.generate": {promptRef: "mindmap.generate"},
	"slides.outline":   {promptRef: "slides.outline"},
}

func (h *ChatHTTPHandler) ActionHandler() http.Handler {
	return http.HandlerFunc(h.serveAction)
}

func (h *ChatHTTPHandler) serveAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.writeActionError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.db == nil || h.auth == nil || h.sessions == nil {
		h.writeActionError(w, http.StatusServiceUnavailable, "Copilot is unavailable.")
		return
	}
	principal, err := h.auth.FromRequest(r.Context(), r)
	if err != nil || principal == nil {
		h.writeActionError(w, http.StatusUnauthorized, "Authentication is required.")
		return
	}
	if h.feature != nil {
		enabled, err := h.feature.CopilotEnabled(r.Context())
		if err != nil || !enabled {
			h.writeActionError(w, http.StatusServiceUnavailable, "Copilot is disabled.")
			return
		}
	}

	sessionID := r.PathValue("sessionId")
	workspaceID, docID, err := h.sessions.GetScope(r.Context(), principal.User.ID, sessionID)
	if err != nil || sessionID == "" {
		h.writeActionError(w, http.StatusNotFound, "Copilot session was not found.")
		return
	}
	personal, err := h.sessions.WorkspaceExists(r.Context(), workspaceID)
	if err != nil {
		h.writeActionError(w, http.StatusInternalServerError, "Copilot is unavailable.")
		return
	}
	personal = !personal
	if !personal && h.authorize != nil {
		if err := h.authorize(r.Context(), principal.User.ID, workspaceID, docID); err != nil {
			h.writeActionError(w, http.StatusForbidden, "Copilot access is denied.")
			return
		}
	}
	if docID == nil || *docID == "" {
		h.writeActionError(w, http.StatusBadRequest, "Copilot actions require a document session.")
		return
	}

	actionID := r.URL.Query().Get("actionId")
	definition, ok := textActionDefinitions[actionID]
	imageAction := isImageFilterAction(actionID)
	if imageAction {
		definition, ok = textActionDefinition{promptRef: actionID}, true
	}
	version := r.URL.Query().Get("actionVersion")
	if version == "" {
		version = "v1"
	}
	if !ok || version != "v1" {
		h.writeActionError(w, http.StatusBadRequest, "The selected Copilot action is unavailable.")
		return
	}
	messageID := r.URL.Query().Get("messageId")
	retry := r.URL.Query().Get("retry") == "true" || r.URL.Query().Get("retry") == "1"
	if messageID == "" {
		h.writeActionError(w, http.StatusBadRequest, "A user message is required for this Copilot action.")
		return
	}
	var content string
	var paramsRaw []byte
	if err := h.db.QueryRow(r.Context(), `SELECT content,COALESCE(params,'{}'::json)
		FROM ai_sessions_messages WHERE id=$1 AND session_id=$2 AND role='user'`, messageID, sessionID).Scan(&content, &paramsRaw); err != nil {
		h.writeActionError(w, http.StatusBadRequest, "Copilot message was not found or cannot be processed.")
		return
	}
	var params map[string]any
	if len(paramsRaw) == 0 || string(paramsRaw) == "null" {
		params = map[string]any{}
	} else if json.Unmarshal(paramsRaw, &params) != nil || params == nil {
		h.writeActionError(w, http.StatusBadRequest, "Copilot message parameters are invalid.")
		return
	}
	params["content"] = content
	if imageAction {
		if personal {
			h.writeActionError(w, http.StatusBadRequest, "Local workspaces do not support generated images.")
			return
		}
		h.serveImageFilterAction(w, r, principal.User.ID, workspaceID, *docID, sessionID, messageID, actionID, version, retry, params)
		return
	}

	route, err := h.resolveRoute(r, principal.User.ID, workspaceID)
	if err != nil {
		h.writeActionError(w, http.StatusBadRequest, "The selected Copilot route is unavailable.")
		return
	}
	if route.credential != nil {
		defer route.credential.Destroy()
	}
	if route.managed && (!h.cfg.FydeOSManagedCopilot || h.cfg.FydeOSManagedCopilotAPIKey == "" || h.cfg.FydeOSManagedCopilotBaseURL == "" || h.cfg.FydeOSManagedCopilotModel == "") {
		h.writeActionError(w, http.StatusServiceUnavailable, "No managed Copilot provider is configured.")
		return
	}
	messages, err := h.actionPromptMessages(r.Context(), definition.promptRef, params)
	if err != nil {
		h.writeActionError(w, http.StatusInternalServerError, "The Copilot action prompt is unavailable.")
		return
	}
	inputSnapshot, err := json.Marshal(map[string]any{"params": params, "messageId": messageID})
	if err != nil {
		h.writeActionError(w, http.StatusBadRequest, "Copilot action input is invalid.")
		return
	}
	previousRunID := ""
	if retry {
		previousRunID = r.URL.Query().Get("runId")
		if previousRunID == "" {
			h.writeActionError(w, http.StatusBadRequest, "A failed action run is required for retry.")
			return
		}
	}
	runID, attempt, err := h.createActionRun(r.Context(), principal.User.ID, workspaceID, *docID, sessionID, messageID, actionID, version, previousRunID, inputSnapshot)
	if err != nil {
		h.writeActionError(w, http.StatusBadRequest, "The Copilot action run could not be created.")
		return
	}

	reservationID := ""
	if route.managed {
		provider, model := route.provider, route.model
		if provider == "" {
			provider = "openai"
		}
		if model == "" {
			model = h.cfg.FydeOSManagedCopilotModel
		}
		reservationID, err = h.sessions.ReserveFydeosManagedRequestForFeature(
			r.Context(), principal.User.ID, workspaceID, sessionID, runID, provider, model, "action", personal, time.Now(),
		)
		if errors.Is(err, ErrManagedQuotaExceeded) {
			h.failActionRun(r.Context(), runID, "managed_quota_exceeded")
			h.writeActionError(w, http.StatusPaymentRequired, "The daily Copilot request limit has been reached.")
			return
		}
		if err != nil {
			h.failActionRun(r.Context(), runID, "managed_quota_unavailable")
			h.writeActionError(w, http.StatusInternalServerError, "Copilot quota is unavailable.")
			return
		}
		if h.quotaPublisher != nil {
			h.quotaPublisher.PublishUserQuotaStateChanged(principal.User.ID, "runtime-invalidation")
		}
	}

	h.writeActionStart(w, runID, actionID, version, messageID, attempt)
	result, err := h.runStructuredAction(r, route, messages)
	if err != nil {
		if reservationID != "" {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			if releaseErr := h.sessions.ReleaseFydeosManagedRequest(releaseCtx, reservationID); releaseErr == nil && h.quotaPublisher != nil {
				h.quotaPublisher.PublishUserQuotaStateChanged(principal.User.ID, "runtime-invalidation")
			}
			cancel()
		}
		errorCode := "action_provider_error"
		if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
			errorCode = "action_aborted"
		}
		h.failActionRun(context.WithoutCancel(r.Context()), runID, errorCode)
		if errorCode != "action_aborted" {
			h.writeActionError(w, http.StatusBadGateway, "The Copilot provider request failed.")
		}
		return
	}
	if strings.TrimSpace(result) == "" {
		if reservationID != "" {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			if err := h.sessions.ReleaseFydeosManagedRequest(releaseCtx, reservationID); err == nil && h.quotaPublisher != nil {
				h.quotaPublisher.PublishUserQuotaStateChanged(principal.User.ID, "runtime-invalidation")
			}
			cancel()
		}
		h.failActionRun(context.WithoutCancel(r.Context()), runID, "action_empty_result")
		h.writeActionError(w, http.StatusBadGateway, "The Copilot action returned an empty result.")
		return
	}
	assistantID, err := h.persistActionAssistant(r.Context(), sessionID, result)
	if err != nil {
		h.failActionRun(context.WithoutCancel(r.Context()), runID, "action_output_projection_failed")
		h.writeActionError(w, http.StatusInternalServerError, "The Copilot action result could not be saved.")
		return
	}
	encodedResult, _ := json.Marshal(map[string]string{"result": result})
	tag, err := h.db.Exec(r.Context(), `UPDATE ai_action_runs SET status='succeeded',assistant_message_id=$2,result=$3,result_summary=$4,error_code=NULL,updated_at=now() WHERE id=$1 AND status='running'`, runID, assistantID, encodedResult, truncateActionSummary(result, 500))
	if err != nil || tag.RowsAffected() != 1 {
		h.failActionRun(r.Context(), runID, "action_run_completion_failed")
		h.writeActionError(w, http.StatusInternalServerError, "The Copilot action result could not be recorded.")
		return
	}
	h.writeActionResult(w, messageID, result)
}

func (h *ChatHTTPHandler) actionPromptMessages(ctx context.Context, promptRef string, params map[string]any) ([]providerMessage, error) {
	rows, err := h.db.Query(ctx, `SELECT message.role::text,message.content
		FROM ai_prompts_metadata prompt
		JOIN ai_prompts_messages message ON message.prompt_id=prompt.id
		WHERE prompt.name=$1 ORDER BY message.idx`, promptRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	content, _ := params["content"].(string)
	language, _ := params["language"].(string)
	if language == "" {
		language = "English"
	}
	messages := make([]providerMessage, 0, 3)
	for rows.Next() {
		var role, template string
		if err := rows.Scan(&role, &template); err != nil {
			return nil, err
		}
		if strings.Contains(template, "{{>guarded_content}}") {
			template = strings.ReplaceAll(template, "{{>guarded_content}}", "\nUser request:\n{{content}}")
		}
		template = strings.NewReplacer("{{content}}", content, "{{language}}", language).Replace(template)
		messages = append(messages, providerMessage{Role: role, Content: template})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, errors.New("action prompt not found")
	}
	return messages, nil
}

func (h *ChatHTTPHandler) createActionRun(ctx context.Context, userID, workspaceID, docID, sessionID, messageID, actionID, version, retryOf string, snapshot []byte) (string, int, error) {
	runID := uuid.NewString()
	attempt := 1
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if retryOf != "" {
		if err := tx.QueryRow(ctx, `SELECT attempt FROM ai_action_runs
			WHERE id=$1 AND user_id=$2 AND workspace_id=$3 AND session_id=$4 AND user_message_id=$5
			AND action_id=$6 AND action_version=$7 FOR SHARE`, retryOf, userID, workspaceID, sessionID, messageID, actionID, version).Scan(&attempt); err != nil {
			return "", 0, err
		}
		attempt++
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ai_action_runs(
		id,user_id,workspace_id,doc_id,session_id,user_message_id,action_id,action_version,status,attempt,retry_of,input_snapshot,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'running',$9,NULLIF($10,''),$11,now(),now())`, runID, userID, workspaceID, docID, sessionID, messageID, actionID, version, attempt, retryOf, snapshot); err != nil {
		return "", 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, err
	}
	return runID, attempt, nil
}

func (h *ChatHTTPHandler) failActionRun(ctx context.Context, runID, code string) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = h.db.Exec(bounded, `UPDATE ai_action_runs SET status='failed',error_code=$2,updated_at=now() WHERE id=$1 AND status='running'`, runID, code)
}

func (h *ChatHTTPHandler) runStructuredAction(r *http.Request, route chatRoute, messages []providerMessage) (string, error) {
	endpointURL, headers, body, streamFormat, err := h.buildProviderRequest(route, messages)
	if err != nil {
		return "", err
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	applyActionResponseFormat(payload, streamFormat)
	body, err = json.Marshal(payload)
	if err != nil {
		return "", err
	}
	endpoint, err := url.Parse(endpointURL)
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Host == "" {
		return "", errors.New("invalid Copilot endpoint")
	}
	if len(body) > maxCopilotRequestBytes {
		return "", errors.New("Copilot request is too large")
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	client := h.client
	if route.transport != nil {
		client = &http.Client{Transport: route.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if route.endpoint.Kind == "openai_compatible" {
			defer client.CloseIdleConnections()
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Copilot provider returned status %d", response.StatusCode)
	}
	limited := &io.LimitedReader{R: response.Body, N: maxCopilotResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var answer strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		if delta := actionTextDelta(streamFormat, data); delta != "" {
			answer.WriteString(delta)
			if answer.Len() > maxCopilotResponseBytes {
				return "", errors.New("Copilot action response is too large")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if limited.N == 0 {
		return "", errors.New("Copilot action response is too large")
	}
	var result struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(answer.String()), &result); err != nil || strings.TrimSpace(result.Result) == "" {
		return "", errors.New("Copilot action response does not match the structured result contract")
	}
	return result.Result, nil
}

func applyActionResponseFormat(body map[string]any, streamFormat string) {
	schema := map[string]any{
		"type": "object", "properties": map[string]any{"result": map[string]any{"type": "string", "minLength": 1}},
		"required": []string{"result"}, "additionalProperties": false,
	}
	switch streamFormat {
	case "openai-chat":
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "action_result", "strict": true, "schema": schema}}
	case "openai-responses":
		body["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "action_result", "strict": true, "schema": schema}}
	case "gemini":
		body["generationConfig"] = map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   map[string]any{"type": "OBJECT", "properties": map[string]any{"result": map[string]any{"type": "STRING"}}, "required": []string{"result"}},
		}
	case "anthropic":
		instruction := "Return only a JSON object matching this schema: {\"result\": string}. Do not include markdown fences or other text."
		if system, ok := body["system"].(string); ok && system != "" {
			body["system"] = system + "\n\n" + instruction
		} else {
			body["system"] = instruction
		}
	}
}

func actionTextDelta(streamFormat, data string) string {
	switch streamFormat {
	case "openai-chat":
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil {
			var out strings.Builder
			for _, choice := range chunk.Choices {
				out.WriteString(choice.Delta.Content)
			}
			return out.String()
		}
	case "openai-responses":
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &event) == nil && event.Type == "response.output_text.delta" {
			return event.Delta
		}
	case "anthropic":
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &event) == nil && event.Type == "content_block_delta" && event.Delta.Type == "text_delta" {
			return event.Delta.Text
		}
	case "gemini":
		var event struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if json.Unmarshal([]byte(data), &event) == nil {
			var out strings.Builder
			for _, candidate := range event.Candidates {
				for _, part := range candidate.Content.Parts {
					out.WriteString(part.Text)
				}
			}
			return out.String()
		}
	}
	return ""
}

func (h *ChatHTTPHandler) persistActionAssistant(ctx context.Context, sessionID, content string) (string, error) {
	assistantID := uuid.NewString()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO ai_sessions_messages(id,session_id,role,content,"streamObjects",attachments,created_at,updated_at)
		VALUES($1,$2,'assistant',$3,'[]','[]',now(),now())`, assistantID, sessionID, content); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE ai_sessions_metadata SET updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, sessionID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return assistantID, nil
}

func (h *ChatHTTPHandler) writeActionResult(w http.ResponseWriter, messageID, result string) {
	_, _ = fmt.Fprintf(w, "event: message\nid: %s\n", messageID)
	for _, line := range strings.Split(result, "\n") {
		_, _ = fmt.Fprintf(w, "data: %s\n", line)
	}
	_, _ = fmt.Fprint(w, "\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (h *ChatHTTPHandler) writeActionStart(w http.ResponseWriter, runID, actionID, version, messageID string, attempt int) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "event: event\nid: %s\ndata: {\"type\":\"action_start\",\"actionId\":%q,\"actionVersion\":%q,\"status\":\"running\",\"runId\":%q,\"attempt\":%d}\n\n", messageID, actionID, version, runID, attempt)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (h *ChatHTTPHandler) writeActionError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.WriteHeader(http.StatusOK)
	payload, _ := json.Marshal(map[string]any{"status": status, "message": message})
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
}

func truncateActionSummary(value string, limit int) string {
	if len([]rune(value)) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}
