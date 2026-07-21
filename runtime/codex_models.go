package remote

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/OpenSlash/agent-bridge/protocol"
)

type codexModelListResponse struct {
	Data       []codexModelListEntry `json:"data"`
	NextCursor *string               `json:"nextCursor"`
}

type codexModelListEntry struct {
	ID                        string                       `json:"id"`
	Model                     string                       `json:"model"`
	DisplayName               string                       `json:"displayName"`
	Description               string                       `json:"description"`
	Hidden                    bool                         `json:"hidden"`
	IsDefault                 bool                         `json:"isDefault"`
	DefaultReasoningEffort    string                       `json:"defaultReasoningEffort"`
	SupportedReasoningEfforts []codexReasoningEffortOption `json:"supportedReasoningEfforts"`
}

type codexReasoningEffortOption struct {
	ReasoningEffort string `json:"reasoningEffort"`
	Description     string `json:"description"`
}

// DiscoverCodexRuntimeModels asks the installed Codex app-server for its live
// model catalog so callers do not need to ship a stale hard-coded model list.
func DiscoverCodexRuntimeModels(ctx context.Context, command string) ([]protocol.RuntimeModelInfo, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("codex command is empty")
	}
	cmd := exec.CommandContext(ctx, command, "app-server", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer stopCodexCatalogProcess(cmd, stdin)

	reader := bufio.NewReader(stdout)
	if err := codexCallSync(stdin, reader, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "veilo-agent", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil); err != nil {
		return nil, err
	}

	models := make([]protocol.RuntimeModelInfo, 0, 12)
	seen := make(map[string]struct{})
	var cursor *string
	for {
		params := map[string]any{"includeHidden": false, "limit": 100}
		if cursor != nil && strings.TrimSpace(*cursor) != "" {
			params["cursor"] = strings.TrimSpace(*cursor)
		}
		var response codexModelListResponse
		if err := codexCallSync(stdin, reader, "model/list", params, &response); err != nil {
			return nil, err
		}
		for _, entry := range response.Data {
			model, ok := runtimeModelInfoFromCodexEntry(entry)
			if !ok {
				continue
			}
			id := model.ID
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			models = append(models, model)
		}
		cursor = response.NextCursor
		if cursor == nil || strings.TrimSpace(*cursor) == "" {
			break
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("codex returned an empty model catalog")
	}
	return models, nil
}

func runtimeModelInfoFromCodexEntry(entry codexModelListEntry) (protocol.RuntimeModelInfo, bool) {
	id := strings.TrimSpace(entry.ID)
	if id == "" {
		id = strings.TrimSpace(entry.Model)
	}
	if id == "" || entry.Hidden {
		return protocol.RuntimeModelInfo{}, false
	}
	efforts := make([]protocol.RuntimeReasoningEffortInfo, 0, len(entry.SupportedReasoningEfforts))
	for _, effort := range entry.SupportedReasoningEfforts {
		effortID := strings.TrimSpace(effort.ReasoningEffort)
		if effortID == "" {
			continue
		}
		efforts = append(efforts, protocol.RuntimeReasoningEffortInfo{
			ID: effortID, Title: reasoningEffortTitle(effortID), Detail: strings.TrimSpace(effort.Description),
		})
	}
	return protocol.RuntimeModelInfo{
		ID: id, Title: strings.TrimSpace(entry.DisplayName), Detail: strings.TrimSpace(entry.Description),
		DefaultReasoningEffort:    strings.TrimSpace(entry.DefaultReasoningEffort),
		SupportedReasoningEfforts: efforts, IsDefault: entry.IsDefault,
	}, true
}

func stopCodexCatalogProcess(cmd *exec.Cmd, stdin io.Closer) {
	_ = stdin.Close()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
}

func reasoningEffortTitle(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "minimal":
		return "Minimal"
	case "low":
		return "Low"
	case "medium":
		return "Medium"
	case "high":
		return "High"
	case "xhigh":
		return "Extra High"
	case "max":
		return "Max"
	case "ultra":
		return "Ultra"
	default:
		return strings.TrimSpace(value)
	}
}
