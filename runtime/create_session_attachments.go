package remote

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

const (
	maxCreateSessionAttachmentBytes = 8 * 1024 * 1024
	maxCreateSessionAttachments     = 6
)

var attachmentFileNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

type resolvedCreateSessionAttachment struct {
	Name     string
	MIMEType string
	Path     string
	Dir      string
}

func resolveCreateSessionAttachments(refs []protocol.CreateSessionAttachmentRef) (resolved []resolvedCreateSessionAttachment, resultErr error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > maxCreateSessionAttachments {
		return nil, fmt.Errorf("too many attachments: maximum is %d", maxCreateSessionAttachments)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("resolve attachment cache: %w", err)
	}

	resolved = make([]resolvedCreateSessionAttachment, 0, len(refs))
	createdDirs := make([]string, 0, len(refs))
	defer func() {
		if resultErr == nil {
			return
		}
		for _, dir := range createdDirs {
			_ = os.RemoveAll(dir)
		}
	}()
	for index, ref := range refs {
		mimeType := strings.ToLower(strings.TrimSpace(ref.MIMEType))
		if !strings.HasPrefix(mimeType, "image/") {
			return nil, fmt.Errorf("attachment %d is not a supported image", index+1)
		}
		data, err := decodeInlineAttachment(ref.BlobRef, mimeType)
		if err != nil {
			return nil, fmt.Errorf("decode attachment %d: %w", index+1, err)
		}
		if len(data) == 0 || len(data) > maxCreateSessionAttachmentBytes {
			return nil, fmt.Errorf("attachment %d exceeds the %d byte limit", index+1, maxCreateSessionAttachmentBytes)
		}
		if ref.ByteSize > 0 && ref.ByteSize != int64(len(data)) {
			return nil, fmt.Errorf("attachment %d size mismatch", index+1)
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		if expected := strings.ToLower(strings.TrimSpace(ref.SHA256)); expected != "" && expected != digest {
			return nil, fmt.Errorf("attachment %d checksum mismatch", index+1)
		}

		id := attachmentFileNameSanitizer.ReplaceAllString(strings.TrimSpace(ref.ID), "-")
		if id == "" {
			id = digest[:16]
		}
		name := attachmentFileNameSanitizer.ReplaceAllString(filepath.Base(strings.TrimSpace(ref.Name)), "-")
		if name == "" || name == "." {
			name = fmt.Sprintf("image-%d%s", index+1, extensionForImageMIME(mimeType))
		}
		dir := filepath.Join(cacheDir, "veilo", "attachments", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create attachment directory: %w", err)
		}
		createdDirs = append(createdDirs, dir)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return nil, fmt.Errorf("write attachment: %w", err)
		}
		resolved = append(resolved, resolvedCreateSessionAttachment{Name: name, MIMEType: mimeType, Path: path, Dir: dir})
	}
	return resolved, nil
}

func (s *Service) setTemporaryCreateSessionAttachments(attachments []resolvedCreateSessionAttachment) {
	dirs := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		dir := strings.TrimSpace(attachment.Dir)
		if dir == "" {
			continue
		}
		dirs = append(dirs, dir)
	}
	s.mu.Lock()
	seen := make(map[string]struct{}, len(s.temporaryAttachmentDirs)+len(dirs))
	merged := make([]string, 0, len(s.temporaryAttachmentDirs)+len(dirs))
	for _, dir := range append(append([]string(nil), s.temporaryAttachmentDirs...), dirs...) {
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		merged = append(merged, dir)
	}
	s.temporaryAttachmentDirs = merged
	s.mu.Unlock()
}

func (s *Service) prepareInputAttachments(input protocol.InputPayload) (string, error) {
	resolved, err := resolveCreateSessionAttachments(input.Attachments)
	if err != nil {
		return "", err
	}
	if len(resolved) == 0 {
		return input.Data, nil
	}
	s.setTemporaryCreateSessionAttachments(resolved)
	return buildInitialInputForRuntime(s.getRuntime(), input.Data, resolved), nil
}

func (s *Service) cleanupTemporaryCreateSessionAttachments() {
	s.mu.Lock()
	dirs := append([]string(nil), s.temporaryAttachmentDirs...)
	s.temporaryAttachmentDirs = nil
	s.mu.Unlock()
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			applog.Errorf("[Remote] remove temporary attachment directory failed: dir=%s err=%v", dir, err)
		}
	}
}

func decodeInlineAttachment(raw, expectedMIME string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	prefix := "data:" + expectedMIME + ";base64,"
	if !strings.HasPrefix(raw, prefix) {
		return nil, fmt.Errorf("attachment must use an encrypted inline data reference")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, prefix))
	if err != nil {
		return nil, fmt.Errorf("invalid base64 data: %w", err)
	}
	return data, nil
}

func extensionForImageMIME(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".img"
	}
}

func buildInitialInputForRuntime(runtime runtimeKind, prompt string, attachments []resolvedCreateSessionAttachment) string {
	prompt = strings.TrimSpace(prompt)
	if len(attachments) == 0 {
		return prompt
	}
	var builder strings.Builder
	if prompt != "" {
		builder.WriteString(prompt)
		builder.WriteString("\n\n")
	}
	switch runtime {
	case runtimeCodex:
		builder.WriteString("Local image attachments are available at the following paths. Inspect them as part of this task:\n")
	default:
		builder.WriteString("Image attachments are available at the following local paths. Use the Read tool to inspect them as part of this task:\n")
	}
	for _, attachment := range attachments {
		fmt.Fprintf(&builder, "- %s (%s)\n", attachment.Path, attachment.MIMEType)
	}
	return strings.TrimSpace(builder.String())
}
