package remote

import (
	"github.com/OpenSlash/agent-bridge/protocol"
)

// DisplayParser 将 runtime 流式输出转换为适合 GUI 展示的消息。
// Codex 的实时通知已经在 bridge 中被转换为 Claude 风格事件，因此统一复用 Claude 解析器。
type DisplayParser struct {
	parser *claudeStreamHistoryParser
}

func NewDisplayParser() *DisplayParser {
	return &DisplayParser{
		parser: newClaudeStreamHistoryParser(nil),
	}
}

func (p *DisplayParser) ParseTextPayload(payload protocol.TextPayload) []protocol.SessionHistoryMessage {
	if p == nil || p.parser == nil {
		return nil
	}
	return p.parser.ParseLine([]byte(payload.Data))
}
