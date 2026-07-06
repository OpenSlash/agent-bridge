package remote

import "testing"

func TestShouldEmitTranscriptMirrorLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "assistant",
			line: `{"type":"assistant","message":{"role":"assistant","content":"hi"}}`,
			want: true,
		},
		{
			name: "user",
			line: `{"type":"user","message":{"role":"user","content":"hello"}}`,
			want: true,
		},
		{
			name: "system",
			line: `{"type":"system","content":"ready"}`,
			want: true,
		},
		{
			name: "summary",
			line: `{"type":"summary","summary":"done"}`,
			want: true,
		},
		{
			name: "progress",
			line: `{"type":"progress","status":"thinking"}`,
			want: true,
		},
		{
			name: "meta entry",
			line: `{"type":"assistant","isMeta":true}`,
			want: false,
		},
		{
			name: "file snapshot",
			line: `{"type":"file-history-snapshot"}`,
			want: false,
		},
		{
			name: "result",
			line: `{"type":"result","subtype":"success"}`,
			want: false,
		},
		{
			name: "invalid json",
			line: `{invalid`,
			want: false,
		},
	}

	for _, tt := range tests {
		if got := shouldEmitTranscriptMirrorLine(tt.line); got != tt.want {
			t.Fatalf("%s: expected %t, got %t", tt.name, tt.want, got)
		}
	}
}

func TestExtractTranscriptMirrorToolUses(t *testing.T) {
	entryType, entry, ok := parseTranscriptMirrorEntry(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"pwd"}},{"type":"text","text":"done"}]}}`)
	if !ok {
		t.Fatal("expected transcript entry to parse")
	}

	toolUses := extractTranscriptMirrorToolUses(entryType, entry)
	if len(toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(toolUses))
	}
	if toolUses[0].RequestID != "toolu_1" || toolUses[0].Tool != "Bash" {
		t.Fatalf("unexpected tool use: %+v", toolUses[0])
	}
	if got := toolUses[0].Input["command"]; got != "pwd" {
		t.Fatalf("expected command pwd, got %+v", got)
	}
}

func TestExtractTranscriptMirrorToolResultIDs(t *testing.T) {
	entryType, entry, ok := parseTranscriptMirrorEntry(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"text","text":"ignored"}]}}`)
	if !ok {
		t.Fatal("expected transcript entry to parse")
	}

	ids := extractTranscriptMirrorToolResultIDs(entryType, entry)
	if len(ids) != 1 || ids[0] != "toolu_1" {
		t.Fatalf("unexpected tool result ids: %+v", ids)
	}
}
