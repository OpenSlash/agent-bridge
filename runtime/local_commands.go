package remote

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func (s *Service) handleLocalSlashCommand(sessionID, rawInput string, historyPusher *sessionHistoryPusher) (bool, error) {
	command, args, ok := parseSlashCommandInvocation(rawInput)
	if !ok {
		return false, nil
	}
	runtime := s.getRuntime()

	if runtime == runtimeCodex {
		switch command {
		case "help":
			helpMessage := buildLocalHelpMessageForRuntime(runtime)
			if err := s.pushLocalCommandHistory(sessionID, rawInput, helpMessage, historyPusher); err != nil {
				return true, err
			}
			return true, s.sendSyntheticSystemEvent(sessionID, helpMessage)
		default:
			if isCodexNativeSlashCommand(command) {
				return false, nil
			}
			return s.handleCodexCustomSlashCommand(sessionID, rawInput, command, args)
		}
	}

	switch command {
	case "help":
		helpMessage := buildLocalHelpMessageForRuntime(runtime)
		if err := s.pushLocalCommandHistory(sessionID, rawInput, helpMessage, historyPusher); err != nil {
			return true, err
		}
		return true, s.sendSyntheticSystemEvent(sessionID, helpMessage)
	case "status":
		status := s.buildLocalStatusMessage(sessionID)
		if err := s.pushLocalCommandHistory(sessionID, rawInput, status, historyPusher); err != nil {
			return true, err
		}
		return true, s.sendSyntheticSystemEvent(sessionID, status)
	case "model":
		return true, s.handleLocalModelSlashCommand(sessionID, rawInput, args, historyPusher, runtime)
	case "config", "login", "logout", "resume", "mcp":
		message := fmt.Sprintf(
			"`/%s` is not supported in the mobile remote proxy yet. Use the desktop app or Claude CLI for this command.",
			command,
		)
		if err := s.pushLocalCommandHistory(sessionID, rawInput, message, historyPusher); err != nil {
			return true, err
		}
		return true, s.sendSyntheticSystemEvent(sessionID, message)
	default:
		return false, nil
	}
}

func (s *Service) handleLocalModelSlashCommand(sessionID, rawInput string, args []string, historyPusher *sessionHistoryPusher, runtime runtimeKind) error {
	models := supportedModelsForRuntime(runtime)
	modelList := "default"
	if len(models) > 0 {
		modelList += "\n- " + strings.Join(models, "\n- ")
	}
	if len(args) == 0 {
		message := "Current model: " + defaultDisplayString(s.getCurrentModel(), "default") + "\nAvailable models:\n- " + modelList
		if err := s.pushLocalCommandHistory(sessionID, rawInput, message, historyPusher); err != nil {
			return err
		}
		return s.sendSyntheticSystemEvent(sessionID, message)
	}

	requested := normalizeLocalModelArgumentForRuntime(runtime, args[0])
	if requested == "" && !isDefaultModelArgument(args[0]) {
		message := fmt.Sprintf("Unsupported model `%s`. Available values: %s", args[0], strings.Join(append([]string{"default"}, models...), ", "))
		if err := s.pushLocalCommandHistory(sessionID, rawInput, message, historyPusher); err != nil {
			return err
		}
		return s.sendSyntheticSystemEvent(sessionID, message)
	}

	message := "Switching model to " + defaultDisplayString(requested, "default") + ". Reconnecting the current session..."
	if err := s.pushLocalCommandHistory(sessionID, rawInput, message, historyPusher); err != nil {
		return err
	}
	if err := s.sendSyntheticSystemEvent(sessionID, message); err != nil {
		return err
	}
	if err := s.restartCommand(sessionID, "", requested, true, "", "", true); err != nil {
		return err
	}
	return s.sendCurrentKeepalive(sessionID)
}

func (s *Service) buildLocalStatusMessage(sessionID string) string {
	sandboxLine := ""
	if sandbox := strings.TrimSpace(s.getCurrentSandboxMode()); sandbox != "" {
		sandboxLine = "\nSandbox: " + sandbox
	}
	return fmt.Sprintf(
		"Session: %s\nDirectory: %s\nModel: %s\nPermission: %s%s\nConnected: %t\nThinking: %t",
		sessionID,
		defaultDisplayString(s.getCurrentDir(), "-"),
		defaultDisplayString(s.getCurrentModel(), "default"),
		defaultDisplayString(s.getCurrentPermissionMode(), protocol.PermissionModeDefault),
		sandboxLine,
		s.running,
		s.getThinking(),
	)
}

func (s *Service) pushLocalCommandHistory(sessionID, rawInput, response string, historyPusher *sessionHistoryPusher) error {
	if historyPusher == nil {
		return nil
	}

	trimmedInput := strings.TrimSpace(rawInput)
	batch := make([]protocol.SessionHistoryMessage, 0, 2)
	if trimmedInput != "" {
		batch = append(batch, protocol.SessionHistoryMessage{
			SourceID:    fmt.Sprintf("input:%d", time.Now().UnixNano()),
			SourceKind:  "client-input",
			Role:        "user",
			Content:     trimmedInput,
			MessageType: "command_input",
			Timestamp:   time.Now().UnixMilli(),
		})
	}
	if strings.TrimSpace(response) != "" {
		batch = append(batch, protocol.SessionHistoryMessage{
			SourceID:    fmt.Sprintf("system:%d", time.Now().UnixNano()),
			SourceKind:  "local-command",
			Role:        "system",
			Content:     response,
			MessageType: "text",
			Timestamp:   time.Now().UnixMilli(),
		})
	}
	return historyPusher.pushBatch(sessionID, batch)
}

func (s *Service) sendSyntheticSystemEvent(sessionID, content string) error {
	payload, err := json.Marshal(map[string]any{
		"type":    "system",
		"content": content,
	})
	if err != nil {
		return err
	}
	return s.writeJSON(protocol.Message{
		Type:      protocol.TypeText,
		SessionID: sessionID,
		Payload: protocol.TextPayload{
			Data:     string(payload),
			Thinking: false,
		},
	})
}

func parseSlashCommandInvocation(raw string) (string, []string, bool) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "/") {
		return "", nil, false
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", nil, false
	}
	command := strings.TrimPrefix(fields[0], "/")
	if command == "" {
		return "", nil, false
	}
	return strings.ToLower(command), fields[1:], true
}

func isDefaultModelArgument(value string) bool {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", "default", "auto":
		return true
	default:
		return false
	}
}

func defaultDisplayString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (s *Service) sendModelCommand(model string) error {
	if model == "" {
		return nil
	}
	if err := s.writeUserMessage("/model " + model); err != nil {
		return err
	}
	s.mu.Lock()
	s.currentModel = model
	s.mu.Unlock()
	return nil
}
