package remote

import (
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

type reconnectSnapshot struct {
	cfg             Config
	sessionID       string
	workingDir      string
	model           string
	reasoningEffort string
	permissionMode  string
	sandboxMode     string

	attachedInputHandler     func(string) error
	attachedInterruptHandler func() error
	attachedConfigHandler    func(protocol.SessionConfigPayload) error
}

func (s *Service) SetAutoReconnectEnabled(enabled bool) {
	s.mu.Lock()
	s.autoReconnect = enabled
	children := append([]*Service(nil), s.children...)
	if !enabled {
		s.cancelReconnectAttemptLocked()
	}
	s.mu.Unlock()

	for _, child := range children {
		child.SetAutoReconnectEnabled(enabled)
	}
}

func (s *Service) CancelReconnects() {
	s.mu.Lock()
	children := append([]*Service(nil), s.children...)
	s.cancelReconnectAttemptLocked()
	s.mu.Unlock()

	for _, child := range children {
		child.CancelReconnects()
	}
}

func (s *Service) AutoReconnectEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autoReconnect
}

func (s *Service) ForgetSession(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}

	if s.cancelReconnectForOwnedSession(sessionID) {
		return true
	}

	s.mu.Lock()
	children := append([]*Service(nil), s.children...)
	s.mu.Unlock()

	for _, child := range children {
		if child.cancelReconnectForOwnedSession(sessionID) {
			s.detachChildBySessionID(sessionID)
			return true
		}
		if child.ForgetSession(sessionID) {
			return true
		}
	}

	return false
}

func (s *Service) cancelReconnectForOwnedSession(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.matchesSessionIDLocked(sessionID) {
		return false
	}
	s.cancelReconnectAttemptLocked()
	return true
}

func (s *Service) scheduleAutoReconnect(snapshot reconnectSnapshot) {
	s.mu.Lock()
	if !s.autoReconnect || s.reconnectAttempt != nil {
		s.mu.Unlock()
		return
	}
	attempt := &reconnectAttempt{cancel: make(chan struct{})}
	s.reconnectAttempt = attempt
	s.mu.Unlock()

	go func() {
		defer s.clearReconnectAttempt(attempt)

		cfg := snapshot.cfg
		if snapshot.workingDir != "" {
			cfg.WorkingDir = snapshot.workingDir
		}
		cfg.Model = snapshot.model
		cfg.ReasoningEffort = snapshot.reasoningEffort
		cfg.PermissionMode = snapshot.permissionMode
		cfg.SandboxMode = snapshot.sandboxMode
		if snapshot.sessionID != "" {
			cfg.SessionID = snapshot.sessionID
			cfg.Resume = true
		}
		attachedHandlers := AttachedHandlers{
			SendInput:   snapshot.attachedInputHandler,
			Interrupt:   snapshot.attachedInterruptHandler,
			ApplyConfig: snapshot.attachedConfigHandler,
		}
		attachedMode := attachedHandlers.SendInput != nil

		backoff := 2 * time.Second
		for {
			select {
			case <-attempt.cancel:
				return
			default:
			}

			var err error
			if attachedMode {
				_, err = s.StartAttached(&cfg, attachedHandlers)
			} else {
				_, err = s.Start(&cfg)
			}
			if err == nil {
				s.emitLocalSessionLifecycle(snapshot.sessionID, protocol.SessionLifecycleResumed, "auto-reconnected")
				applog.Info.Printf("[Remote] session auto-resumed: session=%s cwd=%s", snapshot.sessionID, cfg.WorkingDir)
				return
			} else {
				applog.Errorf("[Remote] session auto-resume failed: session=%s err=%v", snapshot.sessionID, err)
			}

			timer := time.NewTimer(backoff)
			select {
			case <-attempt.cancel:
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		}
	}()
}

func (s *Service) clearReconnectAttempt(attempt *reconnectAttempt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reconnectAttempt == attempt {
		s.reconnectAttempt = nil
	}
}

func (s *Service) cancelReconnectAttemptLocked() {
	if s.reconnectAttempt == nil {
		return
	}
	close(s.reconnectAttempt.cancel)
	s.reconnectAttempt = nil
}

func (s *Service) matchesSessionID(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.matchesSessionIDLocked(sessionID)
}

func (s *Service) matchesSessionIDLocked(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	currentID := strings.TrimSpace(s.sessionID)
	if currentID != "" && currentID == sessionID {
		return true
	}
	return strings.TrimSpace(s.cfg.SessionID) == sessionID
}
