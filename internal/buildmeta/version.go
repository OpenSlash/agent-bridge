package buildmeta

import (
	"fmt"
	"runtime"
	"strings"
)

var (
	Version     = "dev"
	GitCommit   = "unknown"
	BuildTime   = "unknown"
	ProductName = "Remote"
)

func SetProduct(name, _ string) {
	if value := strings.TrimSpace(name); value != "" {
		ProductName = value
	}
}

type VersionInfo struct {
	Version   string `json:"version"`
	GitCommit string `json:"gitCommit"`
	BuildTime string `json:"buildTime"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

func GetVersionInfo() VersionInfo {
	return VersionInfo{
		Version:   Version,
		GitCommit: GitCommit,
		BuildTime: BuildTime,
		GoVersion: runtime.Version(),
		Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}

func GetVersionString() string {
	name := strings.TrimSpace(ProductName)
	if name == "" {
		name = "Remote"
	}
	if Version == "dev" && GitCommit != "" && GitCommit != "unknown" {
		if len(GitCommit) > 8 {
			return fmt.Sprintf("%s %s (%s)", name, Version, GitCommit[:8])
		}
		return fmt.Sprintf("%s %s (%s)", name, Version, GitCommit)
	}
	return fmt.Sprintf("%s %s", name, Version)
}
