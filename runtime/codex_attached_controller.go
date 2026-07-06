package remote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"

	"github.com/gorilla/websocket"
)

type AttachedCodexControllerConfig struct {
	SessionID        string
	RuntimeSessionID string
	WorkingDir       string
	Model            string
	PermissionMode   string
	SandboxMode      string
	Resume           bool
}

type AttachedCodexBootstrap struct {
	ThreadID   string
	WorkingDir string
	Model      string
}

type attachedCodexPendingRequest struct {
	RequestID      string
	RequestIDValue any
	ToolName       string
	Input          map[string]any
	BuildResult    func(protocol.PermissionResponsePayload) any
	ParseResult    func(json.RawMessage) protocol.PermissionResponsePayload
	ForwardToProxy bool
}

type AttachedCodexController struct {
	service *Service
	wsURL   string

	mu                 sync.Mutex
	writeMu            sync.Mutex
	cfg                AttachedCodexControllerConfig
	conn               *websocket.Conn
	connClosed         bool
	initializeResult   map[string]any
	proxyForward       func([]byte) error
	threadID           string
	turnID             string
	currentDir         string
	currentModel       string
	permissionMode     string
	sandboxMode        string
	pendingRPC         map[string]chan codexRPCResponse
	rpcSeq             int64
	pendingReqs        map[string]attachedCodexPendingRequest
	liveAdapter        *codexLiveAdapter
	observedThreadID   string
	observedLiveOutput bool
	snapshotSyncing    bool
	syncedHistory      map[string]struct{}
	forwardedInputs    []attachedCodexForwardedInput
	done               chan struct{}
	doneOnce           sync.Once
	readErr            error
}

type attachedCodexForwardedInput struct {
	Content    string
	RecordedAt time.Time
}

type attachedCodexObservedTurnResult struct {
	ThreadID          string
	EmitSnapshotLines bool
}

func NewAttachedCodexController(service *Service, wsURL string, cfg AttachedCodexControllerConfig) *AttachedCodexController {
	return &AttachedCodexController{
		service: service,
		wsURL:   strings.TrimSpace(wsURL),
		cfg: AttachedCodexControllerConfig{
			SessionID:        strings.TrimSpace(cfg.SessionID),
			RuntimeSessionID: strings.TrimSpace(cfg.RuntimeSessionID),
			WorkingDir:       strings.TrimSpace(cfg.WorkingDir),
			Model:            strings.TrimSpace(cfg.Model),
			PermissionMode:   normalizePermissionModeForRuntime(runtimeCodex, cfg.PermissionMode),
			SandboxMode:      normalizeSandboxModeForRuntime(runtimeCodex, cfg.SandboxMode),
			Resume:           cfg.Resume,
		},
		pendingRPC:    make(map[string]chan codexRPCResponse),
		pendingReqs:   make(map[string]attachedCodexPendingRequest),
		liveAdapter:   newCodexLiveAdapter(),
		syncedHistory: make(map[string]struct{}),
		done:          make(chan struct{}),
	}
}

func (c *AttachedCodexController) Start() (AttachedCodexBootstrap, error) {
	if err := c.connectAndInitialize(); err != nil {
		return AttachedCodexBootstrap{}, err
	}
	bootstrap, err := c.bootstrap()
	if err != nil {
		_ = c.Close()
		return AttachedCodexBootstrap{}, err
	}
	return bootstrap, nil
}

func (c *AttachedCodexController) StartPassive() error {
	return c.connectAndInitialize()
}

func (c *AttachedCodexController) SyncInitialSnapshot() {
	service := c.service
	if service == nil || !service.IsRunning() {
		return
	}
	threadID := c.RuntimeSessionID()
	if threadID == "" {
		c.mu.Lock()
		threadID = firstNonEmpty(strings.TrimSpace(c.cfg.RuntimeSessionID), strings.TrimSpace(c.threadID))
		c.mu.Unlock()
	}
	c.scheduleInitialSnapshotSync(service.SessionID(), threadID)
}

func (c *AttachedCodexController) connectAndInitialize() error {
	conn, err := c.dial()
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	if c.currentDir == "" {
		c.currentDir = strings.TrimSpace(c.cfg.WorkingDir)
	}
	if c.currentModel == "" {
		c.currentModel = strings.TrimSpace(c.cfg.Model)
	}
	if c.permissionMode == "" {
		c.permissionMode = strings.TrimSpace(c.cfg.PermissionMode)
	}
	if c.sandboxMode == "" {
		c.sandboxMode = strings.TrimSpace(c.cfg.SandboxMode)
	}
	if c.syncedHistory == nil {
		c.syncedHistory = make(map[string]struct{})
	}
	c.mu.Unlock()

	go c.readLoop()

	if err := c.call("initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "veilo-cli-native",
			"version": "0.0.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}, &c.initializeResult); err != nil {
		_ = c.Close()
		return err
	}
	return nil
}

func (c *AttachedCodexController) dial() (*websocket.Conn, error) {
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 2 * time.Second
	dialer.NetDialContext = (&net.Dialer{
		Timeout:   2 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext

	var lastErr error
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, _, err := dialer.Dial(c.wsURL, nil)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(150 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("codex app-server unavailable")
	}
	return nil, lastErr
}

func (c *AttachedCodexController) bootstrap() (AttachedCodexBootstrap, error) {
	c.mu.Lock()
	workingDir := firstNonEmpty(strings.TrimSpace(c.cfg.WorkingDir), strings.TrimSpace(c.currentDir))
	model := firstNonEmpty(strings.TrimSpace(c.cfg.Model), strings.TrimSpace(c.currentModel))
	permissionMode := firstNonEmpty(strings.TrimSpace(c.cfg.PermissionMode), strings.TrimSpace(c.permissionMode))
	sandboxMode := firstNonEmpty(strings.TrimSpace(c.cfg.SandboxMode), strings.TrimSpace(c.sandboxMode))
	runtimeSessionID := firstNonEmpty(strings.TrimSpace(c.cfg.RuntimeSessionID), strings.TrimSpace(c.threadID))
	sessionID := strings.TrimSpace(c.cfg.SessionID)
	resume := c.cfg.Resume
	c.mu.Unlock()

	params := map[string]any{
		"cwd":            workingDir,
		"approvalPolicy": codexApprovalPolicy(permissionMode),
		"sandbox":        normalizeSandboxModeForRuntime(runtimeCodex, sandboxMode),
		"personality":    "pragmatic",
	}
	if model != "" {
		params["model"] = model
	}

	method := "thread/start"
	if resume {
		if runtimeSessionID == "" {
			runtimeSessionID = sessionID
		}
		if runtimeSessionID != "" {
			method = "thread/resume"
			params["threadId"] = runtimeSessionID
		}
	}

	var startResult codexThreadStartResult
	if err := c.call(method, params, &startResult); err != nil {
		if method == "thread/resume" && isCodexResumeRolloutMissing(err) {
			delete(params, "threadId")
			if err := c.call("thread/start", params, &startResult); err != nil {
				return AttachedCodexBootstrap{}, err
			}
		} else {
			return AttachedCodexBootstrap{}, err
		}
	}

	threadID := strings.TrimSpace(startResult.Thread.ID)
	if threadID == "" {
		return AttachedCodexBootstrap{}, fmt.Errorf("codex returned empty thread id")
	}
	cwd := strings.TrimSpace(startResult.Cwd)
	if cwd == "" {
		cwd = strings.TrimSpace(startResult.Thread.Cwd)
	}

	c.mu.Lock()
	c.threadID = threadID
	c.currentDir = cwd
	c.currentModel = firstNonEmpty(strings.TrimSpace(startResult.Model), model)
	c.permissionMode = permissionMode
	c.sandboxMode = normalizeSandboxModeForRuntime(runtimeCodex, sandboxMode)
	c.mu.Unlock()

	return AttachedCodexBootstrap{
		ThreadID:   threadID,
		WorkingDir: cwd,
		Model:      firstNonEmpty(strings.TrimSpace(startResult.Model), model),
	}, nil
}

func (c *AttachedCodexController) SendInput(content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	threadID := c.RuntimeSessionID()
	if threadID == "" {
		bootstrap, err := c.bootstrap()
		if err != nil {
			return err
		}
		threadID = bootstrap.ThreadID
	}

	params := map[string]any{
		"threadId": threadID,
		"input": []map[string]any{{
			"type": "text",
			"text": content,
		}},
		"approvalPolicy": codexApprovalPolicy(c.CurrentPermissionMode()),
		"sandboxPolicy":  codexSandboxPolicy(c.CurrentDir(), c.CurrentSandboxMode()),
		"cwd":            c.CurrentDir(),
	}
	var result codexTurnStartResult
	if err := c.call("turn/start", params, &result); err != nil {
		if !isCodexThreadUnavailable(err) {
			return err
		}
		bootstrap, fallbackErr := c.bootstrap()
		if fallbackErr != nil {
			return fmt.Errorf("%w; fallback failed: %v", err, fallbackErr)
		}
		threadID = bootstrap.ThreadID
		params["threadId"] = threadID
		if retryErr := c.call("turn/start", params, &result); retryErr != nil {
			return retryErr
		}
	}
	c.mu.Lock()
	c.turnID = strings.TrimSpace(result.Turn.ID)
	c.mu.Unlock()
	if c.service != nil {
		c.service.syncCodexForwardedInputHistory(threadID, strings.TrimSpace(result.Turn.ID), content)
	}
	return nil
}

func (c *AttachedCodexController) Interrupt() error {
	c.mu.Lock()
	threadID := strings.TrimSpace(c.threadID)
	turnID := strings.TrimSpace(c.turnID)
	c.mu.Unlock()
	if threadID == "" || turnID == "" {
		return fmt.Errorf("no active codex turn to interrupt")
	}
	return c.call("turn/interrupt", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	}, nil)
}

func (c *AttachedCodexController) ApplyConfig(cfg protocol.SessionConfigPayload) error {
	currentDir := c.CurrentDir()
	targetDir := currentDir
	if strings.TrimSpace(cfg.WorkingDir) != "" {
		resolvedDir, err := ResolveDirectoryWithinUserHome(cfg.WorkingDir, currentDir, false)
		if err != nil {
			return err
		}
		targetDir = resolvedDir
	}

	targetModel := c.CurrentModel()
	if cfg.ApplyModel && strings.TrimSpace(cfg.Model) != "" {
		targetModel = strings.TrimSpace(cfg.Model)
	}

	targetPermissionMode := c.CurrentPermissionMode()
	if strings.TrimSpace(cfg.PermissionMode) != "" {
		targetPermissionMode = normalizePermissionModeForRuntime(runtimeCodex, cfg.PermissionMode)
	}

	targetSandboxMode := c.CurrentSandboxMode()
	if strings.TrimSpace(cfg.SandboxMode) != "" {
		targetSandboxMode = normalizeSandboxModeForRuntime(runtimeCodex, cfg.SandboxMode)
	}

	threadID := c.RuntimeSessionID()
	if threadID == "" {
		bootstrap, err := c.bootstrap()
		if err != nil {
			return err
		}
		threadID = bootstrap.ThreadID
	}

	params := map[string]any{
		"threadId":               threadID,
		"cwd":                    targetDir,
		"approvalPolicy":         codexApprovalPolicy(targetPermissionMode),
		"sandbox":                normalizeSandboxModeForRuntime(runtimeCodex, targetSandboxMode),
		"persistExtendedHistory": true,
	}
	if strings.TrimSpace(targetModel) != "" {
		params["model"] = targetModel
	}

	var result codexThreadStartResult
	if err := c.call("thread/fork", params, &result); err != nil {
		if isCodexThreadUnavailable(err) || isCodexThreadNotMaterialized(err) {
			c.mu.Lock()
			c.currentDir = targetDir
			c.currentModel = targetModel
			c.permissionMode = targetPermissionMode
			c.sandboxMode = targetSandboxMode
			c.mu.Unlock()
			if c.service != nil && c.service.IsRunning() {
				c.service.UpdateAttachedSessionState(AttachedSessionStateUpdate{
					WorkingDir:      targetDir,
					Model:           targetModel,
					ApplyModel:      true,
					PermissionMode:  targetPermissionMode,
					ApplyPermission: true,
					SandboxMode:     targetSandboxMode,
					ApplySandbox:    true,
				})
				if err := c.service.sendCurrentKeepalive(c.service.SessionID()); err != nil {
					applog.Errorf("[Remote] codex attached keepalive failed after deferred config: %v", err)
				}
			}
			return nil
		}
		return err
	}

	nextThreadID := strings.TrimSpace(result.Thread.ID)
	if nextThreadID == "" {
		return fmt.Errorf("codex thread/fork returned empty thread id")
	}
	nextDir := strings.TrimSpace(result.Cwd)
	if nextDir == "" {
		nextDir = strings.TrimSpace(result.Thread.Cwd)
	}
	nextModel := firstNonEmpty(strings.TrimSpace(result.Model), targetModel)

	c.mu.Lock()
	c.threadID = nextThreadID
	c.currentDir = nextDir
	c.currentModel = nextModel
	c.permissionMode = targetPermissionMode
	c.sandboxMode = targetSandboxMode
	c.turnID = ""
	c.mu.Unlock()

	if c.service != nil && c.service.IsRunning() {
		c.service.UpdateAttachedSessionState(AttachedSessionStateUpdate{
			RuntimeSessionID: nextThreadID,
			WorkingDir:       nextDir,
			Model:            nextModel,
			ApplyModel:       true,
			PermissionMode:   targetPermissionMode,
			ApplyPermission:  true,
			SandboxMode:      targetSandboxMode,
			ApplySandbox:     true,
		})
		if err := c.service.sendCurrentKeepalive(c.service.SessionID()); err != nil {
			applog.Errorf("[Remote] codex attached keepalive failed after config: %v", err)
		}
	}
	return nil
}

func (c *AttachedCodexController) ResolvePermissionResponse(resp protocol.PermissionResponsePayload) bool {
	requestID := strings.TrimSpace(resp.RequestID)
	if requestID == "" {
		return false
	}

	c.mu.Lock()
	pending, ok := c.pendingReqs[requestID]
	if ok {
		delete(c.pendingReqs, requestID)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}

	if c.service != nil {
		if resp.Decision == protocol.PermissionDecisionApprovedForSession {
			c.service.recordSessionPermission(pending.ToolName, pending.Input)
		}
		if resp.Approved || resp.Decision == protocol.PermissionDecisionApproved || resp.Decision == protocol.PermissionDecisionApprovedForSession {
			c.service.rememberUpdatedToolInput(pending.ToolName, pending.Input, resp.UpdatedInput)
		}
	}

	payload := pending.BuildResult(resp)
	if err := c.writeEnvelope(map[string]any{
		"id":     pending.RequestIDValue,
		"result": payload,
	}); err != nil {
		applog.Errorf("[Remote] codex attached permission response write failed: request=%s err=%v", requestID, err)
		return false
	}
	if pending.ForwardToProxy {
		c.forwardProxyPermissionResponse(pending.RequestIDValue, payload)
	}
	return true
}

func (c *AttachedCodexController) RuntimeSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.threadID)
}

func (c *AttachedCodexController) CurrentDir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.currentDir)
}

func (c *AttachedCodexController) CurrentModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.currentModel)
}

func (c *AttachedCodexController) CurrentPermissionMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return firstNonEmpty(strings.TrimSpace(c.permissionMode), protocol.PermissionModeDefault)
}

func (c *AttachedCodexController) CurrentSandboxMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return firstNonEmpty(strings.TrimSpace(c.sandboxMode), defaultSandboxModeForRuntime(runtimeCodex))
}

func (c *AttachedCodexController) RecordForwardedInput(content string) {
	content = normalizeAttachedCodexForwardedInput(content)
	if content == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneForwardedInputsLocked(time.Now())
	c.forwardedInputs = append(c.forwardedInputs, attachedCodexForwardedInput{
		Content:    content,
		RecordedAt: time.Now(),
	})
}

func (c *AttachedCodexController) Done() <-chan struct{} {
	return c.done
}

func (c *AttachedCodexController) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

func (c *AttachedCodexController) Close() error {
	c.mu.Lock()
	conn := c.conn
	if c.connClosed {
		conn = nil
	}
	c.conn = nil
	c.connClosed = true
	pending := c.pendingRPC
	c.pendingRPC = make(map[string]chan codexRPCResponse)
	c.pendingReqs = make(map[string]attachedCodexPendingRequest)
	c.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
	if conn != nil {
		_ = conn.Close()
	}
	c.closeDone(nil)
	if c.service != nil && c.service.IsRunning() {
		c.service.ClearAttachedPermissionRequest("")
	}
	return nil
}

func (c *AttachedCodexController) readLoop() {
	for {
		c.mu.Lock()
		conn := c.conn
		closed := c.connClosed
		c.mu.Unlock()
		if conn == nil || closed {
			c.closeDone(nil)
			return
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			c.closeDone(err)
			return
		}

		envelope, parseErr := parseCodexRPCEnvelope(data)
		if parseErr != nil {
			c.forwardProxyMessage(data)
			continue
		}
		switch {
		case envelope.isResponse():
			if !c.resolveResponse(envelope) {
				c.forwardProxyMessage(data)
			}
		case envelope.isRequest():
			if c.handleServerRequest(envelope) {
				c.forwardProxyMessage(data)
			}
		case envelope.isNotification():
			c.handleNotification(envelope)
			c.forwardProxyMessage(data)
		}
	}
}

func (c *AttachedCodexController) handleNotification(envelope codexRPCEnvelope) {
	service := c.service
	switch envelope.Method {
	case "turn/started":
		threadID := c.adoptThread(codexNotificationThreadID(envelope.Params))
		c.mu.Lock()
		c.turnID = strings.TrimSpace(getString(parseCodexParamsObject(envelope.Params), "turnId"))
		c.mu.Unlock()
		c.beginObservedTurn(threadID)
		if service != nil && service.IsRunning() {
			_ = service.StartAttachedTurn()
		}
	case "thread/status/changed":
		threadID := c.adoptThread(codexNotificationThreadID(envelope.Params))
		if service != nil && service.IsRunning() {
			statusType := codexThreadStatusType(envelope.Params)
			service.setThinking(statusType == "active")
			if statusType == "active" {
				c.beginObservedTurn(threadID)
				_ = service.StartAttachedTurn()
			}
			if statusType == "idle" {
				service.ClearAttachedPermissionRequest("")
				snapshotResult := c.finishObservedTurn(threadID)
				if snapshotResult.ThreadID != "" {
					c.scheduleSnapshotSync(snapshotResult, protocol.TurnCompleted)
				} else {
					_ = service.FinishAttachedTurn(protocol.TurnCompleted)
				}
			}
		}
	case "turn/completed":
		c.mu.Lock()
		c.turnID = ""
		c.mu.Unlock()
		if service != nil && service.IsRunning() {
			service.ClearAttachedPermissionRequest("")
			status := codexTurnStatusFromParams(envelope.Params)
			finishStatus := protocol.TurnFailed
			switch strings.TrimSpace(status) {
			case "", "completed":
				finishStatus = protocol.TurnCompleted
			case "cancelled":
				finishStatus = protocol.TurnCancelled
			}
			snapshotResult := c.finishObservedTurn(codexNotificationThreadID(envelope.Params))
			if snapshotResult.ThreadID != "" {
				c.scheduleSnapshotSync(snapshotResult, finishStatus)
			} else {
				_ = service.FinishAttachedTurn(finishStatus)
			}
		}
	}

	if service != nil && service.IsRunning() {
		translated := c.translateNotification(envelope)
		if len(translated) > 0 {
			c.markObservedLiveOutput()
		}
		for _, line := range translated {
			_ = service.StartAttachedTurn()
			_ = service.EmitStructuredTextLine(line)
		}
	}
}

func (c *AttachedCodexController) translateNotification(envelope codexRPCEnvelope) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.liveAdapter == nil {
		c.liveAdapter = newCodexLiveAdapter()
	}
	return c.liveAdapter.TranslateNotification(envelope)
}

func (c *AttachedCodexController) adoptThread(threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ""
	}

	c.mu.Lock()
	current := strings.TrimSpace(c.threadID)
	if current == threadID {
		c.mu.Unlock()
		return threadID
	}
	c.threadID = threadID
	c.mu.Unlock()

	service := c.service
	if service != nil && service.IsRunning() {
		service.UpdateAttachedSessionState(AttachedSessionStateUpdate{
			RuntimeSessionID: threadID,
		})
		if err := service.sendCurrentKeepalive(service.SessionID()); err != nil {
			applog.Errorf("[Remote] codex attached adopt keepalive failed: session=%s thread=%s err=%v", service.SessionID(), threadID, err)
		}
		c.scheduleInitialSnapshotSync(service.SessionID(), threadID)
	}
	return threadID
}

func (c *AttachedCodexController) scheduleInitialSnapshotSync(sessionID, threadID string) {
	sessionID = strings.TrimSpace(sessionID)
	threadID = strings.TrimSpace(threadID)
	if sessionID == "" || threadID == "" {
		return
	}

	c.mu.Lock()
	if c.snapshotSyncing {
		c.mu.Unlock()
		return
	}
	c.snapshotSyncing = true
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			c.snapshotSyncing = false
			c.mu.Unlock()
		}()
		if err := c.syncThreadSnapshot(sessionID, threadID, "", false); err != nil && !isCodexThreadUnavailable(err) && !isCodexThreadNotMaterialized(err) {
			applog.Errorf("[Remote] codex attached initial snapshot sync failed: session=%s thread=%s err=%v", sessionID, threadID, err)
		}
	}()
}

func (c *AttachedCodexController) beginObservedTurn(threadID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observedThreadID = strings.TrimSpace(threadID)
	c.observedLiveOutput = false
}

func (c *AttachedCodexController) markObservedLiveOutput() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(c.observedThreadID) == "" {
		return
	}
	c.observedLiveOutput = true
}

func (c *AttachedCodexController) finishObservedTurn(threadID string) attachedCodexObservedTurnResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	observedThreadID := strings.TrimSpace(c.observedThreadID)
	if observedThreadID == "" {
		return attachedCodexObservedTurnResult{}
	}
	if strings.TrimSpace(threadID) == "" {
		threadID = observedThreadID
	}
	threadID = strings.TrimSpace(threadID)
	shouldSync := threadID == observedThreadID
	emitSnapshotLines := shouldSync && !c.observedLiveOutput
	c.observedThreadID = ""
	c.observedLiveOutput = false
	if !shouldSync {
		return attachedCodexObservedTurnResult{}
	}
	return attachedCodexObservedTurnResult{
		ThreadID:          threadID,
		EmitSnapshotLines: emitSnapshotLines,
	}
}

func (c *AttachedCodexController) scheduleSnapshotSync(result attachedCodexObservedTurnResult, finishStatus string) {
	threadID := strings.TrimSpace(result.ThreadID)
	if threadID == "" {
		return
	}
	service := c.service
	if service == nil || !service.IsRunning() {
		return
	}

	c.mu.Lock()
	if c.snapshotSyncing {
		c.mu.Unlock()
		return
	}
	c.snapshotSyncing = true
	c.mu.Unlock()

	go func(sessionID string) {
		defer func() {
			c.mu.Lock()
			c.snapshotSyncing = false
			c.mu.Unlock()
		}()
		if err := c.syncThreadSnapshot(sessionID, threadID, finishStatus, result.EmitSnapshotLines); err != nil && !isCodexThreadUnavailable(err) && !isCodexThreadNotMaterialized(err) {
			applog.Errorf("[Remote] codex attached snapshot sync failed: session=%s thread=%s err=%v", sessionID, threadID, err)
		}
	}(service.SessionID())
}

func (c *AttachedCodexController) syncThreadSnapshot(sessionID, threadID, finishStatus string, emitSnapshotLines bool) error {
	service := c.service
	defer func() {
		if finishStatus != "" && service != nil && service.IsRunning() {
			service.ClearAttachedPermissionRequest("")
			_ = service.FinishAttachedTurn(finishStatus)
		}
	}()

	var readResult codexThreadReadResult
	if err := c.call("thread/read", map[string]any{
		"threadId":     threadID,
		"includeTurns": true,
	}, &readResult); err != nil {
		return err
	}

	batch := buildCodexHistoryBatchFromThread(&readResult.Thread, nil, nil)
	applyRuntimeSessionIDToHistoryBatch(batch, threadID)
	batch = c.filterUnsyncedHistory(batch)
	if len(batch) == 0 {
		return nil
	}

	if emitSnapshotLines && service != nil && service.IsRunning() {
		for _, msg := range batch {
			includeUser := c.shouldEmitSnapshotUserMessage(msg)
			for _, line := range codexHistoryMessageDisplayLines(msg, includeUser) {
				if strings.TrimSpace(line) == "" {
					continue
				}
				if err := service.EmitStructuredTextLine(line); err != nil {
					return err
				}
			}
		}
	}

	historyPusher := newSessionHistoryPusher(service.cfg.ServerURL, service.cfg.Token, service.contentProtector)
	if err := historyPusher.pushBatchWithRuntime(sessionID, threadID, batch); err != nil {
		return err
	}
	c.markHistorySynced(batch)
	return nil
}

func (c *AttachedCodexController) filterUnsyncedHistory(batch []protocol.SessionHistoryMessage) []protocol.SessionHistoryMessage {
	if len(batch) == 0 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.syncedHistory == nil {
		c.syncedHistory = make(map[string]struct{})
	}
	filtered := make([]protocol.SessionHistoryMessage, 0, len(batch))
	for _, msg := range batch {
		key := codexHistoryMessageKey(msg)
		if key == "" {
			filtered = append(filtered, msg)
			continue
		}
		if _, exists := c.syncedHistory[key]; exists {
			continue
		}
		filtered = append(filtered, msg)
	}
	return filtered
}

func (c *AttachedCodexController) markHistorySynced(batch []protocol.SessionHistoryMessage) {
	if len(batch) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.syncedHistory == nil {
		c.syncedHistory = make(map[string]struct{})
	}
	for _, msg := range batch {
		key := codexHistoryMessageKey(msg)
		if key == "" {
			continue
		}
		c.syncedHistory[key] = struct{}{}
	}
}

const attachedCodexForwardedInputTTL = 2 * time.Minute

func normalizeAttachedCodexForwardedInput(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return strings.TrimSpace(content)
}

func (c *AttachedCodexController) shouldEmitSnapshotUserMessage(msg protocol.SessionHistoryMessage) bool {
	if strings.TrimSpace(msg.Role) != "user" {
		return false
	}
	content := normalizeAttachedCodexForwardedInput(msg.Content)
	if content == "" {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.pruneForwardedInputsLocked(now)
	for index, entry := range c.forwardedInputs {
		if entry.Content != content {
			continue
		}
		c.forwardedInputs = append(c.forwardedInputs[:index], c.forwardedInputs[index+1:]...)
		return false
	}
	return true
}

func (c *AttachedCodexController) pruneForwardedInputsLocked(now time.Time) {
	if len(c.forwardedInputs) == 0 {
		return
	}
	cutoff := now.Add(-attachedCodexForwardedInputTTL)
	filtered := c.forwardedInputs[:0]
	for _, entry := range c.forwardedInputs {
		if entry.RecordedAt.After(cutoff) {
			filtered = append(filtered, entry)
		}
	}
	c.forwardedInputs = filtered
}

func (c *AttachedCodexController) handleServerRequest(envelope codexRPCEnvelope) bool {
	service := c.service
	requestID := strings.TrimSpace(envelope.idString())
	if requestID == "" {
		return true
	}

	respond := func(payload any) {
		if err := c.writeEnvelope(map[string]any{
			"id":     envelope.idValue(),
			"result": payload,
		}); err != nil {
			applog.Errorf("[Remote] codex attached request response failed: request=%s method=%s err=%v", requestID, envelope.Method, err)
		}
	}

	register := func(
		toolName string,
		input map[string]any,
		summary string,
		buildResult func(protocol.PermissionResponsePayload) any,
		parseResult func(json.RawMessage) protocol.PermissionResponsePayload,
		forwardToProxy bool,
	) bool {
		if input == nil {
			input = map[string]any{}
		}
		if service != nil && service.ShouldAutoApproveTool(toolName, input) {
			resp := protocol.PermissionResponsePayload{
				RequestID: requestID,
				Approved:  true,
				Decision:  protocol.PermissionDecisionApproved,
			}
			respond(buildResult(resp))
			return false
		}

		c.mu.Lock()
		c.pendingReqs[requestID] = attachedCodexPendingRequest{
			RequestID:      requestID,
			RequestIDValue: envelope.idValue(),
			ToolName:       toolName,
			Input:          input,
			BuildResult:    buildResult,
			ParseResult:    parseResult,
			ForwardToProxy: forwardToProxy,
		}
		c.mu.Unlock()

		if service != nil && service.IsRunning() {
			if err := service.EmitAttachedPermissionRequest(requestID, toolName, input); err != nil {
				applog.Errorf("[Remote] codex attached permission forward failed: request=%s tool=%s err=%v", requestID, toolName, err)
			}
		}
		return forwardToProxy
	}

	params := parseCodexParamsObject(envelope.Params)
	switch envelope.Method {
	case "item/commandExecution/requestApproval":
		input := map[string]any{
			"command":          getString(params, "command"),
			"cwd":              getString(params, "cwd"),
			"command_actions":  params["commandActions"],
			"approval_context": params["networkApprovalContext"],
			"reason":           getString(params, "reason"),
		}
		return register("Bash", input, summarizePermissionRequest("Bash", input), func(resp protocol.PermissionResponsePayload) any {
			switch resp.Decision {
			case protocol.PermissionDecisionApprovedForSession:
				return map[string]any{"decision": "acceptForSession"}
			case protocol.PermissionDecisionDenied:
				return map[string]any{"decision": "decline"}
			case protocol.PermissionDecisionAbort:
				return map[string]any{"decision": "cancel"}
			default:
				return map[string]any{"decision": "accept"}
			}
		}, parseCodexSimpleDecisionResult, false)
	case "item/fileChange/requestApproval":
		input := map[string]any{
			"file_path":  getString(params, "grantRoot"),
			"grant_root": getString(params, "grantRoot"),
			"reason":     getString(params, "reason"),
		}
		return register("Write", input, summarizePermissionRequest("Write", input), func(resp protocol.PermissionResponsePayload) any {
			switch resp.Decision {
			case protocol.PermissionDecisionApprovedForSession:
				return map[string]any{"decision": "acceptForSession"}
			case protocol.PermissionDecisionDenied:
				return map[string]any{"decision": "decline"}
			case protocol.PermissionDecisionAbort:
				return map[string]any{"decision": "cancel"}
			default:
				return map[string]any{"decision": "accept"}
			}
		}, parseCodexSimpleDecisionResult, false)
	case "item/permissions/requestApproval":
		input := map[string]any{
			"permissions": params["permissions"],
			"reason":      getString(params, "reason"),
		}
		return register("Permissions", input, summarizePermissionRequest("Permissions", input), func(resp protocol.PermissionResponsePayload) any {
			scope := "turn"
			if resp.Decision == protocol.PermissionDecisionApprovedForSession {
				scope = "session"
			}
			permissions, _ := params["permissions"].(map[string]any)
			if resp.Decision == protocol.PermissionDecisionDenied || resp.Decision == protocol.PermissionDecisionAbort {
				permissions = map[string]any{}
			}
			return map[string]any{
				"permissions": permissions,
				"scope":       scope,
			}
		}, parseCodexPermissionsApprovalResult, false)
	case "item/tool/requestUserInput":
		return register("RequestUserInput", params, summarizePermissionRequest("RequestUserInput", params), func(resp protocol.PermissionResponsePayload) any {
			return buildCodexUserInputResult(params, resp.UpdatedInput)
		}, nil, false)
	case "item/tool/call":
		toolName := strings.TrimSpace(getString(params, "tool"))
		arguments, _ := params["arguments"].(map[string]any)
		if toolName == "" || !isCodexInteractivePayload(arguments) {
			respond(map[string]any{
				"success": false,
				"contentItems": []map[string]any{{
					"type": "inputText",
					"text": "Client dynamic tool calls are not supported yet.",
				}},
			})
			return false
		}
		return register(toolName, arguments, summarizeCodexInteractiveRequest(toolName, arguments), func(resp protocol.PermissionResponsePayload) any {
			return buildCodexDynamicToolResult(toolName, arguments, resp)
		}, nil, false)
	case "mcpServer/elicitation/request":
		return register("McpElicitation", params, summarizeCodexInteractiveRequest("McpElicitation", params), func(resp protocol.PermissionResponsePayload) any {
			return buildCodexMcpElicitationResult(params, resp)
		}, parseCodexMcpElicitationApprovalResult, false)
	case "execCommandApproval":
		input := map[string]any{
			"approval_id":                        getString(params, "approvalId"),
			"command":                            getString(params, "command"),
			"cwd":                                getString(params, "cwd"),
			"reason":                             getString(params, "reason"),
			"approval_context":                   params["networkApprovalContext"],
			"additional_permissions":             params["additionalPermissions"],
			"proposed_execpolicy_amendment":      params["proposedExecpolicyAmendment"],
			"proposed_network_policy_amendments": params["proposedNetworkPolicyAmendments"],
			"available_decisions":                params["availableDecisions"],
		}
		return register("Bash", input, summarizePermissionRequest("Bash", input), buildCodexExecCommandApprovalResult, parseCodexSimpleDecisionResult, false)
	case "applyPatchApproval":
		input := map[string]any{
			"grant_root": getString(params, "grantRoot"),
			"file_path":  getString(params, "grantRoot"),
			"changes":    params["changes"],
			"reason":     getString(params, "reason"),
		}
		return register("Write", input, summarizePermissionRequest("Write", input), buildCodexApplyPatchApprovalResult, parseCodexSimpleDecisionResult, false)
	}
	return true
}

func (c *AttachedCodexController) call(method string, params any, out any) error {
	requestID, responseCh := c.registerRPC()
	if err := c.writeEnvelope(map[string]any{
		"id":     requestID,
		"method": method,
		"params": params,
	}); err != nil {
		c.clearRPC(requestID)
		return err
	}

	select {
	case response, ok := <-responseCh:
		if !ok {
			return fmt.Errorf("%s cancelled", method)
		}
		if response.Error != nil {
			return fmt.Errorf("%s: %s", method, response.Error.Message)
		}
		if out != nil && len(response.Result) > 0 {
			if err := json.Unmarshal(response.Result, out); err != nil {
				return err
			}
		}
		return nil
	case <-time.After(45 * time.Second):
		c.clearRPC(requestID)
		return fmt.Errorf("%s timed out", method)
	}
}

func (c *AttachedCodexController) registerRPC() (string, chan codexRPCResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rpcSeq++
	requestID := fmt.Sprintf("attached-codex-%d", c.rpcSeq)
	responseCh := make(chan codexRPCResponse, 1)
	c.pendingRPC[requestID] = responseCh
	return requestID, responseCh
}

func (c *AttachedCodexController) clearRPC(requestID string) {
	c.mu.Lock()
	responseCh := c.pendingRPC[requestID]
	delete(c.pendingRPC, requestID)
	c.mu.Unlock()
	if responseCh != nil {
		close(responseCh)
	}
}

func (c *AttachedCodexController) resolveResponse(envelope codexRPCEnvelope) bool {
	requestID := envelope.idString()
	if requestID == "" {
		return false
	}
	c.mu.Lock()
	responseCh := c.pendingRPC[requestID]
	if responseCh != nil {
		delete(c.pendingRPC, requestID)
	}
	c.mu.Unlock()
	if responseCh == nil {
		return false
	}
	responseCh <- codexRPCResponse{
		Result: envelope.Result,
		Error:  envelope.Error,
	}
	close(responseCh)
	return true
}

func (c *AttachedCodexController) writeEnvelope(payload any) error {
	c.mu.Lock()
	conn := c.conn
	closed := c.connClosed
	c.mu.Unlock()
	if conn == nil || closed {
		return fmt.Errorf("codex controller is not connected")
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (c *AttachedCodexController) writeRawMessage(messageType int, data []byte) error {
	c.mu.Lock()
	conn := c.conn
	closed := c.connClosed
	c.mu.Unlock()
	if conn == nil || closed {
		return fmt.Errorf("codex controller is not connected")
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.WriteMessage(messageType, data)
}

func (c *AttachedCodexController) SetProxyForwarder(forward func([]byte) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proxyForward = forward
}

func (c *AttachedCodexController) ForwardProxyClientMessage(messageType int, data []byte, reply func([]byte) error) error {
	if messageType != websocket.TextMessage {
		return c.writeRawMessage(messageType, data)
	}

	envelope, err := parseCodexRPCEnvelope(data)
	if err != nil {
		return c.writeRawMessage(messageType, bytes.TrimSpace(data))
	}
	if envelope.isRequest() && strings.TrimSpace(envelope.Method) == "initialize" {
		if reply == nil {
			return nil
		}
		return reply(c.buildProxyInitializeResponse(envelope.idValue()))
	}
	if envelope.isNotification() && strings.TrimSpace(envelope.Method) == "initialized" {
		return nil
	}
	if envelope.isResponse() {
		c.handleProxyClientResponse(envelope)
	}
	return c.writeRawMessage(messageType, bytes.TrimSpace(data))
}

func (c *AttachedCodexController) buildProxyInitializeResponse(id any) []byte {
	c.mu.Lock()
	result := c.initializeResult
	c.mu.Unlock()
	if result == nil {
		result = map[string]any{}
	}
	data, _ := json.Marshal(map[string]any{
		"id":     id,
		"result": result,
	})
	return data
}

func (c *AttachedCodexController) handleProxyClientResponse(envelope codexRPCEnvelope) {
	service := c.service
	if service == nil || !service.IsRunning() {
		return
	}
	requestID := strings.TrimSpace(envelope.idString())
	if requestID == "" {
		return
	}
	c.mu.Lock()
	pending, ok := c.pendingReqs[requestID]
	if ok {
		delete(c.pendingReqs, requestID)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if pending.ParseResult != nil {
		resp := pending.ParseResult(envelope.Result)
		if resp.RequestID == "" {
			resp.RequestID = requestID
		}
		if resp.Decision == protocol.PermissionDecisionApprovedForSession {
			service.recordSessionPermission(pending.ToolName, pending.Input)
		}
		if resp.Approved || resp.Decision == protocol.PermissionDecisionApproved || resp.Decision == protocol.PermissionDecisionApprovedForSession {
			service.rememberUpdatedToolInput(pending.ToolName, pending.Input, resp.UpdatedInput)
		}
	}
	service.ClearAttachedPermissionRequest(requestID)
	service.resumeCodexAfterPermissionResolution(service.SessionID())
}

func (c *AttachedCodexController) forwardProxyMessage(data []byte) {
	c.mu.Lock()
	forward := c.proxyForward
	c.mu.Unlock()
	if forward == nil || len(data) == 0 {
		return
	}
	if err := forward(data); err != nil {
		applog.Errorf("[Remote] codex proxy forward failed: %v", err)
	}
}

func (c *AttachedCodexController) forwardProxyPermissionResponse(id any, payload any) {
	data, err := json.Marshal(map[string]any{
		"id":     id,
		"result": payload,
	})
	if err != nil {
		applog.Errorf("[Remote] codex proxy permission response marshal failed: %v", err)
		return
	}
	c.forwardProxyMessage(data)
}

func parseCodexSimpleDecisionResult(raw json.RawMessage) protocol.PermissionResponsePayload {
	result := parseCodexParamsObject(raw)
	decision := strings.TrimSpace(strings.ToLower(getString(result, "decision")))
	resp := protocol.PermissionResponsePayload{}
	switch decision {
	case "acceptforsession":
		resp.Approved = true
		resp.Decision = protocol.PermissionDecisionApprovedForSession
	case "decline":
		resp.Decision = protocol.PermissionDecisionDenied
	case "cancel":
		resp.Decision = protocol.PermissionDecisionAbort
	default:
		resp.Approved = true
		resp.Decision = protocol.PermissionDecisionApproved
	}
	return resp
}

func parseCodexPermissionsApprovalResult(raw json.RawMessage) protocol.PermissionResponsePayload {
	result := parseCodexParamsObject(raw)
	if len(result) == 0 {
		return protocol.PermissionResponsePayload{}
	}

	resp := protocol.PermissionResponsePayload{}
	permissions, _ := result["permissions"].(map[string]any)
	if len(permissions) == 0 {
		resp.Decision = protocol.PermissionDecisionDenied
		return resp
	}
	resp.Approved = true
	if strings.EqualFold(strings.TrimSpace(getString(result, "scope")), "session") {
		resp.Decision = protocol.PermissionDecisionApprovedForSession
	} else {
		resp.Decision = protocol.PermissionDecisionApproved
	}
	return resp
}

func parseCodexMcpElicitationApprovalResult(raw json.RawMessage) protocol.PermissionResponsePayload {
	result := parseCodexParamsObject(raw)
	action := strings.TrimSpace(strings.ToLower(getString(result, "action")))
	resp := protocol.PermissionResponsePayload{}
	switch action {
	case "decline":
		resp.Decision = protocol.PermissionDecisionDenied
	case "cancel":
		resp.Decision = protocol.PermissionDecisionAbort
	default:
		resp.Approved = true
		resp.Decision = protocol.PermissionDecisionApproved
	}
	return resp
}

func (c *AttachedCodexController) closeDone(err error) {
	c.doneOnce.Do(func() {
		c.mu.Lock()
		c.readErr = err
		c.connClosed = true
		c.conn = nil
		pending := c.pendingRPC
		c.pendingRPC = make(map[string]chan codexRPCResponse)
		c.mu.Unlock()
		for _, ch := range pending {
			close(ch)
		}
		close(c.done)
	})
}
