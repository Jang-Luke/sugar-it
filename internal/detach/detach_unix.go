//go:build unix

package detach

import (
	"os/exec"
	"syscall"
)

// apply puts the child in a session of its own: no controlling terminal, so
// closing the terminal or Ctrl-C does not reach it.
func apply(cmd *exec.Cmd, _, _ bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
