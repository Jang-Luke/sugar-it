// Package detach starts processes that outlive their parent and its console.
package detach

import "os/exec"

// Start starts a program that shows no window, such as a background daemon.
// newCmd makes the command, because a command can be started only once.
func Start(newCmd func() *exec.Cmd) (*exec.Cmd, error) { return start(newCmd, false) }

// StartGUI starts a program that opens a window. See apply on Windows for why it
// differs from Start.
func StartGUI(newCmd func() *exec.Cmd) (*exec.Cmd, error) { return start(newCmd, true) }

func start(newCmd func() *exec.Cmd, gui bool) (*exec.Cmd, error) {
	cmd := newCmd()
	apply(cmd, true, gui)
	if err := cmd.Start(); err == nil {
		return cmd, nil
	}
	// A job object can forbid breakaway on Windows; start inside it instead.
	cmd = newCmd()
	apply(cmd, false, gui)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
