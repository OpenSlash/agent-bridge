package remote

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/internal/buildmeta"
	"github.com/OpenSlash/agent-bridge/protocol"

	"github.com/gorilla/websocket"
)

func (s *Service) StartAttached(cfg *Config, handlers AttachedHandlers) (string, error) {
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
		return "", fmt.Errorf("attached mode does not support management sessions")
	}
	if handlers.SendInput == nil {
		return "", fmt.Errorf("attached input handler is required")
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

	var contentProtector *contentProtector
	{
		protector, err := newContentProtector()
		if err != nil {
			return "", fmt.Errorf("failed to initialize content protector: %w", err)
		}
		contentProtector = protector
	}

	s.cfg = Config{
		ServerURL:        cfg.ServerURL,
		Token:            cfg.Token,
		Command:          cfg.Command,
		ClaudeCommand:    cfg.ClaudeCommand,
		CodexCommand:     cfg.CodexCommand,
		Args:             append([]string(nil), cfg.Args...),
		WorkingDir:       cwd,
		Model:            cfg.Model,
		PermissionMode:   normalizePermissionModeForRuntime(runtimeKind, cfg.PermissionMode),
		SandboxMode:      normalizeSandboxModeForRuntime(runtimeKind, cfg.SandboxMode),
		HostID:           cfg.HostID,
		SessionID:        cfg.SessionID,
		RuntimeSessionID: cfg.RuntimeSessionID,
		Resume:           cfg.Resume,
		Management:       false,
		ClaudeEnabled:    cfg.ClaudeEnabled,
		CodexEnabled:     cfg.CodexEnabled,
		RuntimeCatalog:   append([]protocol.RuntimeCapability(nil), cfg.RuntimeCatalog...),
	}

	if runtimeKind != runtimeCodex && strings.TrimSpace(cfg.RuntimeSessionID) == "" {
		cfg.RuntimeSessionID = cfg.SessionID
		s.cfg.RuntimeSessionID = cfg.RuntimeSessionID
	}

	wsURL, err := buildWSURL(cfg.ServerURL, "/ws/terminal/register", cfg.Token)
	if err != nil {
		return "", fmt.Errorf("invalid server URL: %w", err)
	}

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = relayDialTimeout
	dialer.NetDialContext = (&net.Dialer{
		Timeout:   relayDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext

	applog.Info.Printf(
		"[Remote] attached relay dialing: cwd=%s session=%s host=%s",
		cwd,
		cfg.SessionID,
		cfg.HostID,
	)

	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to connect server: %w", err)
	}

	hostname, _ := os.Hostname()
	regMsg := protocol.Message{
		Type: protocol.TypeRegister,
		Payload: protocol.RegisterPayload{
			SessionID:        cfg.SessionID,
			RuntimeSessionID: cfg.RuntimeSessionID,
			Hostname:         hostname,
			Cwd:              cwd,
			Pid:              0,
			HostID:           cfg.HostID,
			Command:          registerCommandName(cfg),
			OS:               runtime.GOOS,
			Version:          buildmeta.GetVersionString(),
			Model:            cfg.Model,
			PermissionMode:   normalizePermissionModeForRuntime(runtimeKind, cfg.PermissionMode),
			SandboxMode:      normalizeSandboxModeForRuntime(runtimeKind, cfg.SandboxMode),
		},
	}
	regData, _ := json.Marshal(regMsg)
	if err := conn.SetWriteDeadline(time.Now().Add(relayWriteTimeout)); err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("failed to set register write deadline: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, regData); err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("failed to send register: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	if err := conn.SetReadDeadline(time.Now().Add(relayRegisterReadTimeout)); err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("failed to set register response deadline: %w", err)
	}
	_, respData, err := conn.ReadMessage()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("failed to read register response: %w", err)
	}

	var respMsg protocol.Message
	if err := json.Unmarshal(respData, &respMsg); err != nil || respMsg.Type != protocol.TypeRegistered {
		_ = conn.Close()
		return "", fmt.Errorf("invalid register response")
	}

	s.mu.Lock()
	s.conn = conn
	s.cmd = nil
	s.stdin = nil
	s.stdout = nil
	s.stdoutReader = bufio.NewReader(strings.NewReader(""))
	s.adapter = adapter
	s.runtime = runtimeKind
	s.sessionID = respMsg.SessionID
	s.runtimeSessionID = cfg.RuntimeSessionID
	s.cfg.SessionID = respMsg.SessionID
	s.cfg.RuntimeSessionID = cfg.RuntimeSessionID
	s.done = make(chan struct{})
	s.running = true
	s.currentDir = cwd
	s.currentModel = cfg.Model
	s.currentPermissionMode = normalizePermissionModeForRuntime(runtimeKind, cfg.PermissionMode)
	s.currentSandboxMode = normalizeSandboxModeForRuntime(runtimeKind, cfg.SandboxMode)
	s.contentProtector = contentProtector
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
	s.localMode = false
	s.sink = nil
	s.attachedInputHandler = handlers.SendInput
	s.attachedInterruptHandler = handlers.Interrupt
	s.attachedConfigHandler = handlers.ApplyConfig
	s.mu.Unlock()

	configureRelayConnection(conn)
	s.startBridge()
	if err := s.sendCurrentKeepalive(s.sessionID); err != nil {
		_ = s.StopWithReason("attached-keepalive-failed")
		return "", err
	}

	applog.Info.Printf("[Remote] attached session registered: session=%s command=%s", s.sessionID, cfg.Command)
	return s.sessionID, nil
}
