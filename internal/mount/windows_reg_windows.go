//go:build windows

package mount

import "golang.org/x/sys/windows/registry"

// ReadWinLimits reads the WebClient parameters, applying Windows' defaults for
// values that are not set (BasicAuthLevel 1, FileSizeLimitInBytes 50000000).
func ReadWinLimits() WinLimits {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\WebClient\Parameters`, registry.QUERY_VALUE)
	if err != nil {
		return WinLimits{}
	}
	defer k.Close()
	w := WinLimits{Known: true, BasicAuthLevel: 1, FileSizeLimit: 50000000}
	if v, _, err := k.GetIntegerValue("BasicAuthLevel"); err == nil {
		w.BasicAuthLevel = v
	}
	if v, _, err := k.GetIntegerValue("FileSizeLimitInBytes"); err == nil {
		w.FileSizeLimit = v
	}
	return w
}
