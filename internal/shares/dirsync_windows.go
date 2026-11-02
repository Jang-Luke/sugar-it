//go:build windows

package shares

import "os"

// syncDirIn does nothing: Windows cannot fsync a directory.
func syncDirIn(*os.Root, string) error { return nil }
