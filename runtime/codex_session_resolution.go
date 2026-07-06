package remote

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type CodexSessionResolution struct {
	RuntimeSessionID         string
	Resume                   bool
	RolloutPath              string
	AdoptedRecentSession     bool
	ReplacedRequestedSession bool
}

type codexRolloutMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID            string `json:"id"`
		Timestamp     string `json:"timestamp"`
		Cwd           string `json:"cwd"`
		Originator    string `json:"originator"`
		CLIVersion    string `json:"cli_version"`
		Source        string `json:"source"`
		ModelProvider string `json:"model_provider"`
	} `json:"payload"`
}

type CodexLocalSession struct {
	RuntimeSessionID string
	Cwd              string
	Path             string
	ModelProvider    string
	CLIVersion       string
	Source           string
	Originator       string
	SessionTime      time.Time
	ModTime          time.Time
	LineCount        int
}

var errCodexRolloutNotReady = fmt.Errorf("codex rollout not ready")

func ResolveCodexSessionStart(requestedRuntimeSessionID, cwd string, resume bool) (CodexSessionResolution, error) {
	requested := strings.TrimSpace(requestedRuntimeSessionID)

	if requested != "" && !resume {
		return CodexSessionResolution{
			RuntimeSessionID: requested,
			Resume:           false,
		}, nil
	}

	if requested != "" && resume {
		path, err := findCodexRolloutPath(requested)
		if err == nil {
			return CodexSessionResolution{
				RuntimeSessionID: requested,
				Resume:           true,
				RolloutPath:      path,
			}, nil
		}
		if !isCodexRolloutNotReady(err) {
			return CodexSessionResolution{}, err
		}

		return CodexSessionResolution{
			RuntimeSessionID: requested,
			Resume:           false,
		}, nil
	}

	existingRuntimeSessionID, rolloutPath, ok, err := findRecentCodexRolloutSession(cwd, 0)
	if err != nil {
		return CodexSessionResolution{
			Resume: false,
		}, err
	}
	if ok {
		return CodexSessionResolution{
			RuntimeSessionID:     existingRuntimeSessionID,
			Resume:               true,
			RolloutPath:          rolloutPath,
			AdoptedRecentSession: true,
		}, nil
	}

	return CodexSessionResolution{
		Resume: false,
	}, nil
}

func findCodexRolloutPath(runtimeSessionID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	pattern := filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*"+strings.TrimSpace(runtimeSessionID)+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}

	var (
		latestPath    string
		latestModTime time.Time
	)

	for _, match := range matches {
		info, statErr := os.Stat(match)
		if statErr != nil || info.IsDir() {
			continue
		}

		meta, metaErr := readCodexRolloutMeta(match)
		if metaErr != nil || strings.TrimSpace(meta.Payload.ID) != strings.TrimSpace(runtimeSessionID) {
			continue
		}

		if latestPath != "" && !info.ModTime().After(latestModTime) {
			continue
		}
		latestPath = match
		latestModTime = info.ModTime()
	}

	if latestPath == "" {
		return "", errCodexRolloutNotReady
	}
	return latestPath, nil
}

func findRecentCodexRolloutSession(cwd string, maxAge time.Duration) (runtimeSessionID, path string, ok bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", false, err
	}

	targetCWD := filepath.Clean(strings.TrimSpace(cwd))
	if targetCWD == "" || targetCWD == "." {
		return "", "", false, nil
	}

	matches, err := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"))
	if err != nil {
		return "", "", false, err
	}

	var (
		latestPath             string
		latestRuntimeSessionID string
		latestModTime          time.Time
	)

	for _, match := range matches {
		info, statErr := os.Stat(match)
		if statErr != nil || info.IsDir() {
			continue
		}
		if maxAge > 0 && time.Since(info.ModTime()) > maxAge {
			continue
		}

		meta, metaErr := readCodexRolloutMeta(match)
		if metaErr != nil {
			continue
		}
		if filepath.Clean(strings.TrimSpace(meta.Payload.Cwd)) != targetCWD {
			continue
		}
		if latestPath != "" && !info.ModTime().After(latestModTime) {
			continue
		}

		latestPath = match
		latestRuntimeSessionID = strings.TrimSpace(meta.Payload.ID)
		latestModTime = info.ModTime()
	}

	if latestRuntimeSessionID == "" {
		return "", "", false, nil
	}
	return latestRuntimeSessionID, latestPath, true, nil
}

func ListCodexLocalSessions(cwd string, includeAll bool) ([]CodexLocalSession, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	targetCWD := filepath.Clean(strings.TrimSpace(cwd))
	if !includeAll && (targetCWD == "" || targetCWD == ".") {
		return nil, nil
	}

	matches, err := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}

	latestByID := make(map[string]CodexLocalSession)
	for _, match := range matches {
		info, statErr := os.Stat(match)
		if statErr != nil || info.IsDir() {
			continue
		}

		meta, metaErr := readCodexRolloutMeta(match)
		if metaErr != nil {
			continue
		}
		runtimeSessionID := strings.TrimSpace(meta.Payload.ID)
		if runtimeSessionID == "" {
			continue
		}
		cleanCWD := filepath.Clean(strings.TrimSpace(meta.Payload.Cwd))
		if !includeAll && cleanCWD != targetCWD {
			continue
		}

		sessionTime := time.Time{}
		if rawTimestamp := strings.TrimSpace(meta.Payload.Timestamp); rawTimestamp != "" {
			if parsed, parseErr := time.Parse(time.RFC3339Nano, rawTimestamp); parseErr == nil {
				sessionTime = parsed
			}
		}

		session := CodexLocalSession{
			RuntimeSessionID: runtimeSessionID,
			Cwd:              cleanCWD,
			Path:             match,
			ModelProvider:    strings.TrimSpace(meta.Payload.ModelProvider),
			CLIVersion:       strings.TrimSpace(meta.Payload.CLIVersion),
			Source:           strings.TrimSpace(meta.Payload.Source),
			Originator:       strings.TrimSpace(meta.Payload.Originator),
			SessionTime:      sessionTime,
			ModTime:          info.ModTime(),
			LineCount:        countCodexRolloutLines(match),
		}
		if existing, exists := latestByID[runtimeSessionID]; exists && !session.ModTime.After(existing.ModTime) {
			continue
		}
		latestByID[runtimeSessionID] = session
	}

	sessions := make([]CodexLocalSession, 0, len(latestByID))
	for _, session := range latestByID {
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].ModTime.Equal(sessions[j].ModTime) {
			return sessions[i].ModTime.After(sessions[j].ModTime)
		}
		return sessions[i].RuntimeSessionID < sessions[j].RuntimeSessionID
	})
	return sessions, nil
}

func readCodexRolloutMeta(path string) (codexRolloutMeta, error) {
	file, err := os.Open(path)
	if err != nil {
		return codexRolloutMeta{}, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	line, err := reader.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return codexRolloutMeta{}, err
	}
	line = bytesTrimSpace(line)
	if len(line) == 0 {
		return codexRolloutMeta{}, errCodexRolloutNotReady
	}

	var meta codexRolloutMeta
	if err := json.Unmarshal(line, &meta); err != nil {
		return codexRolloutMeta{}, errCodexRolloutNotReady
	}
	if strings.TrimSpace(meta.Type) != "session_meta" || strings.TrimSpace(meta.Payload.ID) == "" {
		return codexRolloutMeta{}, errCodexRolloutNotReady
	}
	return meta, nil
}

func countCodexRolloutLines(path string) int {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	count := 0
	for {
		_, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return count
		}
		count++
	}
	return count
}

func isCodexRolloutNotReady(err error) bool {
	return err != nil && strings.Contains(err.Error(), errCodexRolloutNotReady.Error())
}

func bytesTrimSpace(data []byte) []byte {
	return []byte(strings.TrimSpace(string(data)))
}
