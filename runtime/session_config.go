package remote

import (
	"strings"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func normalizePermissionMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "", protocol.PermissionModeDefault:
		return protocol.PermissionModeDefault
	case protocol.PermissionModeAcceptEdits:
		return protocol.PermissionModeAcceptEdits
	case protocol.PermissionModePlan:
		return protocol.PermissionModePlan
	case protocol.PermissionModeDontAsk:
		return protocol.PermissionModeDontAsk
	case protocol.PermissionModeBypassPermissions:
		return protocol.PermissionModeBypassPermissions
	case "receive", "accept":
		return protocol.PermissionModeDefault
	case "always-accept", "always_accept", "alwaysAccept", "bypass":
		return protocol.PermissionModeBypassPermissions
	case "reject", "deny":
		return protocol.PermissionModeDontAsk
	default:
		return protocol.PermissionModeDefault
	}
}

func canHotSwapPermissionMode(mode string) bool {
	switch normalizePermissionMode(mode) {
	case protocol.PermissionModeDefault, protocol.PermissionModeAcceptEdits, protocol.PermissionModeBypassPermissions, protocol.PermissionModePlan:
		return true
	default:
		return false
	}
}

func requiresPermissionModeRestart(runtime runtimeKind, currentMode, targetMode string) bool {
	switch runtime {
	case runtimeCodex:
		return false
	default:
		return !canHotSwapPermissionMode(currentMode) || !canHotSwapPermissionMode(targetMode)
	}
}

func requiresSandboxModeRestart(runtime runtimeKind, currentMode, targetMode string) bool {
	switch runtime {
	case runtimeCodex:
		return false
	default:
		return false
	}
}

type sessionConfigDecision struct {
	TargetModel                string
	ApplyModel                 bool
	TargetPermissionMode       string
	TargetSandboxMode          string
	PermissionModeChanged      bool
	PermissionModeNeedsRestart bool
	SandboxModeChanged         bool
	SandboxModeNeedsRestart    bool
	ModelChanged               bool
	WorkingDirChanged          bool
	NeedsRestart               bool
	ResumeConversation         bool
}

func (s *Service) evaluateSessionConfig(cfg protocol.SessionConfigPayload) sessionConfigDecision {
	return EvaluateSessionConfig(SessionConfigContext{
		Runtime:               string(s.getRuntime()),
		CurrentWorkingDir:     s.getCurrentDir(),
		CurrentModel:          s.getCurrentModel(),
		CurrentPermissionMode: s.getCurrentPermissionMode(),
		CurrentSandboxMode:    s.getCurrentSandboxMode(),
	}, cfg)
}

type SessionConfigContext struct {
	Runtime               string
	CurrentWorkingDir     string
	CurrentModel          string
	CurrentPermissionMode string
	CurrentSandboxMode    string
}

func EvaluateSessionConfig(ctx SessionConfigContext, cfg protocol.SessionConfigPayload) sessionConfigDecision {
	runtime := runtimeKindFromString(ctx.Runtime)
	currentDir := strings.TrimSpace(ctx.CurrentWorkingDir)
	currentModel := strings.TrimSpace(ctx.CurrentModel)
	currentPermissionMode := normalizePermissionModeForRuntime(runtime, ctx.CurrentPermissionMode)
	currentSandboxMode := normalizeSandboxModeForRuntime(runtime, ctx.CurrentSandboxMode)

	targetPermissionMode := normalizePermissionModeForRuntime(runtime, cfg.PermissionMode)
	permissionModeChanged := strings.TrimSpace(cfg.PermissionMode) != "" && targetPermissionMode != currentPermissionMode
	permissionModeNeedsRestart := permissionModeChanged && requiresPermissionModeRestart(runtime, currentPermissionMode, targetPermissionMode)
	targetSandboxMode := normalizeSandboxModeForRuntime(runtime, cfg.SandboxMode)
	sandboxModeChanged := strings.TrimSpace(cfg.SandboxMode) != "" && targetSandboxMode != currentSandboxMode
	sandboxModeNeedsRestart := sandboxModeChanged && requiresSandboxModeRestart(runtime, currentSandboxMode, targetSandboxMode)

	targetModel := strings.TrimSpace(cfg.Model)
	modelChanged := cfg.ApplyModel && targetModel != currentModel
	workingDirChanged := strings.TrimSpace(cfg.WorkingDir) != "" && strings.TrimSpace(cfg.WorkingDir) != currentDir
	needsRestart := cfg.Restart || workingDirChanged || permissionModeNeedsRestart || sandboxModeNeedsRestart || modelChanged
	resumeConversation := !cfg.Restart && !workingDirChanged && (permissionModeNeedsRestart || sandboxModeNeedsRestart || modelChanged)

	return sessionConfigDecision{
		TargetModel:                targetModel,
		ApplyModel:                 cfg.ApplyModel,
		TargetPermissionMode:       targetPermissionMode,
		TargetSandboxMode:          targetSandboxMode,
		PermissionModeChanged:      permissionModeChanged,
		PermissionModeNeedsRestart: permissionModeNeedsRestart,
		SandboxModeChanged:         sandboxModeChanged,
		SandboxModeNeedsRestart:    sandboxModeNeedsRestart,
		ModelChanged:               modelChanged,
		WorkingDirChanged:          workingDirChanged,
		NeedsRestart:               needsRestart,
		ResumeConversation:         resumeConversation,
	}
}
