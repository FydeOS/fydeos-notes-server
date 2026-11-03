// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Provider replay is retained only for the current request, including opaque
// reasoning/signature fields required to continue a provider's tool turn.
type providerToolTurn struct {
	calls      []*chatToolCall
	replay     []map[string]any
	blocks     map[int]map[string]any
	arguments  map[int]string
	stopped    map[int]bool
	sawTool    bool
	complete   bool
	stopReason string
}

func (t *providerToolTurn) consume(format, data string) error {
	if format == "openai-chat" {
		return nil
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return errors.New("invalid Copilot provider event")
	}
	kind := rawString(event["type"])
	if kind == "error" || kind == "response.failed" || kind == "response.incomplete" || event["error"] != nil {
		return errors.New("Copilot provider stream failed")
	}
	if t.complete {
		return errors.New("Copilot data received after completion")
	}
	switch format {
	case "openai-responses":
		if kind == "response.output_item.added" || kind == "response.output_item.done" {
			var item struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(event["item"], &item) == nil && item.Type == "function_call" {
				t.sawTool = true
			}
		}
		if kind != "response.completed" {
			return nil
		}
		var response struct {
			Status string           `json:"status"`
			Output []map[string]any `json:"output"`
		}
		if err := json.Unmarshal(event["response"], &response); err != nil || response.Status != "completed" {
			return errors.New("incomplete Copilot response")
		}
		t.replay = response.Output
		for _, item := range response.Output {
			if item["type"] == "function_call" {
				call := newProviderToolCall(stringValue(item["call_id"]), stringValue(item["name"]), stringValue(item["arguments"]))
				if err := t.addCall(call); err != nil {
					return err
				}
			}
		}
		t.complete = true
	case "anthropic":
		var index int
		_ = json.Unmarshal(event["index"], &index)
		if index < 0 || index > 1024 {
			return errors.New("invalid Copilot block index")
		}
		switch kind {
		case "content_block_start":
			if t.blocks == nil {
				t.blocks = make(map[int]map[string]any)
				t.arguments = make(map[int]string)
				t.stopped = make(map[int]bool)
			}
			if t.blocks[index] != nil {
				return errors.New("duplicate Copilot block")
			}
			var block map[string]any
			if err := json.Unmarshal(event["content_block"], &block); err != nil || block == nil {
				return errors.New("invalid Copilot block")
			}
			t.blocks[index] = block
			if block["type"] == "tool_use" {
				t.sawTool = true
			}
		case "content_block_delta":
			var delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
			}
			if err := json.Unmarshal(event["delta"], &delta); err != nil {
				return err
			}
			block := t.blocks[index]
			if block == nil || t.stopped[index] {
				return errors.New("invalid Copilot block delta")
			}
			if delta.Type == "input_json_delta" {
				if block["type"] != "tool_use" {
					return errors.New("invalid Copilot tool delta")
				}
				t.arguments[index] += delta.PartialJSON
				if len(t.arguments[index]) > 16<<10 {
					return errors.New("Copilot tool input is too large")
				}
			} else {
				switch delta.Type {
				case "text_delta":
					block["text"] = stringValue(block["text"]) + delta.Text
				case "thinking_delta":
					block["thinking"] = stringValue(block["thinking"]) + delta.Thinking
				case "signature_delta":
					block["signature"] = stringValue(block["signature"]) + delta.Signature
				}
			}
		case "content_block_stop":
			block := t.blocks[index]
			if block == nil || t.stopped[index] {
				return errors.New("invalid Copilot block stop")
			}
			t.stopped[index] = true
			if block["type"] == "tool_use" {
				if args, ok := t.arguments[index]; ok {
					var input map[string]any
					if json.Unmarshal([]byte(args), &input) != nil || input == nil {
						return errors.New("invalid Copilot tool input")
					}
					block["input"] = input
				}
				args, err := json.Marshal(block["input"])
				if err != nil {
					return err
				}
				if err := t.addCall(newProviderToolCall(stringValue(block["id"]), stringValue(block["name"]), string(args))); err != nil {
					return err
				}
			}
		case "message_delta":
			var delta struct {
				StopReason string `json:"stop_reason"`
			}
			if err := json.Unmarshal(event["delta"], &delta); err != nil {
				return err
			}
			t.stopReason = delta.StopReason
		case "message_stop":
			t.complete = true
			indices := make([]int, 0, len(t.blocks))
			for i := range t.blocks {
				if !t.stopped[i] {
					return errors.New("incomplete Copilot block")
				}
				indices = append(indices, i)
			}
			sort.Ints(indices)
			for _, i := range indices {
				t.replay = append(t.replay, t.blocks[i])
			}
		}
	case "gemini":
		var event struct {
			Candidates []struct {
				Index        int    `json:"index"`
				FinishReason string `json:"finishReason"`
				Content      struct {
					Parts []map[string]any `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		for _, candidate := range event.Candidates {
			if candidate.Index != 0 {
				continue
			}
			for _, part := range candidate.Content.Parts {
				t.replay = append(t.replay, part)
				if value, exists := part["functionCall"]; exists {
					function, ok := value.(map[string]any)
					if !ok || function["partialArgs"] != nil || function["willContinue"] == true {
						return errors.New("invalid Copilot function call")
					}
					args, err := json.Marshal(function["args"])
					if err != nil {
						return err
					}
					providerID := stringValue(function["id"])
					id := providerID
					if id == "" {
						id = fmt.Sprintf("gemini-call-%d", len(t.calls))
					}
					call := newProviderToolCall(id, stringValue(function["name"]), string(args))
					call.ProviderID = providerID
					if err := t.addCall(call); err != nil {
						return err
					}
				}
			}
			if candidate.FinishReason != "" {
				if candidate.FinishReason != "STOP" {
					return errors.New("Copilot generation did not finish")
				}
				t.complete = true
			}
		}
	}
	return nil
}

func (t *providerToolTurn) finish(format string) error {
	if format != "openai-chat" && !t.complete {
		return errors.New("incomplete Copilot provider turn")
	}
	if !t.sawTool {
		return nil
	}
	if !t.complete || len(t.calls) == 0 || (format == "anthropic" && t.stopReason != "tool_use") {
		return errors.New("incomplete Copilot tool turn")
	}
	return nil
}

func (t *providerToolTurn) addCall(call *chatToolCall) error {
	if len(t.calls) >= 8 || call.ID == "" || len(call.ID) > 128 || call.Function.Name == "" || len(call.Function.Name) > 64 || len(call.Function.Arguments) > 16<<10 {
		return errors.New("invalid Copilot tool call")
	}
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
		return errors.New("invalid Copilot tool arguments")
	}
	for _, previous := range t.calls {
		if previous.ID == call.ID {
			return errors.New("duplicate Copilot tool call ID")
		}
	}
	t.calls = append(t.calls, call)
	t.sawTool = true
	return nil
}

func newProviderToolCall(id, name, args string) *chatToolCall {
	call := &chatToolCall{ID: id, Type: "function"}
	call.Function.Name, call.Function.Arguments = name, args
	return call
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
func stringValue(value any) string { text, _ := value.(string); return text }

func providerTools(names []string, format string) []map[string]any {
	result := make([]map[string]any, 0, len(names))
	for _, tool := range openAIChatTools(names) {
		function := tool["function"].(map[string]any)
		item := map[string]any{"name": function["name"], "description": function["description"]}
		switch format {
		case "openai-responses":
			item["type"], item["parameters"], item["strict"] = "function", function["parameters"], false
		case "anthropic":
			item["input_schema"] = function["parameters"]
		case "gemini":
			item["parametersJsonSchema"] = geminiToolSchema(function["parameters"])
		}
		result = append(result, item)
	}
	return result
}

func geminiToolSchema(value any) any {
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, child := range value {
			if key == "oneOf" {
				key = "anyOf"
			}
			if key == "const" {
				result["enum"] = []any{child}
				continue
			}
			result[key] = geminiToolSchema(child)
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			result[i] = geminiToolSchema(child)
		}
		return result
	default:
		return value
	}
}

// Parallel tool results must be sent together in one user turn.
func appendProviderToolResult(messages *[]map[string]any, role, field string, block map[string]any) {
	if len(*messages) > 0 {
		last := (*messages)[len(*messages)-1]
		if last["role"] == role {
			if blocks, ok := last[field].([]map[string]any); ok {
				last[field] = append(blocks, block)
				return
			}
		}
	}
	*messages = append(*messages, map[string]any{"role": role, field: []map[string]any{block}})
}
