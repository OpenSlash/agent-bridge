package remote

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestContentProtectorProtectHistoryBatch(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	protector, err := newContentProtector()
	if err != nil {
		t.Fatalf("newContentProtector failed: %v", err)
	}

	sessionID := "session-123"
	batch := []protocol.SessionHistoryMessage{
		{
			SourceID:       "tool:1",
			SourceKind:     "claude-transcript",
			Role:           "assistant",
			Content:        "Edit file",
			MessageType:    "tool_call",
			ToolName:       "Edit",
			ToolCallID:     "tool-1",
			ToolInput:      `{"file_path":"/tmp/a.txt"}`,
			ToolResult:     `{"ok":true}`,
			IsToolComplete: true,
			Timestamp:      12345,
		},
	}

	protected, err := protector.ProtectHistoryBatch(sessionID, batch)
	if err != nil {
		t.Fatalf("ProtectHistoryBatch failed: %v", err)
	}
	if got, want := protected[0].Role, e2eeHistoryPlaceholderRole; got != want {
		t.Fatalf("unexpected protected role: got %q want %q", got, want)
	}
	if got, want := protected[0].MessageType, e2eeHistoryPlaceholderMessageType; got != want {
		t.Fatalf("unexpected protected message type: got %q want %q", got, want)
	}

	key, err := protector.keyProvider.ResolveKey(sessionID, e2eeScopeSession)
	if err != nil {
		t.Fatalf("ResolveKey failed: %v", err)
	}
	blob, err := decodeProtectedBlobString(protected[0].Content)
	if err != nil {
		t.Fatalf("decodeProtectedBlobString failed: %v", err)
	}
	plaintext, err := decryptProtectedBlob(key, blob, buildHistoryAAD(sessionID, protected[0].SourceID))
	if err != nil {
		t.Fatalf("decryptProtectedBlob failed: %v", err)
	}

	var envelope protectedHistoryEnvelope
	if err := json.Unmarshal(plaintext, &envelope); err != nil {
		t.Fatalf("unmarshal protected history envelope failed: %v", err)
	}
	if envelope.ToolInput != batch[0].ToolInput || envelope.ToolResult != batch[0].ToolResult || envelope.Role != batch[0].Role {
		t.Fatalf("unexpected decrypted history envelope: %+v", envelope)
	}
}

func TestContentProtectorBuildSessionKeyResponseWrapsScopedSessionKey(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	protector, err := newContentProtector()
	if err != nil {
		t.Fatalf("newContentProtector failed: %v", err)
	}

	sessionID := "session-456"
	clientKeyPair, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}

	req := protocol.SessionKeyRequestPayload{
		RequestID:       "req-1",
		ClientPublicKey: base64.StdEncoding.EncodeToString(clientKeyPair.PublicKey().Bytes()),
		Scope:           e2eeScopeSession,
	}
	resp, err := protector.BuildSessionKeyResponse(sessionID, req)
	if err != nil {
		t.Fatalf("BuildSessionKeyResponse failed: %v", err)
	}
	if resp.WrappedKey == nil {
		t.Fatal("expected wrapped session key")
	}

	hostPublicKeyBytes, err := base64.StdEncoding.DecodeString(resp.HostPublicKey)
	if err != nil {
		t.Fatalf("DecodeString failed: %v", err)
	}
	hostPublicKey, err := ecdh.X25519().NewPublicKey(hostPublicKeyBytes)
	if err != nil {
		t.Fatalf("NewPublicKey failed: %v", err)
	}

	sharedSecret, err := clientKeyPair.ECDH(hostPublicKey)
	if err != nil {
		t.Fatalf("ECDH failed: %v", err)
	}

	wrapKey := deriveWrapKey(sharedSecret, sessionID, e2eeScopeSession)
	unwrappedKey, err := decryptProtectedBlob(wrapKey, resp.WrappedKey, buildSessionKeyAAD(sessionID, req.RequestID, e2eeScopeSession))
	if err != nil {
		t.Fatalf("decryptProtectedBlob failed: %v", err)
	}

	expectedKey, err := protector.keyProvider.ResolveKey(sessionID, e2eeScopeSession)
	if err != nil {
		t.Fatalf("ResolveKey failed: %v", err)
	}
	if string(unwrappedKey) != string(expectedKey) {
		t.Fatal("wrapped session key did not match derived scoped key")
	}
}

func TestBuildProtectedMetaIncludesPermissionFields(t *testing.T) {
	meta := buildProtectedMeta(protocol.PermissionRequestPayload{
		RequestID: "req-123",
		Tool:      "Bash",
		Summary:   "Run npm test",
	})

	if got, want := meta["request_id"], "req-123"; got != want {
		t.Fatalf("unexpected request_id: got %q want %q", got, want)
	}
	if got, want := meta["tool"], "Bash"; got != want {
		t.Fatalf("unexpected tool: got %q want %q", got, want)
	}
	if got, want := meta["summary"], "Run npm test"; got != want {
		t.Fatalf("unexpected summary: got %q want %q", got, want)
	}
}

func TestContentProtectorRoundTripsRealtimeSessionPayload(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	protector, err := newContentProtector()
	if err != nil {
		t.Fatalf("newContentProtector failed: %v", err)
	}

	targetID := "session-live"
	original := protocol.TextPayload{
		Data:     "c29tZS1iYXNlNjQ=",
		Thinking: true,
	}

	protected, err := protector.ProtectPayload(targetID, protocol.TypeText, original, false)
	if err != nil {
		t.Fatalf("ProtectPayload failed: %v", err)
	}

	blob, ok := protected.(*protocol.ProtectedBlob)
	if !ok {
		t.Fatalf("expected protected blob payload, got %T", protected)
	}
	if got, want := blob.Scope, e2eeScopeSession; got != want {
		t.Fatalf("unexpected protected scope: got %q want %q", got, want)
	}

	var decoded protocol.TextPayload
	if err := protector.DecodePayload(targetID, protocol.TypeText, protected, false, &decoded); err != nil {
		t.Fatalf("DecodePayload failed: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("decoded payload mismatch: got %+v want %+v", decoded, original)
	}
}

func TestContentProtectorRoundTripsRealtimeManagerPayload(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	protector, err := newContentProtector()
	if err != nil {
		t.Fatalf("newContentProtector failed: %v", err)
	}

	targetID := "host-manager"
	original := protocol.ListDirResponsePayload{
		Path:      "/Users/demo/project",
		RequestID: "req-list-dir",
		Entries: []protocol.DirEntry{
			{
				Name:        "src",
				Path:        "/Users/demo/project/src",
				DisplayPath: "src",
				IsDir:       true,
			},
		},
	}

	protected, err := protector.ProtectPayload(targetID, protocol.TypeListDirResponse, original, true)
	if err != nil {
		t.Fatalf("ProtectPayload failed: %v", err)
	}

	blob, ok := protected.(*protocol.ProtectedBlob)
	if !ok {
		t.Fatalf("expected protected blob payload, got %T", protected)
	}
	if got, want := blob.Scope, e2eeScopeManager; got != want {
		t.Fatalf("unexpected protected scope: got %q want %q", got, want)
	}

	var decoded protocol.ListDirResponsePayload
	if err := protector.DecodePayload(targetID, protocol.TypeListDirResponse, protected, true, &decoded); err != nil {
		t.Fatalf("DecodePayload failed: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("decoded payload mismatch: got %+v want %+v", decoded, original)
	}
}
