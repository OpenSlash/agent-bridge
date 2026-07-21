package remote

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/internal/buildmeta"
	"github.com/OpenSlash/agent-bridge/protocol"

	"github.com/gorilla/websocket"
)

const (
	relayDialTimeout         = 8 * time.Second
	relayRegisterReadTimeout = 8 * time.Second
	relayWriteTimeout        = 5 * time.Second
	relayPongWait            = 30 * time.Second
	relayPingInterval        = 10 * time.Second
	commandResolveTimeout    = 2 * time.Second
)

// Config remote 模式配置
type Config struct {
	ServerURL        string // WS 服务器地址，如 wss://example.com
	Token            string // JWT token
	AgentVersion     string // embedding agent/product version reported to the relay
	Command          string // 要启动的命令，默认 "claude"
	ClaudeCommand    string
	CodexCommand     string
	Args             []string
	WorkingDir       string
	Model            string
	ReasoningEffort  string
	PermissionMode   string
	SandboxMode      string
	HostID           string
	SessionID        string
	RuntimeSessionID string
	Resume           bool
	Management       bool
	ClaudeEnabled    bool
	CodexEnabled     bool
	RuntimeCatalog   []protocol.RuntimeCapability
	HostReadiness    protocol.HostReadiness
}

type sdkControlRequest struct {
	Type      string                   `json:"type"`
	RequestID string                   `json:"request_id"`
	Request   sdkControlRequestPayload `json:"request"`
}

type sdkControlRequestPayload struct {
	Subtype  string         `json:"subtype"`
	ToolName string         `json:"tool_name,omitempty"`
	Input    map[string]any `json:"input,omitempty"`
}

type sdkControlCancelRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
}

type sdkControlResponse struct {
	Type     string                    `json:"type"`
	Response sdkControlResponsePayload `json:"response"`
}

type sdkControlResponsePayload struct {
	Subtype   string               `json:"subtype"`
	RequestID string               `json:"request_id"`
	Response  *sdkPermissionResult `json:"response,omitempty"`
	Error     string               `json:"error,omitempty"`
}

type sdkPermissionResult struct {
	Behavior     string         `json:"behavior"`
	UpdatedInput map[string]any `json:"updatedInput,omitempty"`
	Message      string         `json:"message,omitempty"`
}

type pendingPermissionRequest struct {
	sessionID  string
	responseCh chan protocol.PermissionResponsePayload
	once       sync.Once
}

type reconnectAttempt struct {
	cancel chan struct{}
}

type messageSink func(protocol.Message) error

// Service 管理 remote 代理的生命周期
type Service struct {
	mu                                sync.Mutex
	wsMu                              sync.Mutex
	stdinMu                           sync.Mutex
	running                           bool
	starting                          bool
	runtime                           runtimeKind
	adapter                           runtimeAdapter
	sessionID                         string
	runtimeSessionID                  string
	conn                              *websocket.Conn
	cmd                               *exec.Cmd
	stdin                             io.WriteCloser
	stdout                            io.ReadCloser
	stdoutReader                      *bufio.Reader
	done                              chan struct{}
	wg                                sync.WaitGroup
	thinking                          bool
	cfg                               Config
	currentDir                        string
	currentModel                      string
	currentReasoningEffort            string
	currentPermissionMode             string
	currentSandboxMode                string
	contentProtector                  *contentProtector
	turnActive                        bool
	interruptRequested                bool
	pendingPermissions                map[string]*pendingPermissionRequest
	attachedPermissionShadow          map[string]protocol.PermissionRequestPayload
	pendingInterrupts                 map[string]struct{}
	allowedTools                      map[string]struct{}
	allowedBashLiterals               map[string]struct{}
	allowedBashPrefixes               map[string]struct{}
	updatedToolInputs                 map[string]string
	codexAppServerURL                 string
	children                          []*Service
	createSessionAttempts             map[string]*createSessionAttempt
	temporaryAttachmentDirs           []string
	autoReconnect                     bool
	reconnectAttempt                  *reconnectAttempt
	rpcSeq                            int64
	pendingRPC                        map[string]chan codexRPCResponse
	codexTurnID                       string
	pendingCodexThreadRebind          bool
	codexInputAliases                 map[string]string
	codexObservedThreadID             string
	codexObservedLiveOutput           bool
	codexSnapshotSyncing              bool
	codexSyncedHistory                map[string]map[string]struct{}
	localMode                         bool
	sink                              messageSink
	messageObservers                  map[int]messageSink
	nextMessageObserverID             int
	childStartedHook                  func(*Service, Config)
	attachedInputHandler              func(string) error
	attachedInterruptHandler          func() error
	attachedConfigHandler             func(protocol.SessionConfigPayload) error
	attachedPermissionResponseHandler func(protocol.PermissionResponsePayload) bool
}

// NewService 创建 Service 实例
func NewService() *Service {
	return &Service{}
}

type AttachedHandlers struct {
	SendInput   func(string) error
	Interrupt   func() error
	ApplyConfig func(protocol.SessionConfigPayload) error
}

// StartManagement 启动远程管理会话
func (s *Service) StartManagement(cfg *Config) (string, error) {
	clone := *cfg
	clone.Management = true
	return s.Start(&clone)
}

// StartProxy 启动代理会话；若当前服务已作为管理会话在线，则拉起子代理
func (s *Service) StartProxy(cfg *Config) (string, error) {
	s.mu.Lock()
	running := s.running
	management := s.cfg.Management
	s.mu.Unlock()

	if running && management {
		return s.startChildProxy(cfg)
	}

	clone := *cfg
	clone.Management = false
	return s.Start(&clone)
}

// Start 非阻塞启动 claude (pipes + stream-json) + WS 桥接
func (s *Service) Start(cfg *Config) (string, error) {
	s.mu.Lock()
	if s.running {
		if s.cfg.Management && !cfg.Management {
			s.mu.Unlock()
			return s.startChildProxy(cfg)
		}
		sessionID := s.sessionID
		s.mu.Unlock()
		return sessionID, nil
	}
	if s.starting {
		s.mu.Unlock()
		return "", fmt.Errorf("session is already starting")
	}
	s.starting = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
	}()

	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	if !cfg.Management && !cfg.ClaudeEnabled && !cfg.CodexEnabled {
		cfg.ClaudeEnabled = true
		cfg.CodexEnabled = true
	}
	if cfg.Management && len(cfg.RuntimeCatalog) == 0 && (cfg.ClaudeEnabled || cfg.CodexEnabled) {
		cfg.RuntimeCatalog = BuildRuntimeCatalog(RuntimeCatalogOptions{
			ClaudeEnabled: cfg.ClaudeEnabled,
			CodexEnabled:  cfg.CodexEnabled,
		})
	}
	adapter := resolveRuntimeAdapter(cfg.Command)
	if !cfg.Management {
		resolvedCommand, err := resolveCommandPath(cfg.Command)
		if err != nil {
			return "", fmt.Errorf("failed to resolve command %q: %w", cfg.Command, err)
		}
		cfg.Command = resolvedCommand
		adapter = resolveRuntimeAdapter(cfg.Command)
	}
	runtimeKind := adapter.Kind()

	var contentProtector *contentProtector
	{
		var err error
		contentProtector, err = newContentProtector()
		if err != nil {
			return "", fmt.Errorf("failed to initialize content protector: %w", err)
		}
	}

	hostname, _ := os.Hostname()
	cwd := cfg.WorkingDir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd = filepath.Clean(cwd)
	if info, err := os.Stat(cwd); err != nil {
		return "", fmt.Errorf("invalid working dir %s: %w", cwd, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("working dir is not a directory: %s", cwd)
	}
	if !cfg.Management {
		if err := adapter.PrepareStart(s, cfg, cwd); err != nil {
			return "", err
		}
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
		HostID:           cfg.HostID,
		SessionID:        cfg.SessionID,
		RuntimeSessionID: cfg.RuntimeSessionID,
		Resume:           cfg.Resume,
		Management:       cfg.Management,
		ClaudeEnabled:    cfg.ClaudeEnabled,
		CodexEnabled:     cfg.CodexEnabled,
		RuntimeCatalog:   append([]protocol.RuntimeCapability(nil), cfg.RuntimeCatalog...),
		HostReadiness:    cloneHostReadiness(cfg.HostReadiness),
	}

	var (
		cmd                 *exec.Cmd
		stdinPipe           io.WriteCloser
		stdoutPipe          io.ReadCloser
		stdoutReader        *bufio.Reader
		initialHistoryBatch []protocol.SessionHistoryMessage
		err                 error
	)

	if !cfg.Management {
		cmd, stdinPipe, stdoutPipe, err = s.startCommand(cfg.SessionID, cwd, cfg.Model, cfg.PermissionMode, cfg.Resume)
		if err != nil {
			return "", fmt.Errorf("failed to start command: %w", err)
		}
		stdoutReader = bufio.NewReader(stdoutPipe)
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
		initialHistoryBatch = bootstrap.HistoryBatch
		s.cfg.SessionID = cfg.SessionID
		s.cfg.RuntimeSessionID = cfg.RuntimeSessionID
		s.cfg.Model = cfg.Model
		s.cfg.WorkingDir = cwd
	}

	registerPath := "/ws/terminal/register"
	registerType := protocol.TypeRegister
	expectedRespType := protocol.TypeRegistered
	if cfg.Management {
		registerPath = "/ws/terminal/manager/register"
		registerType = protocol.TypeManagerRegister
		expectedRespType = protocol.TypeManagerRegistered
	}

	wsURL, err := buildWSURL(cfg.ServerURL, registerPath, cfg.Token)
	if err != nil {
		if stdinPipe != nil {
			_ = stdinPipe.Close()
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("invalid server URL: %w", err)
	}

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = relayDialTimeout
	dialer.NetDialContext = (&net.Dialer{
		Timeout:   relayDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext

	applog.Info.Printf(
		"[Remote] relay register dialing: management=%t cwd=%s session=%s host=%s",
		cfg.Management,
		cwd,
		cfg.SessionID,
		cfg.HostID,
	)

	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		if stdinPipe != nil {
			_ = stdinPipe.Close()
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("failed to connect server: %w", err)
	}
	applog.Info.Printf(
		"[Remote] relay register connected: management=%t cwd=%s session=%s host=%s",
		cfg.Management,
		cwd,
		cfg.SessionID,
		cfg.HostID,
	)

	regMsg := protocol.Message{
		Type: registerType,
		Payload: protocol.RegisterPayload{
			SessionID:        cfg.SessionID,
			RuntimeSessionID: cfg.RuntimeSessionID,
			Hostname:         hostname,
			Cwd:              cwd,
			Pid:              processPID(cmd),
			HostID:           cfg.HostID,
			Command:          registerCommandName(cfg),
			OS:               runtime.GOOS,
			Version:          reportedAgentVersion(cfg),
			Model:            cfg.Model,
			ReasoningEffort:  strings.TrimSpace(cfg.ReasoningEffort),
			PermissionMode:   normalizePermissionModeForRuntime(runtimeKind, cfg.PermissionMode),
			SandboxMode:      normalizeSandboxModeForRuntime(runtimeKind, cfg.SandboxMode),
			RuntimeCatalog: func() []protocol.RuntimeCapability {
				if !cfg.Management {
					return nil
				}
				return append([]protocol.RuntimeCapability(nil), cfg.RuntimeCatalog...)
			}(),
			HostReadiness: cloneHostReadiness(cfg.HostReadiness),
		},
	}
	regData, _ := json.Marshal(regMsg)
	if err := conn.SetWriteDeadline(time.Now().Add(relayWriteTimeout)); err != nil {
		_ = conn.Close()
		if stdinPipe != nil {
			_ = stdinPipe.Close()
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("failed to set register write deadline: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, regData); err != nil {
		_ = conn.Close()
		if stdinPipe != nil {
			_ = stdinPipe.Close()
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("failed to send register: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	if err := conn.SetReadDeadline(time.Now().Add(relayRegisterReadTimeout)); err != nil {
		_ = conn.Close()
		if stdinPipe != nil {
			_ = stdinPipe.Close()
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("failed to set register response deadline: %w", err)
	}
	_, respData, err := conn.ReadMessage()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		if stdinPipe != nil {
			_ = stdinPipe.Close()
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("failed to read register response: %w", err)
	}

	var respMsg protocol.Message
	if err := json.Unmarshal(respData, &respMsg); err != nil || respMsg.Type != expectedRespType {
		_ = conn.Close()
		if stdinPipe != nil {
			_ = stdinPipe.Close()
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("invalid register response")
	}

	s.mu.Lock()
	s.conn = conn
	s.cmd = cmd
	s.stdin = stdinPipe
	s.stdout = stdoutPipe
	s.stdoutReader = stdoutReader
	s.adapter = adapter
	s.runtime = runtimeKind
	if cfg.Management {
		payloadBytes, _ := json.Marshal(respMsg.Payload)
		var payload protocol.ManagerRegisteredPayload
		if err := json.Unmarshal(payloadBytes, &payload); err != nil || payload.HostID == "" {
			s.mu.Unlock()
			_ = conn.Close()
			if stdinPipe != nil {
				_ = stdinPipe.Close()
			}
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			return "", fmt.Errorf("invalid manager register response")
		}
		s.sessionID = payload.HostID
		s.cfg.HostID = payload.HostID
	} else {
		s.sessionID = respMsg.SessionID
		s.runtimeSessionID = cfg.RuntimeSessionID
		cfg.SessionID = respMsg.SessionID
		s.cfg.SessionID = respMsg.SessionID
		s.cfg.RuntimeSessionID = cfg.RuntimeSessionID
	}
	s.done = make(chan struct{})
	s.running = true
	s.currentDir = cwd
	s.currentModel = cfg.Model
	s.currentReasoningEffort = strings.TrimSpace(cfg.ReasoningEffort)
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
	s.codexObservedThreadID = ""
	s.codexObservedLiveOutput = false
	s.codexSnapshotSyncing = false
	s.codexSyncedHistory = make(map[string]map[string]struct{})
	s.mu.Unlock()

	configureRelayConnection(conn)
	if len(initialHistoryBatch) > 0 {
		historyPusher := newSessionHistoryPusher(cfg.ServerURL, cfg.Token, s.contentProtector)
		if err := historyPusher.pushBatch(cfg.SessionID, initialHistoryBatch); err != nil {
			applog.Errorf("[Remote] initial codex history sync error: session=%s err=%v", cfg.SessionID, err)
		} else {
			s.markCodexHistorySynced(cfg.SessionID, initialHistoryBatch)
		}
	}

	s.startBridge()
	if !cfg.Management {
		adapter.StartHistorySync(s, s.sessionID)
		if runtimeKind == runtimeCodex && strings.TrimSpace(cfg.RuntimeSessionID) != "" {
			s.scheduleCodexThreadSnapshotSync(s.sessionID, cfg.RuntimeSessionID)
		}
	}

	if cfg.Management {
		applog.Info.Printf("[Remote] Manager registered: host=%s", s.sessionID)
	} else {
		applog.Info.Printf("[Remote] Session registered: %s, proxying %s (PID %d) [stream-json mode]", s.sessionID, cfg.Command, cmd.Process.Pid)
	}
	return s.sessionID, nil
}

func reportedAgentVersion(cfg *Config) string {
	if cfg != nil {
		if version := strings.TrimSpace(cfg.AgentVersion); version != "" {
			return version
		}
	}
	return buildmeta.GetVersionString()
}

// Stop 优雅关闭
func (s *Service) Stop() error {
	return s.StopWithReason("unknown")
}

// StopWithReason 优雅关闭，并记录触发原因
func (s *Service) StopWithReason(reason string) error {
	s.mu.Lock()
	sessionID := s.sessionID
	management := s.cfg.Management
	if !s.running {
		children := s.children
		s.children = nil
		conn := s.conn
		stdin := s.stdin
		cmd := s.cmd
		done := s.done
		s.conn = nil
		s.stdin = nil
		s.stdout = nil
		s.stdoutReader = nil
		s.cmd = nil
		s.done = nil
		s.adapter = nil
		s.runtime = runtimeClaude
		s.sessionID = ""
		s.runtimeSessionID = ""
		s.currentModel = ""
		s.currentReasoningEffort = ""
		s.currentDir = ""
		s.currentPermissionMode = protocol.PermissionModeDefault
		s.currentSandboxMode = ""
		s.turnActive = false
		s.interruptRequested = false
		s.attachedPermissionShadow = nil
		s.pendingInterrupts = nil
		s.pendingRPC = nil
		s.codexTurnID = ""
		s.codexAppServerURL = ""
		s.pendingCodexThreadRebind = false
		s.codexObservedThreadID = ""
		s.codexObservedLiveOutput = false
		s.codexSnapshotSyncing = false
		s.codexSyncedHistory = nil
		s.localMode = false
		s.sink = nil
		s.attachedInputHandler = nil
		s.attachedInterruptHandler = nil
		s.attachedConfigHandler = nil
		s.attachedPermissionResponseHandler = nil
		s.mu.Unlock()
		if management {
			applog.Info.Printf("[Remote] Stop requested for inactive management session: reason=%s session=%s", reason, sessionID)
		}
		if done != nil {
			close(done)
		}
		s.resetPermissionState()
		if stdin != nil {
			_ = stdin.Close()
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		if conn != nil {
			_ = conn.Close()
		}
		for _, child := range children {
			_ = child.Stop()
		}
		s.cleanupTemporaryCreateSessionAttachments()
		return nil
	}
	s.running = false
	children := s.children
	s.children = nil
	conn := s.conn
	stdin := s.stdin
	cmd := s.cmd
	done := s.done
	s.mu.Unlock()

	if management {
		if reason == "unknown" {
			applog.Info.Printf("[Remote] Stopping management session: reason=%s session=%s\n%s", reason, sessionID, string(debug.Stack()))
		} else {
			applog.Info.Printf("[Remote] Stopping management session: reason=%s session=%s", reason, sessionID)
		}
	}

	if sessionID != "" && !management {
		_ = s.writeJSON(protocol.Message{
			Type:      protocol.TypeSessionLifecycle,
			SessionID: sessionID,
			Payload: protocol.SessionLifecyclePayload{
				State:      "stopped",
				Reason:     strings.TrimSpace(reason),
				OccurredAt: time.Now().UnixMilli(),
			},
		})
	}

	if done != nil {
		close(done)
	}
	s.resetPermissionState()

	if conn != nil {
		_ = s.writeMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
	}

	if stdin != nil {
		_ = stdin.Close()
	}

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	s.wg.Wait()

	if conn != nil {
		_ = conn.Close()
	}

	applog.Info.Printf("[Remote] Session %s stopped", sessionID)
	s.mu.Lock()
	s.sessionID = ""
	s.runtimeSessionID = ""
	s.currentModel = ""
	s.currentReasoningEffort = ""
	s.currentDir = ""
	s.currentPermissionMode = protocol.PermissionModeDefault
	s.currentSandboxMode = ""
	s.conn = nil
	s.stdin = nil
	s.stdout = nil
	s.stdoutReader = nil
	s.cmd = nil
	s.done = nil
	s.adapter = nil
	s.runtime = runtimeClaude
	s.thinking = false
	s.attachedPermissionShadow = nil
	s.pendingRPC = nil
	s.codexTurnID = ""
	s.codexAppServerURL = ""
	s.pendingCodexThreadRebind = false
	s.codexObservedThreadID = ""
	s.codexObservedLiveOutput = false
	s.codexSnapshotSyncing = false
	s.codexSyncedHistory = nil
	s.localMode = false
	s.sink = nil
	s.attachedInputHandler = nil
	s.attachedInterruptHandler = nil
	s.attachedConfigHandler = nil
	s.attachedPermissionResponseHandler = nil
	s.mu.Unlock()
	for _, child := range children {
		_ = child.Stop()
	}
	s.cleanupTemporaryCreateSessionAttachments()
	return nil
}

// IsRunning 返回运行状态
func (s *Service) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// SessionID 返回当前会话 ID
func (s *Service) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *Service) HasActiveTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnActive
}

func (s *Service) SendCurrentKeepalive() error {
	return s.sendCurrentKeepalive(s.SessionID())
}

func (s *Service) UpdateRuntimeSelection(claudeEnabled, codexEnabled bool, claudeCommand, codexCommand string, runtimeCatalog []protocol.RuntimeCapability) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.ClaudeEnabled = claudeEnabled
	s.cfg.CodexEnabled = codexEnabled
	s.cfg.ClaudeCommand = strings.TrimSpace(claudeCommand)
	s.cfg.CodexCommand = strings.TrimSpace(codexCommand)
	s.cfg.RuntimeCatalog = append([]protocol.RuntimeCapability(nil), runtimeCatalog...)
}

func (s *Service) UpdateHostReadiness(readiness protocol.HostReadiness) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.HostReadiness = cloneHostReadiness(readiness)
}

func (s *Service) SetChildStartedHook(hook func(*Service, Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.childStartedHook = hook
}

func (s *Service) SetAttachedPermissionResponseHandler(handler func(protocol.PermissionResponsePayload) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachedPermissionResponseHandler = handler
}

func (s *Service) CurrentModel() string {
	return s.getCurrentModel()
}

func (s *Service) RuntimeSessionID() string {
	return s.getRuntimeSessionID()
}

func (s *Service) RuntimeID() string {
	return string(s.getRuntime())
}

func (s *Service) CommandPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.cfg.Command)
}

func (s *Service) CodexAppServerURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.codexAppServerURL)
}

// CurrentDir 返回当前工作目录
func (s *Service) CurrentDir() string {
	return s.getCurrentDir()
}

func (s *Service) CurrentPermissionMode() string {
	return s.getCurrentPermissionMode()
}

func (s *Service) CurrentSandboxMode() string {
	return s.getCurrentSandboxMode()
}

func (s *Service) HasAttachedPermissionRequest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.attachedPermissionShadow) > 0
}

func (s *Service) ShouldAutoApproveTool(toolName string, input map[string]any) bool {
	return s.shouldAutoApproveTool(toolName, input)
}

type AttachedSessionStateUpdate struct {
	RuntimeSessionID string
	WorkingDir       string
	Model            string
	ApplyModel       bool
	PermissionMode   string
	ApplyPermission  bool
	SandboxMode      string
	ApplySandbox     bool
}

func (s *Service) UpdateAttachedSessionState(update AttachedSessionStateUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(update.RuntimeSessionID) != "" {
		s.runtimeSessionID = strings.TrimSpace(update.RuntimeSessionID)
		s.cfg.RuntimeSessionID = s.runtimeSessionID
	}
	if strings.TrimSpace(update.WorkingDir) != "" {
		s.currentDir = strings.TrimSpace(update.WorkingDir)
		s.cfg.WorkingDir = s.currentDir
	}
	if update.ApplyModel || strings.TrimSpace(update.Model) != "" {
		s.currentModel = strings.TrimSpace(update.Model)
		s.cfg.Model = s.currentModel
	}
	if update.ApplyPermission || strings.TrimSpace(update.PermissionMode) != "" {
		s.currentPermissionMode = normalizePermissionModeForRuntime(s.runtime, update.PermissionMode)
		s.cfg.PermissionMode = s.currentPermissionMode
	}
	if update.ApplySandbox || strings.TrimSpace(update.SandboxMode) != "" {
		s.currentSandboxMode = normalizeSandboxModeForRuntime(s.runtime, update.SandboxMode)
		s.cfg.SandboxMode = s.currentSandboxMode
	}
}

func (s *Service) ResetAttachedSessionState() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.thinking = false
	s.turnActive = false
	s.interruptRequested = false
	s.pendingPermissions = make(map[string]*pendingPermissionRequest)
	s.attachedPermissionShadow = make(map[string]protocol.PermissionRequestPayload)
	s.pendingInterrupts = make(map[string]struct{})
}

// LocalSessionIDs 返回当前进程启动的所有代理会话 ID
func (s *Service) LocalSessionIDs() []string {
	s.mu.Lock()
	sessionID := s.sessionID
	children := append([]*Service(nil), s.children...)
	management := s.cfg.Management
	s.mu.Unlock()

	ids := make([]string, 0, 1+len(children))
	if sessionID != "" && !management {
		ids = append(ids, sessionID)
	}
	for _, child := range children {
		ids = append(ids, child.LocalSessionIDs()...)
	}
	return ids
}

// IsManagementRunning 返回远程管理会话是否在线
func (s *Service) IsManagementRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running && s.cfg.Management
}

func (s *Service) getAttachedInputHandler() func(string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attachedInputHandler
}

func (s *Service) getAttachedInterruptHandler() func() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attachedInterruptHandler
}

func (s *Service) getAttachedConfigHandler() func(protocol.SessionConfigPayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attachedConfigHandler
}
