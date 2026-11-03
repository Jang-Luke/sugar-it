//go:build !windows

package mount

// ReadWinLimits has nothing to read outside Windows.
func ReadWinLimits() WinLimits { return WinLimits{} }
