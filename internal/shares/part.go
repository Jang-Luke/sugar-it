package shares

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"

	"sugarintegration/internal/vfs"
)

const (
	partExt = ".part"
	etagExt = ".etag"
	tmpExt  = ".tmp"
)

var keyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func checkKey(key string) error {
	if !keyRE.MatchString(key) {
		return vfs.E(vfs.CodeInvalidRequest, "invalid part key")
	}
	return nil
}

func partPath(key, ext string) string { return vfs.PartDir + "/" + key + ext }

func (l *LocalFS) ensurePartDir() error {
	err := l.root.Mkdir(vfs.PartDir, 0o700)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return mapErr(err)
	}
	return nil
}

// restoreWrite gives the owner write access back to a part that an earlier,
// interrupted CommitPart left with the (possibly read-only) source mode.
func (l *LocalFS) restoreWrite(name string) {
	l.root.Chmod(name, 0o600)
}

// OpenPart opens (creating if needed) the partial file for key, positioned
// at its end so the caller can append, and returns its current size.
func (l *LocalFS) OpenPart(key string) (*os.File, int64, error) {
	if err := checkKey(key); err != nil {
		return nil, 0, err
	}
	if err := l.writable(); err != nil {
		return nil, 0, err
	}
	if err := l.ensurePartDir(); err != nil {
		return nil, 0, err
	}
	l.restoreWrite(partPath(key, partExt))
	f, err := l.root.OpenFile(partPath(key, partExt), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, 0, mapErr(err)
	}
	size, err := f.Seek(0, 2)
	if err != nil {
		f.Close()
		return nil, 0, mapErr(err)
	}
	return f, size, nil
}

// PartETag returns the source etag recorded for key, or "" if none.
func (l *LocalFS) PartETag(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	b, err := l.root.ReadFile(partPath(key, etagExt))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", mapErr(err)
	}
	return string(b), nil
}

// SetPartETag records the source etag for key atomically.
func (l *LocalFS) SetPartETag(key, etag string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := l.writable(); err != nil {
		return err
	}
	if err := l.ensurePartDir(); err != nil {
		return err
	}
	tmp := partPath(key, etagExt+tmpExt)
	if err := l.root.WriteFile(tmp, []byte(etag), 0o600); err != nil {
		return mapErr(err)
	}
	if err := l.root.Rename(tmp, partPath(key, etagExt)); err != nil {
		l.root.Remove(tmp)
		return mapErr(err)
	}
	return nil
}

// ResetPart discards the partial data and etag for key.
func (l *LocalFS) ResetPart(key string) error { return l.RemovePart(key) }

// RemovePart deletes the partial data and etag for key; missing is fine.
func (l *LocalFS) RemovePart(key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := l.writable(); err != nil {
		return err
	}
	for _, ext := range []string{partExt, etagExt, etagExt + tmpExt} {
		if err := l.root.Remove(partPath(key, ext)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return mapErr(err)
		}
	}
	return nil
}

// CommitPart fsyncs the partial file, applies mode and mtime, and atomically
// renames it to dst, replacing a regular file already there (conflict policy
// is the caller's). The part and its etag are gone afterwards, and the new name
// is durable. If syncing the directory fails, the error is returned with the
// complete file already at dst and the part and etag gone, but the name not
// known to be durable: the caller must not treat it as committed, and can
// finish with SyncParent.
func (l *LocalFS) CommitPart(key, dst string, mtime time.Time, mode uint32) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := l.writable(); err != nil {
		return err
	}
	d, err := l.cleanPath(dst)
	if err != nil {
		return err
	}
	if d == "" {
		return vfs.E(vfs.CodeInvalidPath, "cannot commit to the share root")
	}
	pp := partPath(key, partExt)
	l.restoreWrite(pp)
	f, err := l.root.OpenFile(pp, os.O_RDWR, 0)
	if err != nil {
		return mapErr(err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return mapErr(err)
	}
	if err := l.root.Chmod(pp, fs.FileMode(mode&0o777)); err != nil {
		return mapErr(err)
	}
	if !mtime.IsZero() {
		if err := l.root.Chtimes(pp, mtime, mtime); err != nil {
			return mapErr(err)
		}
	}
	if err := l.root.Rename(pp, d); err != nil {
		return mapErr(err)
	}
	etagErr := l.root.Remove(partPath(key, etagExt))
	if err := l.syncParent(d); err != nil {
		return err
	}
	if etagErr != nil && !errors.Is(etagErr, fs.ErrNotExist) {
		return mapErr(etagErr)
	}
	return nil
}

// GCParts removes part-dir files older than olderThan whose key is not
// active. It is internal housekeeping and runs even on read-only shares.
func (l *LocalFS) GCParts(olderThan time.Duration, active func(key string) bool) error {
	d, err := l.root.Open(vfs.PartDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return mapErr(err)
	}
	ents, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return mapErr(err)
	}
	cutoff := time.Now().Add(-olderThan)
	var first error
	for _, e := range ents {
		name := e.Name()
		key := name
		for _, ext := range []string{etagExt + tmpExt, partExt, etagExt, tmpExt} {
			if strings.HasSuffix(name, ext) {
				key = strings.TrimSuffix(name, ext)
				break
			}
		}
		if active != nil && active(key) {
			continue
		}
		info, err := e.Info()
		if err != nil || e.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		if err := l.root.Remove(vfs.PartDir + "/" + name); err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
			first = mapErr(err)
		}
	}
	return first
}
