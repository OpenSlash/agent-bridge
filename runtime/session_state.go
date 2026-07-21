package remote

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

func (s *Service) setThinking(thinking bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.thinking = thinking
}

func (s *Service) setCodexPermissionWaiting(waiting bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.runtime != runtimeCodex {
		return false
	}
	if waiting {
		if !s.turnActive || !s.thinking {
			return false
		}
		s.thinking = false
		return true
	}
	return false
}

func (s *Service) beginTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.thinking = true
	s.turnActive = true
	s.interruptRequested = false
	s.pendingInterrupts = make(map[string]struct{})
}

func (s *Service) getThinking() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thinking
}

func (s *Service) getCurrentDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentDir
}

func (s *Service) getCurrentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentModel
}

func (s *Service) getCurrentReasoningEffort() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.currentReasoningEffort)
}

func (s *Service) setCurrentReasoningEffort(effort string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentReasoningEffort = strings.TrimSpace(effort)
	s.cfg.ReasoningEffort = s.currentReasoningEffort
}

func (s *Service) getCurrentPermissionMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentPermissionMode == "" {
		return protocol.PermissionModeDefault
	}
	return s.currentPermissionMode
}

func (s *Service) setCurrentPermissionMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentPermissionMode = normalizePermissionModeForRuntime(s.runtime, mode)
}

func (s *Service) getCurrentSandboxMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentSandboxMode == "" {
		return defaultSandboxModeForRuntime(s.runtime)
	}
	return s.currentSandboxMode
}

func (s *Service) setCurrentSandboxMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentSandboxMode = normalizeSandboxModeForRuntime(s.runtime, mode)
}

func (s *Service) sendCurrentKeepalive(sessionID string) error {
	applog.Info.Printf(
		"[Remote] keepalive send: session=%s runtime=%s thinking=%t model=%s reasoning=%s cwd=%s permission=%s sandbox=%s",
		sessionID,
		s.getRuntimeSessionID(),
		s.getThinking(),
		s.getCurrentModel(),
		s.getCurrentReasoningEffort(),
		s.getCurrentDir(),
		s.getCurrentPermissionMode(),
		s.getCurrentSandboxMode(),
	)
	msg := protocol.Message{
		Type:      protocol.TypeKeepalive,
		SessionID: sessionID,
		Payload: protocol.KeepalivePayload{
			Thinking:         s.getThinking(),
			Mode:             sessionMode(s.cfg.Management),
			Model:            s.getCurrentModel(),
			ReasoningEffort:  s.getCurrentReasoningEffort(),
			Cwd:              s.getCurrentDir(),
			RuntimeSessionID: s.getRuntimeSessionID(),
			PermissionMode:   s.getCurrentPermissionMode(),
			SandboxMode:      s.getCurrentSandboxMode(),
			RuntimeCatalog:   s.getRuntimeCatalogSnapshot(),
			HostReadiness:    s.getHostReadinessSnapshot(),
		},
	}
	return s.writeJSON(msg)
}

func (s *Service) getRuntimeSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtimeSessionID
}

func (s *Service) getRuntimeCatalogSnapshot() []protocol.RuntimeCapability {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cfg.Management || len(s.cfg.RuntimeCatalog) == 0 {
		return nil
	}
	return append([]protocol.RuntimeCapability(nil), s.cfg.RuntimeCatalog...)
}

func (s *Service) getHostReadinessSnapshot() protocol.HostReadiness {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cfg.Management {
		return protocol.HostReadiness{}
	}
	return cloneHostReadiness(s.cfg.HostReadiness)
}

func cloneHostReadiness(readiness protocol.HostReadiness) protocol.HostReadiness {
	readiness.Issues = append([]protocol.HostReadinessIssue(nil), readiness.Issues...)
	return readiness
}

func (s *Service) sendTurnEnd(sessionID, status string) error {
	return s.writeJSON(protocol.Message{
		Type:      protocol.TypeTurnEnd,
		SessionID: sessionID,
		Payload: protocol.TurnEndPayload{
			Status: status,
		},
	})
}

func sessionMode(management bool) string {
	if management {
		return "manager"
	}
	return "remote"
}

func (s *Service) requestInterrupt() error {
	adapter := s.getRuntimeAdapter()
	if adapter == nil {
		return fmt.Errorf("runtime adapter is not available")
	}
	return adapter.RequestInterrupt(s)
}

func (s *Service) requestClaudeInterrupt() error {
	s.mu.Lock()
	stdin := s.stdin
	turnActive := s.turnActive
	if turnActive && s.pendingInterrupts == nil {
		s.pendingInterrupts = make(map[string]struct{})
	}
	s.mu.Unlock()

	if !turnActive {
		return fmt.Errorf("no active turn to interrupt")
	}
	if stdin == nil {
		return fmt.Errorf("stdin is not available")
	}

	requestID := fmt.Sprintf("interrupt-%d", time.Now().UnixNano())

	s.mu.Lock()
	s.interruptRequested = true
	s.pendingInterrupts[requestID] = struct{}{}
	s.mu.Unlock()

	req := sdkControlRequest{
		Type:      "control_request",
		RequestID: requestID,
		Request: sdkControlRequestPayload{
			Subtype: "interrupt",
		},
	}

	if err := s.writeJSONLineTo(stdin, req); err != nil {
		s.mu.Lock()
		delete(s.pendingInterrupts, requestID)
		if len(s.pendingInterrupts) == 0 {
			s.interruptRequested = false
		}
		s.mu.Unlock()
		return err
	}

	return nil
}

func (s *Service) requestCodexTurnInterrupt() error {
	s.mu.Lock()
	turnActive := s.turnActive
	s.mu.Unlock()
	if !turnActive {
		return fmt.Errorf("no active turn to interrupt")
	}

	s.mu.Lock()
	s.interruptRequested = true
	s.mu.Unlock()
	if err := s.requestCodexInterrupt(); err != nil {
		s.mu.Lock()
		s.interruptRequested = false
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *Service) handleControlResponse(resp sdkControlResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.pendingInterrupts[resp.Response.RequestID]; !ok {
		return
	}

	delete(s.pendingInterrupts, resp.Response.RequestID)
	if resp.Response.Subtype == "error" && len(s.pendingInterrupts) == 0 {
		s.interruptRequested = false
	}
}

func (s *Service) finishTurnFromResult(event map[string]any) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := turnStatusForResult(s.interruptRequested, event)
	s.thinking = false
	s.pendingInterrupts = make(map[string]struct{})
	if !s.turnActive {
		s.interruptRequested = false
		return status, false
	}

	s.turnActive = false
	s.interruptRequested = false
	return status, true
}

func (s *Service) finishTurnOnExit() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pendingInterrupts = make(map[string]struct{})
	s.thinking = false
	s.codexTurnID = ""
	if !s.turnActive {
		s.interruptRequested = false
		return "", false
	}

	status := protocol.TurnFailed
	if s.interruptRequested {
		status = protocol.TurnCancelled
	}

	s.turnActive = false
	s.interruptRequested = false
	return status, true
}

func turnStatusForResult(interrupted bool, event map[string]any) string {
	if interrupted {
		return protocol.TurnCancelled
	}

	if isError, _ := event["is_error"].(bool); isError {
		return protocol.TurnFailed
	}

	if subtype, _ := event["subtype"].(string); subtype != "" && subtype != "success" {
		return protocol.TurnFailed
	}

	return protocol.TurnCompleted
}

func (s *Service) writeJSONLineTo(writer io.Writer, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	_, err = writer.Write(data)
	return err
}
