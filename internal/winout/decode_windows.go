//go:build windows

package winout

import (
	"syscall"
	"unicode/utf8"
	"unsafe"
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procConsoleOutputCP = kernel32.NewProc("GetConsoleOutputCP")
	procOEMCP           = kernel32.NewProc("GetOEMCP")
	procMultiByteToWide = kernel32.NewProc("MultiByteToWideChar")
)

// Decode returns console output as UTF-8. Programs such as reg and net write
// in the console's code page (CP949 on Korean Windows); a GUI process has no
// console, so the OEM code page, which a child console starts with, is used then.
func Decode(b []byte) string {
	if len(b) == 0 || utf8.Valid(b) {
		return string(b)
	}
	cp, _, _ := procConsoleOutputCP.Call()
	if cp == 0 {
		cp, _, _ = procOEMCP.Call()
	}
	if cp == 0 {
		return string(b)
	}
	n, _, _ := procMultiByteToWide.Call(cp, 0, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0, 0)
	if n == 0 {
		return string(b)
	}
	w := make([]uint16, n)
	if r, _, _ := procMultiByteToWide.Call(cp, 0, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&w[0])), n); r == 0 {
		return string(b)
	}
	return syscall.UTF16ToString(w)
}
