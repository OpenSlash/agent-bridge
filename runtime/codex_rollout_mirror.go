package remote

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type AttachedCodexRolloutMirror struct {
	cwd              string
	path             string
	offset           int64
	runtimeSessionID string
}

type AttachedCodexRolloutMirrorHandlers struct {
	HandleRuntimeSessionID func(string) error
	HandleAssistantLine    func(string) error
	HandleTurnComplete     func() error
}

func NewAttachedCodexRolloutMirror(cwd string) *AttachedCodexRolloutMirror {
	return &AttachedCodexRolloutMirror{
		cwd: strings.TrimSpace(cwd),
	}
}

func (m *AttachedCodexRolloutMirror) BeginTurn() error {
	if m == nil {
		return nil
	}
	path, runtimeSessionID, err := m.resolveLatestRollout()
	if err != nil || path == "" {
		return err
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		return statErr
	}
	m.path = path
	m.runtimeSessionID = runtimeSessionID
	m.offset = info.Size()
	return nil
}

func (m *AttachedCodexRolloutMirror) Poll(handlers AttachedCodexRolloutMirrorHandlers) error {
	if m == nil || strings.TrimSpace(m.cwd) == "" {
		return nil
	}

	path, runtimeSessionID, err := m.resolveLatestRollout()
	if err != nil {
		if os.IsNotExist(err) || isCodexRolloutNotReady(err) {
			return nil
		}
		return err
	}
	if path == "" {
		return nil
	}

	if path != m.path {
		m.path = path
		m.offset = 0
	}
	if runtimeSessionID != "" && runtimeSessionID != m.runtimeSessionID {
		m.runtimeSessionID = runtimeSessionID
		if handlers.HandleRuntimeSessionID != nil {
			if err := handlers.HandleRuntimeSessionID(runtimeSessionID); err != nil {
				return err
			}
		}
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
				trimmed := bytesTrimSpace(line)
				if len(trimmed) == 0 || !json.Valid(trimmed) {
					break
				}
			}

			currentOffset += int64(len(line))
			entry := strings.TrimSpace(string(line))
			if handlers.HandleAssistantLine != nil {
				for _, outputLine := range codexRolloutDisplayLines(entry) {
					if strings.TrimSpace(outputLine) == "" {
						continue
					}
					if handleErr := handlers.HandleAssistantLine(outputLine); handleErr != nil {
						return handleErr
					}
				}
			}
			if handlers.HandleTurnComplete != nil && codexRolloutLineMarksTaskComplete(entry) {
				if handleErr := handlers.HandleTurnComplete(); handleErr != nil {
					return handleErr
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

func (m *AttachedCodexRolloutMirror) resolveLatestRollout() (string, string, error) {
	runtimeSessionID, path, ok, err := findRecentCodexRolloutSession(filepath.Clean(strings.TrimSpace(m.cwd)), 0)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", nil
	}
	return path, runtimeSessionID, nil
}

func codexRolloutDisplayLines(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return nil
	}
	if strings.TrimSpace(getString(entry, "type")) != "response_item" {
		return nil
	}

	payload, _ := entry["payload"].(map[string]any)
	if payload == nil || strings.TrimSpace(getString(payload, "type")) != "message" {
		return nil
	}
	role := strings.TrimSpace(getString(payload, "role"))
	if role != "assistant" && role != "user" {
		return nil
	}

	content, _ := payload["content"].([]any)
	if len(content) == 0 {
		return nil
	}

	lines := make([]string, 0, len(content))
	for _, rawItem := range content {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.TrimSpace(getString(item, "type"))
		text := strings.TrimSpace(getString(item, "text"))
		switch role {
		case "assistant":
			if itemType != "output_text" || text == "" {
				continue
			}
			lines = append(lines, marshalCompactJSON(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"role": "assistant",
					"content": []map[string]any{{
						"type": "text",
						"text": text,
					}},
				},
			}))
		case "user":
			if itemType != "input_text" || text == "" {
				continue
			}
			lines = append(lines, marshalCompactJSON(map[string]any{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": text,
				},
			}))
		}
	}
	return lines
}

func codexRolloutLineMarksTaskComplete(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return false
	}
	if strings.TrimSpace(getString(entry, "type")) != "event_msg" {
		return false
	}
	payload, _ := entry["payload"].(map[string]any)
	return strings.TrimSpace(getString(payload, "type")) == "task_complete"
}
