//go:build windows

package remote

import (
	"os/exec"
	"syscall"
)

func hideBackgroundConsole(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000,
	}
}
