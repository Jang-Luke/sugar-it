//go:build !windows

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking flock on f. The kernel drops it
// when the process dies.
func lockFile(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}
