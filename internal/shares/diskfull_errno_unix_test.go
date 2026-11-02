//go:build !windows

package shares

import "syscall"

var diskFullErrno = syscall.ENOSPC
