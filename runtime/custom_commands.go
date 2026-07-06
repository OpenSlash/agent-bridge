package remote

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

type customSlashCommandSpec struct {
	Name   string
	Prompt string
}

func resolveCustomSlashCommand(runtime runtimeKind, currentDir, command string) (*customSlashCommandSpec, error) {
	sources, err := runtimeSlashCommandSources(runtime, currentDir)
	if err != nil {
		return nil, err
	}

	for _, source := range sources {
		spec, resolveErr := findCustomSlashCommandInDir(source.path, source.source, command)
		if resolveErr != nil {
			if os.IsNotExist(resolveErr) {
				continue
			}
			return nil, resolveErr
		}
		if spec != nil {
			return spec, nil
		}
	}
	return nil, nil
}

func findCustomSlashCommandInDir(root, source, command string) (*customSlashCommandSpec, error) {
	if root == "" {
		return nil, nil
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	normalizedCommand := strings.ToLower(strings.TrimSpace(command))
	var result *customSlashCommandSpec
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") || strings.EqualFold(d.Name(), "README.md") {
			return nil
		}

		entry, parseErr := parseSlashCommandFile(root, path, source)
		if parseErr != nil {
			return nil
		}
		if strings.ToLower(strings.TrimSpace(entry.Name)) != normalizedCommand {
			return nil
		}

		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		result = &customSlashCommandSpec{
			Name:   entry.Name,
			Prompt: strings.TrimSpace(stripMarkdownFrontmatter(string(content))),
		}
		return filepath.SkipAll
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return result, nil
}

func renderCustomSlashCommandPrompt(prompt string, args []string) string {
	rendered := strings.TrimSpace(prompt)
	joinedArgs := strings.TrimSpace(strings.Join(args, " "))
	if rendered == "" {
		return ""
	}

	placeholders := []string{"$ARGUMENTS", "{{args}}", "{{arguments}}", "{{input}}"}
	replaced := false
	for _, placeholder := range placeholders {
		if strings.Contains(rendered, placeholder) {
			rendered = strings.ReplaceAll(rendered, placeholder, joinedArgs)
			replaced = true
		}
	}

	if joinedArgs != "" && !replaced {
		rendered += "\n\nUser input:\n" + joinedArgs
	}
	return strings.TrimSpace(rendered)
}

func (s *Service) handleCodexCustomSlashCommand(
	sessionID,
	rawInput,
	command string,
	args []string,
) (bool, error) {
	spec, err := resolveCustomSlashCommand(runtimeCodex, s.getCurrentDir(), command)
	if err != nil || spec == nil {
		return spec != nil, err
	}

	expandedPrompt := renderCustomSlashCommandPrompt(spec.Prompt, args)
	if expandedPrompt == "" {
		return true, nil
	}

	s.recordCodexInputAlias(expandedPrompt, strings.TrimSpace(rawInput))
	if err := s.writeUserMessage(expandedPrompt); err != nil {
		return true, err
	}

	applog.Info.Printf(
		"[Remote] codex custom slash expanded: session=%s command=%s prompt_chars=%d",
		sessionID,
		command,
		len(expandedPrompt),
	)
	s.beginTurn()
	return true, s.writeJSON(protocol.Message{
		Type:      protocol.TypeTurnStart,
		SessionID: sessionID,
		Payload: protocol.TurnStartPayload{
			TurnID: fmt.Sprintf("turn-%d", time.Now().UnixMilli()),
		},
	})
}

func (s *Service) recordCodexInputAlias(expandedPrompt, rawInput string) {
	expandedPrompt = strings.TrimSpace(expandedPrompt)
	rawInput = strings.TrimSpace(rawInput)
	if expandedPrompt == "" || rawInput == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.codexInputAliases == nil {
		s.codexInputAliases = make(map[string]string)
	}
	s.codexInputAliases[expandedPrompt] = rawInput
}

func (s *Service) rewriteCodexUserInput(input string) string {
	normalized := strings.TrimSpace(input)
	if normalized == "" {
		return input
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.codexInputAliases == nil {
		return normalized
	}
	if alias, ok := s.codexInputAliases[normalized]; ok {
		delete(s.codexInputAliases, normalized)
		return alias
	}
	return normalized
}
