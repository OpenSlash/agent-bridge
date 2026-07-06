package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

const (
	maxGitDiffBytes    = 192 * 1024
	defaultGitLogLimit = 40
	maxGitLogLimit     = 100
	gitCommandTimeout  = 12 * time.Second
)

type gitRepoResolution struct {
	requestedPath string
	searchDir     string
	repoRoot      string
}

type gitBranchInfo struct {
	headOID     string
	branch      string
	upstream    string
	aheadCount  int
	behindCount int
	isDetached  bool
}

type gitCommitDetailMeta struct {
	commit      string
	shortOID    string
	subject     string
	body        string
	authorName  string
	authorEmail string
	authoredAt  string
	parentOIDs  []string
}

func (s *Service) handleGitStatus(sessionID string, req protocol.GitStatusPayload) {
	startedAt := time.Now()
	resp := protocol.GitStatusResponsePayload{
		RequestID: req.RequestID,
	}

	response, err := buildGitStatusResponse(req.Path, s.getCurrentDir())
	if err != nil {
		applog.Errorf("[Remote] git-status failed: session=%s request=%s path=%s err=%v duration=%s", sessionID, req.RequestID, req.Path, err, time.Since(startedAt).Round(time.Millisecond))
		resp.Error = err.Error()
	} else {
		applog.Info.Printf(
			"[Remote] git-status resolved: session=%s request=%s path=%s repo=%s files=%d staged=%t unstaged=%t duration=%s",
			sessionID,
			req.RequestID,
			req.Path,
			response.RepoRoot,
			len(response.Files),
			response.HasStagedChanges,
			response.HasUnstagedChanges,
			time.Since(startedAt).Round(time.Millisecond),
		)
		resp = response
		resp.RequestID = req.RequestID
	}

	msg := protocol.Message{
		Type:      protocol.TypeGitStatusResponse,
		SessionID: sessionID,
		Payload:   resp,
	}
	if err := s.writeJSON(msg); err != nil {
		applog.Errorf("[Remote] WS write git-status-response error: %v", err)
	}
}

func (s *Service) handleGitDiff(sessionID string, req protocol.GitDiffPayload) {
	startedAt := time.Now()
	resp := protocol.GitDiffResponsePayload{
		RequestID: req.RequestID,
	}

	response, err := buildGitDiffResponse(req.Path, req.FilePath, req.Staged, s.getCurrentDir())
	if err != nil {
		applog.Errorf(
			"[Remote] git-diff failed: session=%s request=%s path=%s file=%s staged=%t err=%v duration=%s",
			sessionID,
			req.RequestID,
			req.Path,
			req.FilePath,
			req.Staged,
			err,
			time.Since(startedAt).Round(time.Millisecond),
		)
		resp.Error = err.Error()
	} else {
		applog.Info.Printf(
			"[Remote] git-diff resolved: session=%s request=%s path=%s file=%s staged=%t bytes=%d truncated=%t duration=%s",
			sessionID,
			req.RequestID,
			req.Path,
			response.FilePath,
			response.Staged,
			len(response.Diff),
			response.IsTruncated,
			time.Since(startedAt).Round(time.Millisecond),
		)
		resp = response
		resp.RequestID = req.RequestID
	}

	msg := protocol.Message{
		Type:      protocol.TypeGitDiffResponse,
		SessionID: sessionID,
		Payload:   resp,
	}
	if err := s.writeJSON(msg); err != nil {
		applog.Errorf("[Remote] WS write git-diff-response error: %v", err)
	}
}

func (s *Service) handleGitLog(sessionID string, req protocol.GitLogPayload) {
	startedAt := time.Now()
	resp := protocol.GitLogResponsePayload{
		RequestID: req.RequestID,
	}

	response, err := buildGitLogResponse(req.Path, req.Limit, s.getCurrentDir())
	if err != nil {
		applog.Errorf(
			"[Remote] git-log failed: session=%s request=%s path=%s limit=%d err=%v duration=%s",
			sessionID,
			req.RequestID,
			req.Path,
			req.Limit,
			err,
			time.Since(startedAt).Round(time.Millisecond),
		)
		resp.Error = err.Error()
	} else {
		applog.Info.Printf(
			"[Remote] git-log resolved: session=%s request=%s path=%s repo=%s commits=%d duration=%s",
			sessionID,
			req.RequestID,
			req.Path,
			response.RepoRoot,
			len(response.Commits),
			time.Since(startedAt).Round(time.Millisecond),
		)
		resp = response
		resp.RequestID = req.RequestID
	}

	msg := protocol.Message{
		Type:      protocol.TypeGitLogResponse,
		SessionID: sessionID,
		Payload:   resp,
	}
	if err := s.writeJSON(msg); err != nil {
		applog.Errorf("[Remote] WS write git-log-response error: %v", err)
	}
}

func (s *Service) handleGitCommitDetail(sessionID string, req protocol.GitCommitDetailPayload) {
	startedAt := time.Now()
	resp := protocol.GitCommitDetailResponsePayload{
		RequestID: req.RequestID,
	}

	response, err := buildGitCommitDetailResponse(req.Path, req.Commit, s.getCurrentDir())
	if err != nil {
		applog.Errorf(
			"[Remote] git-commit-detail failed: session=%s request=%s path=%s commit=%s err=%v duration=%s",
			sessionID,
			req.RequestID,
			req.Path,
			req.Commit,
			err,
			time.Since(startedAt).Round(time.Millisecond),
		)
		resp.Error = err.Error()
	} else {
		applog.Info.Printf(
			"[Remote] git-commit-detail resolved: session=%s request=%s path=%s commit=%s files=%d bytes=%d truncated=%t duration=%s",
			sessionID,
			req.RequestID,
			req.Path,
			response.Commit,
			len(response.Files),
			len(response.Diff),
			response.IsTruncated,
			time.Since(startedAt).Round(time.Millisecond),
		)
		resp = response
		resp.RequestID = req.RequestID
	}

	msg := protocol.Message{
		Type:      protocol.TypeGitCommitDetailResponse,
		SessionID: sessionID,
		Payload:   resp,
	}
	if err := s.writeJSON(msg); err != nil {
		applog.Errorf("[Remote] WS write git-commit-detail-response error: %v", err)
	}
}

func buildGitStatusResponse(rawPath, relativeBase string) (protocol.GitStatusResponsePayload, error) {
	resolution, err := resolveGitRepoWithinUserHome(rawPath, relativeBase)
	if err != nil {
		return protocol.GitStatusResponsePayload{}, err
	}

	branchInfo, err := readGitBranchInfo(resolution.repoRoot)
	if err != nil {
		return protocol.GitStatusResponsePayload{}, err
	}
	files, hasStaged, hasUnstaged, err := readGitChangedFiles(resolution.repoRoot)
	if err != nil {
		return protocol.GitStatusResponsePayload{}, err
	}

	return protocol.GitStatusResponsePayload{
		Path:               resolution.requestedPath,
		RepoRoot:           resolution.repoRoot,
		RepoName:           filepath.Base(resolution.repoRoot),
		Branch:             branchInfo.branch,
		HeadOID:            branchInfo.headOID,
		Upstream:           branchInfo.upstream,
		AheadCount:         branchInfo.aheadCount,
		BehindCount:        branchInfo.behindCount,
		IsDetached:         branchInfo.isDetached,
		HasStagedChanges:   hasStaged,
		HasUnstagedChanges: hasUnstaged,
		Files:              files,
	}, nil
}

func buildGitDiffResponse(rawPath, rawFilePath string, staged bool, relativeBase string) (protocol.GitDiffResponsePayload, error) {
	resolution, err := resolveGitRepoWithinUserHome(rawPath, relativeBase)
	if err != nil {
		return protocol.GitDiffResponsePayload{}, err
	}

	absoluteFilePath, relativeFilePath, err := resolveGitFileWithinRepo(resolution.repoRoot, rawFilePath, resolution.requestedPath)
	if err != nil {
		return protocol.GitDiffResponsePayload{}, err
	}

	diff, truncated, err := readGitDiff(resolution.repoRoot, absoluteFilePath, relativeFilePath, staged)
	if err != nil {
		return protocol.GitDiffResponsePayload{}, err
	}

	return protocol.GitDiffResponsePayload{
		Path:        resolution.requestedPath,
		FilePath:    filepath.ToSlash(relativeFilePath),
		RepoRoot:    resolution.repoRoot,
		Staged:      staged,
		Diff:        diff,
		IsTruncated: truncated,
	}, nil
}

func buildGitLogResponse(rawPath string, limit int, relativeBase string) (protocol.GitLogResponsePayload, error) {
	resolution, err := resolveGitRepoWithinUserHome(rawPath, relativeBase)
	if err != nil {
		return protocol.GitLogResponsePayload{}, err
	}

	branchInfo, err := readGitBranchInfo(resolution.repoRoot)
	if err != nil {
		return protocol.GitLogResponsePayload{}, err
	}
	commits, err := readGitLogEntries(resolution.repoRoot, normalizeGitLogLimit(limit))
	if err != nil {
		return protocol.GitLogResponsePayload{}, err
	}

	return protocol.GitLogResponsePayload{
		Path:     resolution.requestedPath,
		RepoRoot: resolution.repoRoot,
		RepoName: filepath.Base(resolution.repoRoot),
		Branch:   branchInfo.branch,
		Commits:  commits,
	}, nil
}

func buildGitCommitDetailResponse(rawPath, commit, relativeBase string) (protocol.GitCommitDetailResponsePayload, error) {
	resolution, err := resolveGitRepoWithinUserHome(rawPath, relativeBase)
	if err != nil {
		return protocol.GitCommitDetailResponsePayload{}, err
	}

	trimmedCommit := strings.TrimSpace(commit)
	if trimmedCommit == "" {
		return protocol.GitCommitDetailResponsePayload{}, fmt.Errorf("commit is empty")
	}

	meta, err := readGitCommitDetailMeta(resolution.repoRoot, trimmedCommit)
	if err != nil {
		return protocol.GitCommitDetailResponsePayload{}, err
	}
	files, err := readGitCommitFileStats(resolution.repoRoot, trimmedCommit)
	if err != nil {
		return protocol.GitCommitDetailResponsePayload{}, err
	}
	diff, truncated, err := readGitCommitDiff(resolution.repoRoot, trimmedCommit)
	if err != nil {
		return protocol.GitCommitDetailResponsePayload{}, err
	}

	return protocol.GitCommitDetailResponsePayload{
		Path:        resolution.requestedPath,
		RepoRoot:    resolution.repoRoot,
		RepoName:    filepath.Base(resolution.repoRoot),
		Commit:      meta.commit,
		ShortOID:    meta.shortOID,
		Subject:     meta.subject,
		Body:        meta.body,
		AuthorName:  meta.authorName,
		AuthorEmail: meta.authorEmail,
		AuthoredAt:  meta.authoredAt,
		ParentOIDs:  meta.parentOIDs,
		Files:       files,
		Diff:        diff,
		IsTruncated: truncated,
	}, nil
}

func resolveGitRepoWithinUserHome(rawPath, relativeBase string) (gitRepoResolution, error) {
	homeDir, resolvedHomeDir, err := userHomeDirectoryBounds()
	if err != nil {
		return gitRepoResolution{}, err
	}

	targetPath := strings.TrimSpace(rawPath)
	baseDir := strings.TrimSpace(relativeBase)
	if baseDir == "" {
		baseDir = homeDir
	}
	if targetPath == "" {
		targetPath = baseDir
	}
	if !filepath.IsAbs(targetPath) {
		targetPath = filepath.Join(baseDir, targetPath)
	}

	targetPath = filepath.Clean(targetPath)
	info, err := os.Stat(targetPath)
	if err != nil {
		return gitRepoResolution{}, err
	}

	resolvedTargetPath, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		return gitRepoResolution{}, err
	}
	resolvedTargetPath = filepath.Clean(resolvedTargetPath)
	if !pathWithinBase(resolvedTargetPath, resolvedHomeDir) {
		return gitRepoResolution{}, fmt.Errorf("access denied: path must stay within your home directory (%s)", homeDir)
	}

	searchDir := resolvedTargetPath
	if !info.IsDir() {
		searchDir = filepath.Dir(resolvedTargetPath)
	}
	repoRoot, err := findGitRepoRoot(searchDir)
	if err != nil {
		return gitRepoResolution{}, err
	}
	resolvedRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return gitRepoResolution{}, err
	}
	resolvedRepoRoot = filepath.Clean(resolvedRepoRoot)
	if !pathWithinBase(resolvedRepoRoot, resolvedHomeDir) {
		return gitRepoResolution{}, fmt.Errorf("access denied: path must stay within your home directory (%s)", homeDir)
	}

	return gitRepoResolution{
		requestedPath: resolvedTargetPath,
		searchDir:     searchDir,
		repoRoot:      resolvedRepoRoot,
	}, nil
}

func findGitRepoRoot(startDir string) (string, error) {
	dir := filepath.Clean(strings.TrimSpace(startDir))
	if dir == "" {
		return "", fmt.Errorf("path is empty")
	}

	for {
		gitPath := filepath.Join(dir, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("not a git repository: %s", startDir)
}

func readGitBranchInfo(repoRoot string) (gitBranchInfo, error) {
	output, err := runGitCommand(repoRoot, nil, "status", "--porcelain=v2", "--branch", "--untracked-files=all")
	if err != nil {
		return gitBranchInfo{}, err
	}

	info := gitBranchInfo{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "# ") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}

		switch fields[1] {
		case "branch.oid":
			if fields[2] != "(initial)" {
				info.headOID = fields[2]
			}
		case "branch.head":
			switch fields[2] {
			case "(detached)":
				info.isDetached = true
			case "(initial)":
			default:
				info.branch = fields[2]
			}
		case "branch.upstream":
			info.upstream = fields[2]
		case "branch.ab":
			if len(fields) >= 4 {
				info.aheadCount = parseGitBranchCount(fields[2], "+")
				info.behindCount = parseGitBranchCount(fields[3], "-")
			}
		}
	}

	return info, nil
}

func parseGitBranchCount(value, prefix string) int {
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), prefix)
	count, _ := strconv.Atoi(trimmed)
	return count
}

func readGitChangedFiles(repoRoot string) ([]protocol.GitChangedFile, bool, bool, error) {
	output, err := runGitCommand(repoRoot, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, false, false, err
	}

	data := []byte(output)
	files := make([]protocol.GitChangedFile, 0)
	hasStaged := false
	hasUnstaged := false

	for index := 0; index < len(data); {
		if len(data)-index < 4 {
			break
		}

		statusCode := string(data[index : index+2])
		index += 3

		end := index
		for end < len(data) && data[end] != 0 {
			end++
		}
		if end >= len(data) {
			break
		}
		pathValue := string(data[index:end])
		index = end + 1

		previousPath := ""
		if isGitRenameOrCopy(statusCode) {
			nextEnd := index
			for nextEnd < len(data) && data[nextEnd] != 0 {
				nextEnd++
			}
			if nextEnd >= len(data) {
				break
			}
			previousPath = string(data[index:nextEnd])
			index = nextEnd + 1
		}

		entry := protocol.GitChangedFile{
			Path:         filepath.ToSlash(filepath.Clean(pathValue)),
			AbsolutePath: filepath.Join(repoRoot, filepath.FromSlash(pathValue)),
		}
		if strings.TrimSpace(previousPath) != "" {
			entry.PreviousPath = filepath.ToSlash(filepath.Clean(previousPath))
		}

		if statusCode == "??" {
			entry.IsUntracked = true
			hasUnstaged = true
		} else {
			if stagedStatus := normalizeGitStatusChar(statusCode[0]); stagedStatus != "" {
				entry.StagedStatus = stagedStatus
				hasStaged = true
			}
			if unstagedStatus := normalizeGitStatusChar(statusCode[1]); unstagedStatus != "" {
				entry.UnstagedStatus = unstagedStatus
				hasUnstaged = true
			}
			entry.IsConflicted = isGitConflictedStatus(statusCode)
		}

		files = append(files, entry)
	}

	sort.Slice(files, func(i, j int) bool {
		left := strings.ToLower(files[i].Path)
		right := strings.ToLower(files[j].Path)
		return left < right
	})

	return files, hasStaged, hasUnstaged, nil
}

func isGitRenameOrCopy(statusCode string) bool {
	if len(statusCode) < 2 {
		return false
	}
	return statusCode[0] == 'R' || statusCode[0] == 'C' || statusCode[1] == 'R' || statusCode[1] == 'C'
}

func normalizeGitStatusChar(value byte) string {
	switch value {
	case ' ', '?':
		return ""
	default:
		return string(value)
	}
}

func isGitConflictedStatus(statusCode string) bool {
	switch statusCode {
	case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
		return true
	default:
		return false
	}
}

func resolveGitFileWithinRepo(repoRoot, rawFilePath, fallbackPath string) (string, string, error) {
	filePath := strings.TrimSpace(rawFilePath)
	if filePath == "" {
		filePath = strings.TrimSpace(fallbackPath)
	}
	if filePath == "" {
		return "", "", fmt.Errorf("file path is empty")
	}

	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(repoRoot, filepath.FromSlash(filePath))
	}
	filePath = filepath.Clean(filePath)

	resolvedFilePath := filePath
	if info, err := os.Stat(filePath); err == nil && !info.IsDir() {
		if symlinkPath, evalErr := filepath.EvalSymlinks(filePath); evalErr == nil {
			resolvedFilePath = filepath.Clean(symlinkPath)
		}
	} else if err != nil {
		parentDir := filepath.Dir(filePath)
		if resolvedParent, evalErr := filepath.EvalSymlinks(parentDir); evalErr == nil {
			resolvedFilePath = filepath.Join(filepath.Clean(resolvedParent), filepath.Base(filePath))
		}
	}

	if !pathWithinBase(resolvedFilePath, repoRoot) {
		return "", "", fmt.Errorf("access denied: path must stay within repository (%s)", repoRoot)
	}

	relativePath, err := filepath.Rel(repoRoot, resolvedFilePath)
	if err != nil {
		return "", "", err
	}
	if relativePath == "." || strings.HasPrefix(relativePath, "..") {
		return "", "", fmt.Errorf("access denied: path must stay within repository (%s)", repoRoot)
	}

	return resolvedFilePath, filepath.Clean(relativePath), nil
}

func readGitDiff(repoRoot, absoluteFilePath, relativeFilePath string, staged bool) (string, bool, error) {
	relativePath := filepath.ToSlash(relativeFilePath)

	if !staged {
		tracked, err := gitPathTracked(repoRoot, relativePath)
		if err != nil {
			return "", false, err
		}
		if !tracked {
			return runUntrackedGitDiff(repoRoot, absoluteFilePath, relativePath)
		}
	}

	args := []string{"--no-pager", "diff", "--no-ext-diff"}
	if staged {
		args = append(args, "--cached")
	}
	args = append(args, "--", relativePath)

	output, err := runGitCommand(repoRoot, nil, args...)
	if err != nil {
		return "", false, err
	}

	diff, truncated := truncateUTF8String(output, maxGitDiffBytes)
	return diff, truncated, nil
}

func normalizeGitLogLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultGitLogLimit
	case limit > maxGitLogLimit:
		return maxGitLogLimit
	default:
		return limit
	}
}

func readGitLogEntries(repoRoot string, limit int) ([]protocol.GitCommitLogEntry, error) {
	const fieldSep = "\x1f"
	const recordSep = "\x1e"

	format := "%H%x1f%h%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%P%x1e"
	output, err := runGitCommand(
		repoRoot,
		nil,
		"log",
		fmt.Sprintf("-%d", limit),
		"--date=iso-strict",
		fmt.Sprintf("--pretty=format:%s", format),
	)
	if err != nil {
		return nil, err
	}

	records := strings.Split(output, recordSep)
	commits := make([]protocol.GitCommitLogEntry, 0, len(records))
	for _, record := range records {
		record = strings.Trim(record, "\r\n\t ")
		if record == "" {
			continue
		}

		fields := strings.Split(record, fieldSep)
		if len(fields) < 6 {
			continue
		}

		entry := protocol.GitCommitLogEntry{
			OID:         strings.TrimSpace(fields[0]),
			ShortOID:    strings.TrimSpace(fields[1]),
			Subject:     strings.TrimSpace(fields[5]),
			AuthorName:  strings.TrimSpace(fields[2]),
			AuthorEmail: strings.TrimSpace(fields[3]),
			AuthoredAt:  strings.TrimSpace(fields[4]),
		}
		if len(fields) >= 7 {
			entry.ParentOIDs = strings.Fields(strings.TrimSpace(fields[6]))
		}
		if entry.OID == "" {
			continue
		}
		commits = append(commits, entry)
	}

	return commits, nil
}

func readGitCommitDetailMeta(repoRoot, commit string) (gitCommitDetailMeta, error) {
	const fieldSep = "\x1f"
	format := "%H%x1f%h%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%b%x1f%P"
	output, err := runGitCommand(
		repoRoot,
		nil,
		"show",
		"-s",
		"--date=iso-strict",
		fmt.Sprintf("--format=%s", format),
		commit,
	)
	if err != nil {
		return gitCommitDetailMeta{}, err
	}

	fields := strings.Split(strings.TrimRight(output, "\r\n"), fieldSep)
	if len(fields) < 7 {
		return gitCommitDetailMeta{}, fmt.Errorf("failed to parse commit detail")
	}

	meta := gitCommitDetailMeta{
		commit:      strings.TrimSpace(fields[0]),
		shortOID:    strings.TrimSpace(fields[1]),
		authorName:  strings.TrimSpace(fields[2]),
		authorEmail: strings.TrimSpace(fields[3]),
		authoredAt:  strings.TrimSpace(fields[4]),
		subject:     strings.TrimSpace(fields[5]),
		body:        strings.TrimSpace(fields[6]),
	}
	if len(fields) >= 8 {
		meta.parentOIDs = strings.Fields(strings.TrimSpace(fields[7]))
	}
	if meta.shortOID == "" && len(meta.commit) >= 12 {
		meta.shortOID = meta.commit[:12]
	}
	return meta, nil
}

func readGitCommitFileStats(repoRoot, commit string) ([]protocol.GitCommitFileStat, error) {
	output, err := runGitCommand(
		repoRoot,
		nil,
		"show",
		"--numstat",
		"--format=",
		commit,
	)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(output, "\n")
	files := make([]protocol.GitCommitFileStat, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}

		pathValue := strings.TrimSpace(strings.Join(fields[2:], "\t"))
		if pathValue == "" {
			continue
		}

		file := protocol.GitCommitFileStat{
			Path:     pathValue,
			IsBinary: fields[0] == "-" || fields[1] == "-",
		}
		if !file.IsBinary {
			file.Additions = parseGitDiffCount(fields[0])
			file.Deletions = parseGitDiffCount(fields[1])
		}
		files = append(files, file)
	}

	return files, nil
}

func readGitCommitDiff(repoRoot, commit string) (string, bool, error) {
	output, err := runGitCommand(
		repoRoot,
		nil,
		"--no-pager",
		"show",
		"--patch",
		"--format=",
		"--find-renames",
		commit,
	)
	if err != nil {
		return "", false, err
	}

	diff, truncated := truncateUTF8String(output, maxGitDiffBytes)
	return diff, truncated, nil
}

func parseGitDiffCount(value string) int {
	count, _ := strconv.Atoi(strings.TrimSpace(value))
	return count
}

func gitPathTracked(repoRoot, relativePath string) (bool, error) {
	_, err := runGitCommand(repoRoot, nil, "ls-files", "--error-unmatch", "--", relativePath)
	if err != nil {
		var exitErr *gitExitCodeError
		if errors.As(err, &exitErr) && exitErr.code == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func runUntrackedGitDiff(repoRoot, absoluteFilePath, relativePath string) (string, bool, error) {
	output, err := runGitCommand(repoRoot, map[int]struct{}{1: {}}, "--no-pager", "diff", "--no-index", "--no-ext-diff", "--", os.DevNull, absoluteFilePath)
	if err != nil {
		return "", false, err
	}

	replaced := strings.ReplaceAll(output, absoluteFilePath, relativePath)
	replaced = strings.ReplaceAll(replaced, filepath.ToSlash(absoluteFilePath), relativePath)
	diff, truncated := truncateUTF8String(replaced, maxGitDiffBytes)
	return diff, truncated, nil
}

type gitExitCodeError struct {
	code int
	msg  string
}

func (e *gitExitCodeError) Error() string {
	return e.msg
}

func runGitCommand(repoRoot string, allowedExitCodes map[int]struct{}, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	hideBackgroundConsole(cmd)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git command timed out")
	}
	if err == nil {
		return string(output), nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if _, ok := allowedExitCodes[code]; ok {
			return string(output), nil
		}
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", &gitExitCodeError{
			code: code,
			msg:  fmt.Sprintf("git %s: %s", strings.Join(args, " "), message),
		}
	}

	return "", err
}

func truncateUTF8String(value string, maxBytes int) (string, bool) {
	data := []byte(value)
	if len(data) <= maxBytes {
		return value, false
	}

	limit := maxBytes
	for limit > 0 && !utf8.Valid(data[:limit]) {
		limit--
	}
	if limit <= 0 {
		return "", true
	}
	return string(data[:limit]), true
}
