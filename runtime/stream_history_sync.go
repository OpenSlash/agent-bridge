package remote

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

const streamHistorySourceKind = "claude-stream"

type streamHistorySyncer struct {
	sessionID string
	pusher    *sessionHistoryPusher
	parser    *claudeStreamHistoryParser
	batch     []protocol.SessionHistoryMessage
}

type claudeStreamHistoryParser struct {
	runID   string
	nextSeq int64

	pendingAssistant      strings.Builder
	pendingAssistantID    string
	pendingAssistantStamp int64

	currentBlockType  string
	pendingToolCallID string
	pendingToolName   string
	pendingToolInput  strings.Builder
	pendingToolStamp  int64
	pendingToolSource string
	rewriteToolInput  func(toolName, inputJSON string) string

	sawStreamingBlocks  bool
	emittedTextThisTurn bool
}

func newStreamHistorySyncer(
	sessionID,
	serverURL,
	token string,
	protector *contentProtector,
	rewriteToolInput func(toolName, inputJSON string) string,
) *streamHistorySyncer {
	return &streamHistorySyncer{
		sessionID: sessionID,
		pusher:    newSessionHistoryPusher(serverURL, token, protector),
		parser:    newClaudeStreamHistoryParser(rewriteToolInput),
		batch:     make([]protocol.SessionHistoryMessage, 0, transcriptSyncBatchSize),
	}
}

func newClaudeStreamHistoryParser(rewriteToolInput func(toolName, inputJSON string) string) *claudeStreamHistoryParser {
	return newClaudeStreamHistoryParserWithRunID(fmt.Sprintf("%d", time.Now().UnixNano()), rewriteToolInput)
}

func newClaudeStreamHistoryParserWithRunID(
	runID string,
	rewriteToolInput func(toolName, inputJSON string) string,
) *claudeStreamHistoryParser {
	return &claudeStreamHistoryParser{
		runID:            strings.TrimSpace(runID),
		rewriteToolInput: rewriteToolInput,
	}
}

func (s *streamHistorySyncer) HandleLine(line string) {
	if s == nil || s.parser == nil {
		return
	}
	messages := s.parser.ParseLine([]byte(line))
	if len(messages) == 0 {
		return
	}
	applog.Info.Printf(
		"[Remote] stream history parsed: session=%s count=%d roles=%s types=%s",
		s.sessionID,
		len(messages),
		joinSeenHistoryValues(messages, func(msg protocol.SessionHistoryMessage) string { return msg.Role }),
		joinSeenHistoryValues(messages, func(msg protocol.SessionHistoryMessage) string { return msg.MessageType }),
	)
	s.batch = append(s.batch, messages...)
	if len(s.batch) >= transcriptSyncBatchSize {
		if err := s.Flush(); err != nil {
			applog.Errorf("[Remote] stream history flush error: session=%s err=%v", s.sessionID, err)
		}
	}
}

func (s *streamHistorySyncer) Flush() error {
	if s == nil || len(s.batch) == 0 {
		return nil
	}
	batch := append([]protocol.SessionHistoryMessage(nil), s.batch...)
	s.batch = s.batch[:0]
	return s.pusher.pushBatch(s.sessionID, batch)
}

func (p *claudeStreamHistoryParser) ParseLine(line []byte) []protocol.SessionHistoryMessage {
	var entry map[string]any
	if err := json.Unmarshal(line, &entry); err != nil {
		return nil
	}

	eventType := getString(entry, "type")
	switch eventType {
	case "content_block_start":
		block, _ := entry["content_block"].(map[string]any)
		if block == nil {
			return nil
		}
		p.sawStreamingBlocks = true

		switch getString(block, "type") {
		case "tool_use":
			result := p.commitAssistant()
			p.currentBlockType = "tool_use"
			p.pendingToolCallID = getString(block, "id")
			p.pendingToolName = getString(block, "name")
			p.pendingToolInput.Reset()
			p.pendingToolStamp = currentHistoryTimestamp(entry)
			if p.pendingToolCallID != "" {
				p.pendingToolSource = "tool:" + p.pendingToolCallID
			} else {
				p.pendingToolSource = p.nextSourceID("tool")
			}
			return result
		case "text":
			p.currentBlockType = "text"
			if p.pendingAssistantStamp == 0 {
				p.pendingAssistantStamp = currentHistoryTimestamp(entry)
			}
			if text := strings.TrimSpace(getString(block, "text")); text != "" {
				p.pendingAssistant.WriteString(text)
			}
		}
		return nil

	case "content_block_delta":
		delta, _ := entry["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		switch getString(delta, "type") {
		case "text_delta":
			p.pendingAssistant.WriteString(getString(delta, "text"))
		case "input_json_delta":
			p.pendingToolInput.WriteString(getString(delta, "partial_json"))
		}
		return nil

	case "content_block_stop":
		if p.currentBlockType == "tool_use" {
			result := p.commitToolCall()
			p.currentBlockType = ""
			return result
		}
		p.currentBlockType = ""
		return nil

	case "message_stop":
		result := p.commitAssistant()
		result = append(result, p.commitToolCall()...)
		return result

	case "message_delta":
		delta, _ := entry["delta"].(map[string]any)
		if delta != nil && delta["stop_reason"] != nil {
			result := p.commitAssistant()
			result = append(result, p.commitToolCall()...)
			return result
		}
		return nil

	case "result":
		result := p.commitAssistant()
		result = append(result, p.commitToolCall()...)
		if len(result) == 0 && !p.emittedTextThisTurn {
			if text := strings.TrimSpace(getString(entry, "result")); text != "" {
				result = append(result, protocol.SessionHistoryMessage{
					SourceID:    p.nextSourceID("assistant"),
					SourceKind:  streamHistorySourceKind,
					Role:        "assistant",
					Content:     text,
					MessageType: "text",
					Timestamp:   currentHistoryTimestamp(entry),
				})
			}
		}
		p.resetTurnState()
		return result

	case "user":
		return p.parseUser(entry)

	case "assistant":
		if p.sawStreamingBlocks {
			return nil
		}
		return p.parseAssistant(entry)

	case "system":
		return p.parseSystem(entry)

	default:
		return nil
	}
}

func (p *claudeStreamHistoryParser) parseAssistant(entry map[string]any) []protocol.SessionHistoryMessage {
	message, _ := entry["message"].(map[string]any)
	if message == nil {
		return nil
	}

	role := defaultTranscriptRole(getString(message, "role"), "assistant")
	sourceID := getString(entry, "uuid")
	timestamp := currentHistoryTimestamp(entry)
	content := message["content"]
	var result []protocol.SessionHistoryMessage

	switch blocks := content.(type) {
	case []any:
		for index, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch getString(block, "type") {
			case "text":
				text := strings.TrimSpace(getString(block, "text"))
				if text == "" {
					continue
				}
				msgSourceID := sourceID
				if msgSourceID != "" {
					msgSourceID = fmt.Sprintf("%s:text:%d", msgSourceID, index)
				} else {
					msgSourceID = p.nextSourceID("assistant")
				}
				result = append(result, protocol.SessionHistoryMessage{
					SourceID:    msgSourceID,
					SourceKind:  streamHistorySourceKind,
					Role:        role,
					Content:     text,
					MessageType: "text",
					Timestamp:   timestamp,
				})
				p.emittedTextThisTurn = true
			case "tool_use":
				toolUseID := getString(block, "id")
				msgSourceID := p.nextSourceID("tool")
				if toolUseID != "" {
					msgSourceID = "tool:" + toolUseID
				}
				input := marshalCompactJSON(block["input"])
				if p.rewriteToolInput != nil {
					input = p.rewriteToolInput(getString(block, "name"), input)
				}
				result = append(result, protocol.SessionHistoryMessage{
					SourceID:       msgSourceID,
					SourceKind:     streamHistorySourceKind,
					Role:           role,
					Content:        formatTranscriptToolSummary(getString(block, "name"), input),
					MessageType:    "tool_call",
					ToolName:       getString(block, "name"),
					ToolCallID:     toolUseID,
					ToolInput:      input,
					IsToolComplete: false,
					Timestamp:      timestamp,
				})
			}
		}
	default:
		text := strings.TrimSpace(extractTranscriptContent(content))
		if text != "" {
			result = append(result, protocol.SessionHistoryMessage{
				SourceID:    p.nextSourceID("assistant"),
				SourceKind:  streamHistorySourceKind,
				Role:        role,
				Content:     text,
				MessageType: "text",
				Timestamp:   timestamp,
			})
			p.emittedTextThisTurn = true
		}
	}

	return result
}

func (p *claudeStreamHistoryParser) parseSystem(entry map[string]any) []protocol.SessionHistoryMessage {
	if getString(entry, "subtype") == "compact_boundary" {
		return []protocol.SessionHistoryMessage{{
			SourceID:    firstNonEmpty(getString(entry, "uuid"), p.nextSourceID("system")),
			SourceKind:  streamHistorySourceKind,
			Role:        "system",
			Content:     compactBoundaryText,
			MessageType: "text",
			Timestamp:   currentHistoryTimestamp(entry),
		}}
	}

	content := strings.TrimSpace(extractTranscriptEntryContent(entry))
	display, ok := normalizeTranscriptDisplayEntry(entry, "system", content)
	if !ok {
		return nil
	}

	return []protocol.SessionHistoryMessage{{
		SourceID:    firstNonEmpty(getString(entry, "uuid"), p.nextSourceID("system")),
		SourceKind:  streamHistorySourceKind,
		Role:        display.Role,
		Content:     display.Content,
		MessageType: firstNonEmpty(strings.TrimSpace(display.MessageType), "text"),
		Timestamp:   currentHistoryTimestamp(entry),
	}}
}

func (p *claudeStreamHistoryParser) parseUser(entry map[string]any) []protocol.SessionHistoryMessage {
	message, _ := entry["message"].(map[string]any)
	if message == nil {
		return nil
	}

	role := defaultTranscriptRole(getString(message, "role"), "user")
	sourceID := getString(entry, "uuid")
	timestamp := currentHistoryTimestamp(entry)
	content := message["content"]
	var (
		result    []protocol.SessionHistoryMessage
		textParts []string
	)

	switch blocks := content.(type) {
	case []any:
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch getString(block, "type") {
			case "text":
				text := strings.TrimSpace(extractTranscriptContent(block["text"]))
				if text != "" {
					textParts = append(textParts, text)
				}
			case "tool_result":
				toolUseID := getString(block, "tool_use_id")
				if toolUseID == "" {
					continue
				}
				result = append(result, protocol.SessionHistoryMessage{
					SourceID:       "tool:" + toolUseID,
					SourceKind:     streamHistorySourceKind,
					Role:           "assistant",
					Content:        toolUseID,
					MessageType:    "tool_call",
					ToolCallID:     toolUseID,
					ToolResult:     extractTranscriptContent(block["content"]),
					IsToolComplete: true,
					Timestamp:      timestamp,
				})
			}
		}
	default:
		text := strings.TrimSpace(extractTranscriptContent(content))
		if text != "" {
			textParts = append(textParts, text)
		}
	}

	if len(textParts) > 0 {
		text := strings.TrimSpace(strings.Join(textParts, "\n"))
		display, ok := normalizeTranscriptDisplayEntry(entry, role, text)
		if !ok {
			return result
		}
		msgSourceID := sourceID
		if msgSourceID == "" {
			msgSourceID = p.nextSourceID("user")
		}
		result = append([]protocol.SessionHistoryMessage{{
			SourceID:    msgSourceID,
			SourceKind:  streamHistorySourceKind,
			Role:        display.Role,
			Content:     display.Content,
			MessageType: firstNonEmpty(strings.TrimSpace(display.MessageType), "text"),
			Timestamp:   timestamp,
		}}, result...)
	}

	return result
}

func (p *claudeStreamHistoryParser) commitAssistant() []protocol.SessionHistoryMessage {
	content := strings.TrimSpace(p.pendingAssistant.String())
	if content == "" {
		p.pendingAssistant.Reset()
		p.pendingAssistantID = ""
		p.pendingAssistantStamp = 0
		return nil
	}

	sourceID := p.pendingAssistantID
	if sourceID == "" {
		sourceID = p.nextSourceID("assistant")
	}
	timestamp := p.pendingAssistantStamp
	if timestamp == 0 {
		timestamp = time.Now().UnixMilli()
	}

	p.pendingAssistant.Reset()
	p.pendingAssistantID = ""
	p.pendingAssistantStamp = 0
	p.emittedTextThisTurn = true

	return []protocol.SessionHistoryMessage{{
		SourceID:    sourceID,
		SourceKind:  streamHistorySourceKind,
		Role:        "assistant",
		Content:     content,
		MessageType: "text",
		Timestamp:   timestamp,
	}}
}

func (p *claudeStreamHistoryParser) commitToolCall() []protocol.SessionHistoryMessage {
	if p.pendingToolCallID == "" && p.pendingToolName == "" && p.pendingToolInput.Len() == 0 {
		p.pendingToolSource = ""
		p.pendingToolStamp = 0
		return nil
	}

	sourceID := p.pendingToolSource
	if sourceID == "" {
		sourceID = p.nextSourceID("tool")
	}
	timestamp := p.pendingToolStamp
	if timestamp == 0 {
		timestamp = time.Now().UnixMilli()
	}

	name := p.pendingToolName
	input := p.pendingToolInput.String()
	if p.rewriteToolInput != nil {
		input = p.rewriteToolInput(name, input)
	}
	toolCallID := p.pendingToolCallID

	p.pendingToolCallID = ""
	p.pendingToolName = ""
	p.pendingToolInput.Reset()
	p.pendingToolStamp = 0
	p.pendingToolSource = ""

	return []protocol.SessionHistoryMessage{{
		SourceID:       sourceID,
		SourceKind:     streamHistorySourceKind,
		Role:           "assistant",
		Content:        formatTranscriptToolSummary(name, input),
		MessageType:    "tool_call",
		ToolName:       name,
		ToolCallID:     toolCallID,
		ToolInput:      input,
		IsToolComplete: false,
		Timestamp:      timestamp,
	}}
}

func (p *claudeStreamHistoryParser) nextSourceID(prefix string) string {
	p.nextSeq++
	if p.runID != "" {
		return fmt.Sprintf("stream:%s:%s:%d", p.runID, prefix, p.nextSeq)
	}
	return fmt.Sprintf("stream:%s:%d", prefix, p.nextSeq)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (p *claudeStreamHistoryParser) resetTurnState() {
	p.sawStreamingBlocks = false
	p.emittedTextThisTurn = false
}

func currentHistoryTimestamp(entry map[string]any) int64 {
	if timestamp := parseTranscriptTimestamp(getString(entry, "timestamp")); timestamp > 0 {
		return timestamp
	}
	return time.Now().UnixMilli()
}
