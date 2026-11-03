//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

const (
	createNoWindow   = 0x08000000 // ignored together with detachedProcess; harmless
	detachedProcess  = 0x00000008
	breakawayFromJob = 0x01000000
)

// detach starts the child without a console and in a process group of its own,
// so that closing the parent's console or Ctrl-C does not reach it. With
// breakaway it also leaves the parent's job object: Windows OpenSSH kills every
// process in a session's job when the session ends. A job that forbids
// breakaway makes the start fail, so startDetached retries without it.
func detach(cmd *exec.Cmd, breakaway bool) {
	flags := uint32(syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess | createNoWindow)
	if breakaway {
		flags |= breakawayFromJob
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
}
