package remote

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

func (s *Service) resetPermissionState() {
	s.mu.Lock()
	sessionID := strings.TrimSpace(s.sessionID)
	pending := s.pendingPermissions
	s.pendingPermissions = make(map[string]*pendingPermissionRequest)
	attachedShadow := s.attachedPermissionShadow
	s.attachedPermissionShadow = make(map[string]protocol.PermissionRequestPayload)
	s.allowedTools = make(map[string]struct{})
	s.allowedBashLiterals = make(map[string]struct{})
	s.allowedBashPrefixes = make(map[string]struct{})
	s.mu.Unlock()

	for _, req := range pending {
		s.notifyPermissionCleared(req.sessionID, "")
		closePendingPermission(req)
	}
	for _, req := range attachedShadow {
		s.notifyPermissionCleared(sessionID, req.RequestID)
	}
}

func (s *Service) resolvePermissionResponse(resp protocol.PermissionResponsePayload) {
	s.mu.Lock()
	req := s.pendingPermissions[resp.RequestID]
	if req != nil {
		delete(s.pendingPermissions, resp.RequestID)
	}
	shadowReq, shadowOK := s.attachedPermissionShadow[resp.RequestID]
	attachedHandler := s.attachedPermissionResponseHandler
	sessionID := strings.TrimSpace(s.sessionID)
	s.mu.Unlock()

	if req == nil {
		if shadowOK {
			if attachedHandler != nil && attachedHandler(resp) {
				input := permissionRequestInputMap(shadowReq.Input)
				effectiveInput := input
				if resp.UpdatedInput != nil {
					effectiveInput = resp.UpdatedInput
				}
				approved := resp.Approved || resp.Decision == protocol.PermissionDecisionApproved || resp.Decision == protocol.PermissionDecisionApprovedForSession
				if resp.Decision == protocol.PermissionDecisionApprovedForSession {
					s.recordSessionPermission(shadowReq.Tool, input)
				}
				if approved {
					s.rememberUpdatedToolInput(shadowReq.Tool, input, effectiveInput)
				}
				s.mu.Lock()
				delete(s.attachedPermissionShadow, resp.RequestID)
				s.mu.Unlock()
				s.notifyPermissionCleared(sessionID, resp.RequestID)
				s.resumeCodexAfterPermissionResolution(sessionID)
				return
			}
			s.notifyAttachedPermissionUnsupported(sessionID, shadowReq)
			return
		}
		applog.Errorf(
			"[Remote] permission response dropped: request=%s decision=%s approved=%t reason=no-pending",
			resp.RequestID,
			resp.Decision,
			resp.Approved,
		)
		return
	}

	applog.Info.Printf(
		"[Remote] permission response resolved: session=%s request=%s decision=%s approved=%t has_input=%t",
		req.sessionID,
		resp.RequestID,
		resp.Decision,
		resp.Approved,
		resp.UpdatedInput != nil,
	)
	deliverPendingPermission(req, resp)
	s.resumeCodexAfterPermissionResolution(req.sessionID)
}

func permissionRequestInputMap(value any) map[string]any {
	input, _ := value.(map[string]any)
	if input == nil {
		return map[string]any{}
	}
	return input
}

func (s *Service) cancelPendingPermission(requestID string) {
	s.mu.Lock()
	req := s.pendingPermissions[requestID]
	if req != nil {
		delete(s.pendingPermissions, requestID)
	}
	s.mu.Unlock()

	if req != nil {
		s.notifyPermissionCleared(req.sessionID, requestID)
		closePendingPermission(req)
	}
}

func (s *Service) registerPendingPermission(sessionID, requestID string) chan protocol.PermissionResponsePayload {
	ch := make(chan protocol.PermissionResponsePayload, 1)

	s.mu.Lock()
	if s.pendingPermissions == nil {
		s.pendingPermissions = make(map[string]*pendingPermissionRequest)
	}
	s.pendingPermissions[requestID] = &pendingPermissionRequest{
		sessionID:  sessionID,
		responseCh: ch,
	}
	s.mu.Unlock()

	return ch
}

func (s *Service) EmitAttachedPermissionRequest(requestID, tool string, input map[string]any) error {
	requestID = strings.TrimSpace(requestID)
	tool = strings.TrimSpace(tool)
	if requestID == "" || tool == "" {
		return nil
	}
	if input == nil {
		input = map[string]any{}
	}
	if s.shouldAutoApproveTool(tool, input) {
		return nil
	}

	payload := protocol.PermissionRequestPayload{
		RequestID: requestID,
		Tool:      tool,
		Input:     input,
		Summary:   summarizePermissionRequest(tool, input),
		CreatedAt: time.Now().UnixMilli(),
	}

	s.mu.Lock()
	sessionID := strings.TrimSpace(s.sessionID)
	if sessionID == "" {
		s.mu.Unlock()
		return nil
	}
	if s.attachedPermissionShadow == nil {
		s.attachedPermissionShadow = make(map[string]protocol.PermissionRequestPayload)
	}
	if _, exists := s.attachedPermissionShadow[requestID]; exists {
		s.mu.Unlock()
		return nil
	}
	s.attachedPermissionShadow[requestID] = payload
	s.mu.Unlock()

	if err := s.writeJSON(protocol.Message{
		Type:      protocol.TypePermissionRequest,
		SessionID: sessionID,
		Payload:   payload,
	}); err != nil {
		return err
	}
	if s.setCodexPermissionWaiting(true) {
		if err := s.sendCurrentKeepalive(sessionID); err != nil {
			applog.Errorf("[Remote] codex attached approval-wait keepalive failed: session=%s err=%v", sessionID, err)
		}
	}
	return nil
}

func (s *Service) ClearAttachedPermissionRequest(requestID string) {
	requestID = strings.TrimSpace(requestID)

	s.mu.Lock()
	sessionID := strings.TrimSpace(s.sessionID)
	if sessionID == "" || len(s.attachedPermissionShadow) == 0 {
		s.mu.Unlock()
		return
	}

	requestIDs := make([]string, 0, len(s.attachedPermissionShadow))
	if requestID != "" {
		if _, ok := s.attachedPermissionShadow[requestID]; ok {
			delete(s.attachedPermissionShadow, requestID)
			requestIDs = append(requestIDs, requestID)
		}
	} else {
		for id := range s.attachedPermissionShadow {
			requestIDs = append(requestIDs, id)
		}
		s.attachedPermissionShadow = make(map[string]protocol.PermissionRequestPayload)
	}
	s.mu.Unlock()

	for _, id := range requestIDs {
		s.notifyPermissionCleared(sessionID, id)
	}
}

func (s *Service) notifyAttachedPermissionUnsupported(sessionID string, req protocol.PermissionRequestPayload) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}

	message := "Native Claude UI permission requests currently sync status only. Approve or deny them in the local terminal."
	if strings.TrimSpace(req.Summary) != "" {
		message = fmt.Sprintf("%s Current request: %s", message, strings.TrimSpace(req.Summary))
	}
	if err := s.writeJSON(protocol.Message{
		Type:      protocol.TypeError,
		SessionID: sessionID,
		Payload: protocol.ErrorPayload{
			Message: message,
		},
	}); err != nil {
		applog.Errorf("[Remote] WS write attached-permission error: %v", err)
	}
}

func (s *Service) handlePermissionControlRequest(cmd *exec.Cmd, stdin io.WriteCloser, sessionID string, req sdkControlRequest) {
	input := req.Request.Input
	if input == nil {
		input = map[string]any{}
	}

	if s.shouldAutoApproveTool(req.Request.ToolName, input) {
		if err := s.writePermissionResult(stdin, req.RequestID, true, input, ""); err != nil && s.isCurrentCommand(cmd) {
			applog.Errorf("[Remote] auto-approve permission failed: %v", err)
		}
		return
	}

	responseCh := s.registerPendingPermission(sessionID, req.RequestID)

	msg := protocol.Message{
		Type:      protocol.TypePermissionRequest,
		SessionID: sessionID,
		Payload: protocol.PermissionRequestPayload{
			RequestID: req.RequestID,
			Tool:      req.Request.ToolName,
			Input:     input,
			Summary:   summarizePermissionRequest(req.Request.ToolName, input),
			CreatedAt: time.Now().UnixMilli(),
		},
	}
	if err := s.writeJSON(msg); err != nil {
		s.cancelPendingPermission(req.RequestID)
		if s.isCurrentCommand(cmd) {
			applog.Errorf("[Remote] forward permission request failed: %v", err)
		}
		return
	}

	resp, ok := <-responseCh
	if !ok {
		return
	}

	if resp.Decision == protocol.PermissionDecisionApprovedForSession {
		s.recordSessionPermission(req.Request.ToolName, input)
	}

	effectiveInput := input
	if resp.UpdatedInput != nil {
		effectiveInput = resp.UpdatedInput
	}

	approved := resp.Approved || resp.Decision == protocol.PermissionDecisionApproved || resp.Decision == protocol.PermissionDecisionApprovedForSession
	message := permissionDeniedMessage(resp.Decision)
	if approved {
		message = ""
		s.rememberUpdatedToolInput(req.Request.ToolName, input, effectiveInput)
	}

	if err := s.writePermissionResult(stdin, req.RequestID, approved, effectiveInput, message); err != nil && s.isCurrentCommand(cmd) {
		applog.Errorf("[Remote] permission response write failed: %v", err)
	}
}

func (s *Service) writePermissionResult(stdin io.WriteCloser, requestID string, approved bool, input map[string]any, deniedMessage string) error {
	resp := sdkControlResponse{
		Type: "control_response",
		Response: sdkControlResponsePayload{
			Subtype:   "success",
			RequestID: requestID,
		},
	}

	if approved {
		resp.Response.Response = &sdkPermissionResult{
			Behavior:     "allow",
			UpdatedInput: input,
		}
	} else {
		resp.Response.Response = &sdkPermissionResult{
			Behavior: "deny",
			Message:  deniedMessage,
		}
	}

	return s.writeJSONLineTo(stdin, resp)
}

func (s *Service) shouldAutoApproveTool(toolName string, input map[string]any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.currentPermissionMode == protocol.PermissionModeBypassPermissions {
		return true
	}

	if toolName == "Bash" {
		command, _ := input["command"].(string)
		if command != "" {
			if _, ok := s.allowedBashLiterals[command]; ok {
				return true
			}
			for prefix := range s.allowedBashPrefixes {
				if strings.HasPrefix(command, prefix) {
					return true
				}
			}
		}
	} else if _, ok := s.allowedTools[toolName]; ok {
		return true
	}

	return s.currentPermissionMode == protocol.PermissionModeAcceptEdits && isEditTool(toolName)
}

func (s *Service) recordSessionPermission(toolName string, input map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.allowedTools == nil {
		s.allowedTools = make(map[string]struct{})
	}
	if s.allowedBashLiterals == nil {
		s.allowedBashLiterals = make(map[string]struct{})
	}

	if toolName == "Bash" {
		if command, _ := input["command"].(string); command != "" {
			s.allowedBashLiterals[command] = struct{}{}
		}
		return
	}

	s.allowedTools[toolName] = struct{}{}
}

func isEditTool(toolName string) bool {
	switch toolName {
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return true
	default:
		return false
	}
}

func summarizePermissionRequest(toolName string, input map[string]any) string {
	if toolName == "Bash" {
		if command, _ := input["command"].(string); command != "" {
			return command
		}
	}

	data, err := json.Marshal(input)
	if err != nil || string(data) == "null" || string(data) == "{}" {
		return toolName
	}

	summary := string(data)
	if len(summary) > 180 {
		summary = summary[:177] + "..."
	}
	return summary
}

func permissionDeniedMessage(decision string) string {
	switch decision {
	case protocol.PermissionDecisionAbort:
		return "The user aborted this tool use. Stop and wait for further instructions."
	case protocol.PermissionDecisionDenied:
		fallthrough
	default:
		return "The user denied this tool use. Stop and wait for further instructions."
	}
}

func (s *Service) rememberUpdatedToolInput(toolName string, originalInput, updatedInput map[string]any) {
	originalJSON := normalizeToolInputJSON(marshalCompactJSON(originalInput))
	updatedJSON := normalizeToolInputJSON(marshalCompactJSON(updatedInput))
	if strings.TrimSpace(toolName) == "" || originalJSON == "" || updatedJSON == "" || originalJSON == updatedJSON {
		return
	}

	s.mu.Lock()
	if s.updatedToolInputs == nil {
		s.updatedToolInputs = make(map[string]string)
	}
	s.updatedToolInputs[buildToolInputRewriteKey(toolName, originalJSON)] = updatedJSON
	s.mu.Unlock()
}

func (s *Service) rewriteToolInput(toolName, inputJSON string) string {
	key := buildToolInputRewriteKey(toolName, normalizeToolInputJSON(inputJSON))
	if strings.TrimSpace(key) == "" {
		return inputJSON
	}

	s.mu.Lock()
	updated := s.updatedToolInputs[key]
	s.mu.Unlock()
	if updated == "" {
		return inputJSON
	}
	return updated
}

func buildToolInputRewriteKey(toolName, inputJSON string) string {
	normalizedTool := normalizeToolNameForMatching(toolName)
	normalizedInput := normalizeToolInputJSON(inputJSON)
	if normalizedTool == "" || normalizedInput == "" {
		return ""
	}
	return normalizedTool + "\n" + normalizedInput
}

func normalizeToolNameForMatching(toolName string) string {
	switch strings.TrimSpace(strings.ToLower(toolName)) {
	case "askuserquestion", "ask_user_question", "requestuserinput", "request_user_input":
		return "ask_user_question"
	default:
		return strings.TrimSpace(strings.ToLower(toolName))
	}
}

func normalizeToolInputJSON(inputJSON string) string {
	trimmed := strings.TrimSpace(inputJSON)
	if trimmed == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return trimmed
	}
	return marshalCompactJSON(value)
}

func deliverPendingPermission(req *pendingPermissionRequest, resp protocol.PermissionResponsePayload) {
	req.once.Do(func() {
		req.responseCh <- resp
		close(req.responseCh)
	})
}

func closePendingPermission(req *pendingPermissionRequest) {
	req.once.Do(func() {
		close(req.responseCh)
	})
}

func (s *Service) notifyPermissionCleared(sessionID, requestID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}

	msg := protocol.Message{
		Type:      protocol.TypePermissionCleared,
		SessionID: sessionID,
		Payload: protocol.PermissionClearedPayload{
			RequestID: requestID,
		},
	}
	if err := s.writeJSON(msg); err != nil {
		applog.Errorf("[Remote] WS write permission-cleared error: %v", err)
	}
}

func (s *Service) resumeCodexAfterPermissionResolution(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}

	s.mu.Lock()
	if s.runtime != runtimeCodex || !s.turnActive {
		s.mu.Unlock()
		return
	}
	s.thinking = true
	s.mu.Unlock()

	if err := s.sendCurrentKeepalive(sessionID); err != nil {
		applog.Errorf("[Remote] codex permission-resolution keepalive failed: session=%s err=%v", sessionID, err)
	}
}
