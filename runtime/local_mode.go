package remote

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/protocol"

	"github.com/google/uuid"
)

// StartLocal 启动仅本地使用的 CLI 代理会话，不连接远程 websocket。
func (s *Service) StartLocal(cfg *Config, sink func(protocol.Message) error) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("config is required")
	}

	s.mu.Lock()
	if s.running {
		sessionID := s.sessionID
		s.mu.Unlock()
		return sessionID, nil
	}
	s.mu.Unlock()

	if cfg.Management {
		return "", fmt.Errorf("local mode does not support management sessions")
	}
	if sink == nil {
		return "", fmt.Errorf("message sink is required")
	}
	if cfg.Command == "" {
		cfg.Command = "claude"
	}

	resolvedCommand, err := resolveCommandPath(cfg.Command)
	if err != nil {
		return "", fmt.Errorf("failed to resolve command %q: %w", cfg.Command, err)
	}
	cfg.Command = resolvedCommand

	adapter := resolveRuntimeAdapter(cfg.Command)
	runtimeKind := adapter.Kind()

	cwd := strings.TrimSpace(cfg.WorkingDir)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd = filepath.Clean(cwd)
	if info, err := os.Stat(cwd); err != nil {
		return "", fmt.Errorf("invalid working dir %s: %w", cwd, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("working dir is not a directory: %s", cwd)
	}

	if err := adapter.PrepareStart(s, cfg, cwd); err != nil {
		return "", err
	}

	s.cfg = Config{
		ServerURL:        cfg.ServerURL,
		Token:            cfg.Token,
		AgentVersion:     cfg.AgentVersion,
		Command:          cfg.Command,
		ClaudeCommand:    cfg.ClaudeCommand,
		CodexCommand:     cfg.CodexCommand,
		Args:             append([]string(nil), cfg.Args...),
		WorkingDir:       cwd,
		Model:            cfg.Model,
		PermissionMode:   normalizePermissionModeForRuntime(runtimeKind, cfg.PermissionMode),
		SandboxMode:      normalizeSandboxModeForRuntime(runtimeKind, cfg.SandboxMode),
		SessionID:        cfg.SessionID,
		RuntimeSessionID: cfg.RuntimeSessionID,
		Resume:           cfg.Resume,
		ClaudeEnabled:    cfg.ClaudeEnabled,
		CodexEnabled:     cfg.CodexEnabled,
		RuntimeCatalog:   append([]protocol.RuntimeCapability(nil), cfg.RuntimeCatalog...),
		HostReadiness:    cloneHostReadiness(cfg.HostReadiness),
	}

	cmd, stdinPipe, stdoutPipe, err := s.startCommand(cfg.SessionID, cwd, cfg.Model, cfg.PermissionMode, cfg.Resume)
	if err != nil {
		return "", fmt.Errorf("failed to start command: %w", err)
	}
	stdoutReader := bufio.NewReader(stdoutPipe)

	bootstrap, bootstrapErr := adapter.BootstrapSession(
		s,
		stdinPipe,
		stdoutReader,
		cfg.SessionID,
		cfg.RuntimeSessionID,
		cwd,
		cfg.Model,
		cfg.PermissionMode,
		cfg.SandboxMode,
		cfg.Resume,
	)
	if bootstrapErr != nil {
		_ = stdinPipe.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("failed to bootstrap %s session: %w", adapter.Kind(), bootstrapErr)
	}

	if bootstrap.SessionID != "" {
		cfg.SessionID = bootstrap.SessionID
	}
	if strings.TrimSpace(cfg.SessionID) == "" {
		cfg.SessionID = uuid.NewString()
	}
	if bootstrap.RuntimeSessionID != "" {
		cfg.RuntimeSessionID = bootstrap.RuntimeSessionID
	}
	if bootstrap.Model != "" {
		cfg.Model = bootstrap.Model
	}
	if bootstrap.WorkingDir != "" {
		cwd = bootstrap.WorkingDir
		cfg.WorkingDir = bootstrap.WorkingDir
	}

	s.mu.Lock()
	s.conn = nil
	s.cmd = cmd
	s.stdin = stdinPipe
	s.stdout = stdoutPipe
	s.stdoutReader = stdoutReader
	s.adapter = adapter
	s.runtime = runtimeKind
	s.sessionID = cfg.SessionID
	s.runtimeSessionID = cfg.RuntimeSessionID
	s.done = make(chan struct{})
	s.running = true
	s.currentDir = cwd
	s.currentModel = cfg.Model
	s.currentPermissionMode = normalizePermissionModeForRuntime(runtimeKind, cfg.PermissionMode)
	s.currentSandboxMode = normalizeSandboxModeForRuntime(runtimeKind, cfg.SandboxMode)
	s.contentProtector = nil
	s.turnActive = false
	s.interruptRequested = false
	s.pendingPermissions = make(map[string]*pendingPermissionRequest)
	s.attachedPermissionShadow = make(map[string]protocol.PermissionRequestPayload)
	s.pendingInterrupts = make(map[string]struct{})
	s.allowedTools = make(map[string]struct{})
	s.allowedBashLiterals = make(map[string]struct{})
	s.allowedBashPrefixes = make(map[string]struct{})
	s.updatedToolInputs = make(map[string]string)
	s.pendingRPC = make(map[string]chan codexRPCResponse)
	s.rpcSeq = 0
	s.codexTurnID = ""
	s.codexAppServerURL = ""
	s.localMode = true
	s.sink = messageSink(sink)
	s.mu.Unlock()

	s.startProcessBridge(cmd, stdinPipe, stdoutPipe, cfg.SessionID)
	if err := s.sendCurrentKeepalive(cfg.SessionID); err != nil {
		_ = s.StopWithReason("local-keepalive-failed")
		return "", err
	}

	return cfg.SessionID, nil
}

func (s *Service) SendInput(content string) error {
	s.mu.Lock()
	running := s.running
	sessionID := s.sessionID
	s.mu.Unlock()
	if !running {
		return fmt.Errorf("session is not running")
	}

	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("input is required")
	}

	handled, err := s.handleLocalSlashCommand(sessionID, content, nil)
	if err != nil {
		return err
	}
	if handled {
		return nil
	}

	if err := s.writeUserMessage(content); err != nil {
		return err
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

func (s *Service) Interrupt() error {
	return s.requestInterrupt()
}

func (s *Service) RespondPermission(resp protocol.PermissionResponsePayload) error {
	if strings.TrimSpace(resp.RequestID) == "" {
		return fmt.Errorf("request id is required")
	}
	if strings.TrimSpace(resp.Decision) == "" {
		if resp.Approved {
			resp.Decision = protocol.PermissionDecisionApproved
		} else {
			resp.Decision = protocol.PermissionDecisionDenied
		}
	}
	s.resolvePermissionResponse(resp)
	return nil
}

func (s *Service) ApplySessionConfig(cfg protocol.SessionConfigPayload) error {
	s.mu.Lock()
	running := s.running
	sessionID := s.sessionID
	s.mu.Unlock()
	if !running {
		return fmt.Errorf("session is not running")
	}

	decision := s.evaluateSessionConfig(cfg)
	if decision.PermissionModeChanged && !decision.PermissionModeNeedsRestart {
		s.setCurrentPermissionMode(decision.TargetPermissionMode)
	}
	if decision.SandboxModeChanged && !decision.SandboxModeNeedsRestart {
		s.setCurrentSandboxMode(decision.TargetSandboxMode)
	}
	if decision.NeedsRestart {
		if err := s.restartCommand(
			sessionID,
			cfg.WorkingDir,
			decision.TargetModel,
			decision.ApplyModel,
			decision.TargetPermissionMode,
			decision.TargetSandboxMode,
			decision.ResumeConversation,
		); err != nil {
			return err
		}
	}
	return s.sendCurrentKeepalive(sessionID)
}

func (s *Service) LocalMode() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localMode
}
