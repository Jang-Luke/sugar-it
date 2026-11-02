// Package fsutil holds small filesystem helpers shared across packages.
package fsutil

import (
	"errors"
	"os"
	"path/filepath"
)

// WriteAtomic writes data to path with mode perm via a temp file in the same
// directory, fsync and rename, so readers see either the old or the new file.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil && !errors.Is(err, os.ErrInvalid) {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
