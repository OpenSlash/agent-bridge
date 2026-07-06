//go:build !windows

package remote

import "os/exec"

func hideBackgroundConsole(_ *exec.Cmd) {}
