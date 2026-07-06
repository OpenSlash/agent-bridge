package remote

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type stubRuntimeAdapter struct {
	kind                   runtimeKind
	startCommandCalled     bool
	startCommandSessionID  string
	startCommandWorkingDir string
	startCommandModel      string
	startCommandPermission string
	startCommandResume     bool
	startCommandCmd        *exec.Cmd
	startCommandStdin      io.WriteCloser
	startCommandStdout     io.ReadCloser
	startCommandErr        error

	startProcessBridgeCalled bool
	startProcessBridgeCmd    *exec.Cmd
	startProcessBridgeStdin  io.WriteCloser
	startProcessBridgeStdout io.ReadCloser
	startProcessBridgeReader *bufio.Reader
	startProcessBridgeID     string

	sendUserInputCalled  bool
	sendUserInputContent string
	sendUserInputErr     error

	requestInterruptCalled bool
	requestInterruptErr    error
}

func (s *stubRuntimeAdapter) Kind() runtimeKind {
	if s.kind == "" {
		return runtimeClaude
	}
	return s.kind
}

func (s *stubRuntimeAdapter) PrepareStart(_ *Service, _ *Config, _ string) error {
	return nil
}

func (s *stubRuntimeAdapter) StartCommand(_ *Service, sessionID, workingDir, model, permissionMode string, resume bool) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	s.startCommandCalled = true
	s.startCommandSessionID = sessionID
	s.startCommandWorkingDir = workingDir
	s.startCommandModel = model
	s.startCommandPermission = permissionMode
	s.startCommandResume = resume
	return s.startCommandCmd, s.startCommandStdin, s.startCommandStdout, s.startCommandErr
}

func (s *stubRuntimeAdapter) BootstrapSession(_ *Service, _ io.WriteCloser, _ *bufio.Reader, _ string, _ string, _ string, _ string, _ string, _ string, _ bool) (runtimeBootstrap, error) {
	return runtimeBootstrap{}, nil
}

func (s *stubRuntimeAdapter) StartProcessBridge(_ *Service, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, stdoutReader *bufio.Reader, sessionID string) {
	s.startProcessBridgeCalled = true
	s.startProcessBridgeCmd = cmd
	s.startProcessBridgeStdin = stdin
	s.startProcessBridgeStdout = stdout
	s.startProcessBridgeReader = stdoutReader
	s.startProcessBridgeID = sessionID
}

func (s *stubRuntimeAdapter) SendUserInput(_ *Service, content string) error {
	s.sendUserInputCalled = true
	s.sendUserInputContent = content
	return s.sendUserInputErr
}

func (s *stubRuntimeAdapter) RequestInterrupt(_ *Service) error {
	s.requestInterruptCalled = true
	return s.requestInterruptErr
}

func (s *stubRuntimeAdapter) StartHistorySync(_ *Service, _ string) {}

func TestGetRuntimeAdapterFallsBackToCommand(t *testing.T) {
	service := &Service{cfg: Config{Command: "/opt/homebrew/bin/codex"}}

	adapter := service.getRuntimeAdapter()
	if adapter == nil {
		t.Fatal("expected runtime adapter")
	}
	if got := adapter.Kind(); got != runtimeCodex {
		t.Fatalf("expected codex adapter, got %q", got)
	}
}

func TestStartCommandDelegatesToRuntimeAdapter(t *testing.T) {
	adapter := &stubRuntimeAdapter{}
	service := &Service{adapter: adapter}

	cmd := &exec.Cmd{Path: "claude"}
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe failed: %v", err)
	}
	defer stdinReader.Close()
	defer stdinWriter.Close()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe failed: %v", err)
	}
	defer stdoutReader.Close()
	defer stdoutWriter.Close()

	adapter.startCommandCmd = cmd
	adapter.startCommandStdin = stdinWriter
	adapter.startCommandStdout = stdoutReader

	gotCmd, gotStdin, gotStdout, err := service.startCommand("session-1", "/tmp/project", "sonnet", "default", true)
	if err != nil {
		t.Fatalf("startCommand returned error: %v", err)
	}
	if !adapter.startCommandCalled {
		t.Fatal("expected runtime adapter StartCommand to be called")
	}
	if adapter.startCommandSessionID != "session-1" || adapter.startCommandWorkingDir != "/tmp/project" || adapter.startCommandModel != "sonnet" || adapter.startCommandPermission != "default" || !adapter.startCommandResume {
		t.Fatalf("unexpected adapter arguments: %+v", adapter)
	}
	if gotCmd != cmd || gotStdin != stdinWriter || gotStdout != stdoutReader {
		t.Fatal("expected startCommand to return adapter values")
	}
}

func TestWriteUserMessageDelegatesToRuntimeAdapter(t *testing.T) {
	adapter := &stubRuntimeAdapter{}
	service := &Service{adapter: adapter}

	if err := service.writeUserMessage("hello codex"); err != nil {
		t.Fatalf("writeUserMessage returned error: %v", err)
	}
	if !adapter.sendUserInputCalled {
		t.Fatal("expected SendUserInput to be called")
	}
	if got := adapter.sendUserInputContent; got != "hello codex" {
		t.Fatalf("unexpected forwarded content %q", got)
	}
}

func TestRequestInterruptDelegatesToRuntimeAdapter(t *testing.T) {
	adapter := &stubRuntimeAdapter{}
	service := &Service{adapter: adapter}

	if err := service.requestInterrupt(); err != nil {
		t.Fatalf("requestInterrupt returned error: %v", err)
	}
	if !adapter.requestInterruptCalled {
		t.Fatal("expected RequestInterrupt to be called")
	}
}

func TestStartProcessBridgeDelegatesToRuntimeAdapter(t *testing.T) {
	adapter := &stubRuntimeAdapter{}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe failed: %v", err)
	}
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe failed: %v", err)
	}
	defer stdinReader.Close()
	defer stdinWriter.Close()

	service := &Service{
		adapter:      adapter,
		stdoutReader: bufio.NewReader(stdoutReader),
	}
	cmd := &exec.Cmd{Path: "codex"}

	service.startProcessBridge(cmd, stdinWriter, stdoutReader, "session-bridge")
	if !adapter.startProcessBridgeCalled {
		t.Fatal("expected StartProcessBridge to be called")
	}
	if adapter.startProcessBridgeCmd != cmd || adapter.startProcessBridgeStdin != stdinWriter || adapter.startProcessBridgeStdout != stdoutReader || adapter.startProcessBridgeID != "session-bridge" {
		t.Fatal("unexpected StartProcessBridge arguments")
	}
	if adapter.startProcessBridgeReader == nil {
		t.Fatal("expected buffered stdout reader to be forwarded")
	}
}

func TestMarkDisconnectedClearsRuntimeAdapterState(t *testing.T) {
	adapter := &stubRuntimeAdapter{kind: runtimeCodex}
	service := &Service{
		running:               true,
		adapter:               adapter,
		runtime:               runtimeCodex,
		sessionID:             "session-9",
		done:                  make(chan struct{}),
		currentDir:            "/tmp/project",
		currentModel:          "gpt-5-codex",
		currentPermissionMode: "default",
		pendingInterrupts:     map[string]struct{}{"a": {}},
		pendingRPC:            map[string]chan codexRPCResponse{"rpc-1": make(chan codexRPCResponse, 1)},
		codexTurnID:           "turn-1",
	}

	service.markDisconnected()

	if service.adapter != nil {
		t.Fatal("expected adapter to be cleared after disconnect")
	}
	if got := service.getRuntime(); got != runtimeClaude {
		t.Fatalf("expected runtime reset to claude default, got %q", got)
	}
	if got := service.SessionID(); got != "" {
		t.Fatalf("expected session id cleared, got %q", got)
	}
	if service.pendingRPC != nil {
		t.Fatal("expected pendingRPC to be cleared")
	}
	if service.codexTurnID != "" {
		t.Fatalf("expected codexTurnID cleared, got %q", service.codexTurnID)
	}
}

func TestRequestInterruptFallsBackToConfiguredRuntime(t *testing.T) {
	service := &Service{cfg: Config{Command: "claude"}}
	err := service.requestInterrupt()
	if err == nil {
		t.Fatal("expected no-active-turn error")
	}
	if got := err.Error(); got != "no active turn to interrupt" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestStartCommandWithoutAdapterFallsBackToConfiguredCommand(t *testing.T) {
	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "codex")
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script failed: %v", err)
	}

	service := &Service{
		cfg: Config{
			Command: scriptPath,
			Args:    []string{"--profile", "test"},
		},
	}

	adapter := service.getRuntimeAdapter()
	if adapter == nil {
		t.Fatal("expected runtime adapter")
	}
	if got := adapter.Kind(); got != runtimeCodex {
		t.Fatalf("expected codex adapter, got %q", got)
	}
}
