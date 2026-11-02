//go:build !windows

package shares

import (
	"errors"
	"syscall"
)

func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// IsDiskFull reports whether err is the OS's out-of-space or quota error.
func IsDiskFull(err error) bool { return isDiskFull(err) }
