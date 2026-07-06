package remote

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func resolveDirectoryWithinUserHome(rawPath, relativeBase string, emptyUsesHome bool) (string, error) {
	return resolvePathWithinUserHome(rawPath, relativeBase, emptyUsesHome, true)
}

func ResolveDirectoryWithinUserHome(rawPath, relativeBase string, emptyUsesHome bool) (string, error) {
	return resolveDirectoryWithinUserHome(rawPath, relativeBase, emptyUsesHome)
}

func resolveFileWithinUserHome(rawPath, relativeBase string) (string, error) {
	return resolvePathWithinUserHome(rawPath, relativeBase, false, false)
}

func resolvePathWithinUserHome(rawPath, relativeBase string, emptyUsesHome bool, expectDirectory bool) (string, error) {
	homeDir, resolvedHomeDir, err := userHomeDirectoryBounds()
	if err != nil {
		return "", err
	}

	targetPath := strings.TrimSpace(rawPath)
	switch {
	case targetPath == "":
		if !emptyUsesHome {
			return "", nil
		}
		targetPath = homeDir
	case !filepath.IsAbs(targetPath):
		baseDir := strings.TrimSpace(relativeBase)
		if baseDir == "" {
			baseDir = homeDir
		}
		targetPath = filepath.Join(baseDir, targetPath)
	}

	targetPath = filepath.Clean(targetPath)
	info, err := os.Stat(targetPath)
	if err != nil {
		return "", err
	}
	if expectDirectory && !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", targetPath)
	}
	if !expectDirectory && info.IsDir() {
		return "", fmt.Errorf("path is a directory: %s", targetPath)
	}

	resolvedTargetPath, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		return "", err
	}
	resolvedTargetPath = filepath.Clean(resolvedTargetPath)
	if !pathWithinBase(resolvedTargetPath, resolvedHomeDir) {
		return "", fmt.Errorf("access denied: path must stay within your home directory (%s)", homeDir)
	}

	return resolvedTargetPath, nil
}

func userHomeDirectoryBounds() (string, string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve user home: %w", err)
	}

	homeDir = filepath.Clean(homeDir)
	resolvedHomeDir, err := filepath.EvalSymlinks(homeDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve user home: %w", err)
	}

	return homeDir, filepath.Clean(resolvedHomeDir), nil
}

func pathWithinBase(targetPath, basePath string) bool {
	targetPath = filepath.Clean(targetPath)
	basePath = filepath.Clean(basePath)

	if runtime.GOOS == "windows" {
		targetPath = strings.ToLower(targetPath)
		basePath = strings.ToLower(basePath)
	}

	relativePath, err := filepath.Rel(basePath, targetPath)
	if err != nil {
		return false
	}
	if relativePath == "." {
		return true
	}
	return relativePath != ".." && !strings.HasPrefix(relativePath, ".."+string(filepath.Separator))
}
