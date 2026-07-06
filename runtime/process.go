package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

func (s *Service) startCommand(sessionID, workingDir, model, permissionMode string, resume bool) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	adapter := s.getRuntimeAdapter()
	if adapter == nil {
		return nil, nil, nil, fmt.Errorf("runtime adapter is not available")
	}
	return adapter.StartCommand(s, sessionID, workingDir, model, permissionMode, resume)
}

func (s *Service) startClaudeCommand(sessionID, workingDir, model, permissionMode string, resume bool) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	s.mu.Lock()
	s.codexAppServerURL = ""
	s.mu.Unlock()

	args := append([]string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--permission-prompt-tool", "stdio",
		"--verbose",
	}, s.cfg.Args...)
	if model != "" {
		args = append(args, "--model", model)
	}
	normalizedPermissionMode := normalizePermissionMode(permissionMode)
	if normalizedPermissionMode != "" {
		args = append(args, "--permission-mode", normalizedPermissionMode)
		if normalizedPermissionMode == protocol.PermissionModeBypassPermissions {
			args = append(args, "--allow-dangerously-skip-permissions")
		}
	}
	if resume {
		if sessionID != "" {
			args = append(args, "--resume", sessionID)
		} else {
			args = append(args, "--continue")
		}
	} else if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}

	cmd := exec.Command(s.cfg.Command, args...)
	hideBackgroundConsole(cmd)
	cmd.Env = os.Environ()
	if workingDir != "" {
		cmd.Dir = workingDir
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	s.startCommandStderrLogger(cmd, stderrPipe, "Claude")
	return cmd, stdinPipe, stdoutPipe, nil
}

func (s *Service) startCodexCommand(workingDir string) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	wsURL, err := allocateCodexWSURL()
	if err != nil {
		return nil, nil, nil, err
	}

	args := append(append([]string{}, s.cfg.Args...), "app-server", "--listen", wsURL)

	cmd := exec.Command(s.cfg.Command, args...)
	hideBackgroundConsole(cmd)
	cmd.Env = os.Environ()
	if workingDir != "" {
		cmd.Dir = workingDir
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	s.startCommandStderrLogger(cmd, stderrPipe, "Codex")

	conn, err := dialCodexWS(wsURL)
	if err != nil {
		s.mu.Lock()
		s.codexAppServerURL = ""
		s.mu.Unlock()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, nil, nil, err
	}
	s.mu.Lock()
	s.codexAppServerURL = strings.TrimSpace(wsURL)
	s.mu.Unlock()
	stdinPipe, stdoutPipe := s.startCodexWSBridge(cmd, conn)
	return cmd, stdinPipe, stdoutPipe, nil
}

func (s *Service) startCommandStderrLogger(cmd *exec.Cmd, stderr io.ReadCloser, label string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer stderr.Close()

		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 1024*1024), 4*1024*1024)
		for scanner.Scan() {
			if !s.isCurrentCommand(cmd) {
				continue
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			applog.Errorf(
				"[Remote] %s stderr: pid=%d %s",
				label,
				processPID(cmd),
				debugPreviewString(line, 1200),
			)
		}
		if err := scanner.Err(); err != nil && s.isCurrentCommand(cmd) {
			applog.Errorf("[Remote] %s stderr read error: pid=%d err=%v", label, processPID(cmd), err)
		}
	}()
}

func (s *Service) startProcessBridge(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, sessionID string) {
	adapter := s.getRuntimeAdapter()
	if adapter == nil {
		applog.Errorf("[Remote] runtime adapter unavailable for process bridge")
		return
	}
	adapter.StartProcessBridge(s, cmd, stdin, stdout, s.stdoutReader, sessionID)
}

func (s *Service) startClaudeProcessBridge(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, stdoutReader *bufio.Reader, sessionID string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		reader := stdoutReader
		if reader == nil {
			reader = bufio.NewReader(stdout)
		}
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		historySyncer := newStreamHistorySyncer(sessionID, s.cfg.ServerURL, s.cfg.Token, s.contentProtector, s.rewriteToolInput)
		defer func() {
			if err := historySyncer.Flush(); err != nil {
				applog.Errorf("[Remote] stream history final flush error: session=%s err=%v", sessionID, err)
			}
		}()
		for scanner.Scan() {
			if !s.isCurrentCommand(cmd) {
				continue
			}

			line := scanner.Text()
			if line == "" {
				continue
			}

			var event map[string]interface{}
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				continue
			}

			historySyncer.HandleLine(line)

			eventType, _ := event["type"].(string)
			switch eventType {
			case "control_request":
				var req sdkControlRequest
				if err := json.Unmarshal([]byte(line), &req); err == nil && req.Request.Subtype == "can_use_tool" {
					go s.handlePermissionControlRequest(cmd, stdin, sessionID, req)
				}
				continue
			case "control_response":
				var resp sdkControlResponse
				if err := json.Unmarshal([]byte(line), &resp); err == nil {
					s.handleControlResponse(resp)
				}
				continue
			case "control_cancel_request":
				var req sdkControlCancelRequest
				if err := json.Unmarshal([]byte(line), &req); err == nil {
					s.cancelPendingPermission(req.RequestID)
				}
				continue
			case "system", "assistant":
				s.setThinking(true)
			case "result":
				if err := historySyncer.Flush(); err != nil {
					applog.Errorf("[Remote] stream history flush error: session=%s err=%v", sessionID, err)
				}
				status, shouldEmit := s.finishTurnFromResult(event)
				if shouldEmit {
					if err := s.sendTurnEnd(sessionID, status); err != nil {
						applog.Errorf("[Remote] WS write turn-end error: %v", err)
					}
				}
				continue
			case "message_stop":
				if err := historySyncer.Flush(); err != nil {
					applog.Errorf("[Remote] stream history flush error: session=%s err=%v", sessionID, err)
				}
			}

			msg := protocol.Message{
				Type:      protocol.TypeText,
				SessionID: sessionID,
				Payload: protocol.TextPayload{
					Data:     line,
					Thinking: s.getThinking(),
				},
			}
			if writeErr := s.writeJSON(msg); writeErr != nil {
				applog.Errorf("[Remote] WS write error: %v", writeErr)
				return
			}
		}
		if err := scanner.Err(); err != nil && s.isCurrentCommand(cmd) {
			applog.Errorf("[Remote] stdout read error: %v", err)
		}
	}()
	s.startProcessWaiter(cmd, sessionID, "Process exited")
}

func (s *Service) writeUserMessage(content string) error {
	adapter := s.getRuntimeAdapter()
	if adapter == nil {
		return fmt.Errorf("runtime adapter is not available")
	}
	return adapter.SendUserInput(s, content)
}

func (s *Service) writeClaudeUserMessage(content string) error {
	userMsg := map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role":    "user",
			"content": content,
		},
	}
	jsonLine, _ := json.Marshal(userMsg)
	jsonLine = append(jsonLine, '\n')

	s.mu.Lock()
	stdin := s.stdin
	s.mu.Unlock()
	if stdin == nil {
		return fmt.Errorf("stdin is not available")
	}

	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	_, err := stdin.Write(jsonLine)
	return err
}

func (s *Service) restartCommand(sessionID, workingDir, model string, applyModel bool, permissionMode, sandboxMode string, resume bool) error {
	targetDir := workingDir
	if strings.TrimSpace(targetDir) != "" {
		resolvedTargetDir, err := resolveDirectoryWithinUserHome(targetDir, s.getCurrentDir(), false)
		if err != nil {
			return err
		}
		targetDir = resolvedTargetDir
	} else {
		targetDir = s.getCurrentDir()
		if targetDir == "" {
			cwd, _ := os.Getwd()
			targetDir = cwd
		}
		if !filepath.IsAbs(targetDir) {
			targetDir = filepath.Join(s.getCurrentDir(), targetDir)
		}
		targetDir = filepath.Clean(targetDir)
	}

	info, err := os.Stat(targetDir)
	if err != nil {
		return fmt.Errorf("invalid working dir %s: %w", targetDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working dir is not a directory: %s", targetDir)
	}

	targetModel := model
	if !applyModel {
		targetModel = s.getCurrentModel()
	}
	targetPermissionMode := normalizePermissionModeForRuntime(s.getRuntime(), permissionMode)
	if targetPermissionMode == "" {
		targetPermissionMode = s.getCurrentPermissionMode()
	}
	targetSandboxMode := normalizeSandboxModeForRuntime(s.getRuntime(), sandboxMode)
	if targetSandboxMode == "" {
		targetSandboxMode = s.getCurrentSandboxMode()
	}

	adapter := s.getRuntimeAdapter()
	if adapter == nil {
		return fmt.Errorf("runtime adapter is not available")
	}
	s.mu.Lock()
	resumeRuntimeSessionID := s.runtimeSessionID
	if strings.TrimSpace(resumeRuntimeSessionID) == "" {
		resumeRuntimeSessionID = s.sessionID
	}
	s.mu.Unlock()

	cmd, stdin, stdout, err := s.startCommand(sessionID, targetDir, targetModel, targetPermissionMode, resume)
	if err != nil {
		return err
	}
	stdoutReader := bufio.NewReader(stdout)
	initialHistoryBatch := []protocol.SessionHistoryMessage(nil)
	targetRuntimeSessionID := resumeRuntimeSessionID
	bootstrap, bootstrapErr := adapter.BootstrapSession(
		s,
		stdin,
		stdoutReader,
		sessionID,
		resumeRuntimeSessionID,
		targetDir,
		targetModel,
		targetPermissionMode,
		targetSandboxMode,
		resume || strings.TrimSpace(sessionID) != "",
	)
	if bootstrapErr != nil {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return bootstrapErr
	}
	if bootstrap.Model != "" {
		targetModel = bootstrap.Model
	}
	if bootstrap.RuntimeSessionID != "" {
		targetRuntimeSessionID = bootstrap.RuntimeSessionID
	}
	if bootstrap.WorkingDir != "" {
		targetDir = bootstrap.WorkingDir
	}
	initialHistoryBatch = bootstrap.HistoryBatch

	s.resetPermissionState()

	s.mu.Lock()
	oldCmd := s.cmd
	oldStdin := s.stdin
	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout
	s.stdoutReader = stdoutReader
	s.runtimeSessionID = targetRuntimeSessionID
	s.cfg.RuntimeSessionID = targetRuntimeSessionID
	s.currentDir = targetDir
	s.currentModel = targetModel
	s.currentPermissionMode = targetPermissionMode
	s.currentSandboxMode = targetSandboxMode
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
	if adapter.Kind() != runtimeCodex {
		s.codexAppServerURL = ""
	}
	s.thinking = false
	s.running = true
	s.mu.Unlock()

	s.startProcessBridge(cmd, stdin, stdout, sessionID)
	if len(initialHistoryBatch) > 0 {
		historyPusher := newSessionHistoryPusher(s.cfg.ServerURL, s.cfg.Token, s.contentProtector)
		if err := historyPusher.pushBatch(sessionID, initialHistoryBatch); err != nil {
			applog.Errorf("[Remote] codex restart history sync error: session=%s err=%v", sessionID, err)
		} else {
			s.markCodexHistorySynced(sessionID, initialHistoryBatch)
		}
	}
	if err := s.sendCurrentKeepalive(sessionID); err != nil {
		applog.Errorf("[Remote] keepalive after restart failed: session=%s err=%v", sessionID, err)
	}
	if adapter.Kind() == runtimeCodex && strings.TrimSpace(s.runtimeSessionID) != "" {
		s.scheduleCodexThreadSnapshotSync(sessionID, s.runtimeSessionID)
	}

	if oldStdin != nil {
		_ = oldStdin.Close()
	}
	if oldCmd != nil && oldCmd.Process != nil {
		_ = oldCmd.Process.Signal(syscall.SIGTERM)
	}

	applog.Info.Printf("[Remote] Restarted %s process in %s (PID %d)", adapter.Kind(), targetDir, cmd.Process.Pid)

	return nil
}

func (s *Service) startProcessWaiter(cmd *exec.Cmd, sessionID, exitLabel string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := cmd.Wait()
		if !s.isCurrentCommand(cmd) {
			return
		}
		if err != nil {
			applog.Info.Printf("[Remote] %s: %v", exitLabel, err)
		}
		if status, shouldEmit := s.finishTurnOnExit(); shouldEmit {
			if writeErr := s.sendTurnEnd(sessionID, status); writeErr != nil {
				applog.Errorf("[Remote] WS write turn-end on exit error: %v", writeErr)
			}
		}
		s.markDisconnected()
	}()
}

func (s *Service) isCurrentCommand(cmd *exec.Cmd) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd == cmd
}

func (s *Service) markDisconnected() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	shouldReconnect := s.autoReconnect
	localMode := s.localMode
	sink := s.sink
	snapshot := reconnectSnapshot{
		cfg:                      s.cfg,
		sessionID:                s.sessionID,
		workingDir:               s.currentDir,
		model:                    s.currentModel,
		permissionMode:           s.currentPermissionMode,
		sandboxMode:              s.currentSandboxMode,
		attachedInputHandler:     s.attachedInputHandler,
		attachedInterruptHandler: s.attachedInterruptHandler,
		attachedConfigHandler:    s.attachedConfigHandler,
	}
	conn := s.conn
	stdin := s.stdin
	cmd := s.cmd
	done := s.done
	sessionID := s.sessionID
	s.conn = nil
	s.stdin = nil
	s.stdout = nil
	s.stdoutReader = nil
	s.cmd = nil
	s.done = nil
	s.sessionID = ""
	s.runtimeSessionID = ""
	s.adapter = nil
	s.runtime = runtimeClaude
	s.currentModel = ""
	s.currentDir = ""
	s.currentPermissionMode = protocol.PermissionModeDefault
	s.currentSandboxMode = ""
	s.turnActive = false
	s.interruptRequested = false
	s.pendingInterrupts = nil
	s.pendingRPC = nil
	s.codexTurnID = ""
	s.codexAppServerURL = ""
	s.codexObservedThreadID = ""
	s.codexObservedLiveOutput = false
	s.codexSnapshotSyncing = false
	s.codexSyncedHistory = nil
	s.thinking = false
	s.localMode = false
	s.sink = nil
	s.mu.Unlock()

	if sessionID != "" {
		state := "stopped"
		reason := "connection-lost"
		if shouldReconnect && strings.TrimSpace(snapshot.sessionID) != "" {
			state = protocol.SessionLifecycleReconnecting
		}

		lifecycle := protocol.Message{
			Type:      protocol.TypeSessionLifecycle,
			SessionID: sessionID,
			Payload: protocol.SessionLifecyclePayload{
				State:      state,
				Reason:     reason,
				OccurredAt: time.Now().UnixMilli(),
			},
		}
		if localMode && sink != nil {
			_ = sink(lifecycle)
		}
		s.notifyMessageObservers(lifecycle)
	}

	if done != nil {
		close(done)
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	if conn != nil {
		_ = conn.Close()
	}
	applog.Info.Printf("[Remote] Session disconnected: %s", sessionID)
	if shouldReconnect && strings.TrimSpace(snapshot.sessionID) != "" {
		s.scheduleAutoReconnect(snapshot)
	}
}

func processPID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

func registerCommandName(cfg *Config) string {
	if cfg.Management {
		return "remote-manager"
	}
	return cfg.Command
}

func resolveCommandPath(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("command is empty")
	}

	if strings.ContainsRune(command, os.PathSeparator) {
		if _, err := os.Stat(command); err != nil {
			return "", err
		}
		return command, nil
	}

	if path, err := exec.LookPath(command); err == nil {
		return path, nil
	}

	if path := findCommandInCommonLocations(command); path != "" {
		return path, nil
	}

	if path := findCommandViaShell(command); path != "" {
		return path, nil
	}

	return "", fmt.Errorf("executable not found")
}

func findCommandViaShell(command string) string {
	if runtime.GOOS == "windows" {
		return ""
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandResolveTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, shell, "-l", "-c", "command -v "+shellQuote(command)+" 2>/dev/null")
	cmd.Env = os.Environ()
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	path := strings.TrimSpace(string(output))
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func findCommandInCommonLocations(command string) string {
	homeDir, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join("/usr/local/bin", command),
		filepath.Join("/opt/homebrew/bin", command),
		filepath.Join("/usr/bin", command),
	}

	if homeDir != "" {
		candidates = append(candidates,
			filepath.Join(homeDir, ".local", "bin", command),
			filepath.Join(homeDir, ".npm", "bin", command),
		)
	}

	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData != "" {
			candidates = append(candidates, filepath.Join(appData, "npm", command+".cmd"))
		}
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
