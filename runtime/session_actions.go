package remote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

func (s *Service) handleSessionAction(hostSessionID string, req protocol.SessionActionPayload) {
	resp := s.executeSessionAction(req)
	if err := s.writeJSON(protocol.Message{
		Type:      protocol.TypeSessionActionResult,
		SessionID: hostSessionID,
		Payload:   resp,
	}); err != nil {
		applog.Errorf("[Remote] WS write session-action-result error: %v", err)
	}
}

// PauseSession 暂停当前管理端名下的子会话。
func (s *Service) PauseSession(sessionID string) error {
	return s.applySessionAction(sessionID, protocol.SessionActionPause)
}

// DeleteSession 删除当前管理端名下的子会话，并清理本地 transcript。
func (s *Service) DeleteSession(sessionID string) error {
	return s.applySessionAction(sessionID, protocol.SessionActionDelete)
}

// DeleteSessionTranscript 删除本地 Claude transcript 文件。
func DeleteSessionTranscript(sessionID, cwd string) error {
	return deleteClaudeTranscript(sessionID, cwd)
}

func (s *Service) applySessionAction(sessionID, action string) error {
	resp := s.executeSessionAction(protocol.SessionActionPayload{
		SessionID: strings.TrimSpace(sessionID),
		Action:    strings.TrimSpace(action),
	})
	if strings.TrimSpace(resp.Error) != "" {
		return fmt.Errorf("%s", strings.TrimSpace(resp.Error))
	}
	return nil
}

func (s *Service) executeSessionAction(req protocol.SessionActionPayload) protocol.SessionActionResultPayload {
	resp := protocol.SessionActionResultPayload{
		RequestID: strings.TrimSpace(req.RequestID),
		SessionID: strings.TrimSpace(req.SessionID),
		Action:    strings.TrimSpace(req.Action),
	}

	if !s.cfg.Management {
		resp.Error = "The current connection does not support session management"
		return resp
	}
	if resp.SessionID == "" {
		resp.Error = "Missing session ID"
		return resp
	}

	target := s.findChildBySessionID(resp.SessionID)
	if target == nil {
		resp.Error = "Session was not found or has already ended"
		return resp
	}
	if target.getThinking() {
		switch resp.Action {
		case protocol.SessionActionDelete:
			resp.Error = "The session is still thinking and cannot be deleted"
		default:
			resp.Error = "The session is still thinking and cannot be paused"
		}
		return resp
	}

	targetCWD := target.getCurrentDir()
	stopReason := "session-action"
	lifecycleState := ""
	switch resp.Action {
	case protocol.SessionActionPause:
		stopReason = "paused-by-manager"
		lifecycleState = protocol.SessionLifecyclePaused
	case protocol.SessionActionDelete:
		stopReason = "deleted-by-manager"
		lifecycleState = protocol.SessionLifecycleDeleted
	default:
		resp.Error = fmt.Sprintf("Unsupported session action: %s", resp.Action)
		return resp
	}

	if lifecycleState != "" {
		if err := target.sendSessionLifecycleEvent(lifecycleState, stopReason); err != nil {
			applog.Errorf("[Remote] send session lifecycle failed: session=%s state=%s err=%v", resp.SessionID, lifecycleState, err)
		}
	}

	if err := target.StopWithReason(stopReason); err != nil {
		resp.Error = err.Error()
		return resp
	}
	s.detachChildBySessionID(resp.SessionID)

	if resp.Action == protocol.SessionActionPause {
		if err := s.markSessionStopReason(resp.SessionID, stopReason); err != nil {
			applog.Errorf("[Remote] mark session stop reason failed: session=%s reason=%s err=%v", resp.SessionID, stopReason, err)
		}
	}

	if resp.Action == protocol.SessionActionDelete {
		if err := deleteClaudeTranscript(resp.SessionID, targetCWD); err != nil {
			resp.Error = err.Error()
			return resp
		}
	}

	return resp
}

func (s *Service) findChildBySessionID(sessionID string) *Service {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}

	s.mu.Lock()
	children := append([]*Service(nil), s.children...)
	s.mu.Unlock()

	for _, child := range children {
		if child.matchesSessionID(sessionID) {
			return child
		}
	}
	return nil
}

func (s *Service) sendSessionLifecycleEvent(state, reason string) error {
	sessionID := strings.TrimSpace(s.SessionID())
	if sessionID == "" {
		return nil
	}
	return s.writeJSON(protocol.Message{
		Type:      protocol.TypeSessionLifecycle,
		SessionID: sessionID,
		Payload: protocol.SessionLifecyclePayload{
			State:      strings.TrimSpace(state),
			Reason:     strings.TrimSpace(reason),
			OccurredAt: time.Now().UnixMilli(),
		},
	})
}

func (s *Service) markSessionStopReason(sessionID, stopReason string) error {
	serverURL := strings.TrimSpace(s.cfg.ServerURL)
	token := strings.TrimSpace(s.cfg.Token)
	sessionID = strings.TrimSpace(sessionID)
	stopReason = strings.TrimSpace(stopReason)
	if serverURL == "" || token == "" || sessionID == "" || stopReason == "" {
		return nil
	}

	reqBody, err := json.Marshal(map[string]string{
		"session_id":  sessionID,
		"stop_reason": stopReason,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, serverURL+"/ws/terminal/sessions/mark-stop", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mark stop reason failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *Service) detachChildBySessionID(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := s.children[:0]
	for _, child := range s.children {
		if child == nil || child.matchesSessionID(sessionID) {
			continue
		}
		filtered = append(filtered, child)
	}
	s.children = filtered
}

func deleteClaudeTranscript(sessionID, cwd string) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}

	path, err := findClaudeTranscriptPath(strings.TrimSpace(sessionID), strings.TrimSpace(cwd))
	if err != nil {
		if isTranscriptNotReady(err) || os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to delete the local session transcript: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete the local session transcript: %w", err)
	}
	return nil
}
