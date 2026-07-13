package remote

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestExecuteSessionActionPausesIdleChild(t *testing.T) {
	parent := &Service{}
	parent.cfg.Management = true

	child := &Service{
		running:   true,
		sessionID: "session-1",
		done:      make(chan struct{}),
	}
	parent.children = []*Service{child}

	resp := parent.executeSessionAction(protocol.SessionActionPayload{
		SessionID: "session-1",
		Action:    protocol.SessionActionPause,
	})

	if resp.Error != "" {
		t.Fatalf("expected pause to succeed, got error %q", resp.Error)
	}
	if got := child.SessionID(); got != "" {
		t.Fatalf("expected child session to stop, got session id %q", got)
	}
	if found := parent.findChildBySessionID("session-1"); found != nil {
		t.Fatal("expected child to be detached after pause")
	}
}

func TestExecuteSessionActionRejectsThinkingChild(t *testing.T) {
	parent := &Service{}
	parent.cfg.Management = true

	child := &Service{
		running:   true,
		sessionID: "session-2",
		thinking:  true,
		done:      make(chan struct{}),
	}
	parent.children = []*Service{child}

	resp := parent.executeSessionAction(protocol.SessionActionPayload{
		SessionID: "session-2",
		Action:    protocol.SessionActionDelete,
	})

	if resp.Error == "" {
		t.Fatal("expected delete to be rejected while child is thinking")
	}
	if got := child.SessionID(); got != "session-2" {
		t.Fatalf("expected child session to remain active, got session id %q", got)
	}
}

func TestExecuteSessionActionInterruptsThinkingChild(t *testing.T) {
	adapter := &stubRuntimeAdapter{}
	parent := &Service{}
	parent.cfg.Management = true
	child := &Service{
		running:    true,
		sessionID:  "session-interrupt",
		thinking:   true,
		turnActive: true,
		adapter:    adapter,
		done:       make(chan struct{}),
	}
	parent.children = []*Service{child}

	resp := parent.executeSessionAction(protocol.SessionActionPayload{
		RequestID: "action-1",
		SessionID: "session-interrupt",
		Action:    protocol.SessionActionInterrupt,
	})

	if resp.Error != "" {
		t.Fatalf("expected interrupt to succeed, got error %q", resp.Error)
	}
	if !adapter.requestInterruptCalled {
		t.Fatal("expected runtime adapter interrupt to be called")
	}
	if resp.LifecycleState != protocol.SessionLifecycleInterrupting {
		t.Fatalf("expected interrupting lifecycle state, got %q", resp.LifecycleState)
	}
	if parent.findChildBySessionID("session-interrupt") != child {
		t.Fatal("interrupt must keep the child session attached")
	}
}

func TestExecuteSessionActionForceStopsThinkingChild(t *testing.T) {
	parent := &Service{}
	parent.cfg.Management = true
	child := &Service{
		running:   true,
		sessionID: "session-stop",
		thinking:  true,
		done:      make(chan struct{}),
	}
	parent.children = []*Service{child}

	resp := parent.executeSessionAction(protocol.SessionActionPayload{
		SessionID: "session-stop",
		Action:    protocol.SessionActionStop,
	})

	if resp.Error != "" {
		t.Fatalf("expected stop to succeed, got error %q", resp.Error)
	}
	if resp.LifecycleState != protocol.SessionLifecycleStopped {
		t.Fatalf("expected stopped lifecycle state, got %q", resp.LifecycleState)
	}
	if parent.findChildBySessionID("session-stop") != nil {
		t.Fatal("stopped child must be detached")
	}
}

func TestExecuteSessionActionDeletesTranscript(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/tmp/acw2a-delete-test"
	projectDir := filepath.Join(homeDir, ".claude", "projects", encodeClaudeProjectPath(cwd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir failed: %v", err)
	}

	transcriptPath := filepath.Join(projectDir, "session-3.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript failed: %v", err)
	}

	parent := &Service{}
	parent.cfg.Management = true
	child := &Service{
		running:    true,
		sessionID:  "session-3",
		currentDir: cwd,
		done:       make(chan struct{}),
	}
	parent.children = []*Service{child}

	resp := parent.executeSessionAction(protocol.SessionActionPayload{
		SessionID: "session-3",
		Action:    protocol.SessionActionDelete,
	})

	if resp.Error != "" {
		t.Fatalf("expected delete to succeed, got error %q", resp.Error)
	}
	if _, err := os.Stat(transcriptPath); !os.IsNotExist(err) {
		t.Fatalf("expected transcript to be removed, stat err=%v", err)
	}
}

func TestFindChildBySessionIDMatchesDisconnectedChild(t *testing.T) {
	parent := &Service{}
	parent.cfg.Management = true

	child := &Service{}
	child.cfg.SessionID = "session-offline"
	parent.children = []*Service{child}

	found := parent.findChildBySessionID("session-offline")
	if found != child {
		t.Fatal("expected disconnected child to be found by last known session id")
	}
}

func TestDetachChildBySessionIDRemovesDisconnectedChild(t *testing.T) {
	parent := &Service{}
	parent.cfg.Management = true

	child := &Service{}
	child.cfg.SessionID = "session-offline"
	parent.children = []*Service{child}

	parent.detachChildBySessionID("session-offline")

	if len(parent.children) != 0 {
		t.Fatalf("expected disconnected child to be removed, got %d children", len(parent.children))
	}
}
