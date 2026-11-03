// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package mcphttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/mcpcredentials"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

const (
	maxRequestBodyBytes    = 100 << 10
	maxBatchSize           = 20
	maxConcurrentRequests  = 32
	requestBodyReadTimeout = 5 * time.Second
	defaultProtocol        = "2025-03-26"
)

var supportedProtocolVersions = map[string]struct{}{
	"2025-11-25": {},
	"2025-06-18": {},
	"2025-03-26": {},
	"2024-11-05": {},
	"2024-10-07": {},
}

type CredentialService interface {
	CopilotEnabled(context.Context) (bool, error)
	Authenticate(context.Context, string, string) (*mcpcredentials.AuthenticatedCredential, error)
}

type WorkspaceAuthorizer interface {
	GetWorkspaceAccess(context.Context, string, string) (workspaces.WorkspaceAccess, error)
	AuthorizeDocument(context.Context, string, string, string) (workspaces.DocumentAuthorization, error)
}

type WorkspaceDocumentReader interface {
	DiffWorkspace(context.Context, string, string, []byte) ([]byte, []byte, int64, error)
}

type Handler struct {
	credentials     CredentialService
	authorizer      WorkspaceAuthorizer
	documents       WorkspaceDocumentReader
	requestSlots    chan struct{}
	bodyReadTimeout time.Duration
}

func NewHandler(credentials CredentialService, authorizer WorkspaceAuthorizer, reader WorkspaceDocumentReader) http.Handler {
	return &Handler{
		credentials:     credentials,
		authorizer:      authorizer,
		documents:       reader,
		requestSlots:    make(chan struct{}, maxConcurrentRequests),
		bodyReadTimeout: requestBodyReadTimeout,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reject := func(status int, id any, code int, message string) {
		closeUnreadRequestBody(w, r)
		writeJSONRPCError(w, status, id, code, message)
	}
	workspaceID := r.PathValue("id")
	if workspaceID == "" {
		closeUnreadRequestBody(w, r)
		writeJSON(w, http.StatusNotFound, nil)
		return
	}
	if h.credentials == nil || h.authorizer == nil || h.documents == nil {
		reject(http.StatusInternalServerError, nil, -32603, "Internal error")
		return
	}

	enabled, err := h.credentials.CopilotEnabled(r.Context())
	if err != nil {
		reject(http.StatusInternalServerError, nil, -32603, "Internal error")
		return
	}
	if !enabled {
		reject(http.StatusForbidden, nil, -32000, "Access denied")
		return
	}

	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		reject(http.StatusMethodNotAllowed, nil, -32000, "Method not allowed.")
		return
	}
	if r.Method != http.MethodPost {
		reject(http.StatusMethodNotAllowed, nil, -32000, "Method not allowed.")
		return
	}

	token := extractBearerToken(r.Header.Get("Authorization"))
	credential, err := h.credentials.Authenticate(r.Context(), token, workspaceID)
	if err != nil {
		if errors.Is(err, mcpcredentials.ErrUnauthorized) {
			reject(http.StatusUnauthorized, nil, -32000, "Authentication failed")
			return
		}
		if errors.Is(err, mcpcredentials.ErrCopilotDisabled) {
			reject(http.StatusForbidden, nil, -32000, "Access denied")
			return
		}
		reject(http.StatusInternalServerError, nil, -32603, "Internal error")
		return
	}

	access, err := h.authorizer.GetWorkspaceAccess(r.Context(), credential.UserID, workspaceID)
	if err != nil || !access.Permissions["Workspace_Read"] {
		if err != nil && !errors.Is(err, workspaces.ErrAccessDenied) && !errors.Is(err, workspaces.ErrNotFound) {
			reject(http.StatusInternalServerError, nil, -32603, "Internal error")
			return
		}
		reject(http.StatusForbidden, nil, -32000, "Access denied")
		return
	}

	select {
	case h.requestSlots <- struct{}{}:
		defer func() { <-h.requestSlots }()
	default:
		reject(http.StatusTooManyRequests, nil, -32000, "Too many requests.")
		return
	}

	body, err := readRequestBodyWithTimeout(w, r, h.bodyReadTimeout)
	if err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, -32600, "Invalid Request")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var request any
	if err := decoder.Decode(&request); err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, -32600, "Invalid Request")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSONRPCError(w, http.StatusBadRequest, nil, -32600, "Invalid Request")
		return
	}

	isBatch := false
	messages := []any{request}
	if batch, ok := request.([]any); ok {
		isBatch = true
		messages = batch
	}
	if len(messages) == 0 {
		writeJSONRPCError(w, http.StatusBadRequest, nil, -32600, "Invalid Request")
		return
	}
	if len(messages) > maxBatchSize {
		writeJSONRPCError(w, http.StatusBadRequest, nil, -32600, "Batch size exceeds limit (20).")
		return
	}

	responses := make([]jsonRPCResponse, 0, len(messages))
	for _, message := range messages {
		response := h.handleMessage(r.Context(), message, credential, workspaceID)
		if response != nil {
			responses = append(responses, *response)
		}
	}
	if len(responses) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if isBatch {
		writeJSON(w, http.StatusOK, responses)
		return
	}
	writeJSON(w, http.StatusOK, responses[0])
}

type jsonRPCResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	Result  map[string]any `json:"result,omitempty"`
	Error   *jsonRPCError  `json:"error,omitempty"`
	ID      any            `json:"id"`
}

func (response jsonRPCResponse) MarshalJSON() ([]byte, error) {
	type encodedResponse struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *jsonRPCError   `json:"error,omitempty"`
		ID      any             `json:"id"`
	}
	encoded := encodedResponse{JSONRPC: response.JSONRPC, Error: response.Error, ID: response.ID}
	if response.Result != nil {
		result, err := json.Marshal(response.Result)
		if err != nil {
			return nil, err
		}
		encoded.Result = result
	}
	return json.Marshal(encoded)
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (h *Handler) handleMessage(ctx context.Context, message any, credential *mcpcredentials.AuthenticatedCredential, workspaceID string) *jsonRPCResponse {
	request, ok := message.(map[string]any)
	if !ok || request["jsonrpc"] != "2.0" {
		return rpcError(nil, -32600, "Invalid Request")
	}
	method, ok := request["method"].(string)
	if !ok {
		return rpcError(nil, -32600, "Invalid Request")
	}
	id, hasID := request["id"]
	if hasID && !validRequestID(id) {
		return rpcError(nil, -32600, "Invalid Request")
	}
	notification := !hasID
	if id == nil {
		hasID = true
	}

	switch method {
	case "initialize":
		params, _ := request["params"].(map[string]any)
		version := defaultProtocol
		if params != nil {
			if requested, ok := params["protocolVersion"].(string); ok && requested != "" {
				if _, supported := supportedProtocolVersions[requested]; supported {
					version = requested
				}
			}
		}
		if notification {
			return nil
		}
		return rpcResult(id, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "FydeOS Notes MCP Server for Workspace " + workspaceID,
				"version": "1.0.1",
			},
		})
	case "notifications/initialized", "ping":
		if notification {
			return nil
		}
		return rpcResult(id, map[string]any{})
	case "tools/list":
		if notification {
			return nil
		}
		return rpcResult(id, map[string]any{"tools": h.tools()})
	case "tools/call":
		params, ok := request["params"].(map[string]any)
		if !ok {
			return rpcError(id, -32602, "Invalid params")
		}
		name, ok := params["name"].(string)
		if !ok {
			return rpcError(id, -32602, "Invalid params")
		}
		if !h.hasTool(name) {
			return rpcError(id, -32602, "Tool not found: "+name)
		}
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result, err := h.callTool(ctx, name, args, credential.UserID, workspaceID)
		if err != nil {
			return rpcError(id, -32001, "Error executing tool: "+err.Error())
		}
		if notification {
			return nil
		}
		return rpcResult(id, result)
	default:
		if notification {
			return nil
		}
		return rpcError(id, -32601, "Method not found")
	}
}

func (h *Handler) tools() []map[string]any {
	return []map[string]any{
		{
			"name":        "read_document",
			"title":       "Read Document",
			"description": "Read a document with given ID",
			"inputSchema": map[string]any{
				"type": "object", "properties": map[string]any{"docId": map[string]any{"type": "string"}},
				"required": []string{"docId"}, "additionalProperties": false,
			},
		},
		{
			"name":        "doc_search",
			"title":       "Document Search",
			"description": "Search persisted workspace documents and return bounded passages with Page or canvas locators. Retrieval strategy is selected by the server and never includes files, blobs, attachments, or the web.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":   map[string]any{"type": "string"},
					"doc_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 50},
					"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 20},
				},
				"required": []string{"query"}, "additionalProperties": false,
			},
		},
	}
}

func (h *Handler) hasTool(name string) bool {
	return name == "read_document" || name == "doc_search"
}

func (h *Handler) callTool(ctx context.Context, name string, args map[string]any, userID, workspaceID string) (map[string]any, error) {
	switch name {
	case "read_document":
		docID, ok := args["docId"].(string)
		if !ok {
			return toolError("Invalid arguments: docId: Required"), nil
		}
		content, found, err := h.readDocument(ctx, userID, workspaceID, docID)
		if err != nil {
			return nil, err
		}
		if !found {
			return toolError("Doc with id " + docID + " not found."), nil
		}
		return toolText(content), nil
	case "doc_search":
		return h.searchDocuments(ctx, args, userID, workspaceID)
	default:
		return nil, errors.New("Tool not found: " + name)
	}
}

func (h *Handler) readDocument(ctx context.Context, userID, workspaceID, docID string) (string, bool, error) {
	access, err := h.authorizer.AuthorizeDocument(ctx, userID, workspaceID, docID)
	if errors.Is(err, workspaces.ErrNotFound) || (err == nil && !access.Read) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	update, _, _, err := h.documents.DiffWorkspace(ctx, workspaceID, docID, nil)
	if errors.Is(err, documents.ErrWorkspaceDocNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	projected, found, err := documents.ProjectMarkdown([][]byte{update})
	if err != nil {
		return "", false, err
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return projected.Markdown, found, nil
}

func (h *Handler) searchDocuments(ctx context.Context, args map[string]any, userID, workspaceID string) (map[string]any, error) {
	query, ok := args["query"].(string)
	if !ok || utf16Length(strings.TrimSpace(query)) < 1 || utf16Length(strings.TrimSpace(query)) > 2000 {
		return toolError("Invalid arguments: query: String must contain at least 1 character(s)"), nil
	}
	query = strings.TrimSpace(query)
	limit := 10
	if raw, exists := args["limit"]; exists {
		number, ok := raw.(json.Number)
		if !ok {
			return toolError("Invalid arguments: limit: Expected number, received nan"), nil
		}
		parsed, err := number.Int64()
		if err != nil || parsed < 1 || parsed > 20 {
			return toolError("Invalid arguments: limit: Number must be greater than or equal to 1"), nil
		}
		limit = int(parsed)
	}
	requestedIDs, err := parseDocumentIDs(args["doc_ids"])
	if err != nil {
		return toolError(err.Error()), nil
	}
	if len(requestedIDs) == 0 {
		return toolError("SEARCH_UNAVAILABLE"), nil
	}

	readable := make([]documents.MarkdownDocument, 0, min(limit, len(requestedIDs)))
	readableIDs := make([]string, 0, cap(readable))
	seen := make(map[string]struct{}, len(requestedIDs))
	for _, docID := range requestedIDs {
		if _, duplicate := seen[docID]; duplicate {
			continue
		}
		seen[docID] = struct{}{}
		access, err := h.authorizer.AuthorizeDocument(ctx, userID, workspaceID, docID)
		if errors.Is(err, workspaces.ErrNotFound) || (err == nil && !access.Read) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		update, _, _, err := h.documents.DiffWorkspace(ctx, workspaceID, docID, nil)
		if errors.Is(err, documents.ErrWorkspaceDocNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		projected, found, err := documents.ProjectMarkdown([][]byte{update})
		if err != nil {
			return nil, err
		}
		if found {
			readableIDs = append(readableIDs, docID)
			readable = append(readable, projected)
			if len(readable) == limit {
				break
			}
		}
	}

	maxChars := 20000 / max(len(readable), 1)
	hits := make([]map[string]any, 0, len(readable))
	for index, doc := range readable {
		hits = append(hits, map[string]any{
			"doc_id":     readableIDs[index],
			"title":      doc.Title,
			"excerpt":    truncateUTF16(doc.Markdown, maxChars),
			"visibility": "page",
		})
	}
	return toolTextJSON(map[string]any{
		"retrieval_mode":  "scoped",
		"degraded_reason": "SEARCH_UNAVAILABLE",
		"hits":            hits,
	}), nil
}

func parseDocumentIDs(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("Invalid arguments: doc_ids: Expected array, received object")
	}
	if len(items) > 50 {
		return nil, errors.New("Invalid arguments: doc_ids: Array must contain at most 50 element(s)")
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		id, ok := item.(string)
		if !ok || utf16Length(id) < 1 || utf16Length(id) > 128 {
			return nil, errors.New("Invalid arguments: doc_ids: Invalid input")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func validRequestID(value any) bool {
	switch value.(type) {
	case nil, string, json.Number:
		return true
	default:
		return false
	}
}

func extractBearerToken(value string) string {
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func readRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return readRequestBodyWithTimeout(w, r, requestBodyReadTimeout)
}

func closeUnreadRequestBody(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil || r.Body == http.NoBody {
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	w.Header().Set("Connection", "close")
}

func readRequestBodyWithTimeout(w http.ResponseWriter, r *http.Request, timeout time.Duration) ([]byte, error) {
	controller := http.NewResponseController(w)
	deadlineSet := false
	if err := controller.SetReadDeadline(time.Now().Add(timeout)); err == nil {
		deadlineSet = true
	} else if !errors.Is(err, http.ErrNotSupported) {
		w.Header().Set("Connection", "close")
		return nil, err
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	timer := time.AfterFunc(timeout, func() { _ = r.Body.Close() })
	defer timer.Stop()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if deadlineSet {
			_ = controller.SetReadDeadline(time.Now())
		}
		w.Header().Set("Connection", "close")
		return nil, err
	}
	if deadlineSet {
		if err := controller.SetReadDeadline(time.Time{}); err != nil {
			_ = controller.SetReadDeadline(time.Now())
			w.Header().Set("Connection", "close")
			return nil, err
		}
	}
	return body, nil
}

func rpcResult(id any, result map[string]any) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", Result: result, ID: id}
}

func rpcError(id any, code int, message string) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", Error: &jsonRPCError{Code: code, Message: message}, ID: id}
}

func writeJSONRPCError(w http.ResponseWriter, status int, id any, code int, message string) {
	writeJSON(w, status, rpcError(id, code, message))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func toolText(value string) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": value}}}
}

func toolTextJSON(value any) map[string]any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return toolError("Internal error")
	}
	return toolText(string(encoded))
}

func toolError(value string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []map[string]string{{"type": "text", "text": value}},
	}
}

func utf16Length(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func truncateUTF16(value string, maxUnits int) string {
	if maxUnits <= 0 {
		return ""
	}
	units := 0
	for index, character := range value {
		characterUnits := 1
		if character > 0xffff {
			characterUnits = 2
		}
		if units+characterUnits > maxUnits {
			return value[:index]
		}
		units += characterUnits
	}
	return value
}
