package remote

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/OpenSlash/agent-bridge/protocol"

	"golang.org/x/crypto/hkdf"
)

const (
	e2eeAlgAESGCM256        = "aes-gcm-256"
	e2eeKeyVersionV1        = 1
	e2eeScopeSession        = "session-content"
	e2eeScopeManager        = "manager-content"
	e2eeScopeSessionKeyWrap = "session-key-wrap"

	e2eeHistoryPlaceholderRole        = "protected"
	e2eeHistoryPlaceholderMessageType = "protected"
)

type protectedHistoryEnvelope struct {
	SourceKind     string `json:"source_kind,omitempty"`
	Role           string `json:"role,omitempty"`
	Content        string `json:"content,omitempty"`
	MessageType    string `json:"message_type,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
	ToolCallID     string `json:"tool_call_id,omitempty"`
	ToolInput      string `json:"tool_input,omitempty"`
	ToolResult     string `json:"tool_result,omitempty"`
	IsToolComplete bool   `json:"is_tool_complete,omitempty"`
}

type sessionContentKeyProvider struct {
	masterKey []byte
}

func newSessionContentKeyProvider() (*sessionContentKeyProvider, error) {
	masterKey, err := loadOrCreateE2EEMasterKey()
	if err != nil {
		return nil, err
	}
	return &sessionContentKeyProvider{masterKey: masterKey}, nil
}

func (p *sessionContentKeyProvider) ResolveKey(targetID, scope string) ([]byte, error) {
	if len(p.masterKey) == 0 {
		return nil, fmt.Errorf("master key is unavailable")
	}
	return deriveScopedKey(p.masterKey, targetID, scope), nil
}

type contentProtector struct {
	keyProvider *sessionContentKeyProvider
}

func newContentProtector() (*contentProtector, error) {
	keyProvider, err := newSessionContentKeyProvider()
	if err != nil {
		return nil, err
	}
	return &contentProtector{keyProvider: keyProvider}, nil
}

func (p *contentProtector) BuildSessionKeyResponse(targetID string, req protocol.SessionKeyRequestPayload) (protocol.SessionKeyResponsePayload, error) {
	if p == nil {
		return protocol.SessionKeyResponsePayload{}, fmt.Errorf("content protector is unavailable")
	}

	requestScope := normalizeRequestedScope(strings.TrimSpace(req.Scope))

	sessionKey, err := p.keyProvider.ResolveKey(targetID, requestScope)
	if err != nil {
		return protocol.SessionKeyResponsePayload{}, err
	}

	clientPublicKey, err := decodeX25519PublicKey(req.ClientPublicKey)
	if err != nil {
		return protocol.SessionKeyResponsePayload{}, err
	}

	curve := ecdh.X25519()
	hostPrivateKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return protocol.SessionKeyResponsePayload{}, err
	}

	sharedSecret, err := hostPrivateKey.ECDH(clientPublicKey)
	if err != nil {
		return protocol.SessionKeyResponsePayload{}, err
	}

	wrappingKey := deriveWrapKey(sharedSecret, targetID, requestScope)
	wrappedKey, err := encryptProtectedBlob(
		wrappingKey,
		e2eeScopeSessionKeyWrap,
		sessionKey,
		buildSessionKeyAAD(targetID, req.RequestID, requestScope),
		nil,
	)
	if err != nil {
		return protocol.SessionKeyResponsePayload{}, err
	}

	return protocol.SessionKeyResponsePayload{
		RequestID:     req.RequestID,
		HostPublicKey: base64.StdEncoding.EncodeToString(hostPrivateKey.PublicKey().Bytes()),
		Scope:         requestScope,
		WrappedKey:    wrappedKey,
	}, nil
}

func (p *contentProtector) ProtectPayload(targetID, messageType string, payload any, manager bool) (any, error) {
	if p == nil || !shouldProtectRealtimePayload(messageType) || payload == nil {
		return payload, nil
	}

	scope := payloadScope(manager)
	key, err := p.keyProvider.ResolveKey(targetID, scope)
	if err != nil {
		return nil, err
	}

	plaintext, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return encryptProtectedBlob(
		key,
		scope,
		plaintext,
		buildRealtimeAAD(scope, targetID, messageType),
		buildProtectedMeta(payload),
	)
}

func (p *contentProtector) DecodePayload(targetID, messageType string, payload any, manager bool, out any) error {
	if out == nil {
		return fmt.Errorf("decode payload target is nil")
	}

	if !shouldProtectRealtimePayload(messageType) {
		return decodePlainPayload(payload, out)
	}

	blob, err := decodeProtectedBlobPayload(payload)
	if err != nil {
		return decodePlainPayload(payload, out)
	}

	scope := strings.TrimSpace(blob.Scope)
	if scope == "" {
		scope = payloadScope(manager)
	}

	key, err := p.keyProvider.ResolveKey(targetID, scope)
	if err != nil {
		return err
	}

	plaintext, err := decryptProtectedBlob(
		key,
		blob,
		buildRealtimeAAD(scope, targetID, messageType),
	)
	if err != nil {
		return err
	}

	return json.Unmarshal(plaintext, out)
}

func (p *contentProtector) ProtectHistoryBatch(sessionID string, batch []protocol.SessionHistoryMessage) ([]protocol.SessionHistoryMessage, error) {
	if p == nil || len(batch) == 0 {
		return batch, nil
	}

	key, err := p.keyProvider.ResolveKey(sessionID, e2eeScopeSession)
	if err != nil {
		return nil, err
	}

	protected := make([]protocol.SessionHistoryMessage, 0, len(batch))
	for _, msg := range batch {
		sourceID := strings.TrimSpace(msg.SourceID)
		if sourceID == "" {
			continue
		}

		envelope := protectedHistoryEnvelope{
			SourceKind:     msg.SourceKind,
			Role:           msg.Role,
			Content:        msg.Content,
			MessageType:    msg.MessageType,
			ToolName:       msg.ToolName,
			ToolCallID:     msg.ToolCallID,
			ToolInput:      msg.ToolInput,
			ToolResult:     msg.ToolResult,
			IsToolComplete: msg.IsToolComplete,
		}
		plaintext, err := json.Marshal(envelope)
		if err != nil {
			return nil, err
		}

		blob, err := encryptProtectedBlob(
			key,
			e2eeScopeSession,
			plaintext,
			buildHistoryAAD(sessionID, sourceID),
			nil,
		)
		if err != nil {
			return nil, err
		}

		protected = append(protected, protocol.SessionHistoryMessage{
			SourceID:       msg.SourceID,
			SourceKind:     e2eeHistoryPlaceholderRole,
			Role:           e2eeHistoryPlaceholderRole,
			Content:        marshalProtectedBlob(blob),
			MessageType:    e2eeHistoryPlaceholderMessageType,
			ToolName:       "",
			ToolCallID:     "",
			ToolInput:      "",
			ToolResult:     "",
			IsToolComplete: false,
			Timestamp:      msg.Timestamp,
		})
	}

	return protected, nil
}

func shouldProtectRealtimePayload(messageType string) bool {
	switch strings.TrimSpace(messageType) {
	case protocol.TypeText,
		protocol.TypeInput,
		protocol.TypeControl,
		protocol.TypePermissionRequest,
		protocol.TypePermissionResponse,
		protocol.TypePermissionCleared,
		protocol.TypeSessionConfig,
		protocol.TypeCreateSession,
		protocol.TypeSessionCreated,
		protocol.TypeSessionAction,
		protocol.TypeSessionActionResult,
		protocol.TypeListDir,
		protocol.TypeListDirResponse,
		protocol.TypeReadFile,
		protocol.TypeReadFileResponse,
		protocol.TypeGitStatus,
		protocol.TypeGitStatusResponse,
		protocol.TypeGitDiff,
		protocol.TypeGitDiffResponse,
		protocol.TypeGitLog,
		protocol.TypeGitLogResponse,
		protocol.TypeGitCommitDetail,
		protocol.TypeGitCommitDetailResponse,
		protocol.TypeListCommands,
		protocol.TypeListCommandsResponse,
		protocol.TypeListSkills,
		protocol.TypeListSkillsResponse:
		return true
	default:
		return false
	}
}

func normalizeRequestedScope(scope string) string {
	switch strings.TrimSpace(scope) {
	case "", e2eeScopeSession:
		return e2eeScopeSession
	case e2eeScopeManager:
		return e2eeScopeManager
	default:
		return e2eeScopeSession
	}
}

func payloadScope(manager bool) string {
	if manager {
		return e2eeScopeManager
	}
	return e2eeScopeSession
}

func buildRealtimeAAD(scope, targetID, messageType string) []byte {
	return []byte(strings.Join([]string{scope, targetID, messageType}, "|"))
}

func buildHistoryAAD(sessionID, sourceID string) []byte {
	return []byte(strings.Join([]string{e2eeScopeSession, sessionID, sourceID}, "|"))
}

func buildSessionKeyAAD(targetID, requestID, scope string) []byte {
	return []byte(strings.Join([]string{e2eeScopeSessionKeyWrap, targetID, requestID, scope}, "|"))
}

func encryptProtectedBlob(key []byte, scope string, plaintext []byte, aad []byte, meta map[string]string) (*protocol.ProtectedBlob, error) {
	nonce, ciphertext, err := encryptAESGCM(key, plaintext, aad)
	if err != nil {
		return nil, err
	}

	blob := &protocol.ProtectedBlob{
		Alg:        e2eeAlgAESGCM256,
		Scope:      scope,
		KeyVersion: e2eeKeyVersionV1,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}
	if len(meta) > 0 {
		blob.Meta = meta
	}
	return blob, nil
}

func decryptProtectedBlob(key []byte, blob *protocol.ProtectedBlob, aad []byte) ([]byte, error) {
	if blob == nil {
		return nil, fmt.Errorf("protected blob is required")
	}
	if blob.Alg != "" && blob.Alg != e2eeAlgAESGCM256 {
		return nil, fmt.Errorf("unsupported algorithm: %s", blob.Alg)
	}

	nonce, err := base64.StdEncoding.DecodeString(strings.TrimSpace(blob.Nonce))
	if err != nil {
		return nil, fmt.Errorf("invalid nonce: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(blob.Ciphertext))
	if err != nil {
		return nil, fmt.Errorf("invalid ciphertext: %w", err)
	}

	return decryptAESGCM(key, nonce, ciphertext, aad)
}

func buildProtectedMeta(payload any) map[string]string {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	meta := make(map[string]string)
	if value, ok := raw["request_id"].(string); ok && strings.TrimSpace(value) != "" {
		meta["request_id"] = strings.TrimSpace(value)
	}
	if value, ok := raw["tool"].(string); ok && strings.TrimSpace(value) != "" {
		meta["tool"] = strings.TrimSpace(value)
	}
	if value, ok := raw["summary"].(string); ok && strings.TrimSpace(value) != "" {
		meta["summary"] = strings.TrimSpace(value)
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

func marshalProtectedBlob(blob *protocol.ProtectedBlob) string {
	if blob == nil {
		return ""
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return ""
	}
	return string(data)
}

func decodeProtectedBlobPayload(payload any) (*protocol.ProtectedBlob, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return decodeProtectedBlobString(string(data))
}

func decodeProtectedBlobString(raw string) (*protocol.ProtectedBlob, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "{") {
		return nil, fmt.Errorf("protected blob is empty")
	}

	var blob protocol.ProtectedBlob
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		return nil, err
	}
	if strings.TrimSpace(blob.Nonce) == "" || strings.TrimSpace(blob.Ciphertext) == "" {
		return nil, fmt.Errorf("protected blob is incomplete")
	}
	return &blob, nil
}

func decodePlainPayload(payload any, out any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func encryptAESGCM(key, plaintext, aad []byte) ([]byte, []byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}

	return nonce, gcm.Seal(nil, nonce, plaintext, aad), nil
}

func decryptAESGCM(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, aad)
}

func deriveScopedKey(masterKey []byte, targetID, scope string) []byte {
	reader := hkdf.New(sha256.New, masterKey, nil, []byte("acw2a:e2ee:"+scope+":"+targetID))
	key := make([]byte, 32)
	_, _ = io.ReadFull(reader, key)
	return key
}

func deriveWrapKey(sharedSecret []byte, targetID, scope string) []byte {
	reader := hkdf.New(sha256.New, sharedSecret, nil, []byte("spectra:e2ee:wrap:"+scope+":"+targetID))
	key := make([]byte, 32)
	_, _ = io.ReadFull(reader, key)
	return key
}

func decodeX25519PublicKey(encoded string) (*ecdh.PublicKey, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("invalid client public key: %w", err)
	}
	return ecdh.X25519().NewPublicKey(keyBytes)
}

func loadOrCreateE2EEMasterKey() ([]byte, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(homeDir, ".acw2a", "e2ee")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	path := filepath.Join(dir, "master.key")
	if data, err := os.ReadFile(path); err == nil {
		trimmed := []byte(strings.TrimSpace(string(data)))
		decoded, decodeErr := base64.StdEncoding.DecodeString(string(trimmed))
		if decodeErr == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}

	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}

	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
