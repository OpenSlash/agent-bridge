package remote

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	manager "github.com/OpenSlash/agent-bridge/internal/clientmanager"
	"github.com/OpenSlash/agent-bridge/internal/toolpaths"
	"github.com/OpenSlash/agent-bridge/protocol"
)

const maxFilePreviewBytes = 256 * 1024

func (s *Service) handleListDir(sessionID string, req protocol.ListDirPayload) {
	resp := protocol.ListDirResponsePayload{
		RequestID: req.RequestID,
	}

	resolvedRoot, entries, err := searchDirEntries(req)
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Path = resolvedRoot
		resp.Entries = entries
	}

	msg := protocol.Message{
		Type:      protocol.TypeListDirResponse,
		SessionID: sessionID,
		Payload:   resp,
	}
	if err := s.writeJSON(msg); err != nil {
		applog.Errorf("[Remote] WS write list-dir-response error: %v", err)
	}
}

func (s *Service) handleReadFile(sessionID string, req protocol.ReadFilePayload) {
	resp := protocol.ReadFileResponsePayload{
		RequestID: req.RequestID,
	}

	response, err := readFilePreview(req)
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp = response
		resp.RequestID = req.RequestID
	}

	msg := protocol.Message{
		Type:      protocol.TypeReadFileResponse,
		SessionID: sessionID,
		Payload:   resp,
	}
	if err := s.writeJSON(msg); err != nil {
		applog.Errorf("[Remote] WS write read-file-response error: %v", err)
	}
}

func searchDirEntries(req protocol.ListDirPayload) (string, []protocol.DirEntry, error) {
	root, err := resolveDirectoryWithinUserHome(req.Path, "", true)
	if err != nil {
		return "", nil, err
	}

	query := strings.TrimSpace(req.Query)
	limit := req.Limit
	if limit <= 0 {
		if req.Recursive {
			limit = 20
		} else {
			limit = 200
		}
	}

	if req.Recursive && query != "" {
		entries, err := searchDirEntriesRecursive(root, query, limit)
		return root, entries, err
	}
	entries, err := listDirEntries(root, query, limit)
	return root, entries, err
}

func readFilePreview(req protocol.ReadFilePayload) (protocol.ReadFileResponsePayload, error) {
	path, err := resolveFileWithinUserHome(req.Path, "")
	if err != nil {
		return protocol.ReadFileResponsePayload{}, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return protocol.ReadFileResponsePayload{}, err
	}

	file, err := os.Open(path)
	if err != nil {
		return protocol.ReadFileResponsePayload{}, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxFilePreviewBytes+1))
	if err != nil {
		return protocol.ReadFileResponsePayload{}, err
	}

	truncated := false
	if len(data) > maxFilePreviewBytes {
		data = data[:maxFilePreviewBytes]
		truncated = true
	}

	response := protocol.ReadFileResponsePayload{
		Path:        path,
		Language:    detectPreviewLanguage(path),
		SizeBytes:   info.Size(),
		IsTruncated: truncated,
	}

	if looksLikeBinaryContent(data) {
		response.IsBinary = true
		return response, nil
	}

	response.Content = string(data)
	return response, nil
}

func listDirEntries(root, query string, limit int) ([]protocol.DirEntry, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	lowerQuery := strings.ToLower(strings.TrimSpace(query))
	result := make([]protocol.DirEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if shouldHideResourceEntry(name) {
			continue
		}
		if lowerQuery != "" {
			lowerName := strings.ToLower(name)
			if !strings.HasPrefix(lowerName, lowerQuery) && !strings.Contains(lowerName, lowerQuery) {
				continue
			}
		}
		absolutePath := filepath.Join(root, name)
		displayPath := name
		if root == string(filepath.Separator) {
			displayPath = filepath.ToSlash(filepath.Join("/", name))
		}
		result = append(result, protocol.DirEntry{
			Name:        name,
			Path:        absolutePath,
			DisplayPath: filepath.ToSlash(displayPath),
			IsDir:       entry.IsDir(),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].IsDir != result[j].IsDir {
			return result[i].IsDir
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func looksLikeBinaryContent(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	if !utf8.Valid(data) {
		return true
	}
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

func detectPreviewLanguage(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".swift":
		return "swift"
	case ".kt":
		return "kotlin"
	case ".java":
		return "java"
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".py":
		return "python"
	case ".js":
		return "javascript"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".jsx":
		return "jsx"
	case ".c":
		return "c"
	case ".cc", ".cpp", ".cxx":
		return "cpp"
	case ".h", ".hpp":
		return "cpp"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".xml":
		return "xml"
	case ".toml":
		return "toml"
	case ".sh":
		return "bash"
	case ".md":
		return "markdown"
	case ".sql":
		return "sql"
	case ".html":
		return "html"
	case ".css":
		return "css"
	default:
		return ""
	}
}

type scoredDirEntry struct {
	Entry protocol.DirEntry
	Score int
}

func searchDirEntriesRecursive(root, query string, limit int) ([]protocol.DirEntry, error) {
	lowerQuery := strings.ToLower(filepath.ToSlash(strings.TrimSpace(query)))
	scored := make([]scoredDirEntry, 0, limit)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}

		name := d.Name()
		if shouldHideResourceEntry(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relativePath = filepath.ToSlash(relativePath)
		score := scoreDirEntryMatch(relativePath, lowerQuery, d.IsDir())
		if score < 0 {
			return nil
		}

		scored = append(scored, scoredDirEntry{
			Entry: protocol.DirEntry{
				Name:        name,
				Path:        path,
				DisplayPath: relativePath,
				IsDir:       d.IsDir(),
			},
			Score: score,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score < scored[j].Score
		}
		if scored[i].Entry.IsDir != scored[j].Entry.IsDir {
			return scored[i].Entry.IsDir
		}
		leftPath := strings.ToLower(scored[i].Entry.DisplayPath)
		rightPath := strings.ToLower(scored[j].Entry.DisplayPath)
		if len(leftPath) != len(rightPath) {
			return len(leftPath) < len(rightPath)
		}
		return leftPath < rightPath
	})

	if limit > 0 && len(scored) > limit {
		scored = scored[:limit]
	}

	result := make([]protocol.DirEntry, 0, len(scored))
	for _, item := range scored {
		result = append(result, item.Entry)
	}
	return result, nil
}

func shouldHideResourceEntry(name string) bool {
	return len(name) > 0 && name[0] == '.'
}

func scoreDirEntryMatch(displayPath, lowerQuery string, isDir bool) int {
	normalizedPath := strings.ToLower(filepath.ToSlash(strings.TrimSpace(displayPath)))
	if normalizedPath == "" {
		return -1
	}

	baseName := strings.ToLower(filepath.Base(normalizedPath))
	dirBias := 0
	if isDir {
		dirBias = -1
	}

	switch {
	case baseName == lowerQuery:
		return 0 + dirBias
	case strings.HasPrefix(baseName, lowerQuery):
		return 10 + len(baseName) - len(lowerQuery) + dirBias
	case strings.Contains(baseName, lowerQuery):
		return 30 + strings.Index(baseName, lowerQuery) + dirBias
	case normalizedPath == lowerQuery:
		return 40 + dirBias
	case strings.HasPrefix(normalizedPath, lowerQuery):
		return 50 + len(normalizedPath) - len(lowerQuery) + dirBias
	case strings.Contains(normalizedPath, lowerQuery):
		return 70 + strings.Index(normalizedPath, lowerQuery) + dirBias
	case isSubsequenceMatch(normalizedPath, lowerQuery):
		return 120 + len(normalizedPath) + dirBias
	default:
		return -1
	}
}

func isSubsequenceMatch(target, query string) bool {
	if query == "" {
		return true
	}
	queryRunes := []rune(query)
	index := 0
	for _, ch := range target {
		if ch == queryRunes[index] {
			index++
			if index == len(queryRunes) {
				return true
			}
		}
	}
	return false
}

func (s *Service) handleListCommands(sessionID string, req protocol.ListCommandsPayload) {
	resp := protocol.ListCommandsResponsePayload{
		Query:     strings.TrimSpace(req.Query),
		RequestID: req.RequestID,
	}

	commands, err := s.listSlashCommands(resp.Query)
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Commands = commands
	}

	msg := protocol.Message{
		Type:      protocol.TypeListCommandsResponse,
		SessionID: sessionID,
		Payload:   resp,
	}
	if err := s.writeJSON(msg); err != nil {
		applog.Errorf("[Remote] WS write list-commands-response error: %v", err)
	}
}

func (s *Service) handleListSkills(sessionID string, req protocol.ListSkillsPayload) {
	resp := protocol.ListSkillsResponsePayload{
		Query:     strings.TrimSpace(req.Query),
		RequestID: req.RequestID,
	}

	skills, err := s.listCodexSkills(resp.Query)
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Skills = skills
	}

	msg := protocol.Message{
		Type:      protocol.TypeListSkillsResponse,
		SessionID: sessionID,
		Payload:   resp,
	}
	if err := s.writeJSON(msg); err != nil {
		applog.Errorf("[Remote] WS write list-skills-response error: %v", err)
	}
}

func (s *Service) handleSessionKeyRequest(sessionID string, req protocol.SessionKeyRequestPayload) {
	applog.Info.Printf(
		"[Remote] building session-key-response: session=%s request=%s scope=%s management=%t",
		sessionID,
		req.RequestID,
		req.Scope,
		s.cfg.Management,
	)
	resp, err := s.contentProtector.BuildSessionKeyResponse(sessionID, req)
	if err != nil {
		resp = protocol.SessionKeyResponsePayload{
			RequestID: req.RequestID,
			Scope:     strings.TrimSpace(req.Scope),
			Error:     err.Error(),
		}
	}

	msg := protocol.Message{
		Type:      protocol.TypeSessionKeyResponse,
		SessionID: sessionID,
		Payload:   resp,
	}
	if err := s.writeJSON(msg); err != nil {
		applog.Errorf("[Remote] WS write session-key-response error: %v", err)
		return
	}
	applog.Info.Printf(
		"[Remote] session-key-response sent: session=%s request=%s scope=%s management=%t error=%s",
		sessionID,
		resp.RequestID,
		resp.Scope,
		s.cfg.Management,
		resp.Error,
	)
}

func (s *Service) listSlashCommands(query string) ([]protocol.SlashCommandEntry, error) {
	return listSlashCommandsForContext(s.getRuntime(), s.getCurrentDir(), query)
}

func (s *Service) listCodexSkills(query string) ([]protocol.SkillEntry, error) {
	if s.getRuntime() != runtimeCodex {
		return []protocol.SkillEntry{}, nil
	}

	skills, err := manager.ListAllCodexSkills()
	if err != nil {
		return nil, err
	}

	normalizedQuery := strings.ToLower(strings.TrimSpace(query))
	filtered := make([]protocol.SkillEntry, 0, len(skills))
	for _, skill := range skills {
		name := strings.TrimSpace(skill.Name)
		summary := strings.TrimSpace(skill.Description)
		if name == "" {
			continue
		}
		if normalizedQuery != "" {
			lowerName := strings.ToLower(name)
			lowerSummary := strings.ToLower(summary)
			if !strings.Contains(lowerName, normalizedQuery) && !strings.Contains(lowerSummary, normalizedQuery) {
				continue
			}
		}
		filtered = append(filtered, protocol.SkillEntry{
			Name:    name,
			Summary: summary,
			Source:  codexSkillSource(skill.FilePath),
			Path:    skill.FilePath,
		})
	}

	sort.Slice(filtered, func(i, j int) bool {
		left := filtered[i]
		right := filtered[j]
		if left.Source != right.Source {
			return codexSkillSourcePriority(left.Source) < codexSkillSourcePriority(right.Source)
		}
		return strings.ToLower(left.Name) < strings.ToLower(right.Name)
	})
	return filtered, nil
}

func codexSkillSource(path string) string {
	cleanedPath := filepath.ToSlash(strings.TrimSpace(path))
	codexRoot := filepath.ToSlash(filepath.Join(toolpaths.CodexDir(), "skills"))
	sharedRoot := filepath.ToSlash(toolpaths.AgentsSkillsDir())
	switch {
	case strings.Contains(cleanedPath, "/.system/"):
		return "system"
	case sharedRoot != "" && strings.HasPrefix(cleanedPath, sharedRoot+"/"):
		return "shared"
	case codexRoot != "" && strings.HasPrefix(cleanedPath, codexRoot+"/"):
		return "user"
	default:
		return "shared"
	}
}

func codexSkillSourcePriority(source string) int {
	switch source {
	case "user":
		return 0
	case "shared":
		return 1
	case "system":
		return 2
	default:
		return 3
	}
}

type slashCommandSource struct {
	path     string
	source   string
	priority int
}

type prioritizedSlashCommandEntry struct {
	Entry    protocol.SlashCommandEntry
	Priority int
}

func mergeSlashCommandEntry(entries map[string]prioritizedSlashCommandEntry, entry protocol.SlashCommandEntry, priority int) {
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		return
	}

	current, exists := entries[name]
	if exists && current.Priority > priority {
		return
	}

	entries[name] = prioritizedSlashCommandEntry{
		Entry:    entry,
		Priority: priority,
	}
}

func scanClaudePluginCommandSources() ([]slashCommandSource, error) {
	plugins, err := manager.ListPlugins()
	if err != nil {
		return nil, err
	}

	var sources []slashCommandSource
	seen := make(map[string]struct{})

	for _, plugin := range plugins {
		commandDir := filepath.Join(plugin.InstallPath, "commands")
		info, statErr := os.Stat(commandDir)
		if statErr != nil || !info.IsDir() {
			continue
		}

		if _, ok := seen[commandDir]; ok {
			continue
		}
		seen[commandDir] = struct{}{}

		sourcePrefix := "plugin-disabled"
		priority := 20
		if plugin.Enabled {
			sourcePrefix = "plugin"
			priority = 30
		}

		pluginName := strings.TrimSpace(plugin.Name)
		source := sourcePrefix
		if pluginName != "" {
			source = sourcePrefix + ":" + pluginName
		}

		sources = append(sources, slashCommandSource{
			path:     commandDir,
			source:   source,
			priority: priority,
		})
	}

	marketplaceSources, err := scanMarketplacePluginCommandSources(seen)
	if err != nil {
		return nil, err
	}
	sources = append(sources, marketplaceSources...)

	return sources, nil
}

func scanMarketplacePluginCommandSources(seen map[string]struct{}) ([]slashCommandSource, error) {
	root := manager.PluginsDir()
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}

	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	var sources []slashCommandSource
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() != "commands" {
			return nil
		}
		if _, ok := seen[path]; ok {
			return filepath.SkipDir
		}

		source := "plugin-catalog"
		if pluginName := inferPluginCommandSourceName(path); pluginName != "" {
			source = "plugin-catalog:" + pluginName
		}
		sources = append(sources, slashCommandSource{
			path:     path,
			source:   source,
			priority: 10,
		})
		seen[path] = struct{}{}
		return filepath.SkipDir
	})
	if err != nil {
		return nil, err
	}

	return sources, nil
}

func inferPluginCommandSourceName(path string) string {
	normalized := filepath.ToSlash(strings.TrimSpace(path))
	if normalized == "" {
		return ""
	}

	parts := strings.Split(normalized, "/")
	if len(parts) < 2 {
		return ""
	}
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "commands" {
			continue
		}
		if i > 0 {
			candidate := strings.TrimSpace(parts[i-1])
			if candidate == "" || strings.HasPrefix(candidate, ".") {
				if i > 1 {
					return strings.TrimSpace(parts[i-2])
				}
				return ""
			}
			return candidate
		}
		break
	}
	return ""
}

func scanSlashCommandDir(root, source string) ([]protocol.SlashCommandEntry, error) {
	if root == "" {
		return nil, nil
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("command path is not a directory: %s", root)
	}

	var commands []protocol.SlashCommandEntry
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		if strings.EqualFold(d.Name(), "README.md") {
			return nil
		}

		entry, err := parseSlashCommandFile(root, path, source)
		if err != nil {
			applog.Errorf("[Remote] parse slash command file error: path=%s err=%v", path, err)
			return nil
		}
		commands = append(commands, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return commands, nil
}

func parseSlashCommandFile(root, filePath, source string) (protocol.SlashCommandEntry, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return protocol.SlashCommandEntry{}, err
	}

	relPath, err := filepath.Rel(root, filePath)
	if err != nil {
		return protocol.SlashCommandEntry{}, err
	}
	relPath = filepath.ToSlash(relPath)
	name := strings.TrimSuffix(relPath, filepath.Ext(relPath))
	name = strings.ReplaceAll(name, "/", ":")

	metadata := parseMarkdownFrontmatter(string(content))
	displayName := strings.TrimSpace(metadata["name"])
	if displayName == "" {
		displayName = name
	}

	summary := strings.TrimSpace(metadata["description"])
	if summary == "" {
		summary = extractMarkdownDescription(string(content))
	}

	argumentHint := strings.TrimSpace(metadata["argument-hint"])
	if argumentHint == "" {
		argumentHint = strings.TrimSpace(metadata["argument_hint"])
	}

	return protocol.SlashCommandEntry{
		Name:         displayName,
		Summary:      summary,
		Source:       source,
		ArgumentHint: argumentHint,
	}, nil
}

func parseMarkdownFrontmatter(content string) map[string]string {
	metadata := make(map[string]string)
	lines := strings.Split(content, "\n")
	inFrontmatter := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			}
			break
		}

		if !inFrontmatter {
			continue
		}

		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		metadata[key] = strings.Trim(value, "\"'")
	}

	return metadata
}

func extractMarkdownDescription(content string) string {
	body := stripMarkdownFrontmatter(content)
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if len(trimmed) > 140 {
			return trimmed[:140] + "..."
		}
		return trimmed
	}
	return ""
}

func stripMarkdownFrontmatter(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return content
	}

	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		}
	}

	return content
}

func slashCommandMatchesQuery(entry protocol.SlashCommandEntry, query string) bool {
	query = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(query, "/")))
	if query == "" {
		return true
	}

	name := strings.ToLower(entry.Name)
	summary := strings.ToLower(entry.Summary)
	return strings.HasPrefix(name, query) || strings.Contains(name, query) || strings.Contains(summary, query)
}

func sortSlashCommands(entries []protocol.SlashCommandEntry, query string) {
	query = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(query, "/")))
	sourceRank := map[string]int{
		"project":         0,
		"global":          1,
		"native":          2,
		"built-in":        3,
		"plugin":          4,
		"plugin-disabled": 5,
		"plugin-catalog":  6,
	}

	sort.Slice(entries, func(i, j int) bool {
		left := entries[i]
		right := entries[j]

		leftName := strings.ToLower(left.Name)
		rightName := strings.ToLower(right.Name)
		leftPrefix := query != "" && strings.HasPrefix(leftName, query)
		rightPrefix := query != "" && strings.HasPrefix(rightName, query)
		if leftPrefix != rightPrefix {
			return leftPrefix
		}

		leftRank := sourceRank[normalizeSlashSourceRank(left.Source)]
		rightRank := sourceRank[normalizeSlashSourceRank(right.Source)]
		if leftRank != rightRank {
			return leftRank < rightRank
		}

		return leftName < rightName
	})
}

func normalizeSlashSourceRank(source string) string {
	if strings.HasPrefix(source, "plugin:") {
		return "plugin"
	}
	if strings.HasPrefix(source, "plugin-disabled:") {
		return "plugin-disabled"
	}
	if strings.HasPrefix(source, "plugin-catalog:") {
		return "plugin-catalog"
	}
	return source
}
