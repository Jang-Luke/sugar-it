//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

// detach puts the child in a session of its own: no controlling terminal, so
// closing the terminal or Ctrl-C does not reach it.
func detach(cmd *exec.Cmd, _ bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
