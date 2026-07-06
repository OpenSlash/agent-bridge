package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexRolloutDisplayLinesEmitsAssistantOutputText(t *testing.T) {
	lines := codexRolloutDisplayLines(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello mobile"}]}}`)
	if len(lines) != 1 {
		t.Fatalf("expected 1 display line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"type":"assistant"`) || !strings.Contains(lines[0], `"hello mobile"`) {
		t.Fatalf("unexpected display line: %s", lines[0])
	}
}

func TestCodexRolloutDisplayLinesEmitsUserInputText(t *testing.T) {
	lines := codexRolloutDisplayLines(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello from tui"}]}}`)
	if len(lines) != 1 {
		t.Fatalf("expected 1 display line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"type":"user"`) || !strings.Contains(lines[0], `"hello from tui"`) {
		t.Fatalf("unexpected display line: %s", lines[0])
	}
}

func TestCodexRolloutDisplayLinesSkipsUnsupportedItems(t *testing.T) {
	if lines := codexRolloutDisplayLines(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"reasoning","text":"hidden"}]}}`); len(lines) != 0 {
		t.Fatalf("expected unsupported rollout item to be ignored, got %v", lines)
	}
}

func TestCodexRolloutLineMarksTaskComplete(t *testing.T) {
	if !codexRolloutLineMarksTaskComplete(`{"type":"event_msg","payload":{"type":"task_complete"}}`) {
		t.Fatal("expected task_complete event to be detected")
	}
	if codexRolloutLineMarksTaskComplete(`{"type":"event_msg","payload":{"type":"token_count"}}`) {
		t.Fatal("did not expect non-task-complete event to match")
	}
}

func TestAttachedCodexRolloutMirrorBeginTurnStartsAtEOF(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/tmp/project"
	runtimeSessionID, rolloutPath := writeCodexRolloutForTest(t, homeDir, "2026", "04", "16", "019d9999-c287-7ab0-9ace-2794ff4801f1", cwd)
	appendCodexRolloutLine(t, rolloutPath, `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old"}]}}`)

	mirror := NewAttachedCodexRolloutMirror(cwd)
	if err := mirror.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn returned error: %v", err)
	}
	if mirror.offset == 0 {
		t.Fatal("expected mirror offset to advance to EOF")
	}
	if mirror.path != rolloutPath {
		t.Fatalf("expected mirror path %q, got %q", rolloutPath, mirror.path)
	}
	if mirror.runtimeSessionID != runtimeSessionID {
		t.Fatalf("expected runtime session id %q, got %q", runtimeSessionID, mirror.runtimeSessionID)
	}
}

func TestAttachedCodexRolloutMirrorPollEmitsNewAssistantOutput(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := filepath.Clean("/tmp/project")
	_, rolloutPath := writeCodexRolloutForTest(t, homeDir, "2026", "04", "16", "019d9999-c287-7ab0-9ace-2794ff4801f1", cwd)
	mirror := NewAttachedCodexRolloutMirror(cwd)
	if err := mirror.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn returned error: %v", err)
	}

	appendCodexRolloutLine(t, rolloutPath, `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"new reply"}]}}`)

	var got []string
	if err := mirror.Poll(AttachedCodexRolloutMirrorHandlers{
		HandleAssistantLine: func(line string) error {
			got = append(got, line)
			return nil
		},
	}); err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	if len(got) != 1 || !strings.Contains(got[0], `"new reply"`) {
		t.Fatalf("expected new assistant output, got %v", got)
	}
}

func TestAttachedCodexRolloutMirrorPollEmitsNewUserInput(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := filepath.Clean("/tmp/project")
	_, rolloutPath := writeCodexRolloutForTest(t, homeDir, "2026", "04", "16", "019d9999-c287-7ab0-9ace-2794ff4801f1", cwd)
	mirror := NewAttachedCodexRolloutMirror(cwd)
	if err := mirror.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn returned error: %v", err)
	}

	appendCodexRolloutLine(t, rolloutPath, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"typed in tui"}]}}`)

	var got []string
	if err := mirror.Poll(AttachedCodexRolloutMirrorHandlers{
		HandleAssistantLine: func(line string) error {
			got = append(got, line)
			return nil
		},
	}); err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	if len(got) != 1 || !strings.Contains(got[0], `"type":"user"`) || !strings.Contains(got[0], `"typed in tui"`) {
		t.Fatalf("expected new user input, got %v", got)
	}
}

func TestAttachedCodexRolloutMirrorPollSignalsTaskComplete(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := filepath.Clean("/tmp/project")
	_, rolloutPath := writeCodexRolloutForTest(t, homeDir, "2026", "04", "16", "019d9999-c287-7ab0-9ace-2794ff4801f1", cwd)
	mirror := NewAttachedCodexRolloutMirror(cwd)
	if err := mirror.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn returned error: %v", err)
	}

	appendCodexRolloutLine(t, rolloutPath, `{"type":"event_msg","payload":{"type":"task_complete"}}`)

	handled := false
	if err := mirror.Poll(AttachedCodexRolloutMirrorHandlers{
		HandleTurnComplete: func() error {
			handled = true
			return nil
		},
	}); err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if !handled {
		t.Fatal("expected task_complete event to be handled")
	}
}

func appendCodexRolloutLine(t *testing.T, path, line string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open rollout for append failed: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString(strings.TrimSpace(line) + "\n"); err != nil {
		t.Fatalf("append rollout line failed: %v", err)
	}
}
