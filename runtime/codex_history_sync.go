package remote

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

const codexHistorySourceKind = "codex-stream"

type codexHistorySyncer struct {
	mu               sync.Mutex
	sessionID        string
	runtimeSessionID func() string
	pusher           *sessionHistoryPusher
	batch            []protocol.SessionHistoryMessage
	pendingTools     map[string]protocol.SessionHistoryMessage
	rewriteUserInput func(string) string
	rewriteToolInput func(toolName, inputJSON string) string
}

func newCodexHistorySyncer(
	sessionID,
	serverURL,
	token string,
	protector *contentProtector,
	runtimeSessionID func() string,
	rewriteUserInput func(string) string,
	rewriteToolInput func(toolName, inputJSON string) string,
) *codexHistorySyncer {
	return &codexHistorySyncer{
		sessionID:        sessionID,
		runtimeSessionID: runtimeSessionID,
		pusher:           newSessionHistoryPusher(serverURL, token, protector),
		batch:            make([]protocol.SessionHistoryMessage, 0, transcriptSyncBatchSize),
		pendingTools:     make(map[string]protocol.SessionHistoryMessage),
		rewriteUserInput: rewriteUserInput,
		rewriteToolInput: rewriteToolInput,
	}
}

func (s *codexHistorySyncer) HandleLine(line string) {
	envelope, err := parseCodexRPCEnvelope([]byte(line))
	if err != nil || !envelope.isNotification() {
		return
	}

	var messages []protocol.SessionHistoryMessage
	switch envelope.Method {
	case "item/started":
		messages = s.parseItemStarted(envelope.Params)
	case "item/completed":
		messages = s.parseItemCompleted(envelope.Params)
	default:
		return
	}
	if len(messages) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	applog.Info.Printf(
		"[Remote] codex history parsed: session=%s count=%d roles=%s types=%s",
		s.sessionID,
		len(messages),
		joinSeenHistoryValues(messages, func(msg protocol.SessionHistoryMessage) string { return msg.Role }),
		joinSeenHistoryValues(messages, func(msg protocol.SessionHistoryMessage) string { return msg.MessageType }),
	)
	s.batch = append(s.batch, messages...)
	if len(s.batch) >= transcriptSyncBatchSize {
		if err := s.flushLocked(); err != nil {
			applog.Errorf("[Remote] codex history flush error: session=%s err=%v", s.sessionID, err)
		}
	}
}

func (s *codexHistorySyncer) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *codexHistorySyncer) flushLocked() error {
	if len(s.batch) == 0 {
		return nil
	}
	batch := append([]protocol.SessionHistoryMessage(nil), s.batch...)
	s.batch = s.batch[:0]
	runtimeSessionID := ""
	if s.runtimeSessionID != nil {
		runtimeSessionID = strings.TrimSpace(s.runtimeSessionID())
	}
	applyRuntimeSessionIDToHistoryBatch(batch, runtimeSessionID)
	return s.pusher.pushBatchWithRuntime(s.sessionID, runtimeSessionID, batch)
}

func (s *codexHistorySyncer) parseItemStarted(paramsRaw json.RawMessage) []protocol.SessionHistoryMessage {
	params := parseCodexParamsObject(paramsRaw)
	item, _ := params["item"].(map[string]any)
	message := codexToolCallMessage(item, time.Now().UnixMilli(), s.rewriteToolInput, false)
	if message.SourceID == "" {
		return nil
	}

	s.mu.Lock()
	s.pendingTools[message.SourceID] = message
	s.mu.Unlock()
	return []protocol.SessionHistoryMessage{message}
}

func (s *codexHistorySyncer) parseItemCompleted(paramsRaw json.RawMessage) []protocol.SessionHistoryMessage {
	params := parseCodexParamsObject(paramsRaw)
	item, _ := params["item"].(map[string]any)
	itemType := normalizeCodexItemType(getString(item, "type"))
	timestamp := time.Now().UnixMilli()

	switch itemType {
	case "userMessage":
		return codexUserMessages(item, timestamp, s.rewriteUserInput)
	case "agentMessage":
		return codexAssistantMessages(item, timestamp)
	default:
		sourceID := codexToolSourceID(item)
		message := codexToolCallMessage(item, timestamp, s.rewriteToolInput, true)
		if sourceID != "" {
			s.mu.Lock()
			if pending, ok := s.pendingTools[sourceID]; ok {
				message.ToolInput = pending.ToolInput
				message.Content = pending.Content
				delete(s.pendingTools, sourceID)
			}
			s.mu.Unlock()
		}
		if message.SourceID == "" {
			return nil
		}
		return []protocol.SessionHistoryMessage{message}
	}
}

func buildCodexHistoryBatchFromThread(
	thread *codexThreadSnapshot,
	rewriteUserInput func(string) string,
	rewriteToolInput func(toolName, inputJSON string) string,
) []protocol.SessionHistoryMessage {
	if thread == nil {
		return nil
	}

	// Codex thread snapshots do not carry stable per-item timestamps. Use the sync time
	// as an append-order clock so mobile tail/delta pagination sees newly materialized
	// thread items at the end instead of near the original thread creation time.
	baseTimestamp := time.Now().UnixMilli()
	nextTimestamp := baseTimestamp
	next := func() int64 {
		nextTimestamp++
		return nextTimestamp
	}

	batch := make([]protocol.SessionHistoryMessage, 0, 32)
	for _, turn := range thread.Turns {
		for _, item := range turn.Items {
			switch normalizeCodexItemType(getString(item, "type")) {
			case "userMessage":
				batch = append(batch, codexUserMessages(item, next(), rewriteUserInput)...)
			case "agentMessage":
				batch = append(batch, codexAssistantMessages(item, next())...)
			default:
				message := codexToolCallMessage(item, next(), rewriteToolInput, true)
				if message.SourceID != "" {
					batch = append(batch, message)
				}
			}
		}
	}
	return batch
}

func applyRuntimeSessionIDToHistoryBatch(batch []protocol.SessionHistoryMessage, runtimeSessionID string) {
	runtimeSessionID = strings.TrimSpace(runtimeSessionID)
	if runtimeSessionID == "" {
		return
	}
	for index := range batch {
		batch[index].RuntimeSessionID = runtimeSessionID
	}
}

func codexUserMessages(item map[string]any, timestamp int64, rewriteUserInput func(string) string) []protocol.SessionHistoryMessage {
	itemID := getString(item, "id")
	content, _ := item["content"].([]any)
	result := make([]protocol.SessionHistoryMessage, 0, len(content))
	for index, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text := strings.TrimSpace(getString(block, "text"))
		if text == "" {
			continue
		}
		if rewriteUserInput != nil {
			text = strings.TrimSpace(rewriteUserInput(text))
		}
		if text == "" {
			continue
		}
		messageType := "text"
		if strings.HasPrefix(strings.TrimSpace(text), "/") {
			messageType = "command_input"
		}
		result = append(result, protocol.SessionHistoryMessage{
			SourceID:    codexMessageSourceID(itemID, index),
			SourceKind:  codexHistorySourceKind,
			Role:        "user",
			Content:     text,
			MessageType: messageType,
			Timestamp:   timestamp,
		})
	}
	return result
}

func codexAssistantMessages(item map[string]any, timestamp int64) []protocol.SessionHistoryMessage {
	text := strings.TrimSpace(getString(item, "text"))
	if text == "" {
		content, _ := item["content"].([]any)
		for _, raw := range content {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if candidate := strings.TrimSpace(getString(block, "text")); candidate != "" {
				text = candidate
				break
			}
		}
	}
	if text == "" {
		return nil
	}
	return []protocol.SessionHistoryMessage{{
		SourceID:    codexMessageSourceID(getString(item, "id"), 0),
		SourceKind:  codexHistorySourceKind,
		Role:        "assistant",
		Content:     text,
		MessageType: "text",
		Timestamp:   timestamp,
	}}
}

func codexToolCallMessage(
	item map[string]any,
	timestamp int64,
	rewriteToolInput func(toolName, inputJSON string) string,
	completed bool,
) protocol.SessionHistoryMessage {
	toolName, input, result := codexToolMessageParts(item)
	if toolName == "" {
		return protocol.SessionHistoryMessage{}
	}
	if rewriteToolInput != nil {
		input = rewriteToolInput(toolName, input)
	}
	callID := codexToolCallID(item)
	return protocol.SessionHistoryMessage{
		SourceID:       codexToolSourceID(item),
		SourceKind:     codexHistorySourceKind,
		Role:           "assistant",
		Content:        formatTranscriptToolSummary(toolName, input),
		MessageType:    "tool_call",
		ToolName:       toolName,
		ToolCallID:     callID,
		ToolInput:      input,
		ToolResult:     result,
		IsToolComplete: completed,
		Timestamp:      timestamp,
	}
}

func codexToolSourceID(item map[string]any) string {
	itemID := codexToolCallID(item)
	if itemID == "" {
		return ""
	}
	return "tool:" + itemID
}

func codexToolCallID(item map[string]any) string {
	return firstNonEmpty(
		strings.TrimSpace(getString(item, "id")),
		strings.TrimSpace(getString(item, "callId")),
		strings.TrimSpace(getString(item, "toolUseId")),
	)
}

func codexToolMessageParts(item map[string]any) (string, string, string) {
	itemType := normalizeCodexItemType(getString(item, "type"))
	switch itemType {
	case "commandExecution":
		return codexMarshalToolMessageParts(
			"Bash",
			map[string]any{
				"command": getString(item, "command"),
				"cwd":     getString(item, "cwd"),
			},
			item["aggregatedOutput"],
		)
	case "mcpToolCall":
		toolName := firstNonEmpty(getString(item, "toolName"), getString(item, "name"))
		if toolName == "" {
			toolName = "McpTool"
		}
		return codexMarshalToolMessageParts(
			toolName,
			codexCompactMap(map[string]any{
				"server_name": getString(item, "serverName"),
				"arguments":   codexMapValue(item["arguments"]),
			}),
			firstNonNil(item["result"], item["content"], item["contentItems"], item["structuredContent"], item["error"]),
		)
	case "dynamicToolCall":
		toolName := firstNonEmpty(getString(item, "toolName"), getString(item, "displayName"), getString(item, "name"), "DynamicTool")
		return codexMarshalToolMessageParts(
			toolName,
			codexCompactMap(map[string]any{
				"arguments":   codexMapValue(item["arguments"]),
				"input_text":  getString(item, "inputText"),
				"input_image": item["inputImage"],
			}),
			firstNonNil(item["contentItems"], item["content"], item["structuredContent"], item["result"], item["error"]),
		)
	case "collabAgentToolCall":
		return codexMarshalToolMessageParts(
			"Task",
			codexCompactMap(map[string]any{
				"agent_nickname": getString(item, "agentNickname"),
				"agent_role":     getString(item, "agentRole"),
				"prompt":         firstNonEmpty(getString(item, "prompt"), getString(item, "userPrompt")),
			}),
			firstNonNil(item["result"], item["lastAgentMessage"], item["content"], item["error"]),
		)
	case "webSearch":
		return codexMarshalToolMessageParts(
			"WebSearch",
			codexCompactMap(map[string]any{
				"query":  getString(item, "query"),
				"action": item["action"],
			}),
			firstNonNil(item["result"], item["content"], item["error"]),
		)
	case "imageGeneration":
		return codexMarshalToolMessageParts(
			"ImageGeneration",
			codexCompactMap(map[string]any{
				"prompt":         firstNonEmpty(getString(item, "revisedPrompt"), getString(item, "revised_prompt"), getString(item, "prompt")),
				"revised_prompt": firstNonEmpty(getString(item, "revisedPrompt"), getString(item, "revised_prompt")),
			}),
			codexCompactMap(map[string]any{
				"saved_path": firstNonEmpty(getString(item, "savedPath"), getString(item, "saved_path")),
			}),
		)
	case "fileChange":
		return codexMarshalToolMessageParts(
			"Write",
			codexCompactMap(map[string]any{
				"file_path":    firstNonEmpty(getString(item, "filePath"), getString(item, "path")),
				"changes":      item["changes"],
				"unified_diff": firstNonEmpty(getString(item, "unifiedDiff"), getString(item, "unified_diff")),
			}),
			firstNonNil(item["changes"], item["unifiedDiff"], item["unified_diff"], item["error"]),
		)
	default:
		arguments := codexMapValue(item["arguments"])
		toolName := firstNonEmpty(getString(item, "toolName"), getString(item, "tool"), getString(item, "name"))
		if toolName == "" || len(arguments) == 0 {
			return "", "", ""
		}
		return codexMarshalToolMessageParts(
			toolName,
			arguments,
			firstNonNil(item["contentItems"], item["content"], item["structuredContent"], item["result"], item["error"]),
		)
	}
}

func codexMarshalToolMessageParts(toolName string, input any, result any) (string, string, string) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return "", "", ""
	}
	inputJSON := ""
	switch typed := input.(type) {
	case nil:
	case string:
		inputJSON = strings.TrimSpace(typed)
	default:
		inputJSON = strings.TrimSpace(marshalCompactJSON(typed))
	}
	if inputJSON == "{}" || inputJSON == "[]" {
		inputJSON = ""
	}
	return toolName, inputJSON, coalesceCodexToolResult(result)
}

func coalesceCodexToolResult(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case nil:
			continue
		case string:
			if text := strings.TrimSpace(typed); text != "" {
				return text
			}
		case map[string]any:
			if text := coalesceCodexToolResult(
				typed["text"],
				typed["content"],
				typed["contentItems"],
				typed["structuredContent"],
				typed["message"],
				typed["error"],
				typed["details"],
				typed["detail"],
			); text != "" {
				return text
			}
			raw := strings.TrimSpace(marshalCompactJSON(typed))
			if raw != "" && raw != "{}" {
				return raw
			}
		case []any:
			parts := make([]string, 0, len(typed))
			for _, item := range typed {
				if text := strings.TrimSpace(codexToolResultItemText(item)); text != "" {
					parts = append(parts, text)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n")
			}
			raw := strings.TrimSpace(marshalCompactJSON(typed))
			if raw != "" && raw != "[]" {
				return raw
			}
		default:
			if text := coalesceCodexMessage(typed); text != "" {
				return text
			}
		}
	}
	return ""
}

func codexToolResultItemText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		itemType := strings.TrimSpace(getString(typed, "type"))
		switch itemType {
		case "text", "inputText", "outputText":
			return strings.TrimSpace(firstNonEmpty(getString(typed, "text"), getString(typed, "content")))
		}
		if text := strings.TrimSpace(firstNonEmpty(getString(typed, "text"), getString(typed, "content"), getString(typed, "message"))); text != "" {
			return text
		}
		return coalesceCodexToolResult(
			typed["content"],
			typed["contentItems"],
			typed["structuredContent"],
			typed["result"],
			typed["error"],
		)
	case []any:
		return coalesceCodexToolResult(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func codexCompactMap(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		if key == "" || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) == "" {
				continue
			}
			result[key] = strings.TrimSpace(typed)
		case map[string]any:
			if len(typed) == 0 {
				continue
			}
			result[key] = typed
		default:
			result[key] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func codexMapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	if len(result) == 0 {
		return nil
	}
	return result
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func codexMessageSourceID(itemID string, index int) string {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return ""
	}
	return itemID + ":text:" + strconv.Itoa(index)
}

func normalizeCodexItemType(value string) string {
	switch strings.TrimSpace(value) {
	case "userMessage", "UserMessage":
		return "userMessage"
	case "agentMessage", "AgentMessage":
		return "agentMessage"
	case "commandExecution", "CommandExecution":
		return "commandExecution"
	case "mcpToolCall", "McpToolCall":
		return "mcpToolCall"
	case "dynamicToolCall", "DynamicToolCall":
		return "dynamicToolCall"
	case "collabAgentToolCall", "CollabAgentToolCall":
		return "collabAgentToolCall"
	case "webSearch", "WebSearch":
		return "webSearch"
	case "imageGeneration", "ImageGeneration":
		return "imageGeneration"
	case "fileChange", "FileChange":
		return "fileChange"
	default:
		return strings.TrimSpace(value)
	}
}

func parseCodexParamsObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil
	}
	return result
}
