//go:build !windows

package shares

import "os"

// syncDirIn makes the names in dir durable. It goes through root, so it cannot
// leave the share.
func syncDirIn(root *os.Root, dir string) error {
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
