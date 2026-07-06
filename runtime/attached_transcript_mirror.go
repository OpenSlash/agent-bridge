package remote

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

type AttachedTranscriptMirror struct {
	sessionID string
	cwd       string
	path      string
	offset    int64
}

type AttachedTranscriptMirrorHandlers struct {
	HandleLine               func(string) error
	HandleResult             func(map[string]any) error
	HandlePermissionRequest  func(AttachedTranscriptToolUse) error
	HandlePermissionResolved func(string) error
}

type AttachedTranscriptToolUse struct {
	RequestID string
	Tool      string
	Input     map[string]any
}

func NewAttachedTranscriptMirror(sessionID, cwd string) *AttachedTranscriptMirror {
	return &AttachedTranscriptMirror{
		sessionID: strings.TrimSpace(sessionID),
		cwd:       strings.TrimSpace(cwd),
	}
}

func (m *AttachedTranscriptMirror) Poll(handlers AttachedTranscriptMirrorHandlers) error {
	if m == nil || m.sessionID == "" {
		return nil
	}
	if handlers.HandleLine == nil &&
		handlers.HandleResult == nil &&
		handlers.HandlePermissionRequest == nil &&
		handlers.HandlePermissionResolved == nil {
		return nil
	}

	path, err := findClaudeTranscriptPath(m.sessionID, m.cwd)
	if err != nil {
		if isTranscriptNotReady(err) || os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if path != m.path {
		m.path = path
		m.offset = 0
	}

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < m.offset {
		m.offset = 0
	}
	if _, err := file.Seek(m.offset, io.SeekStart); err != nil {
		return err
	}

	reader := bufio.NewReader(file)
	currentOffset := m.offset
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
			trimmed := strings.TrimSpace(string(line))
			entryType, entry, ok := parseTranscriptMirrorEntry(trimmed)
			if ok && shouldEmitTranscriptMirrorEntry(entryType, entry) && handlers.HandleLine != nil {
				if handleErr := handlers.HandleLine(trimmed); handleErr != nil {
					return handleErr
				}
			}
			if ok && entryType == "result" && handlers.HandleResult != nil {
				if handleErr := handlers.HandleResult(entry); handleErr != nil {
					return handleErr
				}
			}
			if ok && handlers.HandlePermissionRequest != nil {
				for _, toolUse := range extractTranscriptMirrorToolUses(entryType, entry) {
					if handleErr := handlers.HandlePermissionRequest(toolUse); handleErr != nil {
						return handleErr
					}
				}
			}
			if ok && handlers.HandlePermissionResolved != nil {
				for _, requestID := range extractTranscriptMirrorToolResultIDs(entryType, entry) {
					if handleErr := handlers.HandlePermissionResolved(requestID); handleErr != nil {
						return handleErr
					}
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	m.offset = currentOffset
	return nil
}

func shouldEmitTranscriptMirrorLine(line string) bool {
	entryType, entry, ok := parseTranscriptMirrorEntry(line)
	if !ok {
		return false
	}
	return shouldEmitTranscriptMirrorEntry(entryType, entry)
}

func parseTranscriptMirrorEntry(line string) (string, map[string]any, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", nil, false
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return "", nil, false
	}
	entryType := getString(entry, "type")
	if entryType == "" || entryType == "file-history-snapshot" {
		return "", nil, false
	}
	if getBool(entry, "isMeta") {
		return "", nil, false
	}
	return entryType, entry, true
}

func shouldEmitTranscriptMirrorEntry(entryType string, entry map[string]any) bool {
	switch entryType {
	case "assistant", "user", "system", "summary", "progress":
		return true
	default:
		return false
	}
}

func extractTranscriptMirrorToolUses(entryType string, entry map[string]any) []AttachedTranscriptToolUse {
	if entryType != "assistant" {
		return nil
	}

	message, _ := entry["message"].(map[string]any)
	content, _ := message["content"].([]any)
	if len(content) == 0 {
		return nil
	}

	result := make([]AttachedTranscriptToolUse, 0, len(content))
	for _, rawBlock := range content {
		block, ok := rawBlock.(map[string]any)
		if !ok || getString(block, "type") != "tool_use" {
			continue
		}
		requestID := strings.TrimSpace(getString(block, "id"))
		tool := strings.TrimSpace(getString(block, "name"))
		input, _ := block["input"].(map[string]any)
		if requestID == "" || tool == "" {
			continue
		}
		if input == nil {
			input = map[string]any{}
		}
		result = append(result, AttachedTranscriptToolUse{
			RequestID: requestID,
			Tool:      tool,
			Input:     input,
		})
	}
	return result
}

func extractTranscriptMirrorToolResultIDs(entryType string, entry map[string]any) []string {
	if entryType != "user" {
		return nil
	}

	message, _ := entry["message"].(map[string]any)
	content, _ := message["content"].([]any)
	if len(content) == 0 {
		return nil
	}

	result := make([]string, 0, len(content))
	for _, rawBlock := range content {
		block, ok := rawBlock.(map[string]any)
		if !ok || getString(block, "type") != "tool_result" {
			continue
		}
		requestID := strings.TrimSpace(getString(block, "tool_use_id"))
		if requestID != "" {
			result = append(result, requestID)
		}
	}
	return result
}
