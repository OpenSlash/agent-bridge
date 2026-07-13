package remote

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestResolveCreateSessionAttachmentsValidatesAndWritesImage(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	t.Setenv("HOME", cacheDir)
	data := []byte("fake-png-data")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))

	resolved, err := resolveCreateSessionAttachments([]protocol.CreateSessionAttachmentRef{{
		ID:       "image-1",
		Name:     "screen.png",
		MIMEType: "image/png",
		ByteSize: int64(len(data)),
		SHA256:   digest,
		BlobRef:  "data:image/png;base64," + base64.StdEncoding.EncodeToString(data),
	}})
	if err != nil {
		t.Fatalf("resolve attachments: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("expected one attachment, got %d", len(resolved))
	}
	got, err := os.ReadFile(resolved[0].Path)
	if err != nil {
		t.Fatalf("read resolved attachment: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("unexpected attachment contents %q", got)
	}
}

func TestBuildInitialInputUsesRuntimeSpecificImageInstructions(t *testing.T) {
	attachments := []resolvedCreateSessionAttachment{{Path: "/tmp/screen.png", MIMEType: "image/png"}}
	claude := buildInitialInputForRuntime(runtimeClaude, "Review this", attachments)
	codex := buildInitialInputForRuntime(runtimeCodex, "Review this", attachments)
	if !strings.Contains(claude, "Read tool") {
		t.Fatalf("expected Claude Read instruction, got %q", claude)
	}
	if !strings.Contains(codex, "Inspect them") {
		t.Fatalf("expected Codex inspection instruction, got %q", codex)
	}
}

func TestResolveCreateSessionAttachmentsRejectsChecksumMismatch(t *testing.T) {
	_, err := resolveCreateSessionAttachments([]protocol.CreateSessionAttachmentRef{{
		MIMEType: "image/png",
		SHA256:   "wrong",
		BlobRef:  "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("image")),
	}})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
}

func TestResolveCreateSessionAttachmentsCleansPartialWritesOnFailure(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	t.Setenv("HOME", cacheDir)
	data := []byte("first-image")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	_, err := resolveCreateSessionAttachments([]protocol.CreateSessionAttachmentRef{
		{
			ID:       "first-image",
			Name:     "first.png",
			MIMEType: "image/png",
			SHA256:   digest,
			BlobRef:  "data:image/png;base64," + base64.StdEncoding.EncodeToString(data),
		},
		{
			ID:       "invalid-image",
			MIMEType: "image/png",
			SHA256:   "wrong",
			BlobRef:  "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("invalid")),
		},
	})
	if err == nil {
		t.Fatal("expected attachment resolution to fail")
	}
	userCacheDir, cacheErr := os.UserCacheDir()
	if cacheErr != nil {
		t.Fatalf("user cache dir: %v", cacheErr)
	}
	if _, statErr := os.Stat(filepath.Join(userCacheDir, "veilo", "attachments", "first-image")); !os.IsNotExist(statErr) {
		t.Fatalf("expected partial attachment directory removed, stat err=%v", statErr)
	}
}

func TestStopRemovesTemporaryAttachmentDirectories(t *testing.T) {
	dir := t.TempDir()
	filePath := dir + "/screen.png"
	if err := os.WriteFile(filePath, []byte("image"), 0o600); err != nil {
		t.Fatalf("write attachment: %v", err)
	}
	service := NewService()
	service.setTemporaryCreateSessionAttachments([]resolvedCreateSessionAttachment{{Dir: dir, Path: filePath}})

	if err := service.StopWithReason("test"); err != nil {
		t.Fatalf("stop service: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected attachment directory removed, stat err=%v", err)
	}
}
