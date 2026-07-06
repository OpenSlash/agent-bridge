package remote

import (
	"os"
	"strings"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func ListSlashCommandsForRuntime(runtimeID, currentDir, query string) ([]protocol.SlashCommandEntry, error) {
	return listSlashCommandsForContext(runtimeKindFromID(runtimeID), currentDir, query)
}

func ListDirectoryEntries(path, query string, limit int, recursive bool) (string, []protocol.DirEntry, error) {
	return searchDirEntries(protocol.ListDirPayload{
		Path:      strings.TrimSpace(path),
		Query:     strings.TrimSpace(query),
		Limit:     limit,
		Recursive: recursive,
	})
}

func runtimeKindFromID(runtimeID string) runtimeKind {
	switch strings.ToLower(strings.TrimSpace(runtimeID)) {
	case string(runtimeCodex):
		return runtimeCodex
	default:
		return runtimeClaude
	}
}

func listSlashCommandsForContext(runtime runtimeKind, currentDir, query string) ([]protocol.SlashCommandEntry, error) {
	sources, err := runtimeSlashCommandSources(runtime, strings.TrimSpace(currentDir))
	if err != nil {
		return nil, err
	}

	entries := make(map[string]prioritizedSlashCommandEntry)
	for _, item := range runtimeBuiltinSlashCommands(runtime) {
		priority := 40
		if item.Source == "native" {
			priority = 70
		}
		mergeSlashCommandEntry(entries, item, priority)
	}

	for _, source := range sources {
		customEntries, err := scanSlashCommandDir(source.path, source.source)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range customEntries {
			mergeSlashCommandEntry(entries, entry, source.priority)
		}
	}

	filtered := make([]protocol.SlashCommandEntry, 0, len(entries))
	for _, entry := range entries {
		if slashCommandMatchesQuery(entry.Entry, query) {
			filtered = append(filtered, entry.Entry)
		}
	}

	sortSlashCommands(filtered, query)
	return filtered, nil
}
