package remote

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func (s *Service) EmitTerminalText(content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	s.mu.Lock()
	sessionID := s.sessionID
	s.mu.Unlock()
	if sessionID == "" {
		return fmt.Errorf("session is not running")
	}

	payload, err := json.Marshal(map[string]any{
		"type":    "system",
		"content": content,
	})
	if err != nil {
		return err
	}

	return s.writeJSON(protocol.Message{
		Type:      protocol.TypeText,
		SessionID: sessionID,
		Payload: protocol.TextPayload{
			Data:     string(payload),
			Thinking: s.getThinking(),
		},
	})
}

func (s *Service) EmitStructuredTextLine(line string) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	if shouldSuppressSessionStructuredTextLine(line) {
		return nil
	}

	s.mu.Lock()
	sessionID := s.sessionID
	s.mu.Unlock()
	if sessionID == "" {
		return fmt.Errorf("session is not running")
	}

	return s.writeJSON(protocol.Message{
		Type:      protocol.TypeText,
		SessionID: sessionID,
		Payload: protocol.TextPayload{
			Data:     line,
			Thinking: s.getThinking(),
		},
	})
}

func shouldSuppressSessionStructuredTextLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return false
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return false
	}
	if strings.TrimSpace(getString(entry, "type")) != "system" {
		return false
	}

	content := strings.TrimSpace(extractTranscriptEntryContent(entry))
	if _, ok := extractTranscriptTaggedContent(content, "local-command-stdout"); ok {
		return true
	}
	return false
}

func (s *Service) beginAttachedTurn(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	s.beginTurn()
	return s.writeJSON(protocol.Message{
		Type:      protocol.TypeTurnStart,
		SessionID: sessionID,
		Payload: protocol.TurnStartPayload{
			TurnID: fmt.Sprintf("turn-%d", time.Now().UnixMilli()),
		},
	})
}

func (s *Service) StartAttachedTurn() error {
	s.mu.Lock()
	sessionID := strings.TrimSpace(s.sessionID)
	alreadyActive := s.turnActive
	s.mu.Unlock()
	if sessionID == "" || alreadyActive {
		return nil
	}
	return s.beginAttachedTurn(sessionID)
}

func (s *Service) MarkAttachedInterruptRequested() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interruptRequested = true
	if s.pendingInterrupts == nil {
		s.pendingInterrupts = make(map[string]struct{})
	}
}

func (s *Service) FinishAttachedTurnFromResult(event map[string]any) error {
	if event == nil {
		return nil
	}

	status, shouldEmit := s.finishTurnFromResult(event)
	if !shouldEmit {
		return nil
	}

	s.mu.Lock()
	sessionID := strings.TrimSpace(s.sessionID)
	s.mu.Unlock()
	if sessionID == "" {
		return fmt.Errorf("session is not running")
	}

	return s.sendTurnEnd(sessionID, status)
}

func (s *Service) FinishAttachedTurn(status string) error {
	if strings.TrimSpace(status) == "" {
		status = protocol.TurnCompleted
	}

	s.mu.Lock()
	sessionID := strings.TrimSpace(s.sessionID)
	if sessionID == "" || !s.turnActive {
		s.mu.Unlock()
		return nil
	}
	s.thinking = false
	s.turnActive = false
	s.interruptRequested = false
	s.pendingInterrupts = make(map[string]struct{})
	s.mu.Unlock()

	return s.sendTurnEnd(sessionID, status)
}
