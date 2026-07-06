package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestBuildGitStatusResponseParsesTrackedStagedAndUntrackedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	repoRoot := filepath.Join(homeDir, "project")
	mustMkdirAllGit(t, repoRoot)
	runGitInDir(t, repoRoot, "init")
	runGitInDir(t, repoRoot, "config", "user.name", "tester")
	runGitInDir(t, repoRoot, "config", "user.email", "tester@example.com")

	mustWriteFile(t, filepath.Join(repoRoot, "tracked.txt"), "line-1\n")
	runGitInDir(t, repoRoot, "add", "tracked.txt")
	runGitInDir(t, repoRoot, "commit", "-m", "initial")

	mustWriteFile(t, filepath.Join(repoRoot, "tracked.txt"), "line-1\nline-2\n")
	mustWriteFile(t, filepath.Join(repoRoot, "staged.txt"), "staged\n")
	mustWriteFile(t, filepath.Join(repoRoot, "untracked.txt"), "untracked\n")
	runGitInDir(t, repoRoot, "add", "staged.txt")

	resp, err := buildGitStatusResponse(repoRoot, "")
	if err != nil {
		t.Fatalf("buildGitStatusResponse failed: %v", err)
	}

	resolvedRepoRoot, resolveErr := filepath.EvalSymlinks(repoRoot)
	if resolveErr != nil {
		t.Fatalf("resolve repo root failed: %v", resolveErr)
	}
	if resp.RepoRoot != resolvedRepoRoot {
		t.Fatalf("expected repo root %q, got %q", resolvedRepoRoot, resp.RepoRoot)
	}
	if resp.RepoName != "project" {
		t.Fatalf("expected repo name project, got %q", resp.RepoName)
	}
	if !resp.HasStagedChanges {
		t.Fatal("expected staged changes")
	}
	if !resp.HasUnstagedChanges {
		t.Fatal("expected unstaged changes")
	}
	if resp.HeadOID == "" {
		t.Fatal("expected head oid")
	}

	files := map[string]protocol.GitChangedFile{}
	for _, file := range resp.Files {
		files[file.Path] = file
	}

	tracked, ok := files["tracked.txt"]
	if !ok {
		t.Fatalf("missing tracked file entry: %+v", resp.Files)
	}
	if tracked.UnstagedStatus != "M" {
		t.Fatalf("expected tracked.txt to be unstaged modified, got %+v", tracked)
	}

	staged, ok := files["staged.txt"]
	if !ok {
		t.Fatalf("missing staged file entry: %+v", resp.Files)
	}
	if staged.StagedStatus == "" {
		t.Fatalf("expected staged.txt to have staged status, got %+v", staged)
	}

	untracked, ok := files["untracked.txt"]
	if !ok {
		t.Fatalf("missing untracked file entry: %+v", resp.Files)
	}
	if !untracked.IsUntracked {
		t.Fatalf("expected untracked.txt to be untracked, got %+v", untracked)
	}
}

func TestBuildGitDiffResponseTruncatesLargePatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	repoRoot := filepath.Join(homeDir, "project")
	mustMkdirAllGit(t, repoRoot)
	runGitInDir(t, repoRoot, "init")
	runGitInDir(t, repoRoot, "config", "user.name", "tester")
	runGitInDir(t, repoRoot, "config", "user.email", "tester@example.com")

	filePath := filepath.Join(repoRoot, "big.txt")
	mustWriteFile(t, filePath, "seed\n")
	runGitInDir(t, repoRoot, "add", "big.txt")
	runGitInDir(t, repoRoot, "commit", "-m", "initial")

	largeBody := strings.Repeat("0123456789abcdef0123456789abcdef\n", 20000)
	mustWriteFile(t, filePath, largeBody)

	resp, err := buildGitDiffResponse(repoRoot, filePath, false, "")
	if err != nil {
		t.Fatalf("buildGitDiffResponse failed: %v", err)
	}
	if !resp.IsTruncated {
		t.Fatal("expected diff to be truncated")
	}
	if resp.Diff == "" {
		t.Fatal("expected diff content")
	}
	if len([]byte(resp.Diff)) > maxGitDiffBytes {
		t.Fatalf("expected diff size <= %d bytes, got %d", maxGitDiffBytes, len([]byte(resp.Diff)))
	}
}

func TestBuildGitLogResponseReadsRecentCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	repoRoot := filepath.Join(homeDir, "project")
	mustMkdirAllGit(t, repoRoot)
	runGitInDir(t, repoRoot, "init")
	runGitInDir(t, repoRoot, "config", "user.name", "tester")
	runGitInDir(t, repoRoot, "config", "user.email", "tester@example.com")

	mustWriteFile(t, filepath.Join(repoRoot, "tracked.txt"), "line-1\n")
	runGitInDir(t, repoRoot, "add", "tracked.txt")
	runGitInDir(t, repoRoot, "commit", "-m", "initial")

	mustWriteFile(t, filepath.Join(repoRoot, "tracked.txt"), "line-1\nline-2\n")
	runGitInDir(t, repoRoot, "commit", "-am", "second commit")

	resp, err := buildGitLogResponse(repoRoot, 10, "")
	if err != nil {
		t.Fatalf("buildGitLogResponse failed: %v", err)
	}
	if resp.RepoName != "project" {
		t.Fatalf("expected repo name project, got %q", resp.RepoName)
	}
	if len(resp.Commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(resp.Commits))
	}
	if resp.Commits[0].Subject != "second commit" {
		t.Fatalf("expected newest commit subject second commit, got %+v", resp.Commits[0])
	}
	if resp.Commits[0].OID == "" || resp.Commits[0].ShortOID == "" {
		t.Fatalf("expected commit oid fields, got %+v", resp.Commits[0])
	}
}

func TestBuildGitCommitDetailResponseParsesMetadataAndDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	repoRoot := filepath.Join(homeDir, "project")
	mustMkdirAllGit(t, repoRoot)
	runGitInDir(t, repoRoot, "init")
	runGitInDir(t, repoRoot, "config", "user.name", "tester")
	runGitInDir(t, repoRoot, "config", "user.email", "tester@example.com")

	filePath := filepath.Join(repoRoot, "tracked.txt")
	mustWriteFile(t, filePath, "line-1\n")
	runGitInDir(t, repoRoot, "add", "tracked.txt")
	runGitInDir(t, repoRoot, "commit", "-m", "initial")

	mustWriteFile(t, filePath, "line-1\nline-2\n")
	runGitInDir(t, repoRoot, "commit", "-am", "detail subject\n\nbody line")

	commit := strings.TrimSpace(runGitInDir(t, repoRoot, "rev-parse", "HEAD"))
	resp, err := buildGitCommitDetailResponse(repoRoot, commit, "")
	if err != nil {
		t.Fatalf("buildGitCommitDetailResponse failed: %v", err)
	}
	if resp.Commit != commit {
		t.Fatalf("expected commit %q, got %q", commit, resp.Commit)
	}
	if resp.Subject != "detail subject" {
		t.Fatalf("expected subject detail subject, got %q", resp.Subject)
	}
	if !strings.Contains(resp.Body, "body line") {
		t.Fatalf("expected body to contain commit body, got %q", resp.Body)
	}
	if len(resp.Files) != 1 {
		t.Fatalf("expected 1 changed file, got %d", len(resp.Files))
	}
	if resp.Files[0].Path != "tracked.txt" {
		t.Fatalf("expected tracked.txt stat, got %+v", resp.Files[0])
	}
	if !strings.Contains(resp.Diff, "+++ b/tracked.txt") {
		t.Fatalf("expected diff to contain tracked.txt patch, got %q", resp.Diff)
	}
}

func TestResolveGitRepoWithinUserHomeRejectsOutsideHome(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	outsideDir := t.TempDir()
	if _, err := resolveGitRepoWithinUserHome(outsideDir, ""); err == nil {
		t.Fatal("expected outside-home path to be rejected")
	} else if !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("expected access denied error, got %v", err)
	}
}

func mustMkdirAllGit(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGitInDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
	return string(output)
}
