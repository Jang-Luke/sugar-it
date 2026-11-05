//go:build windows

package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	seeMaskFlagNoUI       = 0x00000400

	installerWait = 15 * time.Minute // it may fetch the WebView2 runtime
)

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

func platformOps() (HelperOps, error) {
	return HelperOps{WaitExit: WaitExit, Hold: holdInstaller, RunElevated: runElevated}, nil
}

func holdInstaller(file, want string) (io.Closer, error) {
	return HoldVerified(openDenyWrite, file, want)
}

// openDenyWrite opens a file so that nobody can write, rename or delete it while the
// handle lasts. Starting a program needs read and execute access, which FILE_SHARE_READ
// allows, so this should not keep it from running; ErrInstallerBusy covers it if it does.
func openDenyWrite(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// WaitExit waits for the process to exit. One that is already gone counts.
func WaitExit(pid int, d time.Duration) error {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	return waitHandle(h, d)
}

func waitHandle(h windows.Handle, d time.Duration) error {
	ev, err := windows.WaitForSingleObject(h, uint32(d/time.Millisecond))
	if err != nil {
		return err
	}
	if ev != windows.WAIT_OBJECT_0 {
		return ErrWaitTimeout
	}
	return nil
}

// ProcessImage is the full path of a process's executable.
func ProcessImage(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// runElevated runs file through the "runas" verb, which asks with the UAC prompt,
// and waits for it. This process stays unelevated, so what it starts next is too.
func runElevated(file, params string) (int, error) {
	// ShellExecuteEx may use COM, and the apartment belongs to the thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const coinit = windows.COINIT_APARTMENTTHREADED | windows.COINIT_DISABLE_OLE1DDE
	if err := windows.CoInitializeEx(0, coinit); err == nil {
		defer windows.CoUninitialize()
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return 0, err
	}
	filePtr, err := windows.UTF16PtrFromString(file)
	if err != nil {
		return 0, err
	}
	paramsPtr, err := windows.UTF16PtrFromString(params)
	if err != nil {
		return 0, err
	}
	dir, err := windows.UTF16PtrFromString(filepath.Dir(file))
	if err != nil {
		return 0, err
	}
	info := shellExecuteInfo{
		mask:   seeMaskNoCloseProcess | seeMaskNoAsync | seeMaskFlagNoUI,
		verb:   verb,
		file:   filePtr,
		params: paramsPtr,
		dir:    dir,
		show:   windows.SW_SHOWNORMAL,
	}
	info.size = uint32(unsafe.Sizeof(info))
	r, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		if errors.Is(callErr, windows.ERROR_CANCELLED) {
			return 0, ErrElevationDenied
		}
		if errors.Is(callErr, windows.ERROR_SHARING_VIOLATION) {
			return 0, ErrInstallerBusy
		}
		return 0, fmt.Errorf("start the installer: %w", callErr)
	}
	if info.process == 0 {
		return 0, errors.New("start the installer: no process handle")
	}
	h := windows.Handle(info.process)
	defer windows.CloseHandle(h)
	if err := waitHandle(h, installerWait); err != nil {
		return 0, fmt.Errorf("wait for the installer: %w", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return 0, err
	}
	return int(code), nil
}
