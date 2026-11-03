//go:build windows

package mount

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	mpr                    = windows.NewLazySystemDLL("mpr.dll")
	procWNetUseConnectionW = mpr.NewProc("WNetUseConnectionW")
)

const (
	resourceTypeDisk = 1
	connectRedirect  = 0x80
	noError          = 0
)

// netResource is NETRESOURCEW.
type netResource struct {
	Scope, Type, DisplayType, Usage uint32
	LocalName, RemoteName           *uint16
	Comment, Provider               *uint16
}

// DefaultConnector connects with WNetUseConnectionW from mpr.dll. The password
// is only an argument of this call: no command line, no log, no error text.
// There is no CONNECT_INTERACTIVE, so Windows never shows a dialog.
func DefaultConnector(a ConnectArgs) (string, error) {
	if err := mpr.Load(); err != nil {
		return "", errors.New("cannot load mpr.dll")
	}
	if err := procWNetUseConnectionW.Find(); err != nil {
		return "", errors.New("WNetUseConnectionW not found in mpr.dll")
	}
	remote, err := windows.UTF16PtrFromString(a.RemoteName)
	if err != nil {
		return "", err
	}
	user, err := windows.UTF16PtrFromString(a.User)
	if err != nil {
		return "", err
	}
	pass, err := windows.UTF16PtrFromString(a.Password)
	if err != nil {
		return "", errors.New("the token cannot be passed to Windows")
	}
	nr := netResource{Type: resourceTypeDisk, RemoteName: remote}
	var flags uintptr
	if a.Drive != "" {
		if nr.LocalName, err = windows.UTF16PtrFromString(a.Drive); err != nil {
			return "", err
		}
	} else {
		flags = connectRedirect // Windows picks the drive and tells us which
	}
	access := make([]uint16, 260)
	size := uint32(len(access))
	var result uint32
	r, _, _ := procWNetUseConnectionW.Call(
		0, uintptr(unsafe.Pointer(&nr)), uintptr(unsafe.Pointer(pass)), uintptr(unsafe.Pointer(user)),
		flags, uintptr(unsafe.Pointer(&access[0])), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&result)))
	if r != noError {
		return "", WinError(uint32(syscall.Errno(r)))
	}
	drive := windows.UTF16ToString(access)
	if drive == "" {
		drive = a.Drive
	}
	return drive, nil
}
