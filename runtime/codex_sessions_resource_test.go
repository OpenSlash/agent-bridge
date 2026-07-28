package remote

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestBuildCodexSessionsResponseFiltersAndSorts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	older := writeCodexSessionFixture(t, home, "2026/07/26", "thread-old", "/work/project", "0.144.0", time.Unix(100, 0))
	newer := writeCodexSessionFixture(t, home, "2026/07/27", "thread-new", "/work/project", "0.145.0", time.Unix(200, 0))
	_ = writeCodexSessionFixture(t, home, "2026/07/28", "thread-other", "/work/other", "0.145.0", time.Unix(300, 0))
	if err := os.Chtimes(older, time.Unix(100, 0), time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, time.Unix(200, 0), time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}

	resp, err := buildCodexSessionsResponse(protocol.ListCodexSessionsPayload{Cwd: "/work/project"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(resp.Sessions))
	}
	if resp.Sessions[0].RuntimeSessionID != "thread-new" || resp.Sessions[1].RuntimeSessionID != "thread-old" {
		t.Fatalf("unexpected order: %+v", resp.Sessions)
	}
	if resp.Sessions[0].CLIVersion != "0.145.0" || resp.Sessions[0].LineCount != 3 {
		t.Fatalf("unexpected session metadata: %+v", resp.Sessions[0])
	}
	if resp.Sessions[0].Preview != "Continue implementing the mobile session picker" {
		t.Fatalf("unexpected session preview: %q", resp.Sessions[0].Preview)
	}
}

func TestBuildCodexSessionsResponseIncludesAllAndLimits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCodexSessionFixture(t, home, "2026/07/27", "thread-a", "/work/a", "0.145.0", time.Now())
	writeCodexSessionFixture(t, home, "2026/07/28", "thread-b", "/work/b", "0.145.0", time.Now().Add(time.Minute))

	resp, err := buildCodexSessionsResponse(protocol.ListCodexSessionsPayload{IncludeAll: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Sessions) != 1 || resp.Sessions[0].RuntimeSessionID != "thread-b" {
		t.Fatalf("unexpected limited sessions: %+v", resp.Sessions)
	}
}

func writeCodexSessionFixture(t *testing.T, home, datePath, id, cwd, cliVersion string, timestamp time.Time) string {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", filepath.FromSlash(datePath))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+id+".jsonl")
	content := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"timestamp\":%q,\"cwd\":%q,\"originator\":\"codex_cli_rs\",\"cli_version\":%q,\"source\":\"cli\",\"model_provider\":\"openai\"}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"Initial task\"}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"Continue implementing the mobile session picker\"}}\n", id, timestamp.UTC().Format(time.RFC3339Nano), cwd, cliVersion)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	return path
}
