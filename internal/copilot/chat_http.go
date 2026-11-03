// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/google/uuid"
)

const maxCopilotRequestBytes = 32 << 20
const maxCopilotResponseBytes = 4 << 20
const maxCopilotAttachmentBytes = 16 << 20
const maxCopilotAttachmentsPerMessage = 10
const maxCopilotAttachmentLocatorsPerRequest = 20

type ChatAuthenticator interface {
	FromRequest(context.Context, *http.Request) (*auth.Principal, error)
}

type ChatSessionAuthorizer func(context.Context, string, string, *string) error
type DocumentBlobReader func(context.Context, string, string, string, string) (io.ReadCloser, string, int64, error)

type ChatFeatureReader interface {
	CopilotEnabled(context.Context) (bool, error)
}

type ChatBYOKResolver interface {
	ResolveChatTarget(context.Context, string, string, string, string) (byok.ChatTarget, error)
}

type LocalChatBYOKResolver interface {
	ResolveLocalChatTarget(context.Context, string, string, string, string, string) (byok.ChatTarget, error)
}

type ChatQuotaStatePublisher interface {
	PublishUserQuotaStateChanged(string, string)
}

type chatToolsConfig struct {
	searchWorkspace  bool
	readingDocs      bool
	searchArtifacts  bool
	readingArtifacts bool
}

type ChatHTTPHandler struct {
	db                DB
	cfg               config.Config
	auth              ChatAuthenticator
	sessions          *Store
	authorize         ChatSessionAuthorizer
	feature           ChatFeatureReader
	byok              ChatBYOKResolver
	quotaPublisher    ChatQuotaStatePublisher
	toolExecutor      ChatToolExecutor
	attachments       *SessionAttachments
	client            *http.Client
	remoteAttachments *http.Client
	documentBlobRead  DocumentBlobReader
	imageTargets      ImageTargetResolver
	imageGate         chan struct{}
}

func NewChatHTTPHandler(db DB, cfg config.Config, authenticator ChatAuthenticator, sessions *Store, authorize ChatSessionAuthorizer, feature ChatFeatureReader) *ChatHTTPHandler {
	return &ChatHTTPHandler{
		db: db, cfg: cfg, auth: authenticator, sessions: sessions, authorize: authorize, feature: feature,
		client:            &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		remoteAttachments: byok.NewPublicHTTPSClient(15 * time.Second),
		imageGate:         make(chan struct{}, 2),
	}
}

func (h *ChatHTTPHandler) SetBYOKProfileStore(store ChatBYOKResolver) {
	h.byok = store
}

func (h *ChatHTTPHandler) SetQuotaStatePublisher(publisher ChatQuotaStatePublisher) {
	h.quotaPublisher = publisher
}

func (h *ChatHTTPHandler) SetToolExecutor(executor ChatToolExecutor) {
	h.toolExecutor = executor
}

func (h *ChatHTTPHandler) SetSessionAttachments(attachments *SessionAttachments) {
	h.attachments = attachments
}

func (h *ChatHTTPHandler) SetDocumentBlobReader(reader DocumentBlobReader) {
	h.documentBlobRead = reader
}

func (h *ChatHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.PathValue("endpoint") != "stream-object" && r.PathValue("endpoint") != "images" {
		http.Error(w, "Copilot endpoint is not supported", http.StatusNotFound)
		return
	}
	if h.db == nil || h.auth == nil || h.sessions == nil {
		h.writeStreamError(w, http.StatusServiceUnavailable, "Copilot is unavailable.")
		return
	}
	principal, err := h.auth.FromRequest(r.Context(), r)
	if err != nil || principal == nil {
		h.writeStreamError(w, http.StatusUnauthorized, "Authentication is required.")
		return
	}
	if h.feature != nil {
		enabled, err := h.feature.CopilotEnabled(r.Context())
		if err != nil || !enabled {
			h.writeStreamError(w, http.StatusServiceUnavailable, "Copilot is disabled.")
			return
		}
	}
	sessionID := r.PathValue("sessionId")
	if sessionID == "" {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	workspaceID, docID, err := h.sessions.GetScope(r.Context(), principal.User.ID, sessionID)
	if err != nil {
		h.writeStreamError(w, http.StatusNotFound, "Copilot session was not found.")
		return
	}
	workspaceExists, err := h.sessions.WorkspaceExists(r.Context(), workspaceID)
	if err != nil {
		h.writeStreamError(w, http.StatusInternalServerError, "Copilot is unavailable.")
		return
	}
	personal := !workspaceExists
	if !personal && h.authorize != nil {
		if err := h.authorize(r.Context(), principal.User.ID, workspaceID, docID); err != nil {
			h.writeStreamError(w, http.StatusForbidden, "Copilot access is denied.")
			return
		}
	}
	if r.PathValue("endpoint") == "images" {
		if personal {
			h.writeStreamError(w, http.StatusBadRequest, "Local workspaces do not support generated images.")
			return
		}
		h.serveImages(w, r, principal.User.ID, workspaceID, docID, sessionID)
		return
	}
	route, err := h.resolveRoute(r, principal.User.ID, workspaceID)
	if err != nil {
		h.writeStreamError(w, http.StatusBadRequest, "The selected Copilot route is unavailable.")
		return
	}
	if route.credential != nil {
		defer route.credential.Destroy()
	}
	if route.managed {
		if !h.cfg.FydeOSManagedCopilot || h.cfg.FydeOSManagedCopilotAPIKey == "" || h.cfg.FydeOSManagedCopilotBaseURL == "" || h.cfg.FydeOSManagedCopilotModel == "" {
			h.writeStreamError(w, http.StatusServiceUnavailable, "No managed Copilot provider is configured.")
			return
		}
	}
	session, err := h.sessions.Get(r.Context(), principal.User.ID, workspaceID, sessionID, personal)
	if err != nil {
		h.writeStreamError(w, http.StatusNotFound, "Copilot session was not found.")
		return
	}
	messageID := r.URL.Query().Get("messageId")
	if messageID == "" {
		for index := len(session.Messages) - 1; index >= 0; index-- {
			if session.Messages[index].Role == "user" {
				messageID = session.Messages[index].ID
				break
			}
		}
	}
	messages, err := h.buildMessages(r.Context(), session, messageID)
	if err != nil {
		h.writeStreamError(w, http.StatusBadRequest, "Copilot message was not found or cannot be processed.")
		return
	}
	retrievalScope := artifactScopeForMessage(session, messageID)
	toolsConfig, err := parseCopilotToolsConfig(r.URL.Query().Get("toolsConfig"))
	if err != nil {
		h.writeStreamError(w, http.StatusBadRequest, "Tool configuration is invalid.")
		return
	}
	toolNames, err := h.promptTools(r.Context(), session.PromptName, toolsConfig)
	if err != nil {
		h.writeStreamError(w, http.StatusInternalServerError, "Copilot tools are unavailable.")
		return
	}
	if len(toolNames) > 0 && h.toolExecutor == nil {
		h.writeStreamError(w, http.StatusServiceUnavailable, "Copilot document tools are unavailable.")
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
		reservationID, err = h.sessions.ReserveFydeosManagedRequest(
			r.Context(), principal.User.ID, workspaceID, sessionID, messageID,
			provider, model, personal, time.Now(),
		)
		if errors.Is(err, ErrManagedQuotaExceeded) {
			h.writeStreamError(w, http.StatusPaymentRequired, "The daily Copilot request limit has been reached.")
			return
		}
		if err != nil {
			h.writeStreamError(w, http.StatusInternalServerError, "Copilot quota is unavailable.")
			return
		}
		if h.quotaPublisher != nil {
			h.quotaPublisher.PublishUserQuotaStateChanged(principal.User.ID, "runtime-invalidation")
		}
	}
	if err := h.streamChat(w, r, messageID, retrievalScope, messages, route, toolNames, principal.User.ID, workspaceID); err != nil {
		if reservationID != "" {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			if releaseErr := h.sessions.ReleaseFydeosManagedRequest(releaseCtx, reservationID); releaseErr != nil {
				slog.ErrorContext(releaseCtx, "managed Copilot quota reservation release failed", "error", releaseErr)
			} else if h.quotaPublisher != nil {
				h.quotaPublisher.PublishUserQuotaStateChanged(principal.User.ID, "runtime-invalidation")
			}
			cancel()
		}
		if !errors.Is(err, context.Canceled) {
			h.writeStreamError(w, http.StatusBadGateway, "The Copilot provider request failed.")
		}
	}
}

func parseCopilotToolsConfig(raw string) (chatToolsConfig, error) {
	result := chatToolsConfig{searchWorkspace: true, readingDocs: true, searchArtifacts: true, readingArtifacts: true}
	if strings.TrimSpace(raw) == "" {
		return result, nil
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &config); err != nil || config == nil {
		return chatToolsConfig{}, errors.New("invalid Copilot tools configuration")
	}
	for key, rawValue := range config {
		if key != "searchWorkspace" && key != "readingDocs" && key != "searchArtifacts" && key != "readingArtifacts" {
			return chatToolsConfig{}, fmt.Errorf("unsupported Copilot tool %q", key)
		}
		var value any
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return chatToolsConfig{}, fmt.Errorf("Copilot tool %q must be a boolean", key)
		}
		enabled, ok := value.(bool)
		if !ok {
			return chatToolsConfig{}, fmt.Errorf("Copilot tool %q must be a boolean", key)
		}
		switch key {
		case "searchWorkspace":
			result.searchWorkspace = enabled
		case "readingDocs":
			result.readingDocs = enabled
		case "searchArtifacts":
			result.searchArtifacts = enabled
		case "readingArtifacts":
			result.readingArtifacts = enabled
		}
	}
	return result, nil
}

func (h *ChatHTTPHandler) promptTools(ctx context.Context, promptName string, config chatToolsConfig) ([]string, error) {
	var raw []byte
	if err := h.db.QueryRow(ctx, `SELECT config FROM ai_prompts_metadata WHERE name=$1`, promptName).Scan(&raw); err != nil {
		return nil, err
	}
	var promptConfig struct {
		Tools []string `json:"tools"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &promptConfig); err != nil {
			return nil, err
		}
	}
	tools := make([]string, 0, 5)
	for _, tool := range promptConfig.Tools {
		switch tool {
		case "docSearch":
			if config.searchWorkspace {
				tools = append(tools, "doc_search")
			}
		case "docRead":
			if config.readingDocs {
				tools = append(tools, "doc_read")
			}
		case "docCanvasRead":
			if config.readingDocs {
				tools = append(tools, "doc_canvas_read")
			}
		case "artifactSearch":
			if config.searchArtifacts {
				tools = append(tools, "artifact_search")
			}
		case "artifactRead":
			if config.readingArtifacts {
				tools = append(tools, "artifact_read")
			}
		}
	}
	return tools, nil
}

type chatRoute struct {
	managed    bool
	source     string
	provider   string
	model      string
	endpoint   byok.Endpoint
	baseURL    string
	credential *byok.SensitiveCredential
	transport  http.RoundTripper
}

func (h *ChatHTTPHandler) resolveRoute(r *http.Request, actorID, workspaceID string) (chatRoute, error) {
	target := r.URL.Query().Get("routeTargetId")
	if target == "" || target == "fydeos-managed-openai" {
		return chatRoute{managed: true}, nil
	}
	leaseID := r.URL.Query().Get("byokLeaseId")
	profileID, modelID, local, ok := parseBYOKRouteTarget(target, leaseID)
	if !ok || workspaceID == "" || h.byok == nil {
		return chatRoute{}, byok.ErrTargetUnavailable
	}
	var resolved byok.ChatTarget
	var err error
	if local {
		resolver, ok := h.byok.(LocalChatBYOKResolver)
		if !ok {
			return chatRoute{}, byok.ErrTargetUnavailable
		}
		resolved, err = resolver.ResolveLocalChatTarget(r.Context(), workspaceID, actorID, leaseID, profileID, modelID)
	} else {
		resolved, err = h.byok.ResolveChatTarget(r.Context(), workspaceID, actorID, profileID, modelID)
	}
	if err != nil {
		return chatRoute{}, err
	}
	return chatRoute{
		source: resolved.Source, provider: resolved.Provider, model: resolved.ModelID, endpoint: resolved.Endpoint,
		baseURL: resolved.BaseURL, credential: resolved.Credential, transport: resolved.Transport,
	}, nil
}

type providerMessage struct {
	Replay         []map[string]any     `json:"-"`
	ProviderCallID string               `json:"-"`
	Role           string               `json:"role"`
	Content        any                  `json:"content"`
	Name           string               `json:"name,omitempty"`
	ToolCallID     string               `json:"tool_call_id,omitempty"`
	ToolCalls      []chatToolCall       `json:"tool_calls,omitempty"`
	Attachments    []providerAttachment `json:"-"`
}

type chatToolCall struct {
	ProviderID string `json:"-"`
	ID         string `json:"id"`
	Type       string `json:"type"`
	Function   struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type providerAttachment struct {
	FileName string `json:"fileName"`
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

func (h *ChatHTTPHandler) buildMessages(ctx context.Context, session Session, requestedMessageID string) ([]providerMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if requestedMessageID != "" {
		foundUserMessage := false
		for _, message := range session.Messages {
			if message.ID == requestedMessageID && message.Role == "user" {
				foundUserMessage = true
			}
		}
		if !foundUserMessage {
			return nil, ErrMessageNotFound
		}
	} else {
		for i := len(session.Messages) - 1; i >= 0; i-- {
			if session.Messages[i].Role == "user" {
				requestedMessageID = session.Messages[i].ID
				break
			}
		}
		if requestedMessageID == "" {
			return nil, ErrMessageNotFound
		}
	}
	rows, err := h.db.Query(ctx, `SELECT prompt.role::text,prompt.content
		FROM ai_sessions_metadata session
		JOIN ai_prompts_metadata metadata ON metadata.name=session.prompt_name
		JOIN ai_prompts_messages prompt ON prompt.prompt_id=metadata.id
		WHERE session.id=$1 ORDER BY prompt.idx`, session.ID)
	if err != nil {
		return nil, err
	}
	messages := make([]providerMessage, 0, len(session.Messages)+4)
	for rows.Next() {
		var message providerMessage
		if err := rows.Scan(&message.Role, &message.Content); err != nil {
			rows.Close()
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var totalAttachmentBytes int64
	var totalAttachmentLocators int
	for _, message := range session.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		providerMessage := providerMessage{Role: message.Role, Content: message.Content}
		if message.Role == "user" && len(message.Attachments) > 0 && string(message.Attachments) != "null" && string(message.Attachments) != "[]" {
			if h.attachments == nil {
				return nil, errors.New("Copilot attachments are unavailable")
			}
			locators, err := parseSessionAttachmentLocators(message.Attachments)
			if err != nil {
				return nil, err
			}
			if len(locators) > maxCopilotAttachmentsPerMessage {
				return nil, fmt.Errorf("Copilot message exceeds %d attachments", maxCopilotAttachmentsPerMessage)
			}
			if len(locators) > maxCopilotAttachmentLocatorsPerRequest-totalAttachmentLocators {
				return nil, fmt.Errorf("Copilot request exceeds %d attachments", maxCopilotAttachmentLocatorsPerRequest)
			}
			totalAttachmentLocators += len(locators)
			for _, locator := range locators {
				remaining := int64(maxCopilotAttachmentBytes) - totalAttachmentBytes
				if remaining <= 0 {
					return nil, errors.New("Copilot attachments exceed 16 MiB per request")
				}
				data, metadata, err := h.loadMessageAttachment(ctx, session, locator, remaining)
				if err != nil {
					return nil, err
				}
				totalAttachmentBytes += int64(len(data))
				mimeType, _, err := mime.ParseMediaType(metadata.ContentType)
				if err != nil || mimeType == "" {
					mimeType = http.DetectContentType(data)
				}
				if strings.HasPrefix(mimeType, "text/") || mimeType == "application/json" {
					if len(data) > 1<<20 {
						return nil, errors.New("text attachment is too large")
					}
					providerMessage.Content = fmt.Sprintf("%v\n\n[Attached file: %s]\n%s", providerMessage.Content, attachmentFilename(locator), string(data))
					continue
				}
				if !strings.HasPrefix(mimeType, "image/") && mimeType != "application/pdf" && !strings.HasPrefix(mimeType, "audio/") {
					return nil, fmt.Errorf("unsupported Copilot attachment type %q", mimeType)
				}
				providerMessage.Attachments = append(providerMessage.Attachments, providerAttachment{
					FileName: attachmentFilename(locator), MIMEType: mimeType, Data: base64.StdEncoding.EncodeToString(data),
				})
			}
		}
		messages = append(messages, providerMessage)
	}
	return messages, nil
}

func (h *ChatHTTPHandler) loadMessageAttachment(ctx context.Context, session Session, locator string, maxBytes int64) ([]byte, storage.ObjectMetadata, error) {
	if strings.HasPrefix(locator, "data:") {
		separator := strings.IndexByte(locator, ',')
		if separator < 0 {
			return nil, storage.ObjectMetadata{}, errors.New("invalid Copilot data attachment")
		}
		header := strings.TrimPrefix(locator[:separator], "data:")
		base64Encoded := strings.HasSuffix(strings.ToLower(header), ";base64")
		if base64Encoded {
			header = header[:len(header)-len(";base64")]
		}
		mediaType, _, err := mime.ParseMediaType(header)
		if err != nil || mediaType == "" {
			return nil, storage.ObjectMetadata{}, errors.New("invalid Copilot data attachment media type")
		}
		var data []byte
		if base64Encoded {
			payload := locator[separator+1:]
			maxAllowed := min(int64(maxSessionAttachmentBytes), maxBytes)
			if maxAllowed <= 0 || base64PayloadDecodedLength(payload) > maxAllowed {
				return nil, storage.ObjectMetadata{}, errors.New("invalid or oversized Copilot data attachment")
			}
			data, err = base64.StdEncoding.DecodeString(payload)
		} else {
			payload := locator[separator+1:]
			maxAllowed := min(int64(maxSessionAttachmentBytes), maxBytes)
			if maxAllowed <= 0 || int64(len(payload)) > maxAllowed*3 {
				return nil, storage.ObjectMetadata{}, errors.New("invalid or oversized Copilot data attachment")
			}
			var decoded string
			decoded, err = url.PathUnescape(payload)
			data = []byte(decoded)
		}
		if err != nil || int64(len(data)) > maxSessionAttachmentBytes || int64(len(data)) > maxBytes {
			return nil, storage.ObjectMetadata{}, errors.New("invalid or oversized Copilot data attachment")
		}
		return data, storage.ObjectMetadata{ContentType: mediaType, ContentLength: int64(len(data))}, nil
	}
	if strings.HasPrefix(locator, "https://") || strings.HasPrefix(locator, "gs://") {
		data, contentType, err := fetchRemoteAttachment(ctx, h.remoteAttachments, locator, maxBytes)
		if err != nil {
			return nil, storage.ObjectMetadata{}, err
		}
		return data, storage.ObjectMetadata{ContentType: contentType, ContentLength: int64(len(data))}, nil
	}
	if h.documentBlobRead != nil && session.DocID != nil && *session.DocID != "" && !strings.Contains(locator, "/") {
		body, contentType, size, err := h.documentBlobRead(ctx, session.UserID, session.WorkspaceID, *session.DocID, locator)
		if err != nil {
			return nil, storage.ObjectMetadata{}, err
		}
		if body == nil {
			return nil, storage.ObjectMetadata{}, errors.New("Copilot document attachment is unavailable")
		}
		defer body.Close()
		if size < 0 || size > maxBytes {
			return nil, storage.ObjectMetadata{}, errors.New("Copilot document attachment is too large")
		}
		data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
		if err != nil {
			return nil, storage.ObjectMetadata{}, err
		}
		if int64(len(data)) > maxBytes || int64(len(data)) != size {
			return nil, storage.ObjectMetadata{}, errors.New("Copilot document attachment size mismatch")
		}
		return data, storage.ObjectMetadata{ContentType: contentType, ContentLength: size}, nil
	}
	if h.attachments == nil {
		return nil, storage.ObjectMetadata{}, errors.New("Copilot attachments are unavailable")
	}
	return h.attachments.LoadForProvider(ctx, session.UserID, session.ID, session.WorkspaceID, locator, maxBytes)
}

func base64PayloadDecodedLength(payload string) int64 {
	if len(payload)%4 != 0 {
		return int64(maxSessionAttachmentBytes) + 1
	}
	decodedLength := int64(len(payload) / 4 * 3)
	if strings.HasSuffix(payload, "==") {
		decodedLength -= 2
	} else if strings.HasSuffix(payload, "=") {
		decodedLength--
	}
	return decodedLength
}

func parseSessionAttachmentLocators(raw json.RawMessage) ([]string, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, errors.New("invalid Copilot attachment list")
	}
	locators := make([]string, 0, len(entries))
	for _, entry := range entries {
		var locator string
		if err := json.Unmarshal(entry, &locator); err != nil {
			var object struct {
				URL        string `json:"url"`
				Attachment string `json:"attachment"`
			}
			if err := json.Unmarshal(entry, &object); err != nil {
				return nil, errors.New("invalid Copilot attachment locator")
			}
			locator = object.URL
			if locator == "" {
				locator = object.Attachment
			}
		}
		if locator == "" {
			return nil, errors.New("invalid Copilot attachment locator")
		}
		locators = append(locators, locator)
	}
	return locators, nil
}

func providerText(value any) string {
	text, _ := value.(string)
	return text
}

func openAIChatContent(message providerMessage) (any, error) {
	if len(message.Attachments) == 0 {
		return providerText(message.Content), nil
	}
	parts := []map[string]any{{"type": "text", "text": providerText(message.Content)}}
	for _, attachment := range message.Attachments {
		if !strings.HasPrefix(attachment.MIMEType, "image/") {
			return nil, fmt.Errorf("OpenAI chat completions do not support attachment type %q", attachment.MIMEType)
		}
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": "data:" + attachment.MIMEType + ";base64," + attachment.Data},
		})
	}
	return parts, nil
}

func openAIResponsesContent(message providerMessage) (any, error) {
	if len(message.Attachments) == 0 {
		return providerText(message.Content), nil
	}
	parts := []map[string]any{{"type": "input_text", "text": providerText(message.Content)}}
	for _, attachment := range message.Attachments {
		dataURL := "data:" + attachment.MIMEType + ";base64," + attachment.Data
		if strings.HasPrefix(attachment.MIMEType, "image/") {
			parts = append(parts, map[string]any{"type": "input_image", "image_url": dataURL})
			continue
		}
		if attachment.MIMEType == "application/pdf" {
			parts = append(parts, map[string]any{"type": "input_file", "filename": attachment.FileName, "file_data": dataURL})
			continue
		}
		return nil, fmt.Errorf("OpenAI Responses does not support attachment type %q", attachment.MIMEType)
	}
	return parts, nil
}

func anthropicContent(message providerMessage) (any, error) {
	if len(message.Attachments) == 0 {
		return providerText(message.Content), nil
	}
	parts := []map[string]any{{"type": "text", "text": providerText(message.Content)}}
	for _, attachment := range message.Attachments {
		source := map[string]string{"type": "base64", "media_type": attachment.MIMEType, "data": attachment.Data}
		switch {
		case strings.HasPrefix(attachment.MIMEType, "image/"):
			parts = append(parts, map[string]any{"type": "image", "source": source})
		case attachment.MIMEType == "application/pdf":
			parts = append(parts, map[string]any{"type": "document", "source": source})
		default:
			return nil, fmt.Errorf("Anthropic Messages does not support attachment type %q", attachment.MIMEType)
		}
	}
	return parts, nil
}

func geminiContent(message providerMessage) []map[string]any {
	parts := []map[string]any{{"text": providerText(message.Content)}}
	for _, attachment := range message.Attachments {
		parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": attachment.MIMEType, "data": attachment.Data}})
	}
	return parts
}

func attachmentFilename(locator string) string {
	parsed, err := url.Parse(locator)
	if err != nil {
		return "attachment"
	}
	if strings.HasPrefix(locator, "data:") {
		return "attachment"
	}
	if name := parsed.Query().Get("fileName"); name != "" {
		return filepath.Base(name)
	}
	if name := remoteAttachmentFilename(locator); name != "" && name != "." {
		return name
	}
	return "attachment"
}

func (h *ChatHTTPHandler) streamChat(w http.ResponseWriter, r *http.Request, messageID string, retrievalScope ArtifactRetrievalScope, messages []providerMessage, route chatRoute, toolNames []string, userID, workspaceID string) error {
	client := h.client
	if route.transport != nil {
		client = &http.Client{Transport: route.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if route.endpoint.Kind == "openai_compatible" {
			defer client.CloseIdleConnections()
		}
	}
	allowedTools := make(map[string]struct{}, len(toolNames))
	for _, name := range toolNames {
		allowedTools[name] = struct{}{}
	}
	wroteHeaders := false
	flusher, _ := w.(http.Flusher)
	var answer strings.Builder
	streamObjects := make([]map[string]string, 0)
	toolCallsExecuted := 0

	for round := 0; round <= 3; round++ {
		endpointURL, headers, body, streamFormat, err := h.buildProviderRequest(route, messages, toolNames)
		if err != nil {
			return err
		}
		endpoint, err := url.Parse(endpointURL)
		if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Host == "" {
			return errors.New("invalid Copilot endpoint")
		}
		if len(body) > maxCopilotRequestBytes {
			return errors.New("Copilot request is too large")
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if err != nil {
			return err
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_ = response.Body.Close()
			return fmt.Errorf("Copilot provider returned status %d", response.StatusCode)
		}
		if !wroteHeaders {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-transform")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			wroteHeaders = true
		}
		limitedResponse := &io.LimitedReader{R: response.Body, N: maxCopilotResponseBytes + 1}
		scanner := bufio.NewScanner(limitedResponse)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		var deltas []string
		toolCallDeltas := make(map[int]*chatToolCall)
		var providerTurn providerToolTurn
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			if err := providerTurn.consume(streamFormat, data); err != nil {
				_ = response.Body.Close()
				return err
			}
			switch streamFormat {
			case "openai-chat":
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content   string `json:"content"`
							ToolCalls []struct {
								Index    int    `json:"index"`
								ID       string `json:"id"`
								Type     string `json:"type"`
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(data), &chunk) != nil {
					continue
				}
				for _, choice := range chunk.Choices {
					if choice.Delta.Content != "" {
						deltas = append(deltas, choice.Delta.Content)
					}
					for _, fragment := range choice.Delta.ToolCalls {
						call := toolCallDeltas[fragment.Index]
						if call == nil {
							call = &chatToolCall{Type: "function"}
							toolCallDeltas[fragment.Index] = call
						}
						call.ID += fragment.ID
						if fragment.Type != "" {
							call.Type = fragment.Type
						}
						call.Function.Name += fragment.Function.Name
						call.Function.Arguments += fragment.Function.Arguments
					}
				}
			case "openai-responses":
				var event struct {
					Type  string `json:"type"`
					Delta string `json:"delta"`
				}
				if json.Unmarshal([]byte(data), &event) == nil && event.Type == "response.output_text.delta" && event.Delta != "" {
					deltas = append(deltas, event.Delta)
				}
			case "anthropic":
				var event struct {
					Type  string `json:"type"`
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(data), &event) == nil && event.Type == "content_block_delta" && event.Delta.Type == "text_delta" && event.Delta.Text != "" {
					deltas = append(deltas, event.Delta.Text)
				}
			case "gemini":
				var event struct {
					Candidates []struct {
						Index   int `json:"index"`
						Content struct {
							Parts []struct {
								Text    string `json:"text"`
								Thought bool   `json:"thought"`
							} `json:"parts"`
						} `json:"content"`
					} `json:"candidates"`
				}
				if json.Unmarshal([]byte(data), &event) == nil {
					for _, candidate := range event.Candidates {
						if candidate.Index != 0 {
							continue
						}
						for _, part := range candidate.Content.Parts {
							if part.Text != "" && !part.Thought {
								deltas = append(deltas, part.Text)
							}
						}
					}
				}
			}
		}
		closeErr := response.Body.Close()
		if err := scanner.Err(); err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if limitedResponse.N == 0 {
			return errors.New("Copilot provider response is too large")
		}
		if err := providerTurn.finish(streamFormat); err != nil {
			return err
		}
		for index, call := range providerTurn.calls {
			toolCallDeltas[index] = call
		}
		if len(toolCallDeltas) > 0 {
			if round == 3 || len(toolCallDeltas) > 8-toolCallsExecuted {
				return errors.New("Copilot tool call limit exceeded")
			}
			calls := make([]chatToolCall, 0, len(toolCallDeltas))
			for index := 0; index < len(toolCallDeltas); index++ {
				call := toolCallDeltas[index]
				if call == nil {
					return errors.New("invalid Copilot tool call sequence")
				}
				if _, ok := allowedTools[call.Function.Name]; !ok || call.Type != "function" || call.ID == "" || len(call.ID) > 128 || len(call.Function.Name) > 64 || len(call.Function.Arguments) > 16<<10 {
					return errors.New("invalid Copilot tool call")
				}
				calls = append(calls, *call)
			}
			messages = append(messages, providerMessage{Role: "assistant", Content: strings.Join(deltas, ""), ToolCalls: calls, Replay: providerTurn.replay})
			for _, call := range calls {
				toolCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
				var result any
				var err error
				if scoped, ok := h.toolExecutor.(scopedChatToolExecutor); ok {
					result, err = scoped.ExecuteTurn(toolCtx, userID, workspaceID, messageID, retrievalScope, call.Function.Name, json.RawMessage(call.Function.Arguments))
				} else {
					result, err = h.toolExecutor.Execute(toolCtx, userID, workspaceID, call.Function.Name, json.RawMessage(call.Function.Arguments))
				}
				cancel()
				if err != nil {
					result = map[string]any{"error": "The requested document operation was denied or unavailable."}
				}
				encodedResult, err := json.Marshal(result)
				if err != nil {
					return errors.New("Copilot tool result could not be encoded")
				}
				if len(encodedResult) > maxChatToolOutputBytes {
					return errors.New("Copilot tool result is too large")
				}
				messages = append(messages, providerMessage{Role: "tool", Name: call.Function.Name, ToolCallID: call.ID, ProviderCallID: call.ProviderID, Content: string(encodedResult)})
				toolCallsExecuted++
			}
			continue
		}
		for _, delta := range deltas {
			if answer.Len()+len(delta) > maxCopilotResponseBytes {
				return errors.New("Copilot provider response is too large")
			}
			answer.WriteString(delta)
			streamObject := map[string]string{"type": "text-delta", "textDelta": delta}
			streamObjects = append(streamObjects, streamObject)
			encoded, _ := json.Marshal(streamObject)
			if _, err := fmt.Fprintf(w, "event: message\nid: %s\ndata: %s\n\n", messageID, encoded); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		return h.persistAssistant(r.Context(), r.PathValue("sessionId"), messageID, userID, workspaceID, route, answer.String(), streamObjects)
	}
	return errors.New("Copilot tool execution did not complete")
}

func artifactScopeForMessage(session Session, messageID string) ArtifactRetrievalScope {
	for _, message := range session.Messages {
		if message.ID != messageID || len(message.ScopeSnapshot) == 0 || string(message.ScopeSnapshot) == "null" {
			continue
		}
		var snapshot struct {
			Retrieval ArtifactRetrievalScope `json:"retrieval"`
		}
		if json.Unmarshal(message.ScopeSnapshot, &snapshot) == nil && snapshot.Retrieval.Mode == "required" {
			return snapshot.Retrieval
		}
		break
	}
	return ArtifactRetrievalScope{Mode: "workspace"}
}
func (h *ChatHTTPHandler) buildProviderRequest(route chatRoute, messages []providerMessage, enabledTools ...[]string) (string, map[string]string, []byte, string, error) {
	provider := route.provider
	model := route.model
	baseURL := route.baseURL
	credential := ""
	if route.managed {
		provider, model, baseURL = "openai", h.cfg.FydeOSManagedCopilotModel, h.cfg.FydeOSManagedCopilotBaseURL
		credential = h.cfg.FydeOSManagedCopilotAPIKey
	} else if route.credential != nil {
		credential = string(route.credential.Bytes())
	}
	headers := map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream"}
	body := map[string]any{}
	streamFormat := ""
	path := ""
	switch provider {
	case "openai":
		headers["Authorization"] = "Bearer " + credential
		dialect := route.endpoint.Dialect
		if route.managed {
			dialect = "chat_completions"
		} else if route.endpoint.Kind == "provider_default" || dialect == "" {
			dialect = "responses"
		}
		if dialect == "responses" {
			input := make([]map[string]any, 0, len(messages))
			for _, message := range messages {
				if len(message.Replay) > 0 {
					input = append(input, message.Replay...)
					continue
				}
				if message.Role == "tool" {
					input = append(input, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": providerText(message.Content)})
					continue
				}
				content, err := openAIResponsesContent(message)
				if err != nil {
					return "", nil, nil, "", err
				}
				input = append(input, map[string]any{"role": message.Role, "content": content})
			}
			body = map[string]any{"model": model, "input": input, "stream": true}
			if len(enabledTools) > 0 && len(enabledTools[0]) > 0 {
				body["tools"] = providerTools(enabledTools[0], "openai-responses")
				body["store"] = false
				body["include"] = []string{"reasoning.encrypted_content"}
			}
			path, streamFormat = "/responses", "openai-responses"
		} else if dialect == "chat_completions" {
			chatMessages := make([]map[string]any, 0, len(messages))
			for _, message := range messages {
				if message.Role == "assistant" && len(message.ToolCalls) > 0 {
					chatMessages = append(chatMessages, map[string]any{"role": message.Role, "content": providerText(message.Content), "tool_calls": message.ToolCalls})
					continue
				}
				content, err := openAIChatContent(message)
				if err != nil {
					return "", nil, nil, "", err
				}
				item := map[string]any{"role": message.Role, "content": content}
				if message.Name != "" {
					item["name"] = message.Name
				}
				if message.ToolCallID != "" {
					item["tool_call_id"] = message.ToolCallID
				}
				chatMessages = append(chatMessages, item)
			}
			body = map[string]any{"model": model, "messages": chatMessages, "stream": true}
			if len(enabledTools) > 0 && len(enabledTools[0]) > 0 {
				body["tools"] = openAIChatTools(enabledTools[0])
				body["tool_choice"] = "auto"
			}
			path, streamFormat = "/chat/completions", "openai-chat"
		} else {
			return "", nil, nil, "", errors.New("unsupported OpenAI dialect")
		}
	case "anthropic":
		var system strings.Builder
		conversation := make([]map[string]any, 0, len(messages))
		for _, message := range messages {
			if message.Role == "system" {
				if system.Len() > 0 {
					system.WriteString("\n\n")
				}
				system.WriteString(providerText(message.Content))
				continue
			}
			if len(message.Replay) > 0 {
				conversation = append(conversation, map[string]any{"role": "assistant", "content": message.Replay})
				continue
			}
			if message.Role == "tool" {
				block := map[string]any{"type": "tool_result", "tool_use_id": message.ToolCallID, "content": providerText(message.Content)}
				appendProviderToolResult(&conversation, "user", "content", block)
				continue
			}
			if message.Role == "assistant" || message.Role == "user" {
				content, err := anthropicContent(message)
				if err != nil {
					return "", nil, nil, "", err
				}
				conversation = append(conversation, map[string]any{"role": message.Role, "content": content})
			}
		}
		body = map[string]any{"model": model, "max_tokens": 4096, "messages": conversation, "stream": true}
		if len(enabledTools) > 0 && len(enabledTools[0]) > 0 {
			body["tools"] = providerTools(enabledTools[0], "anthropic")
		}
		if system.Len() > 0 {
			body["system"] = system.String()
		}
		headers["x-api-key"], headers["anthropic-version"] = credential, "2023-06-01"
		path, streamFormat = "/v1/messages", "anthropic"
	case "gemini":
		var systemParts []map[string]string
		contents := make([]map[string]any, 0, len(messages))
		for _, message := range messages {
			if message.Role == "system" {
				systemParts = append(systemParts, map[string]string{"text": providerText(message.Content)})
				continue
			}
			if len(message.Replay) > 0 {
				contents = append(contents, map[string]any{"role": "model", "parts": message.Replay})
				continue
			}
			if message.Role == "tool" {
				var result any
				if err := json.Unmarshal([]byte(providerText(message.Content)), &result); err != nil {
					return "", nil, nil, "", err
				}
				response := map[string]any{"name": message.Name, "response": result}
				if message.ProviderCallID != "" {
					response["id"] = message.ProviderCallID
				}
				appendProviderToolResult(&contents, "user", "parts", map[string]any{"functionResponse": response})
				continue
			}
			role := "user"
			if message.Role == "assistant" {
				role = "model"
			}
			parts := geminiContent(message)
			contents = append(contents, map[string]any{"role": role, "parts": parts})
		}
		body = map[string]any{"contents": contents}
		if len(enabledTools) > 0 && len(enabledTools[0]) > 0 {
			body["tools"] = []any{map[string]any{"functionDeclarations": providerTools(enabledTools[0], "gemini")}}
		}
		if len(systemParts) > 0 {
			body["systemInstruction"] = map[string]any{"parts": systemParts}
		}
		headers["x-goog-api-key"] = credential
		path, streamFormat = "/models/"+url.PathEscape(model)+":streamGenerateContent?alt=sse", "gemini"
	default:
		return "", nil, nil, "", errors.New("unsupported BYOK chat provider")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", nil, nil, "", err
	}
	return strings.TrimRight(baseURL, "/") + path, headers, encoded, streamFormat, nil
}

func openAIChatTools(names []string) []map[string]any {
	definitions := map[string]map[string]any{
		"doc_search": {
			"type": "function",
			"function": map[string]any{
				"name":        "doc_search",
				"description": "Search persisted documents in the current workspace. Results are limited to documents the user can read.",
				"parameters": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"query":   map[string]any{"type": "string", "minLength": 1, "maxLength": maxChatDocSearchQuery},
						"doc_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 50},
						"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": maxChatDocSearchHits},
					}, "required": []string{"query"},
				},
			},
		},
		"doc_read": {
			"type": "function",
			"function": map[string]any{
				"name":        "doc_read",
				"description": "Read persisted Page-mode Markdown from a document the user can access in the current workspace.",
				"parameters": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"doc_id":    map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
						"max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": maxChatDocReadChars},
					}, "required": []string{"doc_id"},
				},
			},
		},
		"doc_canvas_read": {
			"type": "function",
			"function": map[string]any{
				"name":        "doc_canvas_read",
				"description": "Read bounded structure from a persisted document canvas. Use overview first, then a frame, element IDs, or a region. Cursors are revision-bound.",
				"parameters": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"doc_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
						"target": map[string]any{"oneOf": []any{
							map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"const": "overview"}}, "required": []string{"kind"}, "additionalProperties": false},
							map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"const": "frame"}, "frame_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}}, "required": []string{"kind", "frame_id"}, "additionalProperties": false},
							map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"const": "elements"}, "element_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "minItems": 1, "maxItems": maxCanvasReadLimit}}, "required": []string{"kind", "element_ids"}, "additionalProperties": false},
							map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"const": "region"}, "bounds": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "number", "minimum": -10000000, "maximum": 10000000}, "y": map[string]any{"type": "number", "minimum": -10000000, "maximum": 10000000}, "width": map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 10000000}, "height": map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 10000000}}, "required": []string{"x", "y", "width", "height"}, "additionalProperties": false}}, "required": []string{"kind", "bounds"}, "additionalProperties": false},
						}},
						"cursor": map[string]any{"type": "string", "maxLength": 2048},
						"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": maxCanvasReadLimit},
					}, "required": []string{"doc_id", "target"},
				},
			},
		},
		"artifact_search": {
			"type": "function",
			"function": map[string]any{
				"name":        "artifact_search",
				"description": "Search extracted text from ready workspace library artifacts. Message attachments are excluded.",
				"parameters": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"query": map[string]any{"type": "string", "minLength": 1, "maxLength": maxChatDocSearchQuery},
						"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxChatDocSearchHits},
					}, "required": []string{"query"},
				},
			},
		},
		"artifact_read": {
			"type": "function",
			"function": map[string]any{
				"name":        "artifact_read",
				"description": "Read extracted text from a ready workspace library artifact. Use cursor to continue a truncated result.",
				"parameters": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"artifact_id": map[string]any{"type": "string", "format": "uuid"},
						"max_chars":   map[string]any{"type": "integer", "minimum": 1, "maximum": 100000},
						"cursor":      map[string]any{"type": "string", "maxLength": 128},
					}, "required": []string{"artifact_id"},
				},
			},
		},
	}
	result := make([]map[string]any, 0, len(names))
	for _, name := range names {
		if definition, ok := definitions[name]; ok {
			result = append(result, definition)
		}
	}
	return result
}

func (h *ChatHTTPHandler) persistAssistant(ctx context.Context, sessionID, messageID, userID, workspaceID string, route chatRoute, content string, streamObjects []map[string]string) error {
	streamJSON, err := json.Marshal(streamObjects)
	if err != nil {
		return err
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO ai_sessions_messages(id,session_id,role,content,"streamObjects",attachments,created_at,updated_at)
		VALUES($1,$2,'assistant',$3,$4,'[]',now(),now())`, uuid.NewString(), sessionID, content, streamJSON)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE ai_sessions_metadata SET updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, sessionID)
	if err != nil {
		return err
	}
	if route.source == "byok_server" || route.source == "byok_local" {
		_, err = tx.Exec(ctx, `INSERT INTO ai_usage_events(id,workspace_id,user_id,provider,provider_source,billing_unit_id,feature_kind,model,session_id,created_at)
			VALUES($1,$2,$3,$4,$5,$6,'chat',$7,$8,now())`, uuid.NewString(), workspaceID, userID, route.provider, route.source, messageID, route.model, sessionID)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (h *ChatHTTPHandler) writeStreamError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.WriteHeader(http.StatusOK)
	payload, _ := json.Marshal(map[string]any{"status": status, "message": message})
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
}
