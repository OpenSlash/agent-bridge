package remote

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

type codexRPCEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *codexRPCError  `json:"error,omitempty"`
}

type codexRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type codexRPCResponse struct {
	Result json.RawMessage
	Error  *codexRPCError
}

type codexBootstrap struct {
	ThreadID     string
	Model        string
	Cwd          string
	HistoryBatch []protocol.SessionHistoryMessage
}

type codexThreadStartResult struct {
	Thread codexThreadSnapshot `json:"thread"`
	Model  string              `json:"model"`
	Cwd    string              `json:"cwd"`
}

type codexThreadSnapshot struct {
	ID        string            `json:"id"`
	Cwd       string            `json:"cwd"`
	CreatedAt int64             `json:"createdAt"`
	Turns     []codexThreadTurn `json:"turns"`
}

type codexThreadTurn struct {
	ID    string           `json:"id"`
	Items []map[string]any `json:"items"`
}

type codexThreadReadResult struct {
	Thread codexThreadSnapshot `json:"thread"`
}

type codexThreadForkResult struct {
	Thread           codexThreadSnapshot `json:"thread"`
	Model            string              `json:"model"`
	Cwd              string              `json:"cwd"`
	ApprovalPolicy   string              `json:"approvalPolicy"`
	RuntimeSessionID string              `json:"threadId"`
}

type codexTurnStartResult struct {
	Turn struct {
		ID string `json:"id"`
	} `json:"turn"`
}

type codexLiveAdapter struct {
	activeAssistant map[string]struct{}
	activeTools     map[string]struct{}
	toolOutput      map[string]string
	lastSystemLine  map[string]string
	seenUserItems   map[string]struct{}
}

func newCodexLiveAdapter() *codexLiveAdapter {
	return &codexLiveAdapter{
		activeAssistant: make(map[string]struct{}),
		activeTools:     make(map[string]struct{}),
		toolOutput:      make(map[string]string),
		lastSystemLine:  make(map[string]string),
		seenUserItems:   make(map[string]struct{}),
	}
}

func parseCodexRPCEnvelope(line []byte) (codexRPCEnvelope, error) {
	var envelope codexRPCEnvelope
	err := json.Unmarshal(bytes.TrimSpace(line), &envelope)
	return envelope, err
}

func (e codexRPCEnvelope) isNotification() bool {
	return len(e.ID) == 0 && strings.TrimSpace(e.Method) != ""
}

func (e codexRPCEnvelope) isRequest() bool {
	return len(e.ID) > 0 && strings.TrimSpace(e.Method) != ""
}

func (e codexRPCEnvelope) isResponse() bool {
	return len(e.ID) > 0 && strings.TrimSpace(e.Method) == ""
}

func (e codexRPCEnvelope) idString() string {
	if len(e.ID) == 0 {
		return ""
	}
	var stringID string
	if err := json.Unmarshal(e.ID, &stringID); err == nil {
		return stringID
	}
	var numericID int64
	if err := json.Unmarshal(e.ID, &numericID); err == nil {
		return fmt.Sprintf("%d", numericID)
	}
	return strings.TrimSpace(string(e.ID))
}

func (e codexRPCEnvelope) idValue() any {
	if len(e.ID) == 0 {
		return nil
	}
	return json.RawMessage(e.ID)
}

func (s *Service) bootstrapCodexSession(
	stdin io.WriteCloser,
	stdoutReader *bufio.Reader,
	sessionID,
	runtimeSessionID,
	workingDir,
	model,
	permissionMode,
	sandboxMode string,
	resume bool,
) (codexBootstrap, error) {
	if err := codexCallSync(stdin, stdoutReader, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "acw2a",
			"version": "0.0.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}, nil); err != nil {
		return codexBootstrap{}, err
	}

	params := map[string]any{
		"cwd":            workingDir,
		"approvalPolicy": codexApprovalPolicy(permissionMode),
		"sandbox":        normalizeSandboxModeForRuntime(runtimeCodex, sandboxMode),
		"personality":    "pragmatic",
	}
	if strings.TrimSpace(model) != "" {
		params["model"] = strings.TrimSpace(model)
	}

	method := "thread/start"
	resumeThreadID := strings.TrimSpace(runtimeSessionID)
	if resumeThreadID == "" {
		resumeThreadID = strings.TrimSpace(sessionID)
	}
	if resume && resumeThreadID != "" {
		method = "thread/resume"
		params["threadId"] = resumeThreadID
	}
	applog.Info.Printf(
		"[Remote] codex bootstrap start: relay_session=%s runtime_session=%s method=%s cwd=%s model=%s permission=%s",
		strings.TrimSpace(sessionID),
		resumeThreadID,
		method,
		workingDir,
		model,
		permissionMode,
	)

	var startResult codexThreadStartResult
	if err := codexCallSync(stdin, stdoutReader, method, params, &startResult); err != nil {
		if method == "thread/resume" && isCodexResumeRolloutMissing(err) {
			applog.Info.Printf(
				"[Remote] codex native resume unavailable, falling back to detached thread: relay_session=%s runtime_session=%s err=%v",
				strings.TrimSpace(sessionID),
				resumeThreadID,
				err,
			)
			delete(params, "threadId")
			method = "thread/start"
			if startErr := codexCallSync(stdin, stdoutReader, method, params, &startResult); startErr != nil {
				return codexBootstrap{}, startErr
			}
		} else {
			return codexBootstrap{}, err
		}
	}

	threadID := strings.TrimSpace(startResult.Thread.ID)
	if threadID == "" {
		return codexBootstrap{}, fmt.Errorf("codex returned empty thread id")
	}
	applog.Info.Printf(
		"[Remote] codex bootstrap ready: relay_session=%s runtime_session=%s method=%s result_thread=%s cwd=%s model=%s",
		strings.TrimSpace(sessionID),
		resumeThreadID,
		method,
		threadID,
		strings.TrimSpace(startResult.Cwd),
		strings.TrimSpace(startResult.Model),
	)

	var readResult codexThreadReadResult
	if err := codexCallSync(stdin, stdoutReader, "thread/read", map[string]any{
		"threadId":     threadID,
		"includeTurns": true,
	}, &readResult); err != nil {
		if isCodexThreadNotMaterialized(err) {
			applog.Info.Printf("[Remote] codex thread/read deferred until first user message: thread=%s err=%v", threadID, err)
		} else {
			applog.Errorf("[Remote] codex thread/read failed: thread=%s err=%v", threadID, err)
		}
	}

	cwd := strings.TrimSpace(startResult.Cwd)
	if cwd == "" {
		cwd = strings.TrimSpace(startResult.Thread.Cwd)
	}

	return codexBootstrap{
		ThreadID:     threadID,
		Model:        strings.TrimSpace(startResult.Model),
		Cwd:          cwd,
		HistoryBatch: buildCodexHistoryBatchFromThread(&readResult.Thread, s.rewriteCodexUserInput, s.rewriteToolInput),
	}, nil
}

func isCodexResumeRolloutMissing(err error) bool {
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(message, "thread/resume:") && strings.Contains(message, "no rollout found")
}

func isCodexThreadNotMaterialized(err error) bool {
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(message, "thread/read:") &&
		strings.Contains(message, "not materialized yet")
}

func isCodexThreadUnavailable(err error) bool {
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if !strings.Contains(message, "thread") {
		return false
	}
	return strings.Contains(message, "not found") ||
		strings.Contains(message, "no rollout found") ||
		strings.Contains(message, "unknown thread")
}

func codexCallSync(stdin io.WriteCloser, stdoutReader *bufio.Reader, method string, params any, out any) error {
	requestID := fmt.Sprintf("bootstrap-%d", time.Now().UnixNano())
	if err := writeCodexRPC(stdin, requestID, method, params); err != nil {
		return err
	}

	for {
		line, err := stdoutReader.ReadBytes('\n')
		if err != nil {
			return err
		}
		envelope, parseErr := parseCodexRPCEnvelope(line)
		if parseErr != nil || !envelope.isResponse() || envelope.idString() != requestID {
			continue
		}
		if envelope.Error != nil {
			return fmt.Errorf("%s: %s", method, envelope.Error.Message)
		}
		if out != nil && len(envelope.Result) > 0 {
			if err := json.Unmarshal(envelope.Result, out); err != nil {
				return err
			}
		}
		return nil
	}
}

func writeCodexRPC(writer io.Writer, requestID, method string, params any) error {
	return writeCodexRPCEnvelope(writer, map[string]any{
		"id":     requestID,
		"method": method,
		"params": params,
	})
}

func writeCodexRPCEnvelope(writer io.Writer, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = writer.Write(data)
	return err
}

func codexApprovalPolicy(permissionMode string) string {
	switch normalizePermissionMode(permissionMode) {
	case protocol.PermissionModeBypassPermissions:
		return "never"
	case protocol.PermissionModeDontAsk:
		return "untrusted"
	default:
		return "on-request"
	}
}

func codexSandboxPolicy(cwd, sandboxMode string) map[string]any {
	normalizedMode := normalizeSandboxModeForRuntime(runtimeCodex, sandboxMode)
	switch normalizedMode {
	case protocol.SandboxModeDangerFullAccess:
		return map[string]any{
			"type": "dangerFullAccess",
		}
	case protocol.SandboxModeReadOnly:
		return map[string]any{
			"type":          "readOnly",
			"access":        map[string]any{"type": "fullAccess"},
			"networkAccess": true,
		}
	default:
		writableRoots := []string{}
		trimmedCwd := strings.TrimSpace(cwd)
		if trimmedCwd != "" {
			writableRoots = append(writableRoots, trimmedCwd)
		}
		return map[string]any{
			"type":                "workspaceWrite",
			"writableRoots":       writableRoots,
			"readOnlyAccess":      map[string]any{"type": "fullAccess"},
			"networkAccess":       true,
			"excludeTmpdirEnvVar": false,
			"excludeSlashTmp":     false,
		}
	}
}

func (s *Service) startCodexProcessBridge(cmd *exec.Cmd, stdin io.WriteCloser, stdoutReader *bufio.Reader, sessionID string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		scanner := bufio.NewScanner(stdoutReader)
		scanner.Buffer(make([]byte, 1024*1024), 4*1024*1024)
		historySyncer := newCodexHistorySyncer(sessionID, s.cfg.ServerURL, s.cfg.Token, s.contentProtector, s.RuntimeSessionID, s.rewriteCodexUserInput, s.rewriteToolInput)
		liveAdapter := newCodexLiveAdapter()
		lastFailureMessage := ""
		didEmitFailureMessage := false
		defer func() {
			if err := historySyncer.Flush(); err != nil {
				applog.Errorf("[Remote] codex history final flush error: session=%s err=%v", sessionID, err)
			}
		}()

		for scanner.Scan() {
			if !s.isCurrentCommand(cmd) {
				continue
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			envelope, err := parseCodexRPCEnvelope([]byte(line))
			if err != nil {
				continue
			}

			switch {
			case envelope.isResponse():
				s.resolveCodexRPCResponse(envelope)
				continue
			case envelope.isRequest():
				go s.handleCodexServerRequest(cmd, stdin, sessionID, envelope)
				continue
			case !envelope.isNotification():
				continue
			}

			historySyncer.HandleLine(line)

			switch envelope.Method {
			case "turn/started":
				lastFailureMessage = ""
				didEmitFailureMessage = false
				threadID := s.adoptCodexRuntimeThread(sessionID, codexNotificationThreadID(envelope.Params))
				if threadID == "" {
					threadID = s.getCodexThreadID()
				}
				s.beginCodexObservedTurn(threadID)
			case "error":
				errorMessage := codexNotificationErrorMessage(envelope.Params)
				willRetry := codexNotificationErrorWillRetry(envelope.Params)
				if errorMessage != "" {
					lastFailureMessage = errorMessage
				}
				applog.Errorf(
					"[Remote] codex error notification: session=%s will_retry=%t message=%s payload=%s",
					sessionID,
					willRetry,
					errorMessage,
					debugPreviewString(string(envelope.Params), 600),
				)
				if !willRetry && errorMessage != "" && !didEmitFailureMessage {
					if err := s.sendSyntheticSystemEvent(sessionID, "Codex execution failed: "+errorMessage); err != nil {
						applog.Errorf("[Remote] codex error event failed: %v", err)
					} else {
						didEmitFailureMessage = true
					}
				}
			case "thread/status/changed":
				threadID := s.adoptCodexRuntimeThread(sessionID, codexNotificationThreadID(envelope.Params))
				statusType := codexThreadStatusType(envelope.Params)
				statusMessage := codexThreadStatusMessage(envelope.Params)
				applog.Info.Printf(
					"[Remote] codex status changed: session=%s status=%s message=%s payload=%s",
					sessionID,
					statusType,
					statusMessage,
					debugPreviewString(string(envelope.Params), 400),
				)
				s.setThinking(statusType == "active")
				if statusType == "active" {
					s.beginCodexObservedTurn(threadID)
					if err := s.StartAttachedTurn(); err != nil {
						applog.Errorf("[Remote] codex external turn-start failed: session=%s err=%v", sessionID, err)
					}
				}
				if statusType != "" && statusType != "active" && statusType != "idle" {
					lastFailureMessage = statusMessage
					if lastFailureMessage != "" && !didEmitFailureMessage {
						if err := s.sendSyntheticSystemEvent(sessionID, "Codex execution failed: "+lastFailureMessage); err != nil {
							applog.Errorf("[Remote] codex system error event failed: %v", err)
						} else {
							didEmitFailureMessage = true
						}
					}
				}
				if statusType == "idle" {
					s.scheduleCodexThreadSnapshotSync(sessionID, s.finishCodexObservedTurn(threadID))
					if err := historySyncer.Flush(); err != nil {
						applog.Errorf("[Remote] codex history flush error: session=%s err=%v", sessionID, err)
					}
					status, shouldEmit := s.finishCodexTurn("")
					if shouldEmit {
						applog.Info.Printf("[Remote] codex turn completed from idle: session=%s status=%s", sessionID, status)
						if err := s.sendTurnEnd(sessionID, status); err != nil {
							applog.Errorf("[Remote] WS write codex turn-end error: %v", err)
						}
					}
				}
			case "codex/event/task_complete":
				taskMessage := codexTaskCompletionMessage(envelope.Params)
				applog.Info.Printf(
					"[Remote] codex task complete: session=%s message=%s payload=%s",
					sessionID,
					taskMessage,
					debugPreviewString(string(envelope.Params), 400),
				)
				if taskMessage != "" {
					lastFailureMessage = taskMessage
				}
				if err := historySyncer.Flush(); err != nil {
					applog.Errorf("[Remote] codex history flush error: session=%s err=%v", sessionID, err)
				}
			case "turn/completed":
				s.scheduleCodexThreadSnapshotSync(sessionID, s.finishCodexObservedTurn(codexNotificationThreadID(envelope.Params)))
				turnStatus := codexTurnStatusFromParams(envelope.Params)
				turnErrorMessage := codexTurnErrorMessage(envelope.Params)
				if turnErrorMessage == "" {
					turnErrorMessage = lastFailureMessage
				}
				applog.Info.Printf(
					"[Remote] codex turn/completed: session=%s status=%s error=%s payload=%s",
					sessionID,
					turnStatus,
					turnErrorMessage,
					debugPreviewString(string(envelope.Params), 400),
				)
				if turnStatus == "failed" && turnErrorMessage != "" && !didEmitFailureMessage {
					if err := s.sendSyntheticSystemEvent(sessionID, "Codex execution failed: "+turnErrorMessage); err != nil {
						applog.Errorf("[Remote] codex failure event failed: %v", err)
					} else {
						didEmitFailureMessage = true
					}
				}
				if err := historySyncer.Flush(); err != nil {
					applog.Errorf("[Remote] codex history flush error: session=%s err=%v", sessionID, err)
				}
				status, shouldEmit := s.finishCodexTurn(turnStatus)
				if shouldEmit {
					applog.Info.Printf("[Remote] codex turn completed from turn/completed: session=%s status=%s", sessionID, status)
					if err := s.sendTurnEnd(sessionID, status); err != nil {
						applog.Errorf("[Remote] WS write codex turn-end error: %v", err)
					}
				}
			}

			translatedLines := liveAdapter.TranslateNotification(envelope)
			if len(translatedLines) > 0 {
				s.markCodexObservedLiveOutput()
				applog.Info.Printf(
					"[Remote] codex live translated: session=%s method=%s count=%d preview=%s",
					sessionID,
					envelope.Method,
					len(translatedLines),
					debugPreviewString(translatedLines[0], 160),
				)
			}
			for _, translated := range translatedLines {
				msg := protocol.Message{
					Type:      protocol.TypeText,
					SessionID: sessionID,
					Payload: protocol.TextPayload{
						Data:     translated,
						Thinking: s.getThinking(),
					},
				}
				if err := s.writeJSON(msg); err != nil {
					applog.Errorf("[Remote] WS write error: %v", err)
					return
				}
			}
		}
		if err := scanner.Err(); err != nil && s.isCurrentCommand(cmd) {
			applog.Errorf("[Remote] codex stdout read error: %v", err)
		}
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := cmd.Wait()
		if !s.isCurrentCommand(cmd) {
			return
		}
		if err != nil {
			applog.Info.Printf("[Remote] Codex process exited: %v", err)
		}
		if status, shouldEmit := s.finishTurnOnExit(); shouldEmit {
			if writeErr := s.sendTurnEnd(sessionID, status); writeErr != nil {
				applog.Errorf("[Remote] WS write turn-end on exit error: %v", writeErr)
			}
		}
		s.markDisconnected()
	}()
}

func (a *codexLiveAdapter) TranslateNotification(envelope codexRPCEnvelope) []string {
	switch envelope.Method {
	case "item/agentMessage/delta":
		params := parseCodexParamsObject(envelope.Params)
		itemID := strings.TrimSpace(getString(params, "itemId"))
		delta := getString(params, "delta")
		if itemID == "" || delta == "" {
			return nil
		}
		result := make([]string, 0, 2)
		if _, ok := a.activeAssistant[itemID]; !ok {
			a.activeAssistant[itemID] = struct{}{}
			result = append(result, marshalCompactJSON(map[string]any{
				"type":          "content_block_start",
				"source_id":     codexMessageSourceID(itemID, 0),
				"content_block": map[string]any{"type": "text", "text": ""},
			}))
		}
		result = append(result, marshalCompactJSON(map[string]any{
			"type":  "content_block_delta",
			"delta": map[string]any{"type": "text_delta", "text": delta},
		}))
		return result

	case "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
		params := parseCodexParamsObject(envelope.Params)
		itemID := strings.TrimSpace(getString(params, "itemId"))
		text := codexNotificationDeltaText(params)
		if itemID == "" || text == "" {
			return nil
		}
		return a.appendToolOutputDelta(itemID, text)

	case "item/commandExecution/terminalInteraction":
		params := parseCodexParamsObject(envelope.Params)
		text := codexNotificationTerminalInteractionText(params)
		if text == "" {
			return nil
		}
		return []string{codexSystemLocalCommandLine(text)}

	case "item/mcpToolCall/progress":
		params := parseCodexParamsObject(envelope.Params)
		text := codexNotificationProgressText(params)
		if text == "" {
			return nil
		}
		return a.emitSystemLine("mcp-progress:"+strings.TrimSpace(getString(params, "itemId")), codexSystemLine(text))

	case "turn/plan/updated":
		params := parseCodexParamsObject(envelope.Params)
		text := codexNotificationPlanText(params)
		if text == "" {
			return nil
		}
		return a.emitSystemLine("turn-plan", codexSystemLine(text))

	case "item/reasoning/summaryPartAdded":
		params := parseCodexParamsObject(envelope.Params)
		itemID := strings.TrimSpace(getString(params, "itemId"))
		text := codexNotificationReasoningSummaryText(params)
		if text == "" {
			return nil
		}
		return a.emitSystemLine("reasoning-summary:"+itemID+":"+text, codexSystemLine(text))

	case "item/started":
		params := parseCodexParamsObject(envelope.Params)
		item, _ := params["item"].(map[string]any)
		if normalizeCodexItemType(getString(item, "type")) == "userMessage" {
			return a.emitUserItem(item)
		}
		msg := codexToolCallMessage(item, time.Now().UnixMilli(), nil, false)
		if strings.TrimSpace(msg.SourceID) == "" {
			return nil
		}
		itemID := firstNonEmpty(strings.TrimSpace(msg.ToolCallID), strings.TrimSpace(msg.SourceID))
		if itemID == "" {
			return nil
		}
		a.activeTools[itemID] = struct{}{}
		return codexToolCallDisplayLines(msg, true, false)

	case "item/completed":
		params := parseCodexParamsObject(envelope.Params)
		item, _ := params["item"].(map[string]any)
		switch normalizeCodexItemType(getString(item, "type")) {
		case "userMessage":
			return a.emitUserItem(item)
		case "agentMessage":
			itemID := strings.TrimSpace(getString(item, "id"))
			if _, ok := a.activeAssistant[itemID]; ok {
				delete(a.activeAssistant, itemID)
				return []string{marshalCompactJSON(map[string]any{"type": "message_stop"})}
			}
			text := strings.TrimSpace(getString(item, "text"))
			if text == "" {
				return nil
			}
			return []string{marshalCompactJSON(map[string]any{
				"type":      "assistant",
				"uuid":      itemID,
				"source_id": codexMessageSourceID(itemID, 0),
				"message": map[string]any{
					"role": "assistant",
					"content": []map[string]any{{
						"type": "text",
						"text": text,
					}},
				},
			})}
		default:
			msg := codexToolCallMessage(item, time.Now().UnixMilli(), nil, true)
			if strings.TrimSpace(msg.SourceID) == "" {
				return nil
			}
			itemID := firstNonEmpty(strings.TrimSpace(msg.ToolCallID), strings.TrimSpace(msg.SourceID))
			result := a.flushToolOutput(itemID)
			includeStart := true
			if _, ok := a.activeTools[itemID]; ok {
				includeStart = false
				delete(a.activeTools, itemID)
			}
			result = append(result, codexToolCallDisplayLines(msg, includeStart, true)...)
			return result
		}
	}
	return nil
}

func (a *codexLiveAdapter) emitUserItem(item map[string]any) []string {
	messages := codexUserMessages(item, time.Now().UnixMilli(), nil)
	if len(messages) == 0 {
		return nil
	}

	lines := make([]string, 0, len(messages))
	for _, msg := range messages {
		key := codexHistoryMessageKey(msg)
		if key == "" {
			key = strings.TrimSpace(fmt.Sprintf("%s|%s", getString(item, "id"), msg.Content))
		}
		if key != "" {
			if _, exists := a.seenUserItems[key]; exists {
				continue
			}
			a.seenUserItems[key] = struct{}{}
		}
		lines = append(lines, codexHistoryMessageDisplayLines(msg, true)...)
	}
	return lines
}

func (a *codexLiveAdapter) appendToolOutputDelta(itemID, delta string) []string {
	itemID = strings.TrimSpace(itemID)
	delta = strings.ReplaceAll(delta, "\r\n", "\n")
	if itemID == "" || delta == "" {
		return nil
	}

	buffer := a.toolOutput[itemID] + delta
	lines := make([]string, 0, strings.Count(buffer, "\n"))
	for {
		index := strings.IndexByte(buffer, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimRight(buffer[:index], "\r")
		buffer = buffer[index+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, codexSystemLocalCommandLine(line))
	}
	a.toolOutput[itemID] = buffer
	return lines
}

func (a *codexLiveAdapter) flushToolOutput(itemID string) []string {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil
	}
	buffer := strings.TrimRight(strings.TrimSpace(a.toolOutput[itemID]), "\r")
	delete(a.toolOutput, itemID)
	if buffer == "" {
		return nil
	}
	return []string{codexSystemLocalCommandLine(buffer)}
}

func (a *codexLiveAdapter) emitSystemLine(key, line string) []string {
	key = strings.TrimSpace(key)
	line = strings.TrimSpace(line)
	if key == "" || line == "" {
		return nil
	}
	if previous, ok := a.lastSystemLine[key]; ok && previous == line {
		return nil
	}
	a.lastSystemLine[key] = line
	return []string{line}
}

func codexSystemLine(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	return marshalCompactJSON(map[string]any{
		"type":    "system",
		"content": content,
	})
}

func codexSystemLocalCommandLine(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	return marshalCompactJSON(map[string]any{
		"type":    "system",
		"subtype": "local_command",
		"content": "<local-command-stdout>" + content + "</local-command-stdout>",
	})
}

func codexNotificationDeltaText(params map[string]any) string {
	for _, value := range []any{
		params["delta"],
		params["output"],
		params["text"],
		params["content"],
		params["chunk"],
		params["stdout"],
		params["stderr"],
	} {
		if text := codexNotificationChunkText(value); text != "" {
			return text
		}
	}
	return ""
}

func codexNotificationTerminalInteractionText(params map[string]any) string {
	return strings.TrimSpace(coalesceCodexMessage(
		params["message"],
		params["text"],
		params["content"],
		params["prompt"],
		params["question"],
		params["details"],
		params["detail"],
		params["event"],
	))
}

func codexNotificationChunkText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		if strings.TrimSpace(typed) == "" {
			return ""
		}
		return typed
	case map[string]any:
		for _, nested := range []any{
			typed["delta"],
			typed["text"],
			typed["content"],
			typed["message"],
			typed["output"],
			typed["chunk"],
		} {
			if text := codexNotificationChunkText(nested); text != "" {
				return text
			}
		}
		return ""
	default:
		text := coalesceCodexMessage(typed)
		if strings.TrimSpace(text) == "" {
			return ""
		}
		return text
	}
}

func codexNotificationProgressText(params map[string]any) string {
	return strings.TrimSpace(coalesceCodexMessage(
		params["message"],
		params["progress"],
		params["status"],
		params["text"],
		params["content"],
		params["details"],
		params["detail"],
	))
}

func codexNotificationPlanText(params map[string]any) string {
	plan, _ := params["plan"].([]any)
	if len(plan) == 0 {
		if value := strings.TrimSpace(coalesceCodexMessage(params["explanation"], params["message"])); value != "" {
			return value
		}
		return ""
	}

	lines := make([]string, 0, len(plan)+1)
	if explanation := strings.TrimSpace(coalesceCodexMessage(params["explanation"])); explanation != "" {
		lines = append(lines, explanation)
	}
	for _, raw := range plan {
		switch typed := raw.(type) {
		case string:
			text := strings.TrimSpace(typed)
			if text != "" {
				lines = append(lines, "- "+text)
			}
		case map[string]any:
			step := strings.TrimSpace(firstNonEmpty(getString(typed, "step"), getString(typed, "title"), getString(typed, "text")))
			status := strings.TrimSpace(getString(typed, "status"))
			if step == "" {
				continue
			}
			if status != "" {
				lines = append(lines, fmt.Sprintf("- [%s] %s", status, step))
			} else {
				lines = append(lines, "- "+step)
			}
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func codexNotificationReasoningSummaryText(params map[string]any) string {
	summary := firstNonNil(
		params["part"],
		params["summary"],
		params["text"],
		params["content"],
		params["delta"],
	)
	text := strings.TrimSpace(coalesceCodexToolResult(summary))
	if text == "" {
		return ""
	}
	return "Reasoning: " + text
}

func codexThreadStatusThinking(paramsRaw json.RawMessage) bool {
	return codexThreadStatusType(paramsRaw) == "active"
}

func codexThreadStatusType(paramsRaw json.RawMessage) string {
	params := parseCodexParamsObject(paramsRaw)
	status, _ := params["status"].(map[string]any)
	return strings.TrimSpace(getString(status, "type"))
}

func codexTurnStatusFromParams(paramsRaw json.RawMessage) string {
	params := parseCodexParamsObject(paramsRaw)
	turn, _ := params["turn"].(map[string]any)
	return getString(turn, "status")
}

func codexThreadStatusMessage(paramsRaw json.RawMessage) string {
	params := parseCodexParamsObject(paramsRaw)
	status, _ := params["status"].(map[string]any)
	return coalesceCodexMessage(
		status["message"],
		status["error"],
		status["details"],
		status["detail"],
	)
}

func codexTaskCompletionMessage(paramsRaw json.RawMessage) string {
	params := parseCodexParamsObject(paramsRaw)
	msg, _ := params["msg"].(map[string]any)
	return coalesceCodexMessage(
		msg["error"],
		msg["message"],
		msg["details"],
		msg["detail"],
	)
}

func codexTurnErrorMessage(paramsRaw json.RawMessage) string {
	params := parseCodexParamsObject(paramsRaw)
	turn, _ := params["turn"].(map[string]any)
	return coalesceCodexMessage(
		turn["error"],
		turn["message"],
		turn["details"],
		turn["detail"],
	)
}

func codexNotificationErrorMessage(paramsRaw json.RawMessage) string {
	params := parseCodexParamsObject(paramsRaw)
	errorPayload, _ := params["error"].(map[string]any)
	return coalesceCodexMessage(
		errorPayload["message"],
		errorPayload["additionalDetails"],
		errorPayload["details"],
		errorPayload["detail"],
		errorPayload["codexErrorInfo"],
		params["message"],
		params["additionalDetails"],
	)
}

func codexNotificationErrorWillRetry(paramsRaw json.RawMessage) bool {
	params := parseCodexParamsObject(paramsRaw)
	switch typed := params["willRetry"].(type) {
	case bool:
		return typed
	default:
		return false
	}
}

func coalesceCodexMessage(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			trimmed := strings.TrimSpace(typed)
			if trimmed != "" {
				return trimmed
			}
		case map[string]any:
			if text := coalesceCodexMessage(
				typed["message"],
				typed["error"],
				typed["details"],
				typed["detail"],
				typed["cause"],
			); text != "" {
				return text
			}
			if raw := strings.TrimSpace(marshalCompactJSON(typed)); raw != "{}" && raw != "" {
				return raw
			}
		case []any:
			if raw := strings.TrimSpace(marshalCompactJSON(typed)); raw != "[]" && raw != "" {
				return raw
			}
		default:
			if value == nil {
				continue
			}
			text := strings.TrimSpace(fmt.Sprint(value))
			if text != "" && text != "<nil>" {
				return text
			}
		}
	}
	return ""
}

func codexNotificationThreadID(paramsRaw json.RawMessage) string {
	params := parseCodexParamsObject(paramsRaw)
	if params == nil {
		return ""
	}
	if threadID := strings.TrimSpace(getString(params, "threadId")); threadID != "" {
		return threadID
	}
	thread, _ := params["thread"].(map[string]any)
	return strings.TrimSpace(getString(thread, "id"))
}

func codexHistoryMessageKey(msg protocol.SessionHistoryMessage) string {
	if sourceID := strings.TrimSpace(msg.SourceID); sourceID != "" {
		return sourceID
	}
	return strings.TrimSpace(fmt.Sprintf("%s|%s|%s|%s|%d", msg.Role, msg.MessageType, msg.ToolCallID, msg.Content, msg.Timestamp))
}

func codexHistoryMessageDisplayLines(msg protocol.SessionHistoryMessage, includeUser bool) []string {
	if strings.TrimSpace(msg.MessageType) == "tool_call" {
		return codexToolCallDisplayLines(msg, true, msg.IsToolComplete)
	}

	content := strings.TrimSpace(msg.Content)
	if content == "" {
		return nil
	}

	role := strings.TrimSpace(msg.Role)
	switch role {
	case "user":
		if !includeUser {
			return nil
		}
		return []string{marshalCompactJSON(map[string]any{
			"type":      "user",
			"source_id": strings.TrimSpace(msg.SourceID),
			"message": map[string]any{
				"role":    "user",
				"content": content,
			},
		})}
	default:
		return []string{
			marshalCompactJSON(map[string]any{
				"type":          "content_block_start",
				"source_id":     strings.TrimSpace(msg.SourceID),
				"content_block": map[string]any{"type": "text", "text": ""},
			}),
			marshalCompactJSON(map[string]any{
				"type":  "content_block_delta",
				"delta": map[string]any{"type": "text_delta", "text": content},
			}),
			marshalCompactJSON(map[string]any{"type": "message_stop"}),
		}
	}
}

func codexToolCallDisplayLines(msg protocol.SessionHistoryMessage, includeStart, includeResult bool) []string {
	toolName := strings.TrimSpace(msg.ToolName)
	toolCallID := strings.TrimSpace(msg.ToolCallID)
	if toolCallID == "" {
		toolCallID = strings.TrimSpace(msg.SourceID)
	}
	if toolName == "" || toolCallID == "" {
		return nil
	}

	lines := make([]string, 0, 4)
	if includeStart {
		lines = append(lines, marshalCompactJSON(map[string]any{
			"type":          "content_block_start",
			"content_block": map[string]any{"type": "tool_use", "id": toolCallID, "name": toolName},
		}))
		if input := strings.TrimSpace(msg.ToolInput); input != "" {
			lines = append(lines, marshalCompactJSON(map[string]any{
				"type":  "content_block_delta",
				"delta": map[string]any{"type": "input_json_delta", "partial_json": input},
			}))
		}
		lines = append(lines, marshalCompactJSON(map[string]any{"type": "content_block_stop"}))
	}
	if includeResult {
		lines = append(lines, marshalCompactJSON(map[string]any{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": toolCallID,
					"content":     strings.TrimSpace(msg.ToolResult),
				}},
			},
		}))
	}
	return lines
}

func (s *Service) adoptCodexRuntimeThread(sessionID, threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ""
	}

	s.mu.Lock()
	current := strings.TrimSpace(s.runtimeSessionID)
	if current == threadID {
		s.mu.Unlock()
		return threadID
	}
	s.runtimeSessionID = threadID
	s.cfg.RuntimeSessionID = threadID
	s.mu.Unlock()

	applog.Info.Printf("[Remote] codex active thread adopted: session=%s old_thread=%s new_thread=%s", sessionID, current, threadID)
	if strings.TrimSpace(sessionID) != "" {
		if err := s.sendCurrentKeepalive(sessionID); err != nil {
			applog.Errorf("[Remote] codex adopt keepalive failed: session=%s thread=%s err=%v", sessionID, threadID, err)
		}
	}
	return threadID
}

func (s *Service) beginCodexObservedTurn(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codexObservedThreadID = strings.TrimSpace(threadID)
	s.codexObservedLiveOutput = false
}

func (s *Service) markCodexObservedLiveOutput() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.codexObservedThreadID) == "" {
		return
	}
	s.codexObservedLiveOutput = true
}

func (s *Service) finishCodexObservedTurn(threadID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	obervedThreadID := strings.TrimSpace(s.codexObservedThreadID)
	if obervedThreadID == "" {
		return ""
	}
	if strings.TrimSpace(threadID) == "" {
		threadID = obervedThreadID
	}

	shouldSync := strings.TrimSpace(threadID) == obervedThreadID && !s.codexObservedLiveOutput
	s.codexObservedThreadID = ""
	s.codexObservedLiveOutput = false
	if !shouldSync {
		return ""
	}
	return strings.TrimSpace(threadID)
}

func (s *Service) scheduleCodexThreadSnapshotSync(sessionID, threadID string) {
	sessionID = strings.TrimSpace(sessionID)
	threadID = strings.TrimSpace(threadID)
	if sessionID == "" || threadID == "" {
		return
	}

	s.mu.Lock()
	if !s.running || s.runtime != runtimeCodex || s.codexSnapshotSyncing {
		s.mu.Unlock()
		return
	}
	s.codexSnapshotSyncing = true
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			s.codexSnapshotSyncing = false
			s.mu.Unlock()
		}()

		if err := s.syncCodexThreadSnapshot(sessionID, threadID); err != nil && !isCodexThreadNotMaterialized(err) && !isCodexThreadUnavailable(err) {
			applog.Errorf("[Remote] codex snapshot sync failed: session=%s thread=%s err=%v", sessionID, threadID, err)
		}
	}()
}

func (s *Service) syncCodexThreadSnapshot(sessionID, threadID string) error {
	var readResult codexThreadReadResult
	if err := s.codexCall("thread/read", map[string]any{
		"threadId":     threadID,
		"includeTurns": true,
	}, &readResult); err != nil {
		return err
	}

	batch := buildCodexHistoryBatchFromThread(&readResult.Thread, s.rewriteCodexUserInput, s.rewriteToolInput)
	applyRuntimeSessionIDToHistoryBatch(batch, threadID)
	batch = s.filterUnsyncedCodexHistory(sessionID, batch)
	if len(batch) == 0 {
		return nil
	}

	for _, msg := range batch {
		for _, line := range codexHistoryMessageDisplayLines(msg, true) {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if err := s.EmitStructuredTextLine(line); err != nil {
				return err
			}
		}
	}

	historyPusher := newSessionHistoryPusher(s.cfg.ServerURL, s.cfg.Token, s.contentProtector)
	if err := historyPusher.pushBatchWithRuntime(sessionID, threadID, batch); err != nil {
		return err
	}
	s.markCodexHistorySynced(sessionID, batch)
	applog.Info.Printf("[Remote] codex snapshot synced: session=%s thread=%s count=%d", sessionID, threadID, len(batch))
	return nil
}

func (s *Service) filterUnsyncedCodexHistory(sessionID string, batch []protocol.SessionHistoryMessage) []protocol.SessionHistoryMessage {
	if len(batch) == 0 {
		return nil
	}

	sessionID = strings.TrimSpace(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.codexSyncedHistory == nil {
		s.codexSyncedHistory = make(map[string]map[string]struct{})
	}
	syncedForSession := s.codexSyncedHistory[sessionID]

	filtered := make([]protocol.SessionHistoryMessage, 0, len(batch))
	for _, msg := range batch {
		key := codexHistoryMessageKey(msg)
		if key == "" {
			filtered = append(filtered, msg)
			continue
		}
		if syncedForSession != nil {
			if _, exists := syncedForSession[key]; exists {
				continue
			}
		}
		filtered = append(filtered, msg)
	}
	return filtered
}

func (s *Service) markCodexHistorySynced(sessionID string, batch []protocol.SessionHistoryMessage) {
	if len(batch) == 0 {
		return
	}

	sessionID = strings.TrimSpace(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.codexSyncedHistory == nil {
		s.codexSyncedHistory = make(map[string]map[string]struct{})
	}
	if s.codexSyncedHistory[sessionID] == nil {
		s.codexSyncedHistory[sessionID] = make(map[string]struct{})
	}
	for _, msg := range batch {
		key := codexHistoryMessageKey(msg)
		if key == "" {
			continue
		}
		s.codexSyncedHistory[sessionID][key] = struct{}{}
	}
}

func (s *Service) registerCodexRPC() (string, chan codexRPCResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rpcSeq++
	requestID := fmt.Sprintf("codex-%d", s.rpcSeq)
	if s.pendingRPC == nil {
		s.pendingRPC = make(map[string]chan codexRPCResponse)
	}
	responseCh := make(chan codexRPCResponse, 1)
	s.pendingRPC[requestID] = responseCh
	return requestID, responseCh
}

func (s *Service) clearCodexRPC(requestID string) {
	s.mu.Lock()
	responseCh := s.pendingRPC[requestID]
	delete(s.pendingRPC, requestID)
	s.mu.Unlock()
	if responseCh != nil {
		close(responseCh)
	}
}

func (s *Service) resolveCodexRPCResponse(envelope codexRPCEnvelope) {
	requestID := envelope.idString()
	if requestID == "" {
		return
	}

	s.mu.Lock()
	responseCh := s.pendingRPC[requestID]
	if responseCh != nil {
		delete(s.pendingRPC, requestID)
	}
	s.mu.Unlock()
	if responseCh == nil {
		return
	}

	responseCh <- codexRPCResponse{
		Result: envelope.Result,
		Error:  envelope.Error,
	}
	close(responseCh)
}

func (s *Service) codexCall(method string, params any, out any) error {
	requestID, responseCh := s.registerCodexRPC()

	s.mu.Lock()
	stdin := s.stdin
	s.mu.Unlock()
	if stdin == nil {
		s.clearCodexRPC(requestID)
		return fmt.Errorf("stdin is not available")
	}

	if err := s.writeJSONLineTo(stdin, map[string]any{
		"id":     requestID,
		"method": method,
		"params": params,
	}); err != nil {
		s.clearCodexRPC(requestID)
		return err
	}

	select {
	case response, ok := <-responseCh:
		if !ok {
			return fmt.Errorf("codex request cancelled")
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
		s.clearCodexRPC(requestID)
		return fmt.Errorf("%s timed out", method)
	}
}

func (s *Service) startCodexTurn(content string) error {
	s.mu.Lock()
	pendingRebind := s.pendingCodexThreadRebind
	s.mu.Unlock()
	if pendingRebind {
		if _, err := s.rebindCodexThreadConfiguration(); err != nil {
			return fmt.Errorf("codex execution mode rebind failed: %w", err)
		}
	}

	threadID := s.getCodexThreadID()
	if threadID == "" {
		if err := s.startFallbackCodexThread(); err != nil {
			return fmt.Errorf("codex thread is not available: %w", err)
		}
		threadID = s.getCodexThreadID()
		if threadID == "" {
			return fmt.Errorf("codex thread is not available")
		}
	}

	params := map[string]any{
		"threadId": threadID,
		"input": []map[string]any{{
			"type": "text",
			"text": content,
		}},
		"approvalPolicy": codexApprovalPolicy(s.getCurrentPermissionMode()),
		"sandboxPolicy":  codexSandboxPolicy(s.getCurrentDir(), s.getCurrentSandboxMode()),
		"cwd":            s.getCurrentDir(),
	}
	if effort := strings.TrimSpace(s.getCurrentReasoningEffort()); effort != "" {
		params["effort"] = effort
	}
	applog.Info.Printf(
		"[Remote] codex turn/start request: session=%s thread=%s chars=%d cwd=%s approval=%s sandbox=%s",
		s.SessionID(),
		threadID,
		len(strings.TrimSpace(content)),
		s.getCurrentDir(),
		codexApprovalPolicy(s.getCurrentPermissionMode()),
		debugPreviewString(fmt.Sprintf("%v", codexSandboxPolicy(s.getCurrentDir(), s.getCurrentSandboxMode())), 240),
	)

	var result codexTurnStartResult
	if err := s.codexCall("turn/start", params, &result); err != nil {
		if !isCodexThreadUnavailable(err) {
			return err
		}
		if fallbackErr := s.startFallbackCodexThread(); fallbackErr != nil {
			return fmt.Errorf("%w; fallback failed: %v", err, fallbackErr)
		}
		threadID = s.getCodexThreadID()
		params["threadId"] = threadID
		if retryErr := s.codexCall("turn/start", params, &result); retryErr != nil {
			return retryErr
		}
	}
	s.setCodexTurnID(strings.TrimSpace(result.Turn.ID))
	s.syncCodexForwardedInputHistory(threadID, strings.TrimSpace(result.Turn.ID), content)
	applog.Info.Printf("[Remote] codex turn/start ok: session=%s thread=%s turn=%s", s.SessionID(), threadID, strings.TrimSpace(result.Turn.ID))
	return nil
}

func (s *Service) syncCodexForwardedInputHistory(threadID, turnID, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	sourceID := strings.TrimSpace(turnID)
	if sourceID == "" {
		sourceID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	messageType := "text"
	if strings.HasPrefix(content, "/") {
		messageType = "command_input"
	}
	msg := protocol.SessionHistoryMessage{
		SourceID:         "client-input:" + sourceID,
		SourceKind:       "client-input",
		Role:             "user",
		Content:          content,
		MessageType:      messageType,
		RuntimeSessionID: strings.TrimSpace(threadID),
		Timestamp:        time.Now().UnixMilli(),
	}
	historyPusher := newSessionHistoryPusher(s.cfg.ServerURL, s.cfg.Token, s.contentProtector)
	if err := historyPusher.pushBatchWithRuntime(s.SessionID(), strings.TrimSpace(threadID), []protocol.SessionHistoryMessage{msg}); err != nil {
		applog.Errorf("[Remote] codex forwarded input history sync failed: session=%s thread=%s err=%v", s.SessionID(), threadID, err)
	}
}

func (s *Service) startFallbackCodexThread() error {
	params := map[string]any{
		"cwd":            s.getCurrentDir(),
		"approvalPolicy": codexApprovalPolicy(s.getCurrentPermissionMode()),
		"sandbox":        s.getCurrentSandboxMode(),
		"personality":    "pragmatic",
	}
	if model := strings.TrimSpace(s.getCurrentModel()); model != "" {
		params["model"] = model
	}

	var result codexThreadStartResult
	if err := s.codexCall("thread/start", params, &result); err != nil {
		return err
	}

	threadID := strings.TrimSpace(result.Thread.ID)
	if threadID == "" {
		return fmt.Errorf("codex returned empty thread id")
	}

	s.mu.Lock()
	s.runtimeSessionID = threadID
	if model := strings.TrimSpace(result.Model); model != "" {
		s.currentModel = model
		s.cfg.Model = model
	}
	cwd := strings.TrimSpace(result.Cwd)
	if cwd == "" {
		cwd = strings.TrimSpace(result.Thread.Cwd)
	}
	if cwd != "" {
		s.currentDir = cwd
		s.cfg.WorkingDir = cwd
	}
	s.pendingCodexThreadRebind = false
	s.mu.Unlock()

	if relaySessionID := s.SessionID(); strings.TrimSpace(relaySessionID) != "" {
		if err := s.sendCurrentKeepalive(relaySessionID); err != nil {
			applog.Errorf("[Remote] codex fallback keepalive failed: session=%s err=%v", relaySessionID, err)
		}
	}
	return nil
}

func (s *Service) rebindCodexThreadConfiguration() (bool, error) {
	if s.getRuntime() != runtimeCodex {
		return true, nil
	}
	s.mu.Lock()
	s.pendingCodexThreadRebind = true
	s.mu.Unlock()

	if s.getThinking() || strings.TrimSpace(s.getCodexTurnID()) != "" {
		applog.Info.Printf(
			"[Remote] codex thread rebind deferred: session=%s thinking=%t turn=%s thread=%s",
			s.SessionID(),
			s.getThinking(),
			strings.TrimSpace(s.getCodexTurnID()),
			strings.TrimSpace(s.getCodexThreadID()),
		)
		return false, nil
	}

	threadID := strings.TrimSpace(s.getCodexThreadID())
	if threadID == "" {
		applog.Info.Printf("[Remote] codex thread rebind deferred: session=%s reason=empty-thread", s.SessionID())
		return false, nil
	}

	params := map[string]any{
		"threadId":               threadID,
		"cwd":                    s.getCurrentDir(),
		"approvalPolicy":         codexApprovalPolicy(s.getCurrentPermissionMode()),
		"sandbox":                normalizeSandboxModeForRuntime(runtimeCodex, s.getCurrentSandboxMode()),
		"persistExtendedHistory": true,
	}
	if model := strings.TrimSpace(s.getCurrentModel()); model != "" {
		params["model"] = model
	}

	var result codexThreadStartResult
	if err := s.codexCall("thread/fork", params, &result); err != nil {
		if isCodexThreadUnavailable(err) || isCodexThreadNotMaterialized(err) {
			applog.Info.Printf(
				"[Remote] codex thread/fork unavailable, keep local execution mode until next turn: session=%s thread=%s approval=%s sandbox=%s err=%v",
				s.SessionID(),
				threadID,
				codexApprovalPolicy(s.getCurrentPermissionMode()),
				normalizeSandboxModeForRuntime(runtimeCodex, s.getCurrentSandboxMode()),
				err,
			)
			return false, nil
		}
		return false, err
	}

	nextThreadID := strings.TrimSpace(result.Thread.ID)
	if nextThreadID == "" {
		return false, fmt.Errorf("codex thread/fork returned empty thread id")
	}

	s.mu.Lock()
	s.runtimeSessionID = nextThreadID
	s.cfg.RuntimeSessionID = nextThreadID
	if model := strings.TrimSpace(result.Model); model != "" {
		s.currentModel = model
		s.cfg.Model = model
	}
	cwd := strings.TrimSpace(result.Cwd)
	if cwd == "" {
		cwd = strings.TrimSpace(result.Thread.Cwd)
	}
	if cwd != "" {
		s.currentDir = cwd
		s.cfg.WorkingDir = cwd
	}
	s.pendingCodexThreadRebind = false
	s.mu.Unlock()

	applog.Info.Printf(
		"[Remote] codex thread rebound: session=%s old_thread=%s new_thread=%s approval=%s sandbox=%s",
		s.SessionID(),
		threadID,
		nextThreadID,
		codexApprovalPolicy(s.getCurrentPermissionMode()),
		normalizeSandboxModeForRuntime(runtimeCodex, s.getCurrentSandboxMode()),
	)

	if relaySessionID := s.SessionID(); strings.TrimSpace(relaySessionID) != "" {
		if err := s.sendCurrentKeepalive(relaySessionID); err != nil {
			applog.Errorf("[Remote] codex rebound keepalive failed: session=%s err=%v", relaySessionID, err)
		}
	}
	return true, nil
}

func (s *Service) requestCodexInterrupt() error {
	threadID := s.getCodexThreadID()
	turnID := s.getCodexTurnID()
	if threadID == "" || turnID == "" {
		return fmt.Errorf("no active codex turn to interrupt")
	}
	return s.codexCall("turn/interrupt", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	}, nil)
}

func (s *Service) getCodexThreadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.runtimeSessionID) != "" {
		return s.runtimeSessionID
	}
	return s.sessionID
}

func (s *Service) getCodexTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codexTurnID
}

func (s *Service) setCodexTurnID(turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codexTurnID = turnID
}

func (s *Service) finishCodexTurn(status string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.codexTurnID = ""
	s.thinking = false
	if !s.turnActive {
		s.interruptRequested = false
		return "", false
	}

	s.turnActive = false
	defer func() {
		s.interruptRequested = false
	}()

	if s.interruptRequested {
		return protocol.TurnCancelled, true
	}

	switch strings.TrimSpace(status) {
	case "", "completed":
		return protocol.TurnCompleted, true
	case "cancelled":
		return protocol.TurnCancelled, true
	default:
		return protocol.TurnFailed, true
	}
}

func debugPreviewString(value string, limit int) string {
	normalized := strings.ReplaceAll(value, "\n", "\\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\\r")
	if limit > 0 && len(normalized) > limit {
		return normalized[:limit] + "..."
	}
	return normalized
}

func (s *Service) handleCodexServerRequest(cmd *exec.Cmd, stdin io.WriteCloser, sessionID string, envelope codexRPCEnvelope) {
	applog.Info.Printf(
		"[Remote] codex server request: session=%s request=%s method=%s payload=%s",
		sessionID,
		envelope.idString(),
		envelope.Method,
		debugPreviewString(string(envelope.Params), 600),
	)
	switch envelope.Method {
	case "item/commandExecution/requestApproval":
		params := parseCodexParamsObject(envelope.Params)
		input := map[string]any{
			"command":          getString(params, "command"),
			"cwd":              getString(params, "cwd"),
			"command_actions":  params["commandActions"],
			"approval_context": params["networkApprovalContext"],
			"reason":           getString(params, "reason"),
		}
		s.handleCodexPermissionRequest(cmd, stdin, sessionID, envelope.idString(), envelope.idValue(), "Bash", input, summarizePermissionRequest("Bash", input), func(resp protocol.PermissionResponsePayload) any {
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
		})
	case "item/fileChange/requestApproval":
		params := parseCodexParamsObject(envelope.Params)
		input := map[string]any{
			"file_path":  getString(params, "grantRoot"),
			"grant_root": getString(params, "grantRoot"),
			"reason":     getString(params, "reason"),
		}
		s.handleCodexPermissionRequest(cmd, stdin, sessionID, envelope.idString(), envelope.idValue(), "Write", input, summarizePermissionRequest("Write", input), func(resp protocol.PermissionResponsePayload) any {
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
		})
	case "item/permissions/requestApproval":
		params := parseCodexParamsObject(envelope.Params)
		input := map[string]any{
			"permissions": params["permissions"],
			"reason":      getString(params, "reason"),
		}
		s.handleCodexPermissionRequest(cmd, stdin, sessionID, envelope.idString(), envelope.idValue(), "Permissions", input, summarizePermissionRequest("Permissions", input), func(resp protocol.PermissionResponsePayload) any {
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
		})
	case "item/tool/requestUserInput":
		params := parseCodexParamsObject(envelope.Params)
		s.handleCodexPermissionRequest(cmd, stdin, sessionID, envelope.idString(), envelope.idValue(), "RequestUserInput", params, summarizePermissionRequest("RequestUserInput", params), func(resp protocol.PermissionResponsePayload) any {
			return buildCodexUserInputResult(params, resp.UpdatedInput)
		})
	case "item/tool/call":
		params := parseCodexParamsObject(envelope.Params)
		toolName := strings.TrimSpace(getString(params, "tool"))
		arguments, _ := params["arguments"].(map[string]any)
		if toolName == "" || !isCodexInteractivePayload(arguments) {
			_ = s.writeJSONLineTo(stdin, map[string]any{
				"id": envelope.idValue(),
				"result": map[string]any{
					"success": false,
					"contentItems": []map[string]any{{
						"type": "inputText",
						"text": "Client dynamic tool calls are not supported yet.",
					}},
				},
			})
			return
		}
		s.handleCodexPermissionRequest(
			cmd,
			stdin,
			sessionID,
			envelope.idString(),
			envelope.idValue(),
			toolName,
			arguments,
			summarizeCodexInteractiveRequest(toolName, arguments),
			func(resp protocol.PermissionResponsePayload) any {
				return buildCodexDynamicToolResult(toolName, arguments, resp)
			},
		)
	case "mcpServer/elicitation/request":
		params := parseCodexParamsObject(envelope.Params)
		s.handleCodexPermissionRequest(
			cmd,
			stdin,
			sessionID,
			envelope.idString(),
			envelope.idValue(),
			"McpElicitation",
			params,
			summarizeCodexInteractiveRequest("McpElicitation", params),
			func(resp protocol.PermissionResponsePayload) any {
				return buildCodexMcpElicitationResult(params, resp)
			},
		)
	case "execCommandApproval":
		params := parseCodexParamsObject(envelope.Params)
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
		s.handleCodexPermissionRequest(
			cmd,
			stdin,
			sessionID,
			envelope.idString(),
			envelope.idValue(),
			"Bash",
			input,
			summarizePermissionRequest("Bash", input),
			buildCodexExecCommandApprovalResult,
		)
	case "applyPatchApproval":
		params := parseCodexParamsObject(envelope.Params)
		input := map[string]any{
			"grant_root": getString(params, "grantRoot"),
			"file_path":  getString(params, "grantRoot"),
			"changes":    params["changes"],
			"reason":     getString(params, "reason"),
		}
		s.handleCodexPermissionRequest(
			cmd,
			stdin,
			sessionID,
			envelope.idString(),
			envelope.idValue(),
			"Write",
			input,
			summarizePermissionRequest("Write", input),
			buildCodexApplyPatchApprovalResult,
		)
	}
}

func (s *Service) handleCodexPermissionRequest(
	cmd *exec.Cmd,
	stdin io.WriteCloser,
	sessionID string,
	requestID string,
	requestIDValue any,
	toolName string,
	input map[string]any,
	summary string,
	buildResult func(protocol.PermissionResponsePayload) any,
) {
	if s.shouldAutoApproveTool(toolName, input) {
		resp := protocol.PermissionResponsePayload{
			RequestID: requestID,
			Approved:  true,
			Decision:  protocol.PermissionDecisionApproved,
		}
		resultPayload := buildResult(resp)
		if err := s.writeJSONLineTo(stdin, map[string]any{"id": requestIDValue, "result": resultPayload}); err != nil && s.isCurrentCommand(cmd) {
			applog.Errorf("[Remote] codex auto-approve failed: %v", err)
		} else {
			applog.Info.Printf(
				"[Remote] codex auto-approve sent: session=%s request=%s tool=%s payload=%s",
				sessionID,
				requestID,
				toolName,
				debugPreviewString(marshalCompactJSON(resultPayload), 320),
			)
		}
		return
	}

	responseCh := s.registerPendingPermission(sessionID, requestID)
	msg := protocol.Message{
		Type:      protocol.TypePermissionRequest,
		SessionID: sessionID,
		Payload: protocol.PermissionRequestPayload{
			RequestID: requestID,
			Tool:      toolName,
			Input:     input,
			Summary:   summary,
			CreatedAt: time.Now().UnixMilli(),
		},
	}
	applog.Info.Printf(
		"[Remote] codex approval requested: session=%s request=%s tool=%s summary=%s",
		sessionID,
		requestID,
		toolName,
		debugPreviewString(summary, 240),
	)
	if err := s.writeJSON(msg); err != nil {
		s.cancelPendingPermission(requestID)
		if s.isCurrentCommand(cmd) {
			applog.Errorf("[Remote] forward codex permission request failed: %v", err)
		}
		return
	}
	if s.setCodexPermissionWaiting(true) {
		if err := s.sendCurrentKeepalive(sessionID); err != nil {
			applog.Errorf("[Remote] codex approval-wait keepalive failed: session=%s err=%v", sessionID, err)
		}
	}

	resp, ok := <-responseCh
	if !ok {
		return
	}
	if resp.Decision == protocol.PermissionDecisionApprovedForSession {
		s.recordSessionPermission(toolName, input)
	}
	if resp.Approved || resp.Decision == protocol.PermissionDecisionApproved || resp.Decision == protocol.PermissionDecisionApprovedForSession {
		s.rememberUpdatedToolInput(toolName, input, resp.UpdatedInput)
	}
	resultPayload := buildResult(resp)
	if err := s.writeJSONLineTo(stdin, map[string]any{
		"id":     requestIDValue,
		"result": resultPayload,
	}); err != nil && s.isCurrentCommand(cmd) {
		applog.Errorf("[Remote] codex permission response write failed: %v", err)
		return
	}
	applog.Info.Printf(
		"[Remote] codex approval response sent: session=%s request=%s tool=%s decision=%s approved=%t payload=%s",
		sessionID,
		requestID,
		toolName,
		resp.Decision,
		resp.Approved,
		debugPreviewString(marshalCompactJSON(resultPayload), 320),
	)
}

func buildCodexUserInputResult(request map[string]any, updatedInput map[string]any) map[string]any {
	answers := map[string]any{}
	questionValues, _ := request["questions"].([]any)
	answerMap, _ := updatedInput["answers"].(map[string]any)
	for index, rawQuestion := range questionValues {
		question, ok := rawQuestion.(map[string]any)
		if !ok {
			continue
		}
		questionID := strings.TrimSpace(getString(question, "id"))
		if questionID == "" {
			continue
		}
		answerKey := fmt.Sprintf("%d", index)
		labels := normalizeCodexAnswerValues(answerMap[answerKey])
		if len(labels) == 0 && index == 0 {
			labels = normalizeCodexAnswerValues(updatedInput["answer"])
		}
		if len(labels) == 0 {
			continue
		}
		answers[questionID] = map[string]any{
			"answers": labels,
		}
	}
	return map[string]any{"answers": answers}
}

func normalizeCodexAnswerValues(value any) []string {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		return []string{trimmed}
	case []any:
		result := make([]string, 0, len(typed))
		for _, entry := range typed {
			text := strings.TrimSpace(fmt.Sprint(entry))
			if text != "" {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func summarizeCodexInteractiveRequest(toolName string, input map[string]any) string {
	if message := strings.TrimSpace(getString(input, "message")); message != "" {
		if url := strings.TrimSpace(getString(input, "url")); url != "" {
			return message + "\n" + url
		}
		return message
	}
	if question := strings.TrimSpace(getString(input, "question")); question != "" {
		return question
	}
	return summarizePermissionRequest(toolName, input)
}

func isCodexInteractivePayload(input map[string]any) bool {
	if len(input) == 0 {
		return false
	}
	if _, ok := input["questions"]; ok {
		return true
	}
	if _, ok := input["question"]; ok {
		return true
	}
	if _, ok := input["options"]; ok {
		return true
	}
	if _, ok := input["requestedSchema"]; ok {
		return true
	}
	if mode := strings.TrimSpace(getString(input, "mode")); mode == "form" || mode == "url" {
		return true
	}
	return false
}

func buildCodexDynamicToolResult(
	toolName string,
	arguments map[string]any,
	resp protocol.PermissionResponsePayload,
) map[string]any {
	if resp.Decision == protocol.PermissionDecisionDenied || resp.Decision == protocol.PermissionDecisionAbort {
		return map[string]any{
			"success": false,
			"contentItems": []map[string]any{{
				"type": "inputText",
				"text": "User declined " + toolName + ".",
			}},
		}
	}

	text := codexInteractionResultText(resp.UpdatedInput)
	if text == "" {
		text = marshalCompactJSON(arguments)
	}
	return map[string]any{
		"success": true,
		"contentItems": []map[string]any{{
			"type": "inputText",
			"text": text,
		}},
	}
}

func buildCodexMcpElicitationResult(
	_ map[string]any,
	resp protocol.PermissionResponsePayload,
) map[string]any {
	action := "accept"
	switch resp.Decision {
	case protocol.PermissionDecisionDenied:
		action = "decline"
	case protocol.PermissionDecisionAbort:
		action = "cancel"
	}

	result := map[string]any{
		"action": action,
	}
	if action != "accept" {
		return result
	}

	if content := codexMcpElicitationContent(resp.UpdatedInput); content != nil {
		result["content"] = content
	}
	return result
}

func buildCodexExecCommandApprovalResult(resp protocol.PermissionResponsePayload) any {
	return map[string]any{
		"decision": codexSimpleApprovalDecision(resp.Decision),
	}
}

func buildCodexApplyPatchApprovalResult(resp protocol.PermissionResponsePayload) any {
	return map[string]any{
		"decision": codexSimpleApprovalDecision(resp.Decision),
	}
}

func codexSimpleApprovalDecision(decision string) string {
	switch decision {
	case protocol.PermissionDecisionDenied:
		return "decline"
	case protocol.PermissionDecisionAbort:
		return "cancel"
	default:
		return "accept"
	}
}

func codexMcpElicitationContent(updatedInput map[string]any) any {
	if updatedInput == nil {
		return nil
	}
	if content, ok := updatedInput["content"]; ok {
		return content
	}
	if answers, ok := updatedInput["answers"]; ok {
		return answers
	}
	return nil
}

func codexInteractionResultText(updatedInput map[string]any) string {
	if updatedInput == nil {
		return ""
	}
	if answer := strings.TrimSpace(fmt.Sprint(updatedInput["answer"])); answer != "" && answer != "<nil>" {
		return answer
	}
	if answers, ok := updatedInput["answers"]; ok {
		switch typed := answers.(type) {
		case map[string]any:
			return marshalCompactJSON(typed)
		case []any:
			return marshalCompactJSON(typed)
		default:
			if text := strings.TrimSpace(fmt.Sprint(typed)); text != "" && text != "<nil>" {
				return text
			}
		}
	}
	if content, ok := updatedInput["content"]; ok {
		switch typed := content.(type) {
		case string:
			return strings.TrimSpace(typed)
		default:
			return marshalCompactJSON(typed)
		}
	}
	return marshalCompactJSON(updatedInput)
}
