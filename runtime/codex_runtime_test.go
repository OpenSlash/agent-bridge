package remote

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestCodexSandboxPolicyShape(t *testing.T) {
	workspace := codexSandboxPolicy("/tmp/project", protocol.SandboxModeWorkspaceWrite)
	if got := getStringFromAnyMap(workspace, "type"); got != "workspaceWrite" {
		t.Fatalf("expected workspaceWrite policy, got %q", got)
	}
	if roots, ok := workspace["writableRoots"].([]string); !ok || len(roots) != 1 || roots[0] != "/tmp/project" {
		t.Fatalf("expected writableRoots to contain cwd, got %#v", workspace["writableRoots"])
	}
	if access, ok := workspace["readOnlyAccess"].(map[string]any); !ok || getStringFromAnyMap(access, "type") != "fullAccess" {
		t.Fatalf("expected fullAccess readOnlyAccess, got %#v", workspace["readOnlyAccess"])
	}

	readOnly := codexSandboxPolicy("/tmp/project", protocol.SandboxModeReadOnly)
	if got := getStringFromAnyMap(readOnly, "type"); got != "readOnly" {
		t.Fatalf("expected readOnly policy, got %q", got)
	}
	if access, ok := readOnly["access"].(map[string]any); !ok || getStringFromAnyMap(access, "type") != "fullAccess" {
		t.Fatalf("expected fullAccess read-only access, got %#v", readOnly["access"])
	}

	fullAccess := codexSandboxPolicy("/tmp/project", protocol.SandboxModeDangerFullAccess)
	if got := getStringFromAnyMap(fullAccess, "type"); got != "dangerFullAccess" {
		t.Fatalf("expected dangerFullAccess policy, got %q", got)
	}
}

func TestBootstrapCodexSessionUsesRuntimeSessionIDAndFallsBackToThreadStart(t *testing.T) {
	service := &Service{}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdinWriter.Close()
	defer stdoutReader.Close()

	var (
		mu      sync.Mutex
		methods []string
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdinReader)
		for scanner.Scan() {
			envelope, err := parseCodexRPCEnvelope(scanner.Bytes())
			if err != nil || !envelope.isRequest() {
				continue
			}

			var params map[string]any
			_ = json.Unmarshal(envelope.Params, &params)

			mu.Lock()
			methods = append(methods, envelope.Method)
			mu.Unlock()

			switch envelope.Method {
			case "initialize":
				_ = writeCodexRPCEnvelope(stdoutWriter, map[string]any{
					"id":     envelope.idString(),
					"result": map[string]any{},
				})
			case "thread/resume":
				if got := strings.TrimSpace(getString(params, "threadId")); got != "runtime-thread-1" {
					t.Errorf("expected runtime thread id, got %q", got)
				}
				_ = writeCodexRPCEnvelope(stdoutWriter, map[string]any{
					"id": envelope.idString(),
					"error": map[string]any{
						"code":    -32000,
						"message": "no rollout found for thread id runtime-thread-1",
					},
				})
			case "thread/start":
				_ = writeCodexRPCEnvelope(stdoutWriter, map[string]any{
					"id": envelope.idString(),
					"result": map[string]any{
						"thread": map[string]any{
							"id":  "runtime-thread-2",
							"cwd": "/tmp/project",
						},
						"model": "gpt-5-codex",
						"cwd":   "/tmp/project",
					},
				})
			case "thread/read":
				_ = writeCodexRPCEnvelope(stdoutWriter, map[string]any{
					"id": envelope.idString(),
					"result": map[string]any{
						"thread": map[string]any{
							"id":        "runtime-thread-2",
							"cwd":       "/tmp/project",
							"createdAt": 1710000000,
							"turns":     []any{},
						},
					},
				})
				_ = stdoutWriter.Close()
				return
			}
		}
	}()

	bootstrap, err := service.bootstrapCodexSession(
		stdinWriter,
		bufio.NewReader(stdoutReader),
		"relay-session-1",
		"runtime-thread-1",
		"/tmp/project",
		"gpt-5-codex",
		"default",
		protocol.SandboxModeWorkspaceWrite,
		true,
	)
	if err != nil {
		t.Fatalf("bootstrapCodexSession returned error: %v", err)
	}
	if got := bootstrap.ThreadID; got != "runtime-thread-2" {
		t.Fatalf("expected fallback thread id, got %q", got)
	}
	if got := bootstrap.Cwd; got != "/tmp/project" {
		t.Fatalf("expected cwd to be preserved, got %q", got)
	}
	if got := bootstrap.Model; got != "gpt-5-codex" {
		t.Fatalf("expected model to be preserved, got %q", got)
	}

	<-done

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(methods, ","); got != "initialize,thread/resume,thread/start,thread/read" {
		t.Fatalf("unexpected request order: %s", got)
	}
}

func TestCodexNotificationThreadID(t *testing.T) {
	raw := json.RawMessage(`{"threadId":"thread-1","status":{"type":"active"}}`)
	if got := codexNotificationThreadID(raw); got != "thread-1" {
		t.Fatalf("expected direct thread id, got %q", got)
	}

	raw = json.RawMessage(`{"thread":{"id":"thread-2"}}`)
	if got := codexNotificationThreadID(raw); got != "thread-2" {
		t.Fatalf("expected nested thread id, got %q", got)
	}
}

func TestCodexObservedTurnSnapshotDecision(t *testing.T) {
	service := &Service{}
	service.beginCodexObservedTurn("thread-1")
	if got := service.finishCodexObservedTurn("thread-1"); got != "thread-1" {
		t.Fatalf("expected snapshot sync for silent turn, got %q", got)
	}

	service.beginCodexObservedTurn("thread-2")
	service.markCodexObservedLiveOutput()
	if got := service.finishCodexObservedTurn("thread-2"); got != "" {
		t.Fatalf("expected no snapshot sync when live output exists, got %q", got)
	}
}

func TestFilterUnsyncedCodexHistory(t *testing.T) {
	service := &Service{}
	service.markCodexHistorySynced("session-1", []protocol.SessionHistoryMessage{{
		SourceID: "msg-1",
		Role:     "assistant",
		Content:  "hello",
	}})

	filtered := service.filterUnsyncedCodexHistory("session-1", []protocol.SessionHistoryMessage{
		{SourceID: "msg-1", Role: "assistant", Content: "hello"},
		{SourceID: "msg-2", Role: "user", Content: "world"},
	})
	if len(filtered) != 1 {
		t.Fatalf("expected 1 unsynced message, got %d", len(filtered))
	}
	if filtered[0].SourceID != "msg-2" {
		t.Fatalf("expected msg-2 to remain, got %+v", filtered[0])
	}

	otherSessionFiltered := service.filterUnsyncedCodexHistory("session-2", []protocol.SessionHistoryMessage{
		{SourceID: "msg-1", Role: "assistant", Content: "hello"},
	})
	if len(otherSessionFiltered) != 1 {
		t.Fatalf("expected same message to sync for a different relay session, got %d", len(otherSessionFiltered))
	}
}

func TestCodexHistoryMessageDisplayLinesUser(t *testing.T) {
	userLines := codexHistoryMessageDisplayLines(protocol.SessionHistoryMessage{
		Role:    "user",
		Content: "hello",
	}, true)
	if len(userLines) != 1 {
		t.Fatalf("expected 1 user display line, got %d", len(userLines))
	}
	if !strings.Contains(userLines[0], `"type":"user"`) {
		t.Fatalf("expected user display line, got %s", userLines[0])
	}
	if lines := codexHistoryMessageDisplayLines(protocol.SessionHistoryMessage{
		Role:    "user",
		Content: "hello",
	}, false); len(lines) != 0 {
		t.Fatalf("expected user display lines to be suppressed, got %d", len(lines))
	}
}

func TestCodexHistoryMessageDisplayLinesAssistant(t *testing.T) {
	assistantLines := codexHistoryMessageDisplayLines(protocol.SessionHistoryMessage{
		Role:    "assistant",
		Content: "hi",
	}, true)
	if len(assistantLines) != 3 {
		t.Fatalf("expected 3 assistant display lines, got %d", len(assistantLines))
	}
	if !strings.Contains(assistantLines[0], `"type":"content_block_start"`) {
		t.Fatalf("expected assistant start event, got %s", assistantLines[0])
	}
	if !strings.Contains(assistantLines[1], `"type":"content_block_delta"`) {
		t.Fatalf("expected assistant delta event, got %s", assistantLines[1])
	}
	if !strings.Contains(assistantLines[1], `"text":"hi"`) {
		t.Fatalf("expected assistant delta text, got %s", assistantLines[1])
	}
	if !strings.Contains(assistantLines[2], `"type":"message_stop"`) {
		t.Fatalf("expected assistant stop event, got %s", assistantLines[2])
	}
}

func TestCodexHistoryMessageDisplayLinesToolCall(t *testing.T) {
	lines := codexHistoryMessageDisplayLines(protocol.SessionHistoryMessage{
		Role:           "assistant",
		MessageType:    "tool_call",
		ToolName:       "Bash",
		ToolCallID:     "tool-1",
		ToolInput:      `{"command":"pwd"}`,
		ToolResult:     "/tmp/project",
		IsToolComplete: true,
	}, true)
	if len(lines) != 4 {
		t.Fatalf("expected 4 tool call lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"type":"content_block_start"`) || !strings.Contains(lines[0], `"tool_use"`) {
		t.Fatalf("unexpected tool start line: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"input_json_delta"`) {
		t.Fatalf("unexpected tool input line: %s", lines[1])
	}
	if !strings.Contains(lines[3], `"tool_result"`) {
		t.Fatalf("unexpected tool result line: %s", lines[3])
	}
}

func TestCodexLiveAdapterTranslateNotificationDynamicToolLifecycle(t *testing.T) {
	adapter := newCodexLiveAdapter()

	started := codexRPCEnvelope{
		Method: "item/started",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"item": map[string]any{
				"type":     "dynamicToolCall",
				"id":       "tool-2",
				"toolName": "FetchSpec",
				"arguments": map[string]any{
					"package": "codex",
				},
			},
		})),
	}
	startedLines := adapter.TranslateNotification(started)
	if len(startedLines) != 3 {
		t.Fatalf("expected 3 started lines, got %d", len(startedLines))
	}
	if !strings.Contains(startedLines[0], `"type":"content_block_start"`) || !strings.Contains(startedLines[0], `"name":"FetchSpec"`) {
		t.Fatalf("unexpected tool start line: %s", startedLines[0])
	}
	if !strings.Contains(startedLines[1], `"input_json_delta"`) {
		t.Fatalf("unexpected tool input delta line: %s", startedLines[1])
	}
	if !strings.Contains(startedLines[2], `"type":"content_block_stop"`) {
		t.Fatalf("unexpected tool stop line: %s", startedLines[2])
	}

	completed := codexRPCEnvelope{
		Method: "item/completed",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"item": map[string]any{
				"type":     "dynamicToolCall",
				"id":       "tool-2",
				"toolName": "FetchSpec",
				"arguments": map[string]any{
					"package": "codex",
				},
				"contentItems": []any{
					map[string]any{"type": "inputText", "text": "ok"},
				},
			},
		})),
	}
	completedLines := adapter.TranslateNotification(completed)
	if len(completedLines) != 1 {
		t.Fatalf("expected 1 completed line after started event, got %d", len(completedLines))
	}
	if !strings.Contains(completedLines[0], `"tool_result"`) || !strings.Contains(completedLines[0], `"tool_use_id":"tool-2"`) {
		t.Fatalf("unexpected tool result line: %s", completedLines[0])
	}
	if !strings.Contains(completedLines[0], `"content":"ok"`) {
		t.Fatalf("expected extracted tool result text, got %s", completedLines[0])
	}
}

func TestCodexLiveAdapterTranslateNotificationCompletedToolWithoutStarted(t *testing.T) {
	adapter := newCodexLiveAdapter()

	completed := codexRPCEnvelope{
		Method: "item/completed",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"item": map[string]any{
				"type":     "mcpToolCall",
				"callId":   "mcp-1",
				"toolName": "ReadSpec",
				"arguments": map[string]any{
					"topic": "permissions",
				},
				"result": "ok",
			},
		})),
	}
	lines := adapter.TranslateNotification(completed)
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines for standalone completed tool call, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"type":"content_block_start"`) || !strings.Contains(lines[0], `"name":"ReadSpec"`) {
		t.Fatalf("unexpected standalone tool start line: %s", lines[0])
	}
	if !strings.Contains(lines[3], `"tool_result"`) || !strings.Contains(lines[3], `"tool_use_id":"mcp-1"`) {
		t.Fatalf("unexpected standalone tool result line: %s", lines[3])
	}
}

func TestCodexLiveAdapterTranslateNotificationCommandOutputDeltaBuffered(t *testing.T) {
	adapter := newCodexLiveAdapter()

	first := codexRPCEnvelope{
		Method: "item/commandExecution/outputDelta",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"itemId": "tool-1",
			"delta":  "hello",
		})),
	}
	if lines := adapter.TranslateNotification(first); len(lines) != 0 {
		t.Fatalf("expected no emitted lines before newline, got %+v", lines)
	}

	second := codexRPCEnvelope{
		Method: "item/commandExecution/outputDelta",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"itemId": "tool-1",
			"delta":  " world\nsecond line\ntrailing",
		})),
	}
	lines := adapter.TranslateNotification(second)
	if len(lines) != 2 {
		t.Fatalf("expected 2 flushed lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"type":"system"`) || !strings.Contains(lines[0], `hello world`) {
		t.Fatalf("unexpected first output line: %s", lines[0])
	}
	if !strings.Contains(lines[1], `second line`) {
		t.Fatalf("unexpected second output line: %s", lines[1])
	}

	completed := codexRPCEnvelope{
		Method: "item/completed",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"item": map[string]any{
				"type":             "commandExecution",
				"id":               "tool-1",
				"command":          "echo hi",
				"aggregatedOutput": "done",
			},
		})),
	}
	lines = adapter.TranslateNotification(completed)
	if len(lines) != 5 {
		t.Fatalf("expected buffered tail plus tool call completion, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `trailing`) {
		t.Fatalf("expected trailing buffered output before completion, got %s", lines[0])
	}
	if !strings.Contains(lines[4], `"tool_result"`) {
		t.Fatalf("expected tool result event at completion, got %s", lines[4])
	}
}

func TestCodexLiveAdapterTranslateNotificationTerminalInteraction(t *testing.T) {
	adapter := newCodexLiveAdapter()
	lines := adapter.TranslateNotification(codexRPCEnvelope{
		Method: "item/commandExecution/terminalInteraction",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"message": "Proceed?",
		})),
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 terminal interaction line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"subtype":"local_command"`) || !strings.Contains(lines[0], `Proceed?`) {
		t.Fatalf("unexpected terminal interaction line: %s", lines[0])
	}
}

func TestCodexLiveAdapterTranslateNotificationMcpProgress(t *testing.T) {
	adapter := newCodexLiveAdapter()
	lines := adapter.TranslateNotification(codexRPCEnvelope{
		Method: "item/mcpToolCall/progress",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"message": "Searching docs",
		})),
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 progress line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"type":"system"`) || !strings.Contains(lines[0], `Searching docs`) {
		t.Fatalf("unexpected progress line: %s", lines[0])
	}
}

func TestCodexLiveAdapterTranslateNotificationPlanUpdated(t *testing.T) {
	adapter := newCodexLiveAdapter()
	lines := adapter.TranslateNotification(codexRPCEnvelope{
		Method: "turn/plan/updated",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"explanation": "Next steps",
			"plan": []any{
				map[string]any{"step": "Inspect thread state", "status": "completed"},
				map[string]any{"step": "Bridge output deltas", "status": "in_progress"},
			},
		})),
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 plan line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `Next steps`) || !strings.Contains(lines[0], `[in_progress] Bridge output deltas`) {
		t.Fatalf("unexpected plan update line: %s", lines[0])
	}

	duplicate := adapter.TranslateNotification(codexRPCEnvelope{
		Method: "turn/plan/updated",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"explanation": "Next steps",
			"plan": []any{
				map[string]any{"step": "Inspect thread state", "status": "completed"},
				map[string]any{"step": "Bridge output deltas", "status": "in_progress"},
			},
		})),
	})
	if len(duplicate) != 0 {
		t.Fatalf("expected duplicate plan update to be suppressed, got %+v", duplicate)
	}
}

func TestCodexLiveAdapterTranslateNotificationReasoningSummaryPartAdded(t *testing.T) {
	adapter := newCodexLiveAdapter()
	lines := adapter.TranslateNotification(codexRPCEnvelope{
		Method: "item/reasoning/summaryPartAdded",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"itemId": "reason-1",
			"part": map[string]any{
				"type": "text",
				"text": "Need to inspect PTY bridge first.",
			},
		})),
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 reasoning line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `Reasoning: Need to inspect PTY bridge first.`) {
		t.Fatalf("unexpected reasoning line: %s", lines[0])
	}
}

func TestCodexLiveAdapterTranslateNotificationUserMessage(t *testing.T) {
	adapter := newCodexLiveAdapter()

	started := adapter.TranslateNotification(codexRPCEnvelope{
		Method: "item/started",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"item": map[string]any{
				"type": "userMessage",
				"id":   "user-1",
				"content": []any{
					map[string]any{"type": "text", "text": "hi from tui"},
				},
			},
		})),
	})
	if len(started) != 1 {
		t.Fatalf("expected 1 user line, got %d", len(started))
	}
	if !strings.Contains(started[0], `"type":"user"`) || !strings.Contains(started[0], `hi from tui`) {
		t.Fatalf("unexpected user line: %s", started[0])
	}

	completed := adapter.TranslateNotification(codexRPCEnvelope{
		Method: "item/completed",
		Params: json.RawMessage(marshalCompactJSON(map[string]any{
			"item": map[string]any{
				"type": "userMessage",
				"id":   "user-1",
				"content": []any{
					map[string]any{"type": "text", "text": "hi from tui"},
				},
			},
		})),
	})
	if len(completed) != 0 {
		t.Fatalf("expected duplicate completed user item to be suppressed, got %+v", completed)
	}
}

func TestBuildCodexApprovalResults(t *testing.T) {
	if got, _ := buildCodexExecCommandApprovalResult(protocol.PermissionResponsePayload{
		Decision: protocol.PermissionDecisionApproved,
	}).(map[string]any); got["decision"] != "accept" {
		t.Fatalf("expected exec approval accept, got %#v", got)
	}
	if got, _ := buildCodexExecCommandApprovalResult(protocol.PermissionResponsePayload{
		Decision: protocol.PermissionDecisionDenied,
	}).(map[string]any); got["decision"] != "decline" {
		t.Fatalf("expected exec approval decline, got %#v", got)
	}
	if got, _ := buildCodexApplyPatchApprovalResult(protocol.PermissionResponsePayload{
		Decision: protocol.PermissionDecisionAbort,
	}).(map[string]any); got["decision"] != "cancel" {
		t.Fatalf("expected patch approval cancel, got %#v", got)
	}
}

func getStringFromAnyMap(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}
