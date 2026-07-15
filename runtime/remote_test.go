package remote

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenSlash/agent-bridge/protocol"

	"github.com/google/uuid"
)

func TestShouldAutoApproveTool(t *testing.T) {
	svc := &Service{
		currentPermissionMode: protocol.PermissionModeAcceptEdits,
		allowedTools:          make(map[string]struct{}),
		allowedBashLiterals:   make(map[string]struct{}),
		allowedBashPrefixes:   make(map[string]struct{}),
	}

	if !svc.shouldAutoApproveTool("Edit", map[string]any{"file_path": "README.md"}) {
		t.Fatal("acceptEdits should auto-approve edit tools")
	}
	if svc.shouldAutoApproveTool("Bash", map[string]any{"command": "pwd"}) {
		t.Fatal("acceptEdits should not auto-approve bash by default")
	}
	if svc.shouldAutoApproveTool("AskUserQuestion", map[string]any{"question": "继续吗？"}) {
		t.Fatal("AskUserQuestion should require an explicit response")
	}
	if svc.shouldAutoApproveTool("request_user_input", map[string]any{"question": "怎么处理？"}) {
		t.Fatal("request_user_input alias should require an explicit response")
	}

	svc.currentPermissionMode = protocol.PermissionModeBypassPermissions
	if !svc.shouldAutoApproveTool("Bash", map[string]any{"command": "pwd"}) {
		t.Fatal("bypassPermissions should auto-approve all tools")
	}
}

func TestSetCodexPermissionWaitingPausesAndResumesThinking(t *testing.T) {
	svc := &Service{
		runtime:    runtimeCodex,
		thinking:   true,
		turnActive: true,
	}

	if !svc.setCodexPermissionWaiting(true) {
		t.Fatal("expected codex permission wait to update state")
	}
	if svc.getThinking() {
		t.Fatal("expected thinking to pause while waiting for codex approval")
	}

	if svc.setCodexPermissionWaiting(false) {
		t.Fatal("expected codex approval exit to wait for follow-up runtime events")
	}
	if svc.getThinking() {
		t.Fatal("expected thinking to remain paused until codex emits a new status event")
	}
}

func TestSetCodexPermissionWaitingSkipsInactiveOrNonCodexSessions(t *testing.T) {
	claude := &Service{
		runtime:    runtimeClaude,
		thinking:   true,
		turnActive: true,
	}
	if claude.setCodexPermissionWaiting(true) {
		t.Fatal("expected non-codex session to ignore codex approval state toggles")
	}
	if !claude.getThinking() {
		t.Fatal("expected non-codex thinking state to remain unchanged")
	}

	idleCodex := &Service{
		runtime:    runtimeCodex,
		thinking:   false,
		turnActive: false,
	}
	if idleCodex.setCodexPermissionWaiting(false) {
		t.Fatal("expected inactive codex session to ignore approval resume")
	}
	if idleCodex.getThinking() {
		t.Fatal("expected inactive codex session to remain non-thinking")
	}
}

func TestRecordSessionPermission(t *testing.T) {
	svc := &Service{
		allowedTools:        make(map[string]struct{}),
		allowedBashLiterals: make(map[string]struct{}),
		allowedBashPrefixes: make(map[string]struct{}),
	}

	svc.recordSessionPermission("Bash", map[string]any{"command": "pwd"})
	if !svc.shouldAutoApproveTool("Bash", map[string]any{"command": "pwd"}) {
		t.Fatal("approved-for-session bash command should be auto-approved")
	}

	svc.recordSessionPermission("Read", map[string]any{"file_path": "README.md"})
	if !svc.shouldAutoApproveTool("Read", map[string]any{"file_path": "README.md"}) {
		t.Fatal("approved-for-session tool should be auto-approved")
	}
}

func TestSummarizePermissionRequest(t *testing.T) {
	if got := summarizePermissionRequest("Bash", map[string]any{"command": "pwd"}); got != "pwd" {
		t.Fatalf("unexpected bash summary: %q", got)
	}

	got := summarizePermissionRequest("Read", map[string]any{"file_path": "README.md"})
	if got == "" || got == "Read" {
		t.Fatalf("expected structured summary, got %q", got)
	}
}

func TestNormalizePermissionMode(t *testing.T) {
	if got := normalizePermissionMode(protocol.PermissionModeAcceptEdits); got != protocol.PermissionModeAcceptEdits {
		t.Fatalf("expected acceptEdits, got %q", got)
	}
	if got := normalizePermissionMode("receive"); got != protocol.PermissionModeDefault {
		t.Fatalf("expected default for legacy receive, got %q", got)
	}
	if got := normalizePermissionMode("bypass"); got != protocol.PermissionModeBypassPermissions {
		t.Fatalf("expected bypassPermissions, got %q", got)
	}
}

func TestRequiresPermissionModeRestart(t *testing.T) {
	if requiresPermissionModeRestart(runtimeClaude, protocol.PermissionModeDefault, protocol.PermissionModeAcceptEdits) {
		t.Fatal("default -> acceptEdits should hot-swap without restart")
	}
	if requiresPermissionModeRestart(runtimeClaude, protocol.PermissionModeDefault, protocol.PermissionModePlan) {
		t.Fatal("default -> plan should hot-swap without restart")
	}
	if requiresPermissionModeRestart(runtimeClaude, protocol.PermissionModePlan, protocol.PermissionModeDefault) {
		t.Fatal("plan -> default should hot-swap without restart")
	}
	if requiresPermissionModeRestart(runtimeCodex, protocol.PermissionModeDefault, protocol.PermissionModeDontAsk) {
		t.Fatal("codex default -> don't ask should hot-swap without restart")
	}
}

func TestEvaluateSessionConfigRestartsForModelSwitch(t *testing.T) {
	svc := &Service{
		currentDir:            "/tmp/project",
		currentModel:          "claude-sonnet-4-6",
		currentPermissionMode: protocol.PermissionModeDefault,
	}

	decision := svc.evaluateSessionConfig(protocol.SessionConfigPayload{
		Model:      "claude-opus-4-6",
		ApplyModel: true,
	})

	if !decision.ModelChanged {
		t.Fatal("expected model switch to be detected")
	}
	if !decision.NeedsRestart {
		t.Fatal("expected model switch to restart Claude")
	}
	if !decision.ResumeConversation {
		t.Fatal("expected model-only switch to resume the current conversation")
	}
	if decision.TargetModel != "claude-opus-4-6" {
		t.Fatalf("unexpected target model: %q", decision.TargetModel)
	}
}

func TestEvaluateSessionConfigCanResetToDefaultModel(t *testing.T) {
	svc := &Service{
		currentDir:            "/tmp/project",
		currentModel:          "claude-opus-4-6",
		currentPermissionMode: protocol.PermissionModeDefault,
	}

	decision := svc.evaluateSessionConfig(protocol.SessionConfigPayload{
		Model:      "",
		ApplyModel: true,
	})

	if !decision.ModelChanged {
		t.Fatal("expected explicit default-model reset to be detected")
	}
	if decision.TargetModel != "" {
		t.Fatalf("expected empty target model for default reset, got %q", decision.TargetModel)
	}
	if !decision.NeedsRestart || !decision.ResumeConversation {
		t.Fatalf("expected default-model reset to restart and resume, got %+v", decision)
	}
}

func TestEvaluateSessionConfigHotSwapsCodexSandboxSwitch(t *testing.T) {
	svc := &Service{
		runtime:               runtimeCodex,
		currentDir:            "/tmp/project",
		currentModel:          "gpt-5-codex",
		currentPermissionMode: protocol.PermissionModeDefault,
		currentSandboxMode:    protocol.SandboxModeWorkspaceWrite,
	}

	decision := svc.evaluateSessionConfig(protocol.SessionConfigPayload{
		SandboxMode: protocol.SandboxModeReadOnly,
	})

	if !decision.SandboxModeChanged {
		t.Fatal("expected sandbox switch to be detected")
	}
	if decision.SandboxModeNeedsRestart {
		t.Fatal("expected codex sandbox switch to hot-swap without restart")
	}
	if decision.NeedsRestart || decision.ResumeConversation {
		t.Fatalf("expected codex sandbox switch to avoid restart, got %+v", decision)
	}
}

func TestEvaluateSessionConfigHotSwapsCodexPermissionSwitch(t *testing.T) {
	svc := &Service{
		runtime:               runtimeCodex,
		currentDir:            "/tmp/project",
		currentModel:          "gpt-5-codex",
		currentPermissionMode: protocol.PermissionModeDefault,
		currentSandboxMode:    protocol.SandboxModeWorkspaceWrite,
	}

	decision := svc.evaluateSessionConfig(protocol.SessionConfigPayload{
		PermissionMode: protocol.PermissionModeDontAsk,
	})

	if !decision.PermissionModeChanged {
		t.Fatal("expected codex permission switch to be detected")
	}
	if decision.PermissionModeNeedsRestart {
		t.Fatal("expected codex permission switch to hot-swap without restart")
	}
	if decision.NeedsRestart || decision.ResumeConversation {
		t.Fatalf("expected codex permission switch to avoid restart, got %+v", decision)
	}
}

func TestUpdateAttachedSessionStatePreservesExistingValuesOnPartialUpdate(t *testing.T) {
	svc := &Service{
		runtime:               runtimeCodex,
		runtimeSessionID:      "thread-1",
		currentDir:            "/tmp/project",
		currentModel:          "gpt-5-codex",
		currentPermissionMode: protocol.PermissionModeBypassPermissions,
		currentSandboxMode:    protocol.SandboxModeDangerFullAccess,
		cfg: Config{
			RuntimeSessionID: "thread-1",
			WorkingDir:       "/tmp/project",
			Model:            "gpt-5-codex",
			PermissionMode:   protocol.PermissionModeBypassPermissions,
			SandboxMode:      protocol.SandboxModeDangerFullAccess,
		},
	}

	svc.UpdateAttachedSessionState(AttachedSessionStateUpdate{
		RuntimeSessionID: "thread-2",
	})

	if got := svc.RuntimeSessionID(); got != "thread-2" {
		t.Fatalf("expected runtime session id to update, got %q", got)
	}
	if got := svc.CurrentModel(); got != "gpt-5-codex" {
		t.Fatalf("expected model to be preserved, got %q", got)
	}
	if got := svc.CurrentPermissionMode(); got != protocol.PermissionModeBypassPermissions {
		t.Fatalf("expected permission mode to be preserved, got %q", got)
	}
	if got := svc.CurrentSandboxMode(); got != protocol.SandboxModeDangerFullAccess {
		t.Fatalf("expected sandbox mode to be preserved, got %q", got)
	}
}

func TestUpdateAttachedSessionStateCanExplicitlyResetModel(t *testing.T) {
	svc := &Service{
		runtime:      runtimeCodex,
		currentModel: "gpt-5-codex",
		cfg: Config{
			Model: "gpt-5-codex",
		},
	}

	svc.UpdateAttachedSessionState(AttachedSessionStateUpdate{
		Model:      "",
		ApplyModel: true,
	})

	if got := svc.CurrentModel(); got != "" {
		t.Fatalf("expected explicit model reset to clear current model, got %q", got)
	}
}

func TestReportedAgentVersionPrefersEmbeddingProductVersion(t *testing.T) {
	if got := reportedAgentVersion(&Config{AgentVersion: "1.4.32"}); got != "1.4.32" {
		t.Fatalf("expected embedding agent version, got %q", got)
	}
	if got := reportedAgentVersion(&Config{}); strings.TrimSpace(got) == "" {
		t.Fatal("expected bridge build version fallback")
	}
}

func TestTurnStatusForResult(t *testing.T) {
	if got := turnStatusForResult(false, map[string]any{"subtype": "success"}); got != protocol.TurnCompleted {
		t.Fatalf("expected completed for success result, got %q", got)
	}
	if got := turnStatusForResult(false, map[string]any{"subtype": "error_during_execution"}); got != protocol.TurnFailed {
		t.Fatalf("expected failed for error result, got %q", got)
	}
	if got := turnStatusForResult(true, map[string]any{"subtype": "error_during_execution", "is_error": true}); got != protocol.TurnCancelled {
		t.Fatalf("expected cancelled to override error result, got %q", got)
	}
}

func TestHandleControlResponseClearsFailedInterrupt(t *testing.T) {
	svc := &Service{
		interruptRequested: true,
		pendingInterrupts: map[string]struct{}{
			"interrupt-1": {},
		},
	}

	svc.handleControlResponse(sdkControlResponse{
		Response: sdkControlResponsePayload{
			RequestID: "interrupt-1",
			Subtype:   "error",
		},
	})

	if svc.interruptRequested {
		t.Fatal("failed interrupt response should clear interruptRequested")
	}
	if len(svc.pendingInterrupts) != 0 {
		t.Fatal("expected pending interrupt request to be removed")
	}
}

func TestResolveCommandPathAbsolute(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "claude")
	if err := os.WriteFile(commandPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}

	resolved, err := resolveCommandPath(commandPath)
	if err != nil {
		t.Fatalf("resolve absolute command failed: %v", err)
	}
	if resolved != commandPath {
		t.Fatalf("expected %q, got %q", commandPath, resolved)
	}
}

func TestResolveCommandPathFromCommonLocation(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("PATH", "")

	commandDir := filepath.Join(homeDir, ".local", "bin")
	if err := os.MkdirAll(commandDir, 0o755); err != nil {
		t.Fatalf("mkdir command dir: %v", err)
	}

	commandPath := filepath.Join(commandDir, "claude")
	if err := os.WriteFile(commandPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}

	resolved, err := resolveCommandPath("claude")
	if err != nil {
		t.Fatalf("resolve command from common location failed: %v", err)
	}
	if resolved != commandPath {
		t.Fatalf("expected %q, got %q", commandPath, resolved)
	}
}

func TestClaudeTranscriptParserParsesAssistantAndToolResult(t *testing.T) {
	parser := newClaudeTranscriptParser(nil)

	assistantLine := `{"type":"assistant","uuid":"assistant-1","timestamp":"2026-03-12T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"先检查一下。"},{"type":"tool_use","id":"tooluse_1","name":"Read","input":{"file_path":"/tmp/a.txt"}}]}}`
	messages := parser.ParseLine([]byte(assistantLine))
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0].Role != "assistant" || messages[0].Content != "先检查一下。" {
		t.Fatalf("unexpected assistant text message: %+v", messages[0])
	}
	if messages[1].SourceID != "tool:tooluse_1" || !strings.Contains(messages[1].Content, "/tmp/a.txt") {
		t.Fatalf("unexpected tool call message: %+v", messages[1])
	}
	if messages[1].IsToolComplete {
		t.Fatalf("tool call should start incomplete: %+v", messages[1])
	}

	userResultLine := `{"type":"user","uuid":"user-1","timestamp":"2026-03-12T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tooluse_1","content":"hello"}]}}`
	messages = parser.ParseLine([]byte(userResultLine))
	if len(messages) != 1 {
		t.Fatalf("expected 1 tool-result update, got %d", len(messages))
	}
	if messages[0].SourceID != "tool:tooluse_1" {
		t.Fatalf("unexpected source id: %+v", messages[0])
	}
	if !messages[0].IsToolComplete || messages[0].ToolResult != "hello" {
		t.Fatalf("expected completed tool result, got %+v", messages[0])
	}
}

func TestClaudeTranscriptParserParsesUserText(t *testing.T) {
	parser := newClaudeTranscriptParser(nil)

	line := `{"type":"user","uuid":"user-2","timestamp":"2026-03-12T10:00:02.000Z","message":{"role":"user","content":"请帮我检查一下"}}`
	messages := parser.ParseLine([]byte(line))
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Content != "请帮我检查一下" {
		t.Fatalf("unexpected user message: %+v", messages[0])
	}
}

func TestClaudeTranscriptParserMapsCommandInvocationToUser(t *testing.T) {
	parser := newClaudeTranscriptParser(nil)

	line := `{"type":"user","uuid":"user-command","timestamp":"2026-03-12T10:00:02.000Z","message":{"role":"user","content":"<command-message>help</command-message>\n<command-name>/help</command-name>"}}`
	messages := parser.ParseLine([]byte(line))
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Content != "/help" {
		t.Fatalf("expected command invocation to be normalized to user /help, got %+v", messages[0])
	}
	if messages[0].MessageType != "command_input" {
		t.Fatalf("expected command_input message type, got %+v", messages[0])
	}
}

func TestClaudeTranscriptParserMapsLocalCommandStdoutToSystem(t *testing.T) {
	parser := newClaudeTranscriptParser(nil)

	line := `{"type":"system","uuid":"system-command","timestamp":"2026-03-12T10:00:02.000Z","subtype":"local_command","content":"<local-command-stdout>Set model to sonnet (claude-sonnet-4-6)</local-command-stdout>"}`
	messages := parser.ParseLine([]byte(line))
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Role != "system" || messages[0].Content != "Set model to sonnet (claude-sonnet-4-6)" {
		t.Fatalf("expected local command stdout to be normalized to system text, got %+v", messages[0])
	}
}

func TestClaudeTranscriptParserSkipsLocalCommandCaveat(t *testing.T) {
	parser := newClaudeTranscriptParser(nil)

	line := `{"type":"system","uuid":"system-caveat","timestamp":"2026-03-12T10:00:02.000Z","subtype":"local_command","content":"<local-command-caveat>Permission required</local-command-caveat>"}`
	messages := parser.ParseLine([]byte(line))
	if len(messages) != 0 {
		t.Fatalf("expected local command caveat to be ignored, got %+v", messages)
	}
}

func TestClaudeTranscriptParserParsesSystemText(t *testing.T) {
	parser := newClaudeTranscriptParser(nil)

	line := `{"type":"system","uuid":"system-1","timestamp":"2026-03-12T10:00:03.000Z","content":"Available commands:\n/help\n/clear"}`
	messages := parser.ParseLine([]byte(line))
	if len(messages) != 1 {
		t.Fatalf("expected 1 system message, got %d", len(messages))
	}
	if messages[0].Role != "system" || !strings.Contains(messages[0].Content, "/help") {
		t.Fatalf("unexpected system message: %+v", messages[0])
	}
}

func TestClaudeTranscriptParserIgnoresSubtypeOnlyProgress(t *testing.T) {
	parser := newClaudeTranscriptParser(nil)

	line := []byte(`{"type":"progress","subtype":"init","timestamp":"2026-03-12T10:00:03.000Z"}`)
	messages := parser.ParseLine(line)
	if len(messages) != 0 {
		t.Fatalf("expected subtype-only progress to be ignored, got %+v", messages)
	}
}

func TestEncodeClaudeProjectPath(t *testing.T) {
	got := encodeClaudeProjectPath("/Users/jairoguo/develops/Projects/acw2a")
	want := "-Users-jairoguo-develops-Projects-acw2a"
	if got != want {
		t.Fatalf("unexpected encoded path: got %q want %q", got, want)
	}
}

func TestClaudeStreamHistoryParserParsesStreamingTurn(t *testing.T) {
	parser := newClaudeStreamHistoryParser(nil)

	lines := []string{
		`{"type":"content_block_start","content_block":{"type":"text","text":"先"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"检查一下"}}`,
		`{"type":"content_block_stop"}`,
		`{"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_1","name":"Read"}}`,
		`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/tmp/a.txt\"}"}}`,
		`{"type":"content_block_stop"}`,
		`{"type":"message_stop"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hello"}]}}`,
	}

	var result []protocol.SessionHistoryMessage
	for _, line := range lines {
		result = append(result, parser.ParseLine([]byte(line))...)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
	if result[0].Role != "assistant" || result[0].Content != "先检查一下" {
		t.Fatalf("unexpected assistant streaming message: %+v", result[0])
	}
	if result[1].SourceID != "tool:toolu_1" || result[1].ToolName != "Read" || !strings.Contains(result[1].ToolInput, "/tmp/a.txt") {
		t.Fatalf("unexpected tool call message: %+v", result[1])
	}
	if result[2].SourceID != "tool:toolu_1" || !result[2].IsToolComplete || result[2].ToolResult != "hello" {
		t.Fatalf("unexpected tool completion message: %+v", result[2])
	}
}

func TestClaudeStreamHistoryParserSkipsDuplicateAssistantEventAfterStreaming(t *testing.T) {
	parser := newClaudeStreamHistoryParser(nil)

	lines := []string{
		`{"type":"content_block_start","content_block":{"type":"text","text":"你好"}}`,
		`{"type":"content_block_stop"}`,
		`{"type":"message_stop"}`,
		`{"type":"assistant","uuid":"assistant-1","message":{"role":"assistant","content":[{"type":"text","text":"你好"}]}}`,
		`{"type":"result","result":"你好"}`,
	}

	var result []protocol.SessionHistoryMessage
	for _, line := range lines {
		result = append(result, parser.ParseLine([]byte(line))...)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 message after de-duplication, got %d (%+v)", len(result), result)
	}
	if result[0].Content != "你好" {
		t.Fatalf("unexpected content: %+v", result[0])
	}
}

func TestClaudeStreamHistoryParserGeneratedSourceIDsAreScopedPerRun(t *testing.T) {
	parserA := newClaudeStreamHistoryParserWithRunID("run-a", nil)
	parserB := newClaudeStreamHistoryParserWithRunID("run-b", nil)

	line := []byte(`{"type":"result","result":"hello"}`)
	resultA := parserA.ParseLine(line)
	resultB := parserB.ParseLine(line)

	if len(resultA) != 1 || len(resultB) != 1 {
		t.Fatalf("expected one message from each parser, got %d and %d", len(resultA), len(resultB))
	}
	if resultA[0].SourceID == resultB[0].SourceID {
		t.Fatalf("expected distinct source ids across runs, got %q", resultA[0].SourceID)
	}
}

func TestClaudeStreamHistoryParserParsesSystemText(t *testing.T) {
	parser := newClaudeStreamHistoryParserWithRunID("run-system", nil)

	line := []byte(`{"type":"system","content":"Available commands:\n/help\n/clear"}`)
	result := parser.ParseLine(line)

	if len(result) != 1 {
		t.Fatalf("expected 1 system message, got %d", len(result))
	}
	if result[0].Role != "system" || !strings.Contains(result[0].Content, "/help") {
		t.Fatalf("unexpected system message: %+v", result[0])
	}
}

func TestClaudeStreamHistoryParserMapsCommandInvocationToUser(t *testing.T) {
	parser := newClaudeStreamHistoryParserWithRunID("run-command", nil)

	line := []byte(`{"type":"user","message":{"role":"user","content":"<command-name>/help</command-name>"}}`)
	result := parser.ParseLine(line)

	if len(result) != 1 {
		t.Fatalf("expected one normalized command invocation, got %+v", result)
	}
	if result[0].Role != "user" || result[0].Content != "/help" {
		t.Fatalf("expected command wrapper to normalize to user /help, got %+v", result[0])
	}
	if result[0].MessageType != "command_input" {
		t.Fatalf("expected command_input message type, got %+v", result[0])
	}
}

func TestClaudeStreamHistoryParserMapsLocalCommandStdoutToSystem(t *testing.T) {
	parser := newClaudeStreamHistoryParserWithRunID("run-stdout", nil)

	line := []byte(`{"type":"system","subtype":"local_command","content":"<local-command-stdout>\u001b[2mSet model to sonnet (claude-sonnet-4-6)\u001b[22m</local-command-stdout>"}`)
	result := parser.ParseLine(line)

	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].Role != "system" || result[0].Content != "Set model to sonnet (claude-sonnet-4-6)" {
		t.Fatalf("expected stdout wrapper to normalize to clean system message, got %+v", result[0])
	}
}

func TestClaudeStreamHistoryParserSkipsLocalCommandCaveat(t *testing.T) {
	parser := newClaudeStreamHistoryParserWithRunID("run-caveat", nil)

	line := []byte(`{"type":"system","subtype":"local_command","content":"<local-command-caveat>Permission required</local-command-caveat>"}`)
	result := parser.ParseLine(line)

	if len(result) != 0 {
		t.Fatalf("expected caveat wrapper to be ignored, got %+v", result)
	}
}

func TestClaudeStreamHistoryParserIgnoresSubtypeOnlySystemEvent(t *testing.T) {
	parser := newClaudeStreamHistoryParserWithRunID("run-system", nil)

	line := []byte(`{"type":"system","subtype":"init"}`)
	result := parser.ParseLine(line)

	if len(result) != 0 {
		t.Fatalf("expected subtype-only event to be ignored, got %+v", result)
	}
}

func TestFindRecentClaudeTranscriptSession(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/Users/jairoguo/Downloads/aabbcc"
	projectDir := filepath.Join(homeDir, ".claude", "projects", encodeClaudeProjectPath(cwd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	oldID := uuid.NewString()
	oldPath := filepath.Join(projectDir, oldID+".jsonl")
	if err := os.WriteFile(oldPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write old transcript failed: %v", err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old transcript failed: %v", err)
	}

	newID := uuid.NewString()
	newPath := filepath.Join(projectDir, newID+".jsonl")
	if err := os.WriteFile(newPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write new transcript failed: %v", err)
	}

	gotID, gotPath, ok, err := findRecentClaudeTranscriptSession(cwd, 30*time.Minute)
	if err != nil {
		t.Fatalf("find recent transcript returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected a recent transcript to be found")
	}
	if gotID != newID || gotPath != newPath {
		t.Fatalf("unexpected transcript match: got (%q, %q) want (%q, %q)", gotID, gotPath, newID, newPath)
	}
}

func TestResolveClaudeSessionStartRemapsMissingResumeToRecentTranscript(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/Users/jairoguo/Downloads/aabbcc"
	projectDir := filepath.Join(homeDir, ".claude", "projects", encodeClaudeProjectPath(cwd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	actualID := uuid.NewString()
	actualPath := filepath.Join(projectDir, actualID+".jsonl")
	if err := os.WriteFile(actualPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript failed: %v", err)
	}

	requestedID := uuid.NewString()
	resolution, err := resolveClaudeSessionStart(requestedID, cwd, true)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if resolution.SessionID != actualID {
		t.Fatalf("expected adopted session %q, got %q", actualID, resolution.SessionID)
	}
	if !resolution.Resume {
		t.Fatal("expected remapped session to keep resume=true")
	}
	if !resolution.ReplacedRequestedSession {
		t.Fatal("expected requested session id to be remapped")
	}
	if resolution.TranscriptPath != actualPath {
		t.Fatalf("expected transcript path %q, got %q", actualPath, resolution.TranscriptPath)
	}
}

func TestDetectRuntime(t *testing.T) {
	if got := detectRuntime("claude"); got != runtimeClaude {
		t.Fatalf("expected claude runtime, got %q", got)
	}
	if got := detectRuntime("/opt/homebrew/bin/codex"); got != runtimeCodex {
		t.Fatalf("expected codex runtime, got %q", got)
	}
}

func TestCommandForRequestedRuntime(t *testing.T) {
	command, err := commandForRequestedRuntime("", Config{Command: "/usr/local/bin/claude", ClaudeEnabled: true, CodexEnabled: true})
	if err != nil {
		t.Fatalf("expected fallback command, got error: %v", err)
	}
	if command != "/usr/local/bin/claude" {
		t.Fatalf("expected fallback command, got %q", command)
	}

	command, err = commandForRequestedRuntime("claude", Config{ClaudeEnabled: true})
	if err != nil {
		t.Fatalf("expected claude runtime, got error: %v", err)
	}
	if command != "claude" {
		t.Fatalf("expected claude, got %q", command)
	}

	command, err = commandForRequestedRuntime("codex", Config{CodexEnabled: true, CodexCommand: "/managed/codex"})
	if err != nil {
		t.Fatalf("expected codex runtime, got error: %v", err)
	}
	if command != "/managed/codex" {
		t.Fatalf("expected codex, got %q", command)
	}

	if _, err := commandForRequestedRuntime("codex", Config{CodexEnabled: false}); err == nil {
		t.Fatal("expected disabled runtime error")
	}

	if _, err := commandForRequestedRuntime("custom", Config{}); err == nil {
		t.Fatal("expected unsupported runtime error")
	}
}

func TestCodexRuntimeArgsUseResumeSubcommand(t *testing.T) {
	if !codexRuntimeArgsUseResumeSubcommand([]string{"--cd", "/tmp/project", "resume", "--last"}) {
		t.Fatal("expected explicit codex resume subcommand to be detected")
	}
	if codexRuntimeArgsUseResumeSubcommand([]string{"fix", "resume handling"}) {
		t.Fatal("did not expect prompt text to be treated as resume subcommand")
	}
}

func TestBuildCodexHistoryBatchFromThread(t *testing.T) {
	startedAt := time.Now().Add(-time.Second).UnixMilli()
	thread := &codexThreadSnapshot{
		ID:        "thread-1",
		CreatedAt: 1773895742,
		Turns: []codexThreadTurn{{
			ID: "turn-1",
			Items: []map[string]any{
				{
					"type": "userMessage",
					"id":   "user-1",
					"content": []any{
						map[string]any{"type": "text", "text": "Read README.md"},
					},
				},
				{
					"type":             "commandExecution",
					"id":               "call-1",
					"command":          "cat README.md",
					"cwd":              "/tmp/project",
					"aggregatedOutput": "# acw2a\n",
				},
				{
					"type":  "agentMessage",
					"id":    "assistant-1",
					"text":  "# acw2a",
					"phase": "final_answer",
				},
				{
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
			},
		}},
	}

	batch := buildCodexHistoryBatchFromThread(thread, nil, nil)
	if len(batch) != 4 {
		t.Fatalf("expected 4 history messages, got %d", len(batch))
	}
	if batch[0].Role != "user" || batch[0].Content != "Read README.md" {
		t.Fatalf("unexpected user message: %+v", batch[0])
	}
	if batch[1].MessageType != "tool_call" || batch[1].ToolName != "Bash" || !batch[1].IsToolComplete {
		t.Fatalf("unexpected tool call message: %+v", batch[1])
	}
	if batch[2].Role != "assistant" || batch[2].Content != "# acw2a" {
		t.Fatalf("unexpected assistant message: %+v", batch[2])
	}
	if batch[3].MessageType != "tool_call" || batch[3].ToolName != "FetchSpec" || !batch[3].IsToolComplete {
		t.Fatalf("unexpected dynamic tool message: %+v", batch[3])
	}
	if batch[3].ToolResult != "ok" {
		t.Fatalf("expected dynamic tool result text to be extracted, got %+v", batch[3])
	}
	for index, msg := range batch {
		if msg.Timestamp < startedAt {
			t.Fatalf("expected snapshot timestamp to use sync time, got index=%d timestamp=%d started_at=%d", index, msg.Timestamp, startedAt)
		}
		if index > 0 && msg.Timestamp <= batch[index-1].Timestamp {
			t.Fatalf("expected snapshot timestamps to be strictly increasing, got previous=%d current=%d", batch[index-1].Timestamp, msg.Timestamp)
		}
	}
}

func TestCodexHistorySyncerTracksDynamicToolCallLifecycle(t *testing.T) {
	syncer := &codexHistorySyncer{
		pendingTools: make(map[string]protocol.SessionHistoryMessage),
	}

	started := json.RawMessage(marshalCompactJSON(map[string]any{
		"item": map[string]any{
			"type":     "dynamicToolCall",
			"id":       "tool-2",
			"toolName": "FetchSpec",
			"arguments": map[string]any{
				"package": "codex",
			},
		},
	}))
	startMessages := syncer.parseItemStarted(started)
	if len(startMessages) != 1 {
		t.Fatalf("expected 1 started message, got %d", len(startMessages))
	}
	if startMessages[0].MessageType != "tool_call" || startMessages[0].ToolName != "FetchSpec" || startMessages[0].IsToolComplete {
		t.Fatalf("unexpected started tool call message: %+v", startMessages[0])
	}
	if len(syncer.pendingTools) != 1 {
		t.Fatalf("expected pending tool to be tracked, got %d", len(syncer.pendingTools))
	}

	completed := json.RawMessage(marshalCompactJSON(map[string]any{
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
	}))
	completedMessages := syncer.parseItemCompleted(completed)
	if len(completedMessages) != 1 {
		t.Fatalf("expected 1 completed message, got %d", len(completedMessages))
	}
	if completedMessages[0].MessageType != "tool_call" || completedMessages[0].ToolName != "FetchSpec" || !completedMessages[0].IsToolComplete {
		t.Fatalf("unexpected completed tool call message: %+v", completedMessages[0])
	}
	if !strings.Contains(completedMessages[0].ToolInput, "\"package\":\"codex\"") {
		t.Fatalf("expected tool input to be preserved from started event, got %+v", completedMessages[0])
	}
	if completedMessages[0].ToolResult != "ok" {
		t.Fatalf("expected tool result text to be extracted, got %+v", completedMessages[0])
	}
	if len(syncer.pendingTools) != 0 {
		t.Fatalf("expected pending tools to be cleared, got %d", len(syncer.pendingTools))
	}
}

func TestBuildCodexUserInputResult(t *testing.T) {
	request := map[string]any{
		"questions": []any{
			map[string]any{"id": "language", "question": "Select a language"},
			map[string]any{"id": "scope", "question": "Select scope"},
		},
	}
	updatedInput := map[string]any{
		"answers": map[string]any{
			"0": "Go",
			"1": []any{"mobile", "shared"},
		},
	}

	result := buildCodexUserInputResult(request, updatedInput)
	answers, _ := result["answers"].(map[string]any)
	if len(answers) != 2 {
		t.Fatalf("expected 2 answers, got %+v", result)
	}

	language, _ := answers["language"].(map[string]any)
	if values, _ := language["answers"].([]string); len(values) != 1 || values[0] != "Go" {
		t.Fatalf("unexpected language response: %+v", language)
	}
}

func TestBuildCodexMcpElicitationResult(t *testing.T) {
	result := buildCodexMcpElicitationResult(
		map[string]any{"mode": "form"},
		protocol.PermissionResponsePayload{
			Decision: protocol.PermissionDecisionApproved,
			UpdatedInput: map[string]any{
				"content": map[string]any{
					"title":      "爆款内容",
					"publishNow": true,
				},
			},
		},
	)

	if result["action"] != "accept" {
		t.Fatalf("expected accept action, got %+v", result)
	}
	content, _ := result["content"].(map[string]any)
	if content["title"] != "爆款内容" || content["publishNow"] != true {
		t.Fatalf("unexpected mcp content: %+v", content)
	}
}

func TestBuildCodexDynamicToolResult(t *testing.T) {
	result := buildCodexDynamicToolResult(
		"request_user_input",
		map[string]any{"question": "Select"},
		protocol.PermissionResponsePayload{
			Decision: protocol.PermissionDecisionApproved,
			UpdatedInput: map[string]any{
				"answers": map[string]any{"0": "iOS"},
			},
		},
	)

	if result["success"] != true {
		t.Fatalf("expected successful result, got %+v", result)
	}
	contentItems, _ := result["contentItems"].([]map[string]any)
	if len(contentItems) != 1 || !strings.Contains(fmt.Sprint(contentItems[0]["text"]), "iOS") {
		t.Fatalf("unexpected content items: %+v", result["contentItems"])
	}
}

func TestResolveClaudeSessionStartRemapsMissingResumeToOlderTranscript(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/Users/jairoguo/Downloads/aabbcc"
	projectDir := filepath.Join(homeDir, ".claude", "projects", encodeClaudeProjectPath(cwd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	actualID := uuid.NewString()
	actualPath := filepath.Join(projectDir, actualID+".jsonl")
	if err := os.WriteFile(actualPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript failed: %v", err)
	}
	oldTime := time.Now().Add(-6 * time.Hour)
	if err := os.Chtimes(actualPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes transcript failed: %v", err)
	}

	requestedID := uuid.NewString()
	resolution, err := resolveClaudeSessionStart(requestedID, cwd, true)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if resolution.SessionID != actualID {
		t.Fatalf("expected adopted older session %q, got %q", actualID, resolution.SessionID)
	}
	if !resolution.Resume {
		t.Fatal("expected remapped older session to keep resume=true")
	}
	if !resolution.ReplacedRequestedSession {
		t.Fatal("expected requested session id to be remapped")
	}
	if resolution.TranscriptPath != actualPath {
		t.Fatalf("expected transcript path %q, got %q", actualPath, resolution.TranscriptPath)
	}
}

func TestResolveClaudeSessionStartKeepsRequestedResumeWhenTranscriptExists(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/Users/jairoguo/Downloads/aabbcc"
	projectDir := filepath.Join(homeDir, ".claude", "projects", encodeClaudeProjectPath(cwd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	requestedID := uuid.NewString()
	requestedPath := filepath.Join(projectDir, requestedID+".jsonl")
	if err := os.WriteFile(requestedPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript failed: %v", err)
	}

	resolution, err := resolveClaudeSessionStart(requestedID, cwd, true)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if resolution.SessionID != requestedID {
		t.Fatalf("expected session %q, got %q", requestedID, resolution.SessionID)
	}
	if !resolution.Resume {
		t.Fatal("expected resume=true")
	}
	if resolution.ReplacedRequestedSession {
		t.Fatal("did not expect requested session to be remapped")
	}
	if resolution.TranscriptPath != requestedPath {
		t.Fatalf("expected transcript path %q, got %q", requestedPath, resolution.TranscriptPath)
	}
}

func TestResolveClaudeSessionStartAdoptsOlderTranscriptWhenStartingWithoutSessionID(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cwd := "/Users/jairoguo/works/develop/YY/Temp/ai-health-app-service"
	projectDir := filepath.Join(homeDir, ".claude", "projects", encodeClaudeProjectPath(cwd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	transcriptID := uuid.NewString()
	transcriptPath := filepath.Join(projectDir, transcriptID+".jsonl")
	if err := os.WriteFile(transcriptPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript failed: %v", err)
	}
	oldTime := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(transcriptPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes transcript failed: %v", err)
	}

	resolution, err := resolveClaudeSessionStart("", cwd, false)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if resolution.SessionID != transcriptID {
		t.Fatalf("expected transcript session %q, got %q", transcriptID, resolution.SessionID)
	}
	if !resolution.Resume {
		t.Fatal("expected auto-adopted transcript to set resume=true")
	}
	if !resolution.AdoptedRecentSession {
		t.Fatal("expected transcript to be marked as adopted")
	}
	if resolution.TranscriptPath != transcriptPath {
		t.Fatalf("expected transcript path %q, got %q", transcriptPath, resolution.TranscriptPath)
	}
}

func TestNormalizeTranscriptDisplayEntryMapsCompactSummaryToSystem(t *testing.T) {
	display, ok := normalizeTranscriptDisplayEntry(
		nil,
		"user",
		"This session is being continued from a previous conversation that ran out of context. Summary:\n1. Test",
	)
	if !ok {
		t.Fatal("expected compact summary to be displayed")
	}
	if display.Role != "system" {
		t.Fatalf("expected compact summary role to be system, got %q", display.Role)
	}
}

func TestNormalizeTranscriptDisplayEntryMapsCompactedStdoutToSystemMessage(t *testing.T) {
	display, ok := normalizeTranscriptDisplayEntry(
		nil,
		"user",
		"<local-command-stdout>Compacted </local-command-stdout>",
	)
	if !ok {
		t.Fatal("expected compact stdout to be displayed")
	}
	if display.Role != "system" {
		t.Fatalf("expected compact stdout role to be system, got %q", display.Role)
	}
	if display.Content != compactBoundaryText {
		t.Fatalf("expected compact stdout content %q, got %q", compactBoundaryText, display.Content)
	}
}

func TestClaudeTranscriptParserMapsCompactBoundaryToSystemMessage(t *testing.T) {
	parser := newClaudeTranscriptParser(nil)
	result := parser.ParseLine([]byte(`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","timestamp":"2026-03-16T00:24:59.321Z","uuid":"compact-boundary-1"}`))
	if len(result) != 1 {
		t.Fatalf("expected one compact boundary message, got %d", len(result))
	}
	if got := result[0].Role; got != "system" {
		t.Fatalf("expected system role, got %q", got)
	}
	if got := result[0].Content; got != compactBoundaryText {
		t.Fatalf("expected compact boundary content %q, got %q", compactBoundaryText, got)
	}
}

func TestSearchDirEntriesRecursive(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	if err := os.MkdirAll(filepath.Join(root, "src", "components"), 0o755); err != nil {
		t.Fatalf("mkdir nested dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "components", "CommandPalette.swift"), []byte(""), 0o644); err != nil {
		t.Fatalf("write command palette: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(""), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}

	_, entries, err := searchDirEntries(protocol.ListDirPayload{
		Path:      root,
		Query:     "command",
		Recursive: true,
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("search dir entries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one fuzzy match")
	}
	if entries[0].DisplayPath != "src/components/CommandPalette.swift" {
		t.Fatalf("unexpected top fuzzy match: %+v", entries[0])
	}
	if entries[0].Path == "" {
		t.Fatal("expected absolute path in search result")
	}
}

func TestSearchDirEntriesDefaultsToUserHome(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	projectDir := filepath.Join(homeDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	resolvedRoot, entries, err := searchDirEntries(protocol.ListDirPayload{})
	if err != nil {
		t.Fatalf("search dir entries: %v", err)
	}
	resolvedHomeDir, err := filepath.EvalSymlinks(homeDir)
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	if resolvedRoot != resolvedHomeDir {
		t.Fatalf("expected resolved root %q, got %q", resolvedHomeDir, resolvedRoot)
	}
	if len(entries) != 1 || entries[0].Name != "project" {
		t.Fatalf("unexpected home entries: %+v", entries)
	}
}

func TestSearchDirEntriesRejectsOutsideUserHome(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	outsideDir := t.TempDir()
	if _, _, err := searchDirEntries(protocol.ListDirPayload{Path: outsideDir}); err == nil {
		t.Fatal("expected outside path to be rejected")
	}
}

func TestReadFilePreviewReturnsContentAndLanguage(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	filePath := filepath.Join(homeDir, "Sources", "App.swift")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir file parent: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("struct Demo {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	resp, err := readFilePreview(protocol.ReadFilePayload{Path: filePath})
	if err != nil {
		t.Fatalf("read file preview: %v", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		t.Fatalf("resolve file path: %v", err)
	}
	if resp.Path != resolvedPath {
		t.Fatalf("expected path %q, got %q", resolvedPath, resp.Path)
	}
	if resp.Content != "struct Demo {}\n" {
		t.Fatalf("unexpected content: %q", resp.Content)
	}
	if resp.Language != "swift" {
		t.Fatalf("expected swift language, got %q", resp.Language)
	}
	if resp.IsBinary {
		t.Fatal("expected text file preview to be non-binary")
	}
}

func TestReadFilePreviewRejectsOutsideUserHome(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	outsideDir := t.TempDir()
	filePath := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(filePath, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	if _, err := readFilePreview(protocol.ReadFilePayload{Path: filePath}); err == nil {
		t.Fatal("expected outside file to be rejected")
	}
}

func TestReadFilePreviewMarksBinaryContent(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	filePath := filepath.Join(homeDir, "image.bin")
	if err := os.WriteFile(filePath, []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatalf("write binary file: %v", err)
	}

	resp, err := readFilePreview(protocol.ReadFilePayload{Path: filePath})
	if err != nil {
		t.Fatalf("read binary preview: %v", err)
	}
	if !resp.IsBinary {
		t.Fatal("expected binary preview to be marked binary")
	}
	if resp.Content != "" {
		t.Fatalf("expected binary preview content to stay empty, got %q", resp.Content)
	}
}
