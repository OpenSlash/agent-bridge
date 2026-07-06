package remote

import (
	"bufio"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

type runtimeKind string

const (
	runtimeClaude runtimeKind = "claude"
	runtimeCodex  runtimeKind = "codex"
)

type runtimeBootstrap struct {
	SessionID        string
	RuntimeSessionID string
	Model            string
	WorkingDir       string
	HistoryBatch     []protocol.SessionHistoryMessage
}

type runtimeAdapter interface {
	Kind() runtimeKind
	PrepareStart(s *Service, cfg *Config, cwd string) error
	StartCommand(s *Service, sessionID, workingDir, model, permissionMode string, resume bool) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error)
	BootstrapSession(s *Service, stdin io.WriteCloser, stdoutReader *bufio.Reader, sessionID, runtimeSessionID, workingDir, model, permissionMode, sandboxMode string, resume bool) (runtimeBootstrap, error)
	StartProcessBridge(s *Service, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, stdoutReader *bufio.Reader, sessionID string)
	SendUserInput(s *Service, content string) error
	RequestInterrupt(s *Service) error
	StartHistorySync(s *Service, sessionID string)
}

type claudeRuntimeAdapter struct{}

type codexRuntimeAdapter struct{}

func (claudeRuntimeAdapter) Kind() runtimeKind {
	return runtimeClaude
}

func (claudeRuntimeAdapter) PrepareStart(_ *Service, cfg *Config, cwd string) error {
	if cfg.Management {
		return nil
	}

	requestedSessionID := strings.TrimSpace(cfg.SessionID)
	resolution, err := resolveClaudeSessionStart(cfg.SessionID, cwd, cfg.Resume)
	if err != nil {
		applog.Errorf("[Remote] resolve Claude session error: requested=%s cwd=%s err=%v", requestedSessionID, cwd, err)
		return nil
	}

	cfg.SessionID = resolution.SessionID
	cfg.Resume = resolution.Resume
	switch {
	case resolution.ReplacedRequestedSession:
		applog.Info.Printf(
			"[Remote] remapped requested Claude session: requested=%s adopted=%s cwd=%s transcript=%s",
			requestedSessionID,
			resolution.SessionID,
			cwd,
			resolution.TranscriptPath,
		)
	case resolution.AdoptedRecentSession:
		applog.Info.Printf(
			"[Remote] adopting recent Claude session: session=%s cwd=%s transcript=%s",
			resolution.SessionID,
			cwd,
			resolution.TranscriptPath,
		)
	case requestedSessionID != "" && !cfg.Resume:
		applog.Info.Printf(
			"[Remote] requested resume transcript unavailable, starting detached proxy: session=%s cwd=%s",
			resolution.SessionID,
			cwd,
		)
	}
	return nil
}

func (claudeRuntimeAdapter) StartCommand(s *Service, sessionID, workingDir, model, permissionMode string, resume bool) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	return s.startClaudeCommand(sessionID, workingDir, model, permissionMode, resume)
}

func (claudeRuntimeAdapter) BootstrapSession(_ *Service, _ io.WriteCloser, _ *bufio.Reader, _ string, _ string, _ string, _ string, _ string, _ string, _ bool) (runtimeBootstrap, error) {
	return runtimeBootstrap{}, nil
}

func (claudeRuntimeAdapter) StartProcessBridge(s *Service, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, stdoutReader *bufio.Reader, sessionID string) {
	s.startClaudeProcessBridge(cmd, stdin, stdout, stdoutReader, sessionID)
}

func (claudeRuntimeAdapter) SendUserInput(s *Service, content string) error {
	return s.writeClaudeUserMessage(content)
}

func (claudeRuntimeAdapter) RequestInterrupt(s *Service) error {
	return s.requestClaudeInterrupt()
}

func (claudeRuntimeAdapter) StartHistorySync(s *Service, sessionID string) {
	s.startTranscriptSync(sessionID)
}

func (codexRuntimeAdapter) Kind() runtimeKind {
	return runtimeCodex
}

func (codexRuntimeAdapter) PrepareStart(_ *Service, cfg *Config, cwd string) error {
	if codexRuntimeArgsUseResumeSubcommand(cfg.Args) {
		return nil
	}

	requestedRuntimeSessionID := strings.TrimSpace(cfg.RuntimeSessionID)
	resolution, err := ResolveCodexSessionStart(cfg.RuntimeSessionID, cwd, cfg.Resume)
	if err != nil {
		applog.Errorf("[Remote] resolve Codex session error: requested=%s cwd=%s err=%v", requestedRuntimeSessionID, cwd, err)
		return nil
	}

	cfg.RuntimeSessionID = resolution.RuntimeSessionID
	cfg.Resume = resolution.Resume
	switch {
	case resolution.ReplacedRequestedSession:
		applog.Info.Printf(
			"[Remote] remapped requested Codex session: requested=%s adopted=%s cwd=%s rollout=%s",
			requestedRuntimeSessionID,
			resolution.RuntimeSessionID,
			cwd,
			resolution.RolloutPath,
		)
	case resolution.AdoptedRecentSession:
		applog.Info.Printf(
			"[Remote] adopting recent Codex session: runtime_session=%s cwd=%s rollout=%s",
			resolution.RuntimeSessionID,
			cwd,
			resolution.RolloutPath,
		)
	case requestedRuntimeSessionID != "" && !cfg.Resume:
		applog.Info.Printf(
			"[Remote] requested Codex rollout unavailable, starting detached thread: runtime_session=%s cwd=%s",
			resolution.RuntimeSessionID,
			cwd,
		)
	}
	return nil
}

func codexRuntimeArgsUseResumeSubcommand(args []string) bool {
	firstPositional := ""
	for index := 0; index < len(args); index++ {
		arg := strings.TrimSpace(args[index])
		if arg == "" {
			continue
		}
		switch {
		case codexRuntimeArgConsumesValue(arg):
			if index+1 < len(args) {
				index++
			}
			continue
		case strings.HasPrefix(arg, "-"):
			continue
		default:
			firstPositional = arg
		}
		if firstPositional != "" {
			break
		}
	}
	return firstPositional == "resume"
}

func codexRuntimeArgConsumesValue(arg string) bool {
	switch strings.TrimSpace(arg) {
	case "--config", "-c", "--model", "-m", "--profile", "-p", "--image", "-i", "--remote", "--remote-auth-token-env", "--enable", "--disable", "--cd", "-C", "--ask-for-approval", "-a", "--sandbox", "-s":
		return true
	default:
		return false
	}
}

func (codexRuntimeAdapter) StartCommand(s *Service, _ string, workingDir, _ string, _ string, _ bool) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	return s.startCodexCommand(workingDir)
}

func (codexRuntimeAdapter) BootstrapSession(s *Service, stdin io.WriteCloser, stdoutReader *bufio.Reader, sessionID, runtimeSessionID, workingDir, model, permissionMode, sandboxMode string, resume bool) (runtimeBootstrap, error) {
	bootstrap, err := s.bootstrapCodexSession(stdin, stdoutReader, sessionID, runtimeSessionID, workingDir, model, permissionMode, sandboxMode, resume)
	if err != nil {
		return runtimeBootstrap{}, err
	}
	return runtimeBootstrap{
		RuntimeSessionID: bootstrap.ThreadID,
		Model:            bootstrap.Model,
		WorkingDir:       bootstrap.Cwd,
		HistoryBatch:     bootstrap.HistoryBatch,
	}, nil
}

func (codexRuntimeAdapter) StartProcessBridge(s *Service, cmd *exec.Cmd, stdin io.WriteCloser, _ io.ReadCloser, stdoutReader *bufio.Reader, sessionID string) {
	s.startCodexProcessBridge(cmd, stdin, stdoutReader, sessionID)
}

func (codexRuntimeAdapter) SendUserInput(s *Service, content string) error {
	return s.startCodexTurn(content)
}

func (codexRuntimeAdapter) RequestInterrupt(s *Service) error {
	return s.requestCodexTurnInterrupt()
}

func (codexRuntimeAdapter) StartHistorySync(_ *Service, _ string) {}

func detectRuntime(command string) runtimeKind {
	return runtimeKindFromString(filepath.Base(command))
}

func runtimeKindFromString(value string) runtimeKind {
	name := strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(name, "codex"):
		return runtimeCodex
	default:
		return runtimeClaude
	}
}

func resolveRuntimeAdapter(command string) runtimeAdapter {
	switch detectRuntime(command) {
	case runtimeCodex:
		return codexRuntimeAdapter{}
	default:
		return claudeRuntimeAdapter{}
	}
}

func (s *Service) getRuntime() runtimeKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime == "" {
		return runtimeClaude
	}
	return s.runtime
}

func (s *Service) getRuntimeAdapter() runtimeAdapter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adapter != nil {
		return s.adapter
	}
	return resolveRuntimeAdapter(s.cfg.Command)
}
