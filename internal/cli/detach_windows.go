//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

const (
	createNoWindow  = 0x08000000 // ignored together with detachedProcess; harmless
	detachedProcess = 0x00000008
)

// detach starts the child without a console and in a process group of its own,
// so that closing the parent's console or Ctrl-C does not reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess | createNoWindow,
		HideWindow:    true,
	}
}
