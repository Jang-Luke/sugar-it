package cli

import (
	"io"

	"sugarintegration/internal/mount"
	"sugarintegration/internal/service"
)

// ExecuteWithRunners is Execute with the OS command runners replaced, so tests
// never mount or install anything.
func ExecuteWithRunners(args []string, stdout, stderr io.Writer, m mount.Runner, s service.Runner) int {
	return execute(&env{stdout: stdout, stderr: stderr, mountRunner: m, svcRunner: s}, args)
}
