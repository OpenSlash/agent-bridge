package remote

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestAttachedCodexControllerShouldEmitSnapshotUserMessage(t *testing.T) {
	controller := &AttachedCodexController{}

	if !controller.shouldEmitSnapshotUserMessage(protocol.SessionHistoryMessage{
		Role:    "user",
		Content: "typed from tui",
	}) {
		t.Fatal("expected TUI-originated user message to be emitted")
	}

	controller.RecordForwardedInput("typed from mobile")
	if controller.shouldEmitSnapshotUserMessage(protocol.SessionHistoryMessage{
		Role:    "user",
		Content: "typed from mobile",
	}) {
		t.Fatal("expected forwarded mobile input to be suppressed once")
	}

	if !controller.shouldEmitSnapshotUserMessage(protocol.SessionHistoryMessage{
		Role:    "user",
		Content: "typed from mobile",
	}) {
		t.Fatal("expected forwarded input suppression to be consumed after one match")
	}
}

func TestAttachedCodexControllerPrunesExpiredForwardedInput(t *testing.T) {
	controller := &AttachedCodexController{
		forwardedInputs: []attachedCodexForwardedInput{{
			Content:    "stale",
			RecordedAt: time.Now().Add(-attachedCodexForwardedInputTTL - time.Second),
		}},
	}

	if !controller.shouldEmitSnapshotUserMessage(protocol.SessionHistoryMessage{
		Role:    "user",
		Content: "stale",
	}) {
		t.Fatal("expected expired forwarded input not to suppress snapshot user message")
	}
}

func TestAttachedCodexControllerHandlesLiveOutputDeltaNotifications(t *testing.T) {
	service := NewService()
	service.running = true
	service.localMode = true
	service.runtime = runtimeCodex
	service.sessionID = "session-1"
	service.turnActive = true
	service.thinking = true
	service.sink = func(msg protocol.Message) error {
		return nil
	}

	observed := make([]protocol.Message, 0, 2)
	service.AddMessageObserver(func(msg protocol.Message) error {
		observed = append(observed, msg)
		return nil
	})

	controller := NewAttachedCodexController(service, "ws://127.0.0.1:1", AttachedCodexControllerConfig{})
	controller.beginObservedTurn("thread-1")
	controller.handleNotification(codexRPCEnvelope{
		Method: "item/commandExecution/outputDelta",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"itemId": "tool-1",
			"delta":  "hello from tool\n",
		})),
	})

	if !controller.observedLiveOutput {
		t.Fatal("expected live output delta to mark observed turn output")
	}
	if len(observed) != 0 {
		t.Fatalf("expected local command stdout to be suppressed from session text, got %d messages", len(observed))
	}
}

func TestAttachedCodexControllerFinishObservedTurnStillSyncsAfterLiveOutput(t *testing.T) {
	controller := NewAttachedCodexController(nil, "ws://127.0.0.1:1", AttachedCodexControllerConfig{})

	controller.beginObservedTurn("thread-1")
	controller.markObservedLiveOutput()

	result := controller.finishObservedTurn("thread-1")
	if result.ThreadID != "thread-1" {
		t.Fatalf("expected snapshot sync thread to remain available, got %q", result.ThreadID)
	}
	if result.EmitSnapshotLines {
		t.Fatal("expected live-output turn to skip snapshot re-emission")
	}
}

func TestAttachedCodexControllerProxyPermissionResponseClearsMatchingRequestOnly(t *testing.T) {
	service := NewService()
	service.running = true
	service.localMode = true
	service.runtime = runtimeCodex
	service.sessionID = "session-1"
	service.turnActive = true
	service.thinking = false
	service.sink = func(msg protocol.Message) error { return nil }
	service.attachedPermissionShadow = map[string]protocol.PermissionRequestPayload{
		"req-1": {
			RequestID: "req-1",
			Tool:      "Bash",
			Input:     map[string]any{"command": "pwd"},
		},
	}

	controller := NewAttachedCodexController(service, "ws://127.0.0.1:1", AttachedCodexControllerConfig{})
	controller.pendingReqs["req-1"] = attachedCodexPendingRequest{
		RequestID:   "req-1",
		ToolName:    "Bash",
		Input:       map[string]any{"command": "pwd"},
		BuildResult: buildCodexExecCommandApprovalResult,
		ParseResult: parseCodexSimpleDecisionResult,
	}

	controller.handleProxyClientResponse(codexRPCEnvelope{
		ID:     json.RawMessage(`"other-req"`),
		Result: json.RawMessage(`{"decision":"accept"}`),
	})
	if _, ok := service.attachedPermissionShadow["req-1"]; !ok {
		t.Fatal("expected unrelated proxy response not to clear pending permission")
	}

	controller.handleProxyClientResponse(codexRPCEnvelope{
		ID:     json.RawMessage(`"req-1"`),
		Result: json.RawMessage(`{"decision":"acceptForSession"}`),
	})
	if _, ok := service.attachedPermissionShadow["req-1"]; ok {
		t.Fatal("expected matching proxy response to clear pending permission")
	}
	if !service.getThinking() {
		t.Fatal("expected matching proxy response to resume codex thinking")
	}
	if !service.shouldAutoApproveTool("Bash", map[string]any{"command": "pwd"}) {
		t.Fatal("expected session-scoped TUI approval to be remembered")
	}
}

func TestAttachedCodexControllerSuppressesApprovalRequestsToProxy(t *testing.T) {
	service := NewService()
	service.running = true
	service.localMode = true
	service.runtime = runtimeCodex
	service.sessionID = "session-1"
	service.sink = func(msg protocol.Message) error { return nil }

	controller := NewAttachedCodexController(service, "ws://127.0.0.1:1", AttachedCodexControllerConfig{})
	forward := controller.handleServerRequest(codexRPCEnvelope{
		ID:     json.RawMessage(`"req-1"`),
		Method: "applyPatchApproval",
		Params: json.RawMessage(`{"grantRoot":"/tmp/demo","reason":"need write"}`),
	})
	if forward {
		t.Fatal("expected approval request not to be forwarded to proxy TUI")
	}

	pending, ok := controller.pendingReqs["req-1"]
	if !ok {
		t.Fatal("expected approval request to be tracked for mobile resolution")
	}
	if pending.ForwardToProxy {
		t.Fatal("expected approval request to stay mobile-only")
	}
}
