package clientmanager

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/OpenSlash/agent-bridge/internal/toolpaths"
)

type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Directory   string `json:"directory"`
	FilePath    string `json:"filePath"`
	IsSymlink   bool   `json:"isSymlink"`
	License     string `json:"license"`
}

type Plugin struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Marketplace string   `json:"marketplace"`
	Version     string   `json:"version"`
	Scope       string   `json:"scope"`
	Enabled     bool     `json:"enabled"`
	InstallPath string   `json:"installPath"`
	InstalledAt string   `json:"installedAt"`
	LastUpdated string   `json:"lastUpdated"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Homepage    string   `json:"homepage"`
	Keywords    []string `json:"keywords"`
	Agents      []string `json:"agents"`
	Commands    []string `json:"commands"`
	Skills      []string `json:"skills"`
}

func PluginsDir() string {
	return toolpaths.PluginsDir()
}

func ListPlugins() ([]Plugin, error) {
	root := PluginsDir()
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []Plugin{}, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return []Plugin{}, nil
	}

	plugins := []Plugin{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(d.Name()) {
		case "plugin.json", "package.json":
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var plugin Plugin
		if err := json.Unmarshal(data, &plugin); err != nil {
			return nil
		}
		if plugin.Name == "" {
			plugin.Name = filepath.Base(filepath.Dir(path))
		}
		if plugin.InstallPath == "" {
			plugin.InstallPath = filepath.Dir(path)
		}
		plugins = append(plugins, plugin)
		return filepath.SkipDir
	})
	if err != nil {
		return nil, err
	}
	return plugins, nil
}

func ListAllCodexSkills() ([]Skill, error) {
	sources := []string{
		toolpaths.AgentsSkillsDir(),
		filepath.Join(toolpaths.CodexDir(), "skills"),
	}

	skillsByPath := make(map[string]Skill)
	for _, source := range sources {
		skills, err := listCodexSkillsFromRoot(source)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, skill := range skills {
			if skill.FilePath != "" {
				skillsByPath[skill.FilePath] = skill
			}
		}
	}

	result := make([]Skill, 0, len(skillsByPath))
	for _, skill := range skillsByPath {
		result = append(result, skill)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].FilePath < result[j].FilePath
	})
	return result, nil
}

func listCodexSkillsFromRoot(root string) ([]Skill, error) {
	if !toolpaths.FileExists(root) {
		return nil, os.ErrNotExist
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	var skills []Skill
	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}

		entryPath := filepath.Join(root, entry.Name())
		if entry.Name() == ".system" {
			nested, nestedErr := listSkillsFromDir(entryPath)
			if nestedErr == nil {
				skills = append(skills, nested...)
			}
			continue
		}

		skillPath := filepath.Join(entryPath, "SKILL.md")
		if !toolpaths.FileExists(skillPath) && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		skill, err := parseSkillDir(entryPath, entry.Name())
		if err != nil {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			skill.IsSymlink = true
		}
		skills = append(skills, skill)
	}
	return skills, nil
}

func listSkillsFromDir(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var skills []Skill
	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		skillPath := filepath.Join(dir, entry.Name())
		skill, err := parseSkillDir(skillPath, entry.Name())
		if err != nil {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			skill.IsSymlink = true
		}
		skills = append(skills, skill)
	}
	return skills, nil
}

func parseSkillDir(skillPath, dirName string) (Skill, error) {
	skill := Skill{
		Name:      dirName,
		Directory: dirName,
		FilePath:  skillPath,
	}
	metadata, err := parseSkillMd(filepath.Join(skillPath, "SKILL.md"))
	if err == nil {
		if metadata["name"] != "" {
			skill.Name = metadata["name"]
		}
		skill.Description = metadata["description"]
		skill.License = metadata["license"]
	}
	return skill, nil
}

func parseSkillMd(filePath string) (map[string]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	metadata := make(map[string]string)
	scanner := bufio.NewScanner(file)
	inFrontmatter := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			}
			break
		}
		if !inFrontmatter {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		metadata[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return metadata, scanner.Err()
}
