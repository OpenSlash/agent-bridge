package remote

import (
	"fmt"
	"os"
	"strings"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

type createSessionAttempt struct {
	done    chan struct{}
	payload protocol.SessionCreatedPayload
}

func createSessionKey(req protocol.CreateSessionPayload) string {
	if key := strings.TrimSpace(req.IdempotencyKey); key != "" {
		return key
	}
	return strings.TrimSpace(req.RequestID)
}

func (s *Service) beginCreateSession(req protocol.CreateSessionPayload) (protocol.SessionCreatedPayload, bool) {
	key := createSessionKey(req)
	if key == "" {
		return protocol.SessionCreatedPayload{}, false
	}

	s.mu.Lock()
	if s.createSessionAttempts == nil {
		s.createSessionAttempts = make(map[string]*createSessionAttempt)
	}
	if attempt := s.createSessionAttempts[key]; attempt != nil {
		done := attempt.done
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		payload := attempt.payload
		s.mu.Unlock()
		payload.RequestID = strings.TrimSpace(req.RequestID)
		payload.IdempotencyKey = key
		return payload, true
	}
	s.createSessionAttempts[key] = &createSessionAttempt{done: make(chan struct{})}
	s.mu.Unlock()
	return protocol.SessionCreatedPayload{}, false
}

func (s *Service) completeCreateSession(parentSessionID string, req protocol.CreateSessionPayload, payload protocol.SessionCreatedPayload) {
	payload.RequestID = strings.TrimSpace(req.RequestID)
	payload.IdempotencyKey = createSessionKey(req)
	if key := createSessionKey(req); key != "" {
		s.mu.Lock()
		if attempt := s.createSessionAttempts[key]; attempt != nil {
			attempt.payload = payload
			close(attempt.done)
		}
		s.mu.Unlock()
	}
	s.writeSessionCreated(parentSessionID, payload)
}

func (s *Service) writeSessionCreated(parentSessionID string, payload protocol.SessionCreatedPayload) {
	if err := s.writeJSON(protocol.Message{
		Type:      protocol.TypeSessionCreated,
		SessionID: parentSessionID,
		Payload:   payload,
	}); err != nil {
		applog.Errorf("[Remote] WS write session-created error: %v", err)
	}
}

func (s *Service) addChild(child *Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.children = append(s.children, child)
}

func (s *Service) notifyChildStarted(child *Service, cfg Config) {
	s.mu.Lock()
	hook := s.childStartedHook
	s.mu.Unlock()
	if hook == nil || child == nil {
		return
	}
	hook(child, cfg)
}

func (s *Service) handleCreateSession(parentSessionID string, req protocol.CreateSessionPayload) {
	if cached, duplicate := s.beginCreateSession(req); duplicate {
		s.writeSessionCreated(parentSessionID, cached)
		return
	}
	applog.Info.Printf(
		"[Remote] create-session requested: parent=%s cwd=%s runtime=%s model=%s permission=%s sandbox=%s resume=%s runtime_resume=%s",
		parentSessionID,
		req.WorkingDir,
		req.Runtime,
		req.Model,
		req.PermissionMode,
		req.SandboxMode,
		req.ResumeSessionID,
		req.ResumeRuntimeSessionID,
	)

	childCfg := s.cfg
	childCfg.Management = false
	childCfg.HostID = s.sessionID
	workingDir, err := resolveDirectoryWithinUserHome(req.WorkingDir, "", true)
	if err != nil {
		payload := protocol.SessionCreatedPayload{
			Error: err.Error(),
		}
		s.completeCreateSession(parentSessionID, req, payload)
		return
	}
	childCfg.WorkingDir = workingDir
	if requestedCommand, err := commandForRequestedRuntime(req.Runtime, childCfg); err != nil {
		payload := protocol.SessionCreatedPayload{
			Error: err.Error(),
		}
		s.completeCreateSession(parentSessionID, req, payload)
		return
	} else if requestedCommand != "" {
		childCfg.Command = requestedCommand
	}
	if childCfg.WorkingDir == "" {
		if homeDir, homeErr := os.UserHomeDir(); homeErr == nil {
			childCfg.WorkingDir = homeDir
		} else {
			childCfg.WorkingDir = s.getCurrentDir()
		}
	}
	if req.Model != "" {
		childCfg.Model = req.Model
	}
	if req.PermissionMode != "" {
		childCfg.PermissionMode = req.PermissionMode
	}
	if req.SandboxMode != "" {
		childCfg.SandboxMode = req.SandboxMode
	}
	if req.ResumeSessionID != "" {
		childCfg.SessionID = req.ResumeSessionID
		childCfg.RuntimeSessionID = req.ResumeRuntimeSessionID
		childCfg.Resume = true
	}
	resolvedAttachments, err := resolveCreateSessionAttachments(req.Attachments)
	if err != nil {
		payload := protocol.SessionCreatedPayload{Error: err.Error()}
		s.completeCreateSession(parentSessionID, req, payload)
		return
	}

	payload := protocol.SessionCreatedPayload{}

	child := NewService()
	child.SetAutoReconnectEnabled(s.AutoReconnectEnabled())
	sessionID, err := child.StartProxy(&childCfg)
	if err != nil {
		payload.Error = err.Error()
		applog.Errorf(
			"[Remote] create-session failed: parent=%s resume=%s err=%v",
			parentSessionID,
			req.ResumeSessionID,
			err,
		)
	} else {
		payload.SessionID = sessionID
		initialInput := buildInitialInputForRuntime(detectRuntime(childCfg.Command), req.InitialPrompt, resolvedAttachments)
		if initialInput != "" {
			if inputErr := child.SendInput(initialInput); inputErr != nil {
				_ = child.StopWithReason("initial-prompt-failed")
				payload.SessionID = ""
				payload.Error = fmt.Sprintf("failed to submit initial prompt: %v", inputErr)
				s.completeCreateSession(parentSessionID, req, payload)
				return
			}
		}
		s.addChild(child)
		childCfg.SessionID = sessionID
		if currentDir := strings.TrimSpace(child.CurrentDir()); currentDir != "" {
			childCfg.WorkingDir = currentDir
		}
		if currentModel := strings.TrimSpace(child.CurrentModel()); currentModel != "" {
			childCfg.Model = currentModel
		}
		if runtimeSessionID := strings.TrimSpace(child.RuntimeSessionID()); runtimeSessionID != "" {
			childCfg.RuntimeSessionID = runtimeSessionID
		}
		s.notifyChildStarted(child, childCfg)
		applog.Info.Printf(
			"[Remote] create-session ready: parent=%s session=%s resume=%s runtime_resume=%s",
			parentSessionID,
			sessionID,
			req.ResumeSessionID,
			req.ResumeRuntimeSessionID,
		)
	}

	s.completeCreateSession(parentSessionID, req, payload)
}

func (s *Service) startChildProxy(cfg *Config) (string, error) {
	childCfg := *cfg
	childCfg.Management = false
	if childCfg.Command == "" {
		childCfg.Command = "claude"
	}
	if childCfg.ServerURL == "" || childCfg.Token == "" {
		s.mu.Lock()
		childCfg.ServerURL = s.cfg.ServerURL
		childCfg.Token = s.cfg.Token
		if childCfg.Command == "" {
			childCfg.Command = s.cfg.Command
		}
		if len(childCfg.Args) == 0 {
			childCfg.Args = append([]string(nil), s.cfg.Args...)
		}
		s.mu.Unlock()
	}
	if childCfg.WorkingDir == "" {
		childCfg.WorkingDir = s.getCurrentDir()
	}
	if childCfg.HostID == "" {
		childCfg.HostID = s.sessionID
	}
	if childCfg.PermissionMode == "" {
		childCfg.PermissionMode = protocol.PermissionModeDefault
	}
	if childCfg.SandboxMode == "" {
		childCfg.SandboxMode = defaultSandboxModeForRuntime(detectRuntime(childCfg.Command))
	}
	if childCfg.Resume && childCfg.SessionID == "" {
		childCfg.Resume = false
	}

	child := NewService()
	child.SetAutoReconnectEnabled(s.AutoReconnectEnabled())
	sessionID, err := child.Start(&childCfg)
	if err != nil {
		return "", err
	}
	s.addChild(child)
	childCfg.SessionID = sessionID
	if currentDir := strings.TrimSpace(child.CurrentDir()); currentDir != "" {
		childCfg.WorkingDir = currentDir
	}
	if currentModel := strings.TrimSpace(child.CurrentModel()); currentModel != "" {
		childCfg.Model = currentModel
	}
	if runtimeSessionID := strings.TrimSpace(child.RuntimeSessionID()); runtimeSessionID != "" {
		childCfg.RuntimeSessionID = runtimeSessionID
	}
	s.notifyChildStarted(child, childCfg)
	return sessionID, nil
}

func commandForRequestedRuntime(runtimeID string, cfg Config) (string, error) {
	fallbackCommand := strings.TrimSpace(cfg.Command)
	switch strings.TrimSpace(strings.ToLower(runtimeID)) {
	case "":
		return fallbackCommand, nil
	case string(runtimeClaude):
		if !cfg.ClaudeEnabled {
			return "", fmt.Errorf("runtime disabled: %s", runtimeClaude)
		}
		if command := strings.TrimSpace(cfg.ClaudeCommand); command != "" {
			return command, nil
		}
		return string(runtimeClaude), nil
	case string(runtimeCodex):
		if !cfg.CodexEnabled {
			return "", fmt.Errorf("runtime disabled: %s", runtimeCodex)
		}
		if command := strings.TrimSpace(cfg.CodexCommand); command != "" {
			return command, nil
		}
		return string(runtimeCodex), nil
	default:
		return "", fmt.Errorf("unsupported runtime: %s", runtimeID)
	}
}
