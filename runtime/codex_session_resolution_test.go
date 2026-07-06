package remote

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFindRecentCodexRolloutSession(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/Users/jairoguo/develops/Projects/acw2a"
	oldID, oldPath := writeCodexRolloutForTest(t, homeDir, "2026", "04", "15", "019d85e6-ef6c-7e52-87b1-ea1ae4cfee28", cwd)
	newID, newPath := writeCodexRolloutForTest(t, homeDir, "2026", "04", "16", "019d942f-c287-7ab0-9ace-2794ff4801f1", cwd)

	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old rollout failed: %v", err)
	}

	gotID, gotPath, ok, err := findRecentCodexRolloutSession(cwd, 30*time.Minute)
	if err != nil {
		t.Fatalf("find recent Codex rollout returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected a recent Codex rollout to be found")
	}
	if gotID != newID || gotPath != newPath {
		t.Fatalf("unexpected rollout match: got (%q, %q) want (%q, %q)", gotID, gotPath, newID, newPath)
	}
	if gotID == oldID {
		t.Fatalf("expected latest rollout to win, got stale rollout %q", gotID)
	}
}

func TestResolveCodexSessionStartKeepsRequestedSessionWhenRequestedResumeRolloutMissing(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/Users/jairoguo/develops/Projects/acw2a"
	writeCodexRolloutForTest(t, homeDir, "2026", "04", "16", "019d942f-c287-7ab0-9ace-2794ff4801f1", cwd)

	requestedID := "019d9999-c287-7ab0-9ace-2794ff4801f1"
	resolution, err := ResolveCodexSessionStart(requestedID, cwd, true)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if resolution.RuntimeSessionID != requestedID {
		t.Fatalf("expected requested runtime session %q, got %q", requestedID, resolution.RuntimeSessionID)
	}
	if resolution.Resume {
		t.Fatal("expected resume=false when requested rollout is unavailable")
	}
	if resolution.ReplacedRequestedSession {
		t.Fatal("did not expect requested runtime session id to be remapped")
	}
	if resolution.RolloutPath != "" {
		t.Fatalf("expected empty rollout path, got %q", resolution.RolloutPath)
	}
}

func TestResolveCodexSessionStartKeepsRequestedResumeWhenRolloutExists(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/Users/jairoguo/develops/Projects/acw2a"
	requestedID, requestedPath := writeCodexRolloutForTest(t, homeDir, "2026", "04", "16", "019d942f-c287-7ab0-9ace-2794ff4801f1", cwd)

	resolution, err := ResolveCodexSessionStart(requestedID, cwd, true)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if resolution.RuntimeSessionID != requestedID {
		t.Fatalf("expected runtime session %q, got %q", requestedID, resolution.RuntimeSessionID)
	}
	if !resolution.Resume {
		t.Fatal("expected resume=true")
	}
	if resolution.ReplacedRequestedSession {
		t.Fatal("did not expect requested runtime session to be remapped")
	}
	if resolution.RolloutPath != requestedPath {
		t.Fatalf("expected rollout path %q, got %q", requestedPath, resolution.RolloutPath)
	}
}

func TestResolveCodexSessionStartAdoptsRecentRolloutWhenStartingWithoutRuntimeSessionID(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/Users/jairoguo/develops/Projects/acw2a"
	rolloutID, rolloutPath := writeCodexRolloutForTest(t, homeDir, "2026", "04", "16", "019d942f-c287-7ab0-9ace-2794ff4801f1", cwd)

	resolution, err := ResolveCodexSessionStart("", cwd, false)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if resolution.RuntimeSessionID != rolloutID {
		t.Fatalf("expected rollout session %q, got %q", rolloutID, resolution.RuntimeSessionID)
	}
	if !resolution.Resume {
		t.Fatal("expected auto-adopted rollout to set resume=true")
	}
	if !resolution.AdoptedRecentSession {
		t.Fatal("expected rollout to be marked as adopted")
	}
	if resolution.RolloutPath != rolloutPath {
		t.Fatalf("expected rollout path %q, got %q", rolloutPath, resolution.RolloutPath)
	}
}

func TestListCodexLocalSessionsIncludesOlderProviderHiddenSessions(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/Users/jairoguo/works/develop/YY/Operation/wechat"
	oldID, oldPath := writeCodexRolloutForTest(t, homeDir, "2026", "03", "23", "019d1afc-5919-7c82-8269-f976eaa48af6", cwd)
	newID, newPath := writeCodexRolloutForTest(t, homeDir, "2026", "04", "30", "019ddc55-7627-7ac3-9a71-b621d3bae978", cwd)
	otherID, _ := writeCodexRolloutForTest(t, homeDir, "2026", "04", "30", "019d89d8-e8f2-7813-b629-990ac5421569", "/tmp/other")

	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old rollout failed: %v", err)
	}
	newTime := time.Now()
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatalf("chtimes new rollout failed: %v", err)
	}

	sessions, err := ListCodexLocalSessions(cwd, false)
	if err != nil {
		t.Fatalf("list local sessions returned error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 cwd-scoped sessions, got %d: %+v", len(sessions), sessions)
	}
	if sessions[0].RuntimeSessionID != newID || sessions[1].RuntimeSessionID != oldID {
		t.Fatalf("expected newest session first, got %+v", sessions)
	}
	if sessions[0].RuntimeSessionID == otherID || sessions[1].RuntimeSessionID == otherID {
		t.Fatalf("did not expect other cwd session in scoped list: %+v", sessions)
	}

	allSessions, err := ListCodexLocalSessions(cwd, true)
	if err != nil {
		t.Fatalf("list all local sessions returned error: %v", err)
	}
	if len(allSessions) != 3 {
		t.Fatalf("expected all 3 sessions, got %d: %+v", len(allSessions), allSessions)
	}
}

func writeCodexRolloutForTest(t *testing.T, homeDir, year, month, day, runtimeSessionID, cwd string) (string, string) {
	t.Helper()

	rolloutDir := filepath.Join(homeDir, ".codex", "sessions", year, month, day)
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatalf("mkdir rollout dir failed: %v", err)
	}

	path := filepath.Join(rolloutDir, "rollout-"+year+"-"+month+"-"+day+"T10-47-31-"+runtimeSessionID+".jsonl")
	meta := map[string]any{
		"type": "session_meta",
		"payload": map[string]any{
			"id":  runtimeSessionID,
			"cwd": cwd,
		},
	}
	line, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal rollout meta failed: %v", err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
		t.Fatalf("write rollout failed: %v", err)
	}

	return runtimeSessionID, path
}
