package remote

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"

	"github.com/google/uuid"
)

const (
	transcriptSourceKind       = "claude-transcript"
	transcriptSyncBatchSize    = 80
	transcriptSyncPollInterval = 1200 * time.Millisecond
)

type transcriptSyncer struct {
	sessionID string
	pusher    *sessionHistoryPusher

	path   string
	offset int64
	parser *claudeTranscriptParser
}

type sessionHistoryPusher struct {
	serverURL string
	token     string
	client    *http.Client
	protector *contentProtector
}

type claudeTranscriptParser struct {
	pendingTools     map[string]protocol.SessionHistoryMessage
	rewriteToolInput func(toolName, inputJSON string) string
}

type claudeSessionResolution struct {
	SessionID                string
	Resume                   bool
	TranscriptPath           string
	AdoptedRecentSession     bool
	ReplacedRequestedSession bool
}

type ClaudeSessionResolution struct {
	SessionID                string
	Resume                   bool
	TranscriptPath           string
	AdoptedRecentSession     bool
	ReplacedRequestedSession bool
}

func ResolveClaudeSessionStart(requestedSessionID, cwd string, resume bool) (ClaudeSessionResolution, error) {
	resolution, err := resolveClaudeSessionStart(requestedSessionID, cwd, resume)
	if err != nil {
		return ClaudeSessionResolution{}, err
	}
	return ClaudeSessionResolution{
		SessionID:                resolution.SessionID,
		Resume:                   resolution.Resume,
		TranscriptPath:           resolution.TranscriptPath,
		AdoptedRecentSession:     resolution.AdoptedRecentSession,
		ReplacedRequestedSession: resolution.ReplacedRequestedSession,
	}, nil
}

func (s *Service) startTranscriptSync(sessionID string) {
	if sessionID == "" {
		return
	}

	syncer := &transcriptSyncer{
		sessionID: sessionID,
		pusher:    newSessionHistoryPusher(s.cfg.ServerURL, s.cfg.Token, s.contentProtector),
		parser:    newClaudeTranscriptParser(s.rewriteToolInput),
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for attempt := 0; attempt < 10; attempt++ {
			select {
			case <-s.done:
				return
			default:
			}

			if err := syncer.Sync(s.getCurrentDir()); err == nil {
				return
			} else if !isTranscriptNotReady(err) {
				applog.Errorf("[Remote] transcript sync error: session=%s err=%v", sessionID, err)
				return
			}

			select {
			case <-s.done:
				return
			case <-time.After(transcriptSyncPollInterval):
			}
		}
		applog.Info.Printf("[Remote] transcript sync unavailable: session=%s cwd=%s", sessionID, s.getCurrentDir())
	}()
}

func newSessionHistoryPusher(serverURL, token string, protector *contentProtector) *sessionHistoryPusher {
	return &sessionHistoryPusher{
		serverURL: serverURL,
		token:     token,
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
		protector: protector,
	}
}

func newClaudeTranscriptParser(rewriteToolInput func(toolName, inputJSON string) string) *claudeTranscriptParser {
	return &claudeTranscriptParser{
		pendingTools:     make(map[string]protocol.SessionHistoryMessage),
		rewriteToolInput: rewriteToolInput,
	}
}

func resolveClaudeSessionStart(requestedSessionID, cwd string, resume bool) (claudeSessionResolution, error) {
	requested := strings.TrimSpace(requestedSessionID)

	if requested != "" && !resume {
		return claudeSessionResolution{
			SessionID: requested,
			Resume:    false,
		}, nil
	}

	if requested != "" && resume {
		path, err := findClaudeTranscriptPath(requested, cwd)
		if err == nil {
			return claudeSessionResolution{
				SessionID:      requested,
				Resume:         true,
				TranscriptPath: path,
			}, nil
		}
		if !isTranscriptNotReady(err) {
			return claudeSessionResolution{}, err
		}

		existingSessionID, transcriptPath, ok, recentErr := findRecentClaudeTranscriptSession(cwd, 0)
		if recentErr != nil {
			return claudeSessionResolution{
				SessionID: requested,
				Resume:    false,
			}, recentErr
		}
		if ok {
			return claudeSessionResolution{
				SessionID:                existingSessionID,
				Resume:                   true,
				TranscriptPath:           transcriptPath,
				ReplacedRequestedSession: existingSessionID != requested,
			}, nil
		}

		return claudeSessionResolution{
			SessionID: requested,
			Resume:    false,
		}, nil
	}

	existingSessionID, transcriptPath, ok, err := findRecentClaudeTranscriptSession(cwd, 0)
	if err != nil {
		return claudeSessionResolution{
			SessionID: uuid.NewString(),
			Resume:    false,
		}, err
	}
	if ok {
		return claudeSessionResolution{
			SessionID:            existingSessionID,
			Resume:               true,
			TranscriptPath:       transcriptPath,
			AdoptedRecentSession: true,
		}, nil
	}

	return claudeSessionResolution{
		SessionID: uuid.NewString(),
		Resume:    false,
	}, nil
}

func (s *transcriptSyncer) Sync(cwd string) error {
	path, err := findClaudeTranscriptPath(s.sessionID, cwd)
	if err != nil {
		return err
	}
	if path != s.path {
		s.path = path
		s.offset = 0
		s.parser = newClaudeTranscriptParser(s.parser.rewriteToolInput)
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < s.offset {
		s.offset = 0
		s.parser = newClaudeTranscriptParser(s.parser.rewriteToolInput)
	}

	if _, err := file.Seek(s.offset, io.SeekStart); err != nil {
		return err
	}

	reader := bufio.NewReader(file)
	currentOffset := s.offset
	batch := make([]protocol.SessionHistoryMessage, 0, transcriptSyncBatchSize)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			hasTerminator := line[len(line)-1] == '\n'
			if !hasTerminator && err == io.EOF {
				trimmed := bytes.TrimSpace(line)
				if len(trimmed) == 0 || !json.Valid(trimmed) {
					break
				}
			}

			currentOffset += int64(len(line))
			messages := s.parser.ParseLine(line)
			if len(messages) > 0 {
				batch = append(batch, messages...)
			}

			if len(batch) >= transcriptSyncBatchSize {
				if syncErr := s.pushBatch(batch); syncErr != nil {
					s.reset()
					return syncErr
				}
				batch = batch[:0]
				s.offset = currentOffset
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			s.reset()
			return err
		}
	}

	if len(batch) > 0 {
		if err := s.pushBatch(batch); err != nil {
			s.reset()
			return err
		}
	}
	s.offset = currentOffset
	return nil
}

func (s *transcriptSyncer) pushBatch(batch []protocol.SessionHistoryMessage) error {
	if s.pusher == nil {
		return nil
	}
	return s.pusher.pushBatch(s.sessionID, batch)
}

func (p *sessionHistoryPusher) pushBatch(sessionID string, batch []protocol.SessionHistoryMessage) error {
	return p.pushBatchWithRuntime(sessionID, "", batch)
}

func (p *sessionHistoryPusher) pushBatchWithRuntime(sessionID, runtimeSessionID string, batch []protocol.SessionHistoryMessage) error {
	if len(batch) == 0 {
		return nil
	}
	if strings.TrimSpace(p.serverURL) == "" || strings.TrimSpace(p.token) == "" {
		return nil
	}

	if p.protector != nil {
		protectedBatch, err := p.protector.ProtectHistoryBatch(sessionID, batch)
		if err != nil {
			return err
		}
		batch = protectedBatch
	}

	endpoint, err := buildHTTPURL(p.serverURL, "/ws/terminal/sessions/history/sync", p.token)
	if err != nil {
		return err
	}

	payload := protocol.SessionHistorySyncRequest{
		SessionID:        sessionID,
		RuntimeSessionID: strings.TrimSpace(runtimeSessionID),
		Messages:         batch,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("history sync failed: %s %s", resp.Status, strings.TrimSpace(string(respBody)))
	}

	applog.Info.Printf(
		"[Remote] history sync pushed: session=%s count=%d roles=%s kinds=%s first=%s last=%s",
		sessionID,
		len(batch),
		joinSeenHistoryValues(batch, func(msg protocol.SessionHistoryMessage) string { return msg.Role }),
		joinSeenHistoryValues(batch, func(msg protocol.SessionHistoryMessage) string { return msg.SourceKind }),
		strings.TrimSpace(batch[0].SourceID),
		strings.TrimSpace(batch[len(batch)-1].SourceID),
	)

	return nil
}

func (s *transcriptSyncer) reset() {
	s.offset = 0
	s.parser = newClaudeTranscriptParser(s.parser.rewriteToolInput)
}

func (p *claudeTranscriptParser) ParseLine(line []byte) []protocol.SessionHistoryMessage {
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(line), &entry); err != nil {
		return nil
	}

	entryType := getString(entry, "type")
	if entryType == "" || entryType == "file-history-snapshot" {
		return nil
	}
	if getBool(entry, "isMeta") {
		return nil
	}

	timestamp := parseTranscriptTimestamp(getString(entry, "timestamp"))
	uuid := getString(entry, "uuid")
	message, _ := entry["message"].(map[string]any)

	switch entryType {
	case "assistant":
		return p.parseAssistant(uuid, timestamp, message)
	case "user":
		return p.parseUser(entry, uuid, timestamp, message)
	case "system", "summary", "progress":
		return p.parseSystem(entryType, uuid, timestamp, entry)
	default:
		return nil
	}
}

func (p *claudeTranscriptParser) parseAssistant(sourceID string, timestamp int64, message map[string]any) []protocol.SessionHistoryMessage {
	if message == nil {
		return nil
	}

	content := message["content"]
	role := defaultTranscriptRole(getString(message, "role"), "assistant")
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
				result = append(result, protocol.SessionHistoryMessage{
					SourceID:    fmt.Sprintf("%s:text:%d", sourceID, index),
					SourceKind:  transcriptSourceKind,
					Role:        role,
					Content:     text,
					MessageType: "text",
					Timestamp:   timestamp,
				})
			case "tool_use":
				toolUseID := getString(block, "id")
				if toolUseID == "" {
					continue
				}
				input := marshalCompactJSON(block["input"])
				if p.rewriteToolInput != nil {
					input = p.rewriteToolInput(getString(block, "name"), input)
				}
				msg := protocol.SessionHistoryMessage{
					SourceID:       "tool:" + toolUseID,
					SourceKind:     transcriptSourceKind,
					Role:           role,
					Content:        formatTranscriptToolSummary(getString(block, "name"), input),
					MessageType:    "tool_call",
					ToolName:       getString(block, "name"),
					ToolCallID:     toolUseID,
					ToolInput:      input,
					IsToolComplete: false,
					Timestamp:      timestamp,
				}
				p.pendingTools[toolUseID] = msg
				result = append(result, msg)
			}
		}
	default:
		text := strings.TrimSpace(getString(message, "content"))
		if text != "" {
			result = append(result, protocol.SessionHistoryMessage{
				SourceID:    sourceID,
				SourceKind:  transcriptSourceKind,
				Role:        role,
				Content:     text,
				MessageType: "text",
				Timestamp:   timestamp,
			})
		}
	}

	return result
}

func (p *claudeTranscriptParser) parseUser(entry map[string]any, sourceID string, timestamp int64, message map[string]any) []protocol.SessionHistoryMessage {
	if message == nil {
		return nil
	}

	role := defaultTranscriptRole(getString(message, "role"), "user")
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
				existing, ok := p.pendingTools[toolUseID]
				if !ok {
					existing = protocol.SessionHistoryMessage{
						SourceID:    "tool:" + toolUseID,
						SourceKind:  transcriptSourceKind,
						Role:        "assistant",
						Content:     toolUseID,
						MessageType: "tool_call",
						ToolCallID:  toolUseID,
						Timestamp:   timestamp,
					}
				}

				existing.ToolResult = extractTranscriptContent(block["content"])
				existing.IsToolComplete = true
				if existing.Timestamp == 0 {
					existing.Timestamp = timestamp
				}
				p.pendingTools[toolUseID] = existing
				result = append(result, existing)
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
		result = append([]protocol.SessionHistoryMessage{{
			SourceID:    sourceID,
			SourceKind:  transcriptSourceKind,
			Role:        display.Role,
			Content:     display.Content,
			MessageType: firstNonEmpty(strings.TrimSpace(display.MessageType), "text"),
			Timestamp:   timestamp,
		}}, result...)
	}

	return result
}

func (p *claudeTranscriptParser) parseSystem(entryType, sourceID string, timestamp int64, entry map[string]any) []protocol.SessionHistoryMessage {
	if getString(entry, "subtype") == "compact_boundary" {
		return []protocol.SessionHistoryMessage{{
			SourceID:    firstNonEmpty(strings.TrimSpace(sourceID), fmt.Sprintf("%s:%d", entryType, timestamp)),
			SourceKind:  transcriptSourceKind,
			Role:        "system",
			Content:     compactBoundaryText,
			MessageType: "text",
			Timestamp:   timestamp,
		}}
	}

	content := strings.TrimSpace(extractTranscriptEntryContent(entry))
	display, ok := normalizeTranscriptDisplayEntry(entry, "system", content)
	if !ok {
		return nil
	}

	msgSourceID := strings.TrimSpace(sourceID)
	if msgSourceID == "" {
		msgSourceID = fmt.Sprintf("%s:%d", entryType, timestamp)
	}

	return []protocol.SessionHistoryMessage{{
		SourceID:    msgSourceID,
		SourceKind:  transcriptSourceKind,
		Role:        display.Role,
		Content:     display.Content,
		MessageType: firstNonEmpty(strings.TrimSpace(display.MessageType), "text"),
		Timestamp:   timestamp,
	}}
}

func parseTranscriptTimestamp(value string) int64 {
	if value == "" {
		return time.Now().UnixMilli()
	}

	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Now().UnixMilli()
	}
	return parsed.UnixMilli()
}

func defaultTranscriptRole(value, fallback string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "user", "assistant", "system":
		return value
	default:
		return fallback
	}
}

func extractTranscriptContent(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch getString(block, "type") {
			case "text":
				text := strings.TrimSpace(getString(block, "text"))
				if text != "" {
					parts = append(parts, text)
				}
			case "tool_reference":
				name := strings.TrimSpace(getString(block, "tool_name"))
				if name != "" {
					parts = append(parts, name)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(data)
	}
}

func extractTranscriptEntryContent(entry map[string]any) string {
	if entry == nil {
		return ""
	}

	if message, ok := entry["message"].(map[string]any); ok {
		if content := strings.TrimSpace(extractTranscriptContent(message["content"])); content != "" {
			return content
		}
		if content := strings.TrimSpace(extractTranscriptContent(message["text"])); content != "" {
			return content
		}
	}

	for _, key := range []string{"content", "text", "result", "summary", "data"} {
		if content := strings.TrimSpace(extractTranscriptContent(entry[key])); content != "" {
			return content
		}
	}

	return ""
}

func formatTranscriptToolSummary(name, inputJSON string) string {
	if inputJSON == "" {
		return strings.TrimSpace(name)
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &obj); err != nil {
		return inputJSON
	}

	switch name {
	case "Bash":
		return getString(obj, "command")
	case "Read", "Edit", "Write", "NotebookEdit":
		return getString(obj, "file_path")
	case "Glob", "Grep":
		return getString(obj, "pattern")
	case "Task":
		if value := getString(obj, "description"); value != "" {
			return value
		}
		return getString(obj, "prompt")
	case "WebSearch":
		return getString(obj, "query")
	case "WebFetch":
		return getString(obj, "url")
	default:
		return inputJSON
	}
}

func marshalCompactJSON(value any) string {
	if value == nil {
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func getString(data map[string]any, key string) string {
	value, ok := data[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func getBool(data map[string]any, key string) bool {
	value, ok := data[key]
	if !ok {
		return false
	}
	typed, ok := value.(bool)
	return ok && typed
}

func findClaudeTranscriptPath(sessionID, cwd string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	projectDir := encodeClaudeProjectPath(cwd)
	if projectDir != "" {
		candidate := filepath.Join(home, ".claude", "projects", projectDir, sessionID+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return "", err
	}
	if len(matches) > 0 {
		return matches[0], nil
	}

	return "", errTranscriptNotReady
}

func findRecentClaudeTranscriptSession(cwd string, maxAge time.Duration) (sessionID, path string, ok bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", false, err
	}

	projectDir := encodeClaudeProjectPath(cwd)
	if projectDir == "" {
		return "", "", false, nil
	}

	transcriptDir := filepath.Join(home, ".claude", "projects", projectDir)
	entries, err := os.ReadDir(transcriptDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", false, nil
		}
		return "", "", false, err
	}

	var (
		latestPath    string
		latestSession string
		latestModTime time.Time
	)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		modTime := info.ModTime()
		if !latestModTime.IsZero() && !modTime.After(latestModTime) {
			continue
		}
		candidateSessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
		if _, parseErr := uuid.Parse(candidateSessionID); parseErr != nil {
			continue
		}
		latestModTime = modTime
		latestSession = candidateSessionID
		latestPath = filepath.Join(transcriptDir, entry.Name())
	}

	if latestSession == "" {
		return "", "", false, nil
	}
	if maxAge > 0 && time.Since(latestModTime) > maxAge {
		return "", "", false, nil
	}

	return latestSession, latestPath, true, nil
}

func encodeClaudeProjectPath(cwd string) string {
	cleaned := filepath.Clean(strings.TrimSpace(cwd))
	if cleaned == "" || cleaned == "." {
		return ""
	}

	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "",
	)
	encoded := replacer.Replace(cleaned)
	if !strings.HasPrefix(encoded, "-") {
		encoded = "-" + encoded
	}
	return encoded
}

func joinSeenHistoryValues(messages []protocol.SessionHistoryMessage, pick func(protocol.SessionHistoryMessage) string) string {
	seen := make(map[string]struct{})
	values := make([]string, 0, len(messages))
	for _, msg := range messages {
		value := strings.TrimSpace(pick(msg))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}

var errTranscriptNotReady = fmt.Errorf("transcript not ready")

func isTranscriptNotReady(err error) bool {
	return err != nil && strings.Contains(err.Error(), errTranscriptNotReady.Error())
}
