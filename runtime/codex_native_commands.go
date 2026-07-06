package remote

import "github.com/OpenSlash/agent-bridge/protocol"

// codexNativeSlashCommands returns the built-in slash commands exposed by the
// controlled Codex runtime compatibility window (currently 0.118.x).
//
// Source of truth:
// https://github.com/openai/codex/blob/main/codex-rs/tui_app_server/src/slash_command.rs
func codexNativeSlashCommands() []protocol.SlashCommandEntry {
	return []protocol.SlashCommandEntry{
		{Name: "model", Summary: "Choose what model and reasoning effort to use.", Source: "native"},
		{Name: "fast", Summary: "Toggle Fast mode to enable fastest inference at 2X plan usage.", Source: "native"},
		{Name: "approvals", Summary: "Choose what Codex is allowed to do.", Source: "native"},
		{Name: "permissions", Summary: "Choose what Codex is allowed to do.", Source: "native"},
		{Name: "setup-default-sandbox", Summary: "Set up elevated agent sandbox.", Source: "native"},
		{Name: "experimental", Summary: "Toggle experimental features.", Source: "native"},
		{Name: "skills", Summary: "Use skills to improve how Codex performs specific tasks.", Source: "native"},
		{Name: "review", Summary: "Review my current changes and find issues.", Source: "native", ArgumentHint: "[instructions]"},
		{Name: "rename", Summary: "Rename the current thread.", Source: "native", ArgumentHint: "[name]"},
		{Name: "new", Summary: "Start a new chat during a conversation.", Source: "native"},
		{Name: "resume", Summary: "Resume a saved chat.", Source: "native"},
		{Name: "fork", Summary: "Fork the current chat.", Source: "native"},
		{Name: "init", Summary: "Create an AGENTS.md file with instructions for Codex.", Source: "native"},
		{Name: "compact", Summary: "Summarize conversation to prevent hitting the context limit.", Source: "native"},
		{Name: "plan", Summary: "Switch to Plan mode.", Source: "native", ArgumentHint: "[instructions]"},
		{Name: "collab", Summary: "Change collaboration mode (experimental).", Source: "native"},
		{Name: "agent", Summary: "Switch the active agent thread.", Source: "native"},
		{Name: "diff", Summary: "Show git diff (including untracked files).", Source: "native"},
		{Name: "copy", Summary: "Copy the latest Codex output to your clipboard.", Source: "native"},
		{Name: "mention", Summary: "Mention a file.", Source: "native"},
		{Name: "status", Summary: "Show current session configuration and token usage.", Source: "native"},
		{Name: "debug-config", Summary: "Show config layers and requirement sources for debugging.", Source: "native"},
		{Name: "statusline", Summary: "Configure which items appear in the status line.", Source: "native"},
		{Name: "theme", Summary: "Choose a syntax highlighting theme.", Source: "native"},
		{Name: "mcp", Summary: "List configured MCP tools.", Source: "native"},
		{Name: "apps", Summary: "Manage apps.", Source: "native"},
		{Name: "plugins", Summary: "Browse plugins.", Source: "native"},
		{Name: "logout", Summary: "Log out of Codex.", Source: "native"},
		{Name: "quit", Summary: "Exit Codex.", Source: "native"},
		{Name: "exit", Summary: "Exit Codex.", Source: "native"},
		{Name: "feedback", Summary: "Send logs to maintainers.", Source: "native"},
		{Name: "ps", Summary: "List background terminals.", Source: "native"},
		{Name: "stop", Summary: "Stop all background terminals.", Source: "native"},
		{Name: "clear", Summary: "Clear the terminal and start a new chat.", Source: "native"},
		{Name: "personality", Summary: "Choose a communication style for Codex.", Source: "native"},
		{Name: "realtime", Summary: "Toggle realtime voice mode (experimental).", Source: "native"},
		{Name: "settings", Summary: "Configure realtime microphone/speaker.", Source: "native"},
		{Name: "subagents", Summary: "Switch the active agent thread.", Source: "native"},
	}
}

func isCodexNativeSlashCommand(name string) bool {
	for _, entry := range codexNativeSlashCommands() {
		if entry.Name == name {
			return true
		}
	}
	return false
}
