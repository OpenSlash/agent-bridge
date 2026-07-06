package remote

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/OpenSlash/agent-bridge/internal/toolpaths"
	"github.com/OpenSlash/agent-bridge/protocol"
)

type RuntimeCatalogOptions struct {
	ClaudeEnabled bool
	CodexEnabled  bool
}

func runtimeTitle(runtime runtimeKind) string {
	switch runtime {
	case runtimeCodex:
		return "Codex"
	default:
		return "Claude"
	}
}

func runtimeModelCatalogForRuntime(runtime runtimeKind) []protocol.RuntimeModelInfo {
	switch runtime {
	case runtimeCodex:
		return []protocol.RuntimeModelInfo{
			{ID: "gpt-5.4", Title: "GPT-5.4"},
			{ID: "gpt-5.4-mini", Title: "GPT-5.4 mini"},
			{ID: "gpt-5.3-codex", Title: "GPT-5.3 Codex"},
			{ID: "gpt-5.3-codex-spark", Title: "GPT-5.3 Codex Spark"},
			{ID: "gpt-5.2-codex", Title: "GPT-5.2 Codex"},
			{ID: "gpt-5-codex", Title: "GPT-5 Codex"},
		}
	default:
		return []protocol.RuntimeModelInfo{
			{ID: "claude-sonnet-4-6", Title: "Sonnet 4.6"},
			{ID: "claude-opus-4-6", Title: "Opus 4.6"},
			{ID: "claude-haiku-4-5", Title: "Haiku 4.5"},
		}
	}
}

func BuildRuntimeCatalog(options RuntimeCatalogOptions) []protocol.RuntimeCapability {
	runtimes := make([]runtimeKind, 0, 2)
	if options.ClaudeEnabled {
		runtimes = append(runtimes, runtimeClaude)
	}
	if options.CodexEnabled {
		runtimes = append(runtimes, runtimeCodex)
	}
	catalog := make([]protocol.RuntimeCapability, 0, len(runtimes))
	for _, runtime := range runtimes {
		catalog = append(catalog, protocol.RuntimeCapability{
			ID:     string(runtime),
			Title:  runtimeTitle(runtime),
			Models: runtimeModelCatalogForRuntime(runtime),
		})
	}
	return catalog
}

func buildRuntimeCatalog() []protocol.RuntimeCapability {
	return BuildRuntimeCatalog(RuntimeCatalogOptions{
		ClaudeEnabled: true,
		CodexEnabled:  true,
	})
}

func normalizePermissionModeForRuntime(runtime runtimeKind, mode string) string {
	normalized := normalizePermissionMode(mode)
	switch runtime {
	case runtimeCodex:
		switch normalized {
		case protocol.PermissionModeDontAsk, protocol.PermissionModeBypassPermissions:
			return normalized
		default:
			return protocol.PermissionModeDefault
		}
	default:
		return normalized
	}
}

func defaultSandboxModeForRuntime(runtime runtimeKind) string {
	switch runtime {
	case runtimeCodex:
		return protocol.SandboxModeWorkspaceWrite
	default:
		return ""
	}
}

func normalizeSandboxModeForRuntime(runtime runtimeKind, mode string) string {
	normalized := strings.TrimSpace(mode)
	switch runtime {
	case runtimeCodex:
		switch normalized {
		case protocol.SandboxModeReadOnly, protocol.SandboxModeWorkspaceWrite, protocol.SandboxModeDangerFullAccess:
			return normalized
		default:
			return protocol.SandboxModeWorkspaceWrite
		}
	default:
		return ""
	}
}

func supportedModelsForRuntime(runtime runtimeKind) []string {
	catalog := runtimeModelCatalogForRuntime(runtime)
	models := make([]string, 0, len(catalog))
	for _, entry := range catalog {
		if strings.TrimSpace(entry.ID) == "" {
			continue
		}
		models = append(models, entry.ID)
	}
	return models
}

func runtimeAllowsCustomSlashCommands(runtime runtimeKind) bool {
	return true
}

func runtimeBuiltinSlashCommands(runtime runtimeKind) []protocol.SlashCommandEntry {
	modelHint := "[model]"
	modelSummary := "Switch the active model for this session."
	if runtime == runtimeCodex {
		commands := []protocol.SlashCommandEntry{
			{Name: "help", Summary: "Show available slash commands and usage tips.", Source: "built-in"},
		}
		return append(commands, codexNativeSlashCommands()...)
	}

	commands := []protocol.SlashCommandEntry{
		{Name: "help", Summary: "Show available slash commands and usage tips.", Source: "built-in"},
		{Name: "status", Summary: "Show current proxy session status.", Source: "built-in"},
		protocol.SlashCommandEntry{Name: "model", Summary: modelSummary, Source: "built-in", ArgumentHint: modelHint},
		protocol.SlashCommandEntry{Name: "compact", Summary: "Compact conversation context to reduce token usage.", Source: "built-in"},
		protocol.SlashCommandEntry{Name: "clear", Summary: "Clear the current conversation context.", Source: "built-in"},
	}
	return commands
}

func runtimeSlashCommandSources(runtime runtimeKind, currentDir string) ([]slashCommandSource, error) {
	if !runtimeAllowsCustomSlashCommands(runtime) {
		return nil, nil
	}

	sources := make([]slashCommandSource, 0, 4)
	switch runtime {
	case runtimeCodex:
		sources = append(sources, slashCommandSource{
			path:     toolpaths.CodexCommandsDir(),
			source:   "global",
			priority: 50,
		})
		if currentDir != "" {
			sources = append(sources, slashCommandSource{
				path:     filepath.Join(currentDir, ".codex", "commands"),
				source:   "project",
				priority: 60,
			})
		}
	default:
		sources = append(sources, slashCommandSource{
			path:     toolpaths.CommandsDir(),
			source:   "global",
			priority: 50,
		})
		pluginSources, err := scanClaudePluginCommandSources()
		if err != nil {
			return nil, err
		}
		sources = append(sources, pluginSources...)
		if currentDir != "" {
			sources = append(sources, slashCommandSource{
				path:     filepath.Join(currentDir, ".claude", "commands"),
				source:   "project",
				priority: 60,
			})
		}
	}

	return sources, nil
}

func buildLocalHelpMessageForRuntime(runtime runtimeKind) string {
	models := append([]string{"default"}, supportedModelsForRuntime(runtime)...)
	if runtime != runtimeCodex {
		return fmt.Sprintf(
			"Remote slash commands:\n- /help\n- /status\n- /model [%s]\n\nProject and global custom slash commands are forwarded to Claude.\nSome Claude desktop-only commands are not available in the mobile remote proxy.",
			strings.Join(models, "|"),
		)
	}

	return fmt.Sprintf(
		"Remote slash commands:\n- /help\n- Native Codex slash commands from the controlled runtime (for example /model, /clear, /compact, /review, /resume, /permissions, /approvals)\n- Supported models: %s\n\n`@` file and directory completion is still available. Codex skills can be selected from the dedicated skill entry in the mobile input box. Custom commands are loaded from ~/.codex/commands and the project .codex/commands directory.",
		strings.Join(models, "|"),
	)
}

func normalizeLocalModelArgumentForRuntime(runtime runtimeKind, value string) string {
	normalized := strings.TrimSpace(strings.ToLower(value))
	switch runtime {
	case runtimeCodex:
		switch normalized {
		case "", "default", "auto":
			return ""
		case "gpt-5.4", "gpt54", "gpt-54":
			return "gpt-5.4"
		case "gpt-5.4-mini", "gpt54-mini", "gpt-54-mini", "gpt-5-mini", "gpt5.4-mini":
			return "gpt-5.4-mini"
		case "gpt-5.3-codex", "gpt53-codex", "gpt-53-codex":
			return "gpt-5.3-codex"
		case "gpt-5.3-codex-spark", "gpt53-codex-spark", "gpt-53-codex-spark", "spark", "codex-spark":
			return "gpt-5.3-codex-spark"
		case "gpt-5.2-codex", "gpt52-codex", "gpt-52-codex":
			return "gpt-5.2-codex"
		case "gpt-5-codex", "gpt5-codex", "gpt-5", "gpt5":
			return "gpt-5-codex"
		default:
			return ""
		}
	default:
		switch normalized {
		case "", "default", "auto":
			return ""
		case "claude-sonnet-4-6", "claude-sonnet-4.6", "sonnet", "sonnet-4-6", "sonnet-4.6":
			return "claude-sonnet-4-6"
		case "claude-opus-4-6", "claude-opus-4.6", "opus", "opus-4-6", "opus-4.6":
			return "claude-opus-4-6"
		case "claude-haiku-4-5", "claude-haiku-4.5", "haiku", "haiku-4-5", "haiku-4.5":
			return "claude-haiku-4-5"
		default:
			return ""
		}
	}
}
