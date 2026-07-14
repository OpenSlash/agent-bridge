package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestNormalizePermissionModeForRuntime(t *testing.T) {
	if got := normalizePermissionModeForRuntime(runtimeClaude, protocol.PermissionModeAcceptEdits); got != protocol.PermissionModeAcceptEdits {
		t.Fatalf("expected claude acceptEdits, got %q", got)
	}
	if got := normalizePermissionModeForRuntime(runtimeCodex, protocol.PermissionModeAcceptEdits); got != protocol.PermissionModeDefault {
		t.Fatalf("expected codex acceptEdits to collapse to default, got %q", got)
	}
	if got := normalizePermissionModeForRuntime(runtimeCodex, protocol.PermissionModeDontAsk); got != protocol.PermissionModeDontAsk {
		t.Fatalf("expected codex dontAsk to stay enabled, got %q", got)
	}
}

func TestNormalizeSandboxModeForRuntime(t *testing.T) {
	if got := normalizeSandboxModeForRuntime(runtimeClaude, protocol.SandboxModeDangerFullAccess); got != "" {
		t.Fatalf("expected claude sandbox to be ignored, got %q", got)
	}
	if got := normalizeSandboxModeForRuntime(runtimeCodex, ""); got != protocol.SandboxModeWorkspaceWrite {
		t.Fatalf("expected codex empty sandbox to default to workspace-write, got %q", got)
	}
	if got := normalizeSandboxModeForRuntime(runtimeCodex, protocol.SandboxModeReadOnly); got != protocol.SandboxModeReadOnly {
		t.Fatalf("expected codex read-only sandbox to stay enabled, got %q", got)
	}
}

func TestRuntimeModelCatalogMatchesRuntime(t *testing.T) {
	catalog := buildRuntimeCatalog()
	if len(catalog) != 2 {
		t.Fatalf("expected runtime catalog for 2 runtimes, got %d", len(catalog))
	}
	if catalog[0].ID != string(runtimeClaude) || len(catalog[0].Models) != 3 {
		t.Fatalf("expected claude runtime catalog, got %#v", catalog[0])
	}
	if catalog[1].ID != string(runtimeCodex) || len(catalog[1].Models) != 6 {
		t.Fatalf("expected codex runtime catalog, got %#v", catalog[1])
	}
	if !catalog[0].SupportsImages || !catalog[1].SupportsImages {
		t.Fatalf("expected both runtime adapters to support image input, got %#v", catalog)
	}
	if got := supportedModelsForRuntime(runtimeClaude); len(got) != 3 {
		t.Fatalf("expected 3 claude models, got %v", got)
	}
	if got := supportedModelsForRuntime(runtimeCodex); len(got) != 6 || got[0] != "gpt-5.4" || got[len(got)-1] != "gpt-5-codex" {
		t.Fatalf("expected codex model catalog, got %v", got)
	}
	if got := normalizeLocalModelArgumentForRuntime(runtimeCodex, "gpt-5.4"); got != "gpt-5.4" {
		t.Fatalf("expected gpt-5.4 alias to normalize to gpt-5.4, got %q", got)
	}
	if got := normalizeLocalModelArgumentForRuntime(runtimeCodex, "spark"); got != "gpt-5.3-codex-spark" {
		t.Fatalf("expected spark alias to normalize to gpt-5.3-codex-spark, got %q", got)
	}
	if got := normalizeLocalModelArgumentForRuntime(runtimeCodex, "gpt-5"); got != "gpt-5-codex" {
		t.Fatalf("expected gpt-5 alias to normalize to gpt-5-codex, got %q", got)
	}
}

func TestRuntimeSlashCommandExposure(t *testing.T) {
	if !runtimeAllowsCustomSlashCommands(runtimeClaude) {
		t.Fatal("expected claude runtime to allow custom slash commands")
	}
	if !runtimeAllowsCustomSlashCommands(runtimeCodex) {
		t.Fatal("expected codex runtime to allow custom slash commands")
	}

	help := buildLocalHelpMessageForRuntime(runtimeCodex)
	if !strings.Contains(help, "Codex skills") {
		t.Fatalf("unexpected codex help message: %q", help)
	}

	codexCommands := runtimeBuiltinSlashCommands(runtimeCodex)
	if len(codexCommands) < 20 {
		t.Fatalf("expected codex to expose its native slash command set, got %d commands", len(codexCommands))
	}
	if !containsSlashCommand(codexCommands, "review") {
		t.Fatalf("expected codex native commands to include /review, got %v", codexCommands)
	}
	if !containsSlashCommand(codexCommands, "permissions") {
		t.Fatalf("expected codex native commands to include /permissions, got %v", codexCommands)
	}
	if !containsSlashCommand(codexCommands, "clear") {
		t.Fatalf("expected codex native commands to include /clear, got %v", codexCommands)
	}
}

func TestRuntimeSlashCommandSourcesAreRuntimeSpecific(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "project")
	mustMkdirAll(t, filepath.Join(home, ".claude", "commands"))
	mustMkdirAll(t, filepath.Join(home, ".codex", "commands"))
	mustMkdirAll(t, filepath.Join(project, ".claude", "commands"))
	mustMkdirAll(t, filepath.Join(project, ".codex", "commands"))
	mustMkdirAll(t, filepath.Join(home, ".claude", "plugins", "marketplaces", "demo-plugin", "commands"))

	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	claudeSources, err := runtimeSlashCommandSources(runtimeClaude, project)
	if err != nil {
		t.Fatalf("expected claude command sources, got error: %v", err)
	}
	codexSources, err := runtimeSlashCommandSources(runtimeCodex, project)
	if err != nil {
		t.Fatalf("expected codex command sources, got error: %v", err)
	}

	assertHasSourcePath(t, claudeSources, filepath.Join(home, ".claude", "commands"))
	assertHasSourcePath(t, claudeSources, filepath.Join(project, ".claude", "commands"))
	assertHasSourcePath(t, claudeSources, filepath.Join(home, ".claude", "plugins", "marketplaces", "demo-plugin", "commands"))
	assertLacksSourcePath(t, claudeSources, filepath.Join(home, ".codex", "commands"))

	assertHasSourcePath(t, codexSources, filepath.Join(home, ".codex", "commands"))
	assertHasSourcePath(t, codexSources, filepath.Join(project, ".codex", "commands"))
	assertLacksSourcePath(t, codexSources, filepath.Join(home, ".claude", "commands"))
	assertLacksSourcePath(t, codexSources, filepath.Join(project, ".claude", "commands"))
	assertLacksSourcePath(t, codexSources, filepath.Join(home, ".claude", "plugins", "marketplaces", "demo-plugin", "commands"))
}

func assertHasSourcePath(t *testing.T, sources []slashCommandSource, want string) {
	t.Helper()
	for _, source := range sources {
		if filepath.Clean(source.path) == filepath.Clean(want) {
			return
		}
	}
	t.Fatalf("expected source path %q in %#v", want, sources)
}

func assertLacksSourcePath(t *testing.T, sources []slashCommandSource, unwanted string) {
	t.Helper()
	for _, source := range sources {
		if filepath.Clean(source.path) == filepath.Clean(unwanted) {
			t.Fatalf("did not expect source path %q in %#v", unwanted, sources)
		}
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %q failed: %v", path, err)
	}
}

func containsSlashCommand(entries []protocol.SlashCommandEntry, name string) bool {
	for _, entry := range entries {
		if entry.Name == name {
			return true
		}
	}
	return false
}
