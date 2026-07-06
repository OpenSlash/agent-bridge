package remote

import (
	"regexp"
	"strings"
)

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

const compactSummaryPrefix = "This session is being continued from a previous conversation that ran out of context."
const compactBoundaryText = "Compact session completed"

type transcriptDisplayMessage struct {
	Role        string
	Content     string
	MessageType string
}

func normalizeTranscriptDisplayEntry(_ map[string]any, fallbackRole, rawContent string) (transcriptDisplayMessage, bool) {
	content := strings.TrimSpace(rawContent)
	if content == "" {
		return transcriptDisplayMessage{}, false
	}

	if command := extractTranscriptCommandInvocation(content); command != "" {
		return transcriptDisplayMessage{
			Role:        "user",
			Content:     command,
			MessageType: "command_input",
		}, true
	}

	if stdout, ok := extractTranscriptTaggedContent(content, "local-command-stdout"); ok {
		cleaned := cleanTranscriptTaggedText(stdout)
		if compactCompletionText, ok := normalizeCompactCommandStdout(cleaned); ok {
			return transcriptDisplayMessage{
				Role:        "system",
				Content:     compactCompletionText,
				MessageType: "text",
			}, true
		}
		if shouldIgnoreLocalCommandStdout(cleaned) {
			return transcriptDisplayMessage{}, false
		}
		return transcriptDisplayMessage{
			Role:        "system",
			Content:     cleaned,
			MessageType: "text",
		}, true
	}

	if _, ok := extractTranscriptTaggedContent(content, "local-command-caveat"); ok {
		return transcriptDisplayMessage{}, false
	}

	if shouldIgnoreTranscriptArtifact(content) {
		return transcriptDisplayMessage{}, false
	}

	if strings.HasPrefix(content, compactSummaryPrefix) {
		return transcriptDisplayMessage{
			Role:        "system",
			Content:     content,
			MessageType: "text",
		}, true
	}

	return transcriptDisplayMessage{
		Role:        fallbackRole,
		Content:     content,
		MessageType: "text",
	}, true
}

func shouldIgnoreLocalCommandStdout(content string) bool {
	normalized := strings.TrimSpace(strings.ToLower(content))
	return normalized == ""
}

func normalizeCompactCommandStdout(content string) (string, bool) {
	normalized := strings.TrimSpace(strings.ToLower(content))
	if normalized == "compacted" || strings.HasPrefix(normalized, "compacted ") {
		return compactBoundaryText, true
	}
	return "", false
}

func extractTranscriptCommandInvocation(content string) string {
	command := cleanTranscriptTaggedText(extractFirstTranscriptTag(content, "command-name"))
	if command == "" {
		command = cleanTranscriptTaggedText(extractFirstTranscriptTag(content, "command-message"))
		if command != "" && !strings.HasPrefix(command, "/") {
			command = "/" + command
		}
	}
	if command == "" {
		return ""
	}
	return strings.TrimSpace(command)
}

func shouldIgnoreTranscriptArtifact(content string) bool {
	for _, marker := range []string{
		"<local-command-caveat>",
		"<command-name>",
		"<command-message>",
		"<command-args>",
	} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

func extractTranscriptTaggedContent(content, tag string) (string, bool) {
	value := extractFirstTranscriptTag(content, tag)
	if value == "" {
		return "", false
	}
	return value, true
}

func extractFirstTranscriptTag(content, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(content, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(content[start:], close)
	if end < 0 {
		return ""
	}
	return content[start : start+end]
}

func cleanTranscriptTaggedText(content string) string {
	content = ansiEscapePattern.ReplaceAllString(content, "")
	return strings.TrimSpace(content)
}
