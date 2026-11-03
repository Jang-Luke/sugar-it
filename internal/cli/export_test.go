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

// RotateLog is rotateLog for tests.
var RotateLog = rotateLog

// ChildArgs is the command line the background start gives the detached daemon.
func ChildArgs(configDir, listen, control, dav string, advertise []string) []string {
	return childArgs(configDir, daemonFlags{listen: listen, control: control, dav: dav, advertise: advertise})
}

// SetServiceOS makes the service commands act for goos until the test ends.
func SetServiceOS(goos string) (restore func()) {
	old := serviceOS
	serviceOS = goos
	return func() { serviceOS = old }
}
