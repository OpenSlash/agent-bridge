package toolpaths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func ClaudeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

func CodexDir() string {
	if custom := strings.TrimSpace(os.Getenv("CODEX_HOME")); custom != "" {
		return custom
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func AgentsSkillsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents", "skills")
}

func AgentsDir() string {
	return filepath.Join(ClaudeDir(), "agents")
}

func CommandsDir() string {
	return filepath.Join(ClaudeDir(), "commands")
}

func CodexCommandsDir() string {
	return filepath.Join(CodexDir(), "commands")
}

func SkillsDir() string {
	return filepath.Join(ClaudeDir(), "skills")
}

func PluginsDir() string {
	return filepath.Join(ClaudeDir(), "plugins")
}

func EnsureDir(path string) error {
	return os.MkdirAll(path, 0755)
}

func IsWindows() bool {
	return runtime.GOOS == "windows"
}

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
