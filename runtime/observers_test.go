package remote

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestWriteJSONNotifiesObserversWithPlainPayload(t *testing.T) {
	service := NewService()
	service.localMode = true
	service.sink = func(msg protocol.Message) error {
		return nil
	}

	var observed protocol.Message
	remove := service.AddMessageObserver(func(msg protocol.Message) error {
		observed = msg
		return nil
	})
	defer remove()

	err := service.writeJSON(protocol.Message{
		Type:      protocol.TypeKeepalive,
		SessionID: "session-1",
		Payload: protocol.KeepalivePayload{
			Model: "claude-sonnet",
		},
	})
	if err != nil {
		t.Fatalf("writeJSON returned error: %v", err)
	}

	if observed.Type != protocol.TypeKeepalive {
		t.Fatalf("expected keepalive, got %q", observed.Type)
	}
	payload, ok := observed.Payload.(protocol.KeepalivePayload)
	if !ok {
		t.Fatalf("expected keepalive payload, got %T", observed.Payload)
	}
	if payload.Model != "claude-sonnet" {
		t.Fatalf("expected plain payload model, got %q", payload.Model)
	}
}

func TestEmitTerminalTextNotifiesObservers(t *testing.T) {
	service := NewService()
	service.running = true
	service.sessionID = "session-1"
	service.localMode = true
	service.sink = func(msg protocol.Message) error {
		return nil
	}

	var observed protocol.Message
	service.AddMessageObserver(func(msg protocol.Message) error {
		observed = msg
		return nil
	})

	if err := service.EmitTerminalText("hello"); err != nil {
		t.Fatalf("EmitTerminalText returned error: %v", err)
	}

	if observed.Type != protocol.TypeText {
		t.Fatalf("expected text message, got %q", observed.Type)
	}
	payload, ok := observed.Payload.(protocol.TextPayload)
	if !ok {
		t.Fatalf("expected text payload, got %T", observed.Payload)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(payload.Data), &data); err != nil {
		t.Fatalf("failed to parse payload data: %v", err)
	}
	if data["type"] != "system" || data["content"] != "hello" {
		t.Fatalf("unexpected payload data: %+v", data)
	}
}

func TestStopWithReasonNotifiesObservers(t *testing.T) {
	service := NewService()
	service.running = true
	service.sessionID = "session-1"
	service.done = make(chan struct{})
	service.localMode = true
	service.sink = func(msg protocol.Message) error {
		return nil
	}

	var lifecycle protocol.SessionLifecyclePayload
	service.AddMessageObserver(func(msg protocol.Message) error {
		if msg.Type != protocol.TypeSessionLifecycle {
			return nil
		}
		payload, ok := msg.Payload.(protocol.SessionLifecyclePayload)
		if ok {
			lifecycle = payload
		}
		return nil
	})

	if err := service.StopWithReason("unit-test"); err != nil {
		t.Fatalf("StopWithReason returned error: %v", err)
	}

	if lifecycle.State != "stopped" {
		t.Fatalf("expected stopped lifecycle, got %q", lifecycle.State)
	}
	if lifecycle.Reason != "unit-test" {
		t.Fatalf("expected reason unit-test, got %q", lifecycle.Reason)
	}
}

func TestFinishAttachedTurnFromResultEmitsTurnEndStatus(t *testing.T) {
	tests := []struct {
		name               string
		interruptRequested bool
		event              map[string]any
		want               string
	}{
		{
			name:  "completed",
			event: map[string]any{"subtype": "success"},
			want:  protocol.TurnCompleted,
		},
		{
			name:  "failed",
			event: map[string]any{"subtype": "error_during_execution", "is_error": true},
			want:  protocol.TurnFailed,
		},
		{
			name:               "cancelled",
			interruptRequested: true,
			event:              map[string]any{"subtype": "error_during_execution", "is_error": true},
			want:               protocol.TurnCancelled,
		},
	}

	for _, tt := range tests {
		service := NewService()
		service.running = true
		service.sessionID = "session-1"
		service.localMode = true
		service.sink = func(msg protocol.Message) error {
			return nil
		}
		service.turnActive = true
		service.thinking = true
		service.interruptRequested = tt.interruptRequested

		var observed protocol.Message
		service.AddMessageObserver(func(msg protocol.Message) error {
			observed = msg
			return nil
		})

		if err := service.FinishAttachedTurnFromResult(tt.event); err != nil {
			t.Fatalf("%s: FinishAttachedTurnFromResult returned error: %v", tt.name, err)
		}

		if observed.Type != protocol.TypeTurnEnd {
			t.Fatalf("%s: expected turn-end message, got %q", tt.name, observed.Type)
		}
		payload, ok := observed.Payload.(protocol.TurnEndPayload)
		if !ok {
			t.Fatalf("%s: expected turn-end payload, got %T", tt.name, observed.Payload)
		}
		if payload.Status != tt.want {
			t.Fatalf("%s: expected status %q, got %q", tt.name, tt.want, payload.Status)
		}
	}
}

func TestMarkDisconnectedNotifiesReconnectingWhenAutoReconnectEnabled(t *testing.T) {
	service := NewService()
	service.running = true
	service.autoReconnect = true
	service.sessionID = "session-1"
	service.done = make(chan struct{})
	service.cfg = Config{
		Command:    "/path/that/does/not/exist",
		WorkingDir: t.TempDir(),
	}
	service.currentDir = service.cfg.WorkingDir
	service.currentPermissionMode = protocol.PermissionModeDefault
	service.currentSandboxMode = protocol.SandboxModeWorkspaceWrite

	lifecycleCh := make(chan protocol.SessionLifecyclePayload, 1)
	service.AddMessageObserver(func(msg protocol.Message) error {
		if msg.Type != protocol.TypeSessionLifecycle {
			return nil
		}
		payload, ok := msg.Payload.(protocol.SessionLifecyclePayload)
		if ok {
			select {
			case lifecycleCh <- payload:
			default:
			}
		}
		return nil
	})

	service.markDisconnected()
	defer service.CancelReconnects()

	select {
	case lifecycle := <-lifecycleCh:
		if lifecycle.State != protocol.SessionLifecycleReconnecting {
			t.Fatalf("expected reconnecting lifecycle, got %q", lifecycle.State)
		}
		if lifecycle.Reason != "connection-lost" {
			t.Fatalf("expected connection-lost reason, got %q", lifecycle.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lifecycle event")
	}
}

func TestMarkDisconnectedNotifiesReconnectingForManagementAutoReconnect(t *testing.T) {
	service := NewService()
	service.running = true
	service.autoReconnect = true
	service.sessionID = "manager-1"
	service.done = make(chan struct{})
	service.cfg = Config{
		Management: true,
		ServerURL:  "http://127.0.0.1:1",
		WorkingDir: t.TempDir(),
	}
	service.currentDir = service.cfg.WorkingDir
	service.currentPermissionMode = protocol.PermissionModeDefault
	service.currentSandboxMode = protocol.SandboxModeWorkspaceWrite

	lifecycleCh := make(chan protocol.SessionLifecyclePayload, 1)
	service.AddMessageObserver(func(msg protocol.Message) error {
		if msg.Type != protocol.TypeSessionLifecycle {
			return nil
		}
		payload, ok := msg.Payload.(protocol.SessionLifecyclePayload)
		if ok {
			select {
			case lifecycleCh <- payload:
			default:
			}
		}
		return nil
	})

	service.markDisconnected()
	defer service.CancelReconnects()

	select {
	case lifecycle := <-lifecycleCh:
		if lifecycle.State != protocol.SessionLifecycleReconnecting {
			t.Fatalf("expected reconnecting lifecycle, got %q", lifecycle.State)
		}
		if lifecycle.Reason != "connection-lost" {
			t.Fatalf("expected connection-lost reason, got %q", lifecycle.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lifecycle event")
	}
}

func TestMarkDisconnectedNotifiesStoppedWithConnectionLostWhenAutoReconnectDisabled(t *testing.T) {
	service := NewService()
	service.running = true
	service.autoReconnect = false
	service.sessionID = "session-1"
	service.done = make(chan struct{})
	service.cfg = Config{
		Command:    "/path/that/does/not/exist",
		WorkingDir: t.TempDir(),
	}
	service.currentDir = service.cfg.WorkingDir
	service.currentPermissionMode = protocol.PermissionModeDefault
	service.currentSandboxMode = protocol.SandboxModeWorkspaceWrite

	lifecycleCh := make(chan protocol.SessionLifecyclePayload, 1)
	service.AddMessageObserver(func(msg protocol.Message) error {
		if msg.Type != protocol.TypeSessionLifecycle {
			return nil
		}
		payload, ok := msg.Payload.(protocol.SessionLifecyclePayload)
		if ok {
			select {
			case lifecycleCh <- payload:
			default:
			}
		}
		return nil
	})

	service.markDisconnected()

	select {
	case lifecycle := <-lifecycleCh:
		if lifecycle.State != "stopped" {
			t.Fatalf("expected stopped lifecycle, got %q", lifecycle.State)
		}
		if lifecycle.Reason != "connection-lost" {
			t.Fatalf("expected connection-lost reason, got %q", lifecycle.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lifecycle event")
	}
}

func TestEmitAttachedPermissionRequestAndClear(t *testing.T) {
	service := NewService()
	service.running = true
	service.sessionID = "session-1"
	service.localMode = true
	service.sink = func(msg protocol.Message) error {
		return nil
	}
	service.attachedPermissionShadow = make(map[string]protocol.PermissionRequestPayload)
	service.allowedTools = make(map[string]struct{})
	service.allowedBashLiterals = make(map[string]struct{})
	service.allowedBashPrefixes = make(map[string]struct{})

	observed := make([]protocol.Message, 0, 2)
	service.AddMessageObserver(func(msg protocol.Message) error {
		observed = append(observed, msg)
		return nil
	})

	if err := service.EmitAttachedPermissionRequest("toolu_1", "Bash", map[string]any{"command": "pwd"}); err != nil {
		t.Fatalf("EmitAttachedPermissionRequest returned error: %v", err)
	}
	service.ClearAttachedPermissionRequest("toolu_1")

	if len(observed) != 2 {
		t.Fatalf("expected 2 observed messages, got %d", len(observed))
	}
	if observed[0].Type != protocol.TypePermissionRequest {
		t.Fatalf("expected first message to be permission-request, got %q", observed[0].Type)
	}
	if observed[1].Type != protocol.TypePermissionCleared {
		t.Fatalf("expected second message to be permission-cleared, got %q", observed[1].Type)
	}
}

func TestEmitAttachedPermissionRequestRestoresCodexWaitingState(t *testing.T) {
	service := NewService()
	service.running = true
	service.sessionID = "session-1"
	service.runtime = runtimeCodex
	service.turnActive = true
	service.thinking = true
	service.localMode = true
	service.sink = func(msg protocol.Message) error {
		return nil
	}
	service.attachedPermissionShadow = make(map[string]protocol.PermissionRequestPayload)
	service.allowedTools = make(map[string]struct{})
	service.allowedBashLiterals = make(map[string]struct{})
	service.allowedBashPrefixes = make(map[string]struct{})

	observed := make([]protocol.Message, 0, 2)
	service.AddMessageObserver(func(msg protocol.Message) error {
		observed = append(observed, msg)
		return nil
	})

	if err := service.EmitAttachedPermissionRequest("toolu_exec_1", "Bash", map[string]any{"command": "pwd"}); err != nil {
		t.Fatalf("EmitAttachedPermissionRequest returned error: %v", err)
	}

	if service.getThinking() {
		t.Fatal("expected codex thinking to pause while waiting for attached permission approval")
	}
	if len(observed) != 2 {
		t.Fatalf("expected permission-request and keepalive, got %d messages", len(observed))
	}
	if observed[0].Type != protocol.TypePermissionRequest {
		t.Fatalf("expected first message to be permission-request, got %q", observed[0].Type)
	}
	if observed[1].Type != protocol.TypeKeepalive {
		t.Fatalf("expected second message to be keepalive, got %q", observed[1].Type)
	}
}

func TestResolvePermissionResponseForAttachedShadowUsesAttachedHandler(t *testing.T) {
	service := NewService()
	service.running = true
	service.sessionID = "session-1"
	service.localMode = true
	service.sink = func(msg protocol.Message) error {
		return nil
	}
	service.attachedPermissionShadow = map[string]protocol.PermissionRequestPayload{
		"toolu_1": {
			RequestID: "toolu_1",
			Tool:      "Bash",
			Input:     map[string]any{"command": "pwd"},
			Summary:   "pwd",
		},
	}

	handled := false
	service.SetAttachedPermissionResponseHandler(func(resp protocol.PermissionResponsePayload) bool {
		handled = resp.RequestID == "toolu_1"
		return handled
	})

	observed := make([]protocol.Message, 0, 1)
	service.AddMessageObserver(func(msg protocol.Message) error {
		observed = append(observed, msg)
		return nil
	})

	service.resolvePermissionResponse(protocol.PermissionResponsePayload{
		RequestID: "toolu_1",
		Approved:  true,
		Decision:  protocol.PermissionDecisionApproved,
	})

	if !handled {
		t.Fatal("expected attached permission handler to be called")
	}
	if len(observed) != 1 {
		t.Fatalf("expected 1 observed message, got %d", len(observed))
	}
	if observed[0].Type != protocol.TypePermissionCleared {
		t.Fatalf("expected permission-cleared message, got %q", observed[0].Type)
	}
	if _, ok := service.attachedPermissionShadow["toolu_1"]; ok {
		t.Fatal("expected attached permission shadow to be cleared")
	}
}

func TestResolvePermissionResponseForAttachedShadowRestoresCodexThinking(t *testing.T) {
	service := NewService()
	service.running = true
	service.localMode = true
	service.sessionID = "session-1"
	service.runtime = runtimeCodex
	service.turnActive = true
	service.thinking = false
	service.sink = func(msg protocol.Message) error {
		return nil
	}
	service.attachedPermissionShadow = map[string]protocol.PermissionRequestPayload{
		"toolu_1": {
			RequestID: "toolu_1",
			Tool:      "Bash",
			Input:     map[string]any{"command": "pwd"},
			Summary:   "pwd",
		},
	}
	service.SetAttachedPermissionResponseHandler(func(resp protocol.PermissionResponsePayload) bool {
		return resp.RequestID == "toolu_1"
	})

	observed := make([]protocol.Message, 0, 2)
	service.AddMessageObserver(func(msg protocol.Message) error {
		observed = append(observed, msg)
		return nil
	})

	service.resolvePermissionResponse(protocol.PermissionResponsePayload{
		RequestID: "toolu_1",
		Approved:  true,
		Decision:  protocol.PermissionDecisionApproved,
	})

	if !service.getThinking() {
		t.Fatal("expected codex thinking to resume after attached permission response")
	}
	if len(observed) != 2 {
		t.Fatalf("expected permission-cleared and keepalive, got %d messages", len(observed))
	}
	if observed[0].Type != protocol.TypePermissionCleared {
		t.Fatalf("expected first message to be permission-cleared, got %q", observed[0].Type)
	}
	if observed[1].Type != protocol.TypeKeepalive {
		t.Fatalf("expected second message to be keepalive, got %q", observed[1].Type)
	}
}

func TestResolvePermissionResponseForPendingRestoresCodexThinking(t *testing.T) {
	service := NewService()
	service.running = true
	service.localMode = true
	service.sessionID = "session-1"
	service.runtime = runtimeCodex
	service.turnActive = true
	service.thinking = false
	service.sink = func(msg protocol.Message) error {
		return nil
	}
	responseCh := service.registerPendingPermission("session-1", "req-1")

	observed := make([]protocol.Message, 0, 1)
	service.AddMessageObserver(func(msg protocol.Message) error {
		observed = append(observed, msg)
		return nil
	})

	service.resolvePermissionResponse(protocol.PermissionResponsePayload{
		RequestID: "req-1",
		Approved:  true,
		Decision:  protocol.PermissionDecisionApproved,
	})

	resp, ok := <-responseCh
	if !ok || resp.RequestID != "req-1" {
		t.Fatalf("expected pending permission response to be delivered, got ok=%t resp=%+v", ok, resp)
	}
	if !service.getThinking() {
		t.Fatal("expected codex thinking to resume after pending permission response")
	}
	if len(observed) != 1 || observed[0].Type != protocol.TypeKeepalive {
		t.Fatalf("expected keepalive after resolving pending permission, got %+v", observed)
	}
}

func TestHasAttachedPermissionRequestReflectsShadowState(t *testing.T) {
	service := NewService()
	if service.HasAttachedPermissionRequest() {
		t.Fatal("expected empty service to report no attached permission request")
	}

	service.attachedPermissionShadow = map[string]protocol.PermissionRequestPayload{
		"toolu_1": {RequestID: "toolu_1"},
	}
	if !service.HasAttachedPermissionRequest() {
		t.Fatal("expected pending attached permission request to be reported")
	}

	service.attachedPermissionShadow = map[string]protocol.PermissionRequestPayload{}
	if service.HasAttachedPermissionRequest() {
		t.Fatal("expected cleared attached permission request state")
	}
}
