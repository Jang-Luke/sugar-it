package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"sugarintegration/internal/shares"
	"sugarintegration/internal/vfs"
)

const (
	copyBuf      = 256 << 10
	maxNameTries = 10000
)

// Fetch copies one regular file from src to dst, following spec section 7:
// stat, conflict check, resume-or-restart, stream, fsync, verify the hash and
// the source etag, then an atomic commit. The destination only ever holds a
// complete, verified file. Fetch never touches the source (the caller removes
// it for a move, after Fetch returns). goos is the destination OS
// (runtime.GOOS) used for name validation.
//
// progress receives deltas whose running sum is always the number of bytes
// held in the part file: bytes already in a resumed part are reported once as
// a positive delta when resuming starts, new bytes as they are written, and a
// restart, part reset or removal reports a negative delta equal to everything
// reported so far. After a commit the sum equals the file size.
func Fetch(ctx context.Context, src vfs.FS, srcPath string, dst Dest, dstPath, partKey string,
	c Conflict, goos string, progress func(n int64)) (FetchResult, error) {
	if progress == nil {
		progress = func(int64) {}
	}
	if dst.ReadOnly() {
		return FetchResult{}, vfs.E(vfs.CodeReadOnly, "destination share is read-only")
	}
	dstClean, err := vfs.Clean(dstPath)
	if err != nil {
		return FetchResult{}, err
	}
	if dstClean == "" {
		return FetchResult{}, vfs.E(vfs.CodeInvalidPath, "destination is the share root")
	}
	if !shares.ValidName(goos, path.Base(dstClean)) {
		return FetchResult{}, vfs.E(vfs.CodeInvalidName, "name %q cannot be created on %s", path.Base(dstClean), goos)
	}
	switch c {
	case ConflictFail, ConflictSkip, ConflictOverwrite, ConflictRename:
	default:
		return FetchResult{}, vfs.E(vfs.CodeInvalidRequest, "unknown conflict policy %q", c)
	}

	held := int64(0) // bytes reported so far for this call
	report := func(d int64) {
		held += d
		progress(d)
	}
	drop := func() { // the part is gone: progress returns to zero
		if held != 0 {
			report(-held)
		}
	}

	// 1. source stat
	se, err := src.Stat(ctx, srcPath)
	if err != nil {
		return FetchResult{}, err
	}
	switch se.Type {
	case vfs.TypeFile:
	case vfs.TypeSymlink:
		return FetchResult{Skipped: true, SkipReason: SkippedSymlink}, nil
	default:
		return FetchResult{}, vfs.E(vfs.CodeInvalidRequest, "source is not a regular file")
	}

	// 2. conflict decision
	final, res, err := resolveDest(ctx, dst, dstClean, c)
	if err != nil || res.Skipped {
		return res, err
	}

	// 3-6. transfer; a changed source gets one clean restart.
	var entry vfs.Entry
	for attempt := 0; ; attempt++ {
		entry, err = fetchOnce(ctx, src, srcPath, dst, partKey, report, drop)
		if err == nil {
			break
		}
		if vfs.CodeOf(err) != vfs.CodeChanged {
			return FetchResult{}, err
		}
		drop()
		if rerr := dst.RemovePart(partKey); rerr != nil {
			return FetchResult{}, rerr
		}
		if attempt >= 1 {
			return FetchResult{}, err
		}
	}

	// The destination may have changed while we transferred: decide again.
	final, res, err = resolveDest(ctx, dst, dstClean, c)
	if err != nil || res.Skipped {
		drop()
		dst.RemovePart(partKey)
		return res, err
	}
	// 7-8. mtime and mode are applied by the atomic commit.
	if err := dst.CommitPart(partKey, final, entry.ModTime, entry.Mode); err != nil {
		return FetchResult{}, err
	}
	return FetchResult{Dst: final, SrcETag: entry.ETag}, nil
}

// resolveDest applies the conflict policy to dstClean and returns the final
// destination path.
func resolveDest(ctx context.Context, dst Dest, dstClean string, c Conflict) (string, FetchResult, error) {
	e, err := dst.Stat(ctx, dstClean)
	switch {
	case vfs.CodeOf(err) == vfs.CodeNotFound:
		return dstClean, FetchResult{}, nil
	case err != nil:
		return "", FetchResult{}, err
	}
	switch c {
	case ConflictSkip:
		return "", FetchResult{Skipped: true, SkipReason: SkippedExists}, nil
	case ConflictOverwrite:
		if e.Type == vfs.TypeDir {
			return "", FetchResult{}, vfs.E(vfs.CodeConflict, "destination is a directory")
		}
		return dstClean, FetchResult{}, nil
	case ConflictRename:
		p, err := freeName(ctx, dst, dstClean)
		return p, FetchResult{}, err
	}
	return "", FetchResult{}, vfs.E(vfs.CodeConflict, "destination already exists")
}

// freeName returns "name (N).ext" for the smallest N that is free.
func freeName(ctx context.Context, dst Dest, p string) (string, error) {
	for i := 1; i < maxNameTries; i++ {
		cand := numberedName(p, i)
		if _, err := dst.Stat(ctx, cand); vfs.CodeOf(err) == vfs.CodeNotFound {
			return cand, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", vfs.E(vfs.CodeConflict, "no free name for %q", p)
}

// numberedName returns "name (i).ext" for p.
func numberedName(p string, i int) string {
	dir, base := path.Split(p)
	ext := path.Ext(base)
	if ext == base { // dotfile: no extension
		ext = ""
	}
	return fmt.Sprintf("%s%s (%d)%s", dir, strings.TrimSuffix(base, ext), i, ext)
}

// fetchOnce brings the part file to a complete, fsynced, verified copy of the
// source and returns the source entry it matches. A `changed` error means the
// source moved under us; the caller resets the part.
func fetchOnce(ctx context.Context, src vfs.FS, srcPath string, dst Dest, key string,
	report func(int64), drop func()) (vfs.Entry, error) {
	se, err := src.Stat(ctx, srcPath)
	if err != nil {
		return vfs.Entry{}, err
	}
	if se.Type != vfs.TypeFile {
		return vfs.Entry{}, vfs.E(vfs.CodeChanged, "source is no longer a regular file")
	}

	// 3. resume only when the recorded etag is this source's etag.
	f, size, err := dst.OpenPart(key)
	if err != nil {
		return vfs.Entry{}, err
	}
	closed := false
	closeF := func() error {
		if closed {
			return nil
		}
		closed = true
		return f.Close()
	}
	defer closeF()
	if size > 0 {
		pe, err := dst.PartETag(key)
		if err != nil {
			return vfs.Entry{}, err
		}
		if pe == "" || pe != se.ETag || size > se.Size {
			closeF()
			if err := dst.ResetPart(key); err != nil {
				return vfs.Entry{}, err
			}
			if f, size, err = dst.OpenPart(key); err != nil {
				return vfs.Entry{}, err
			}
			closed = false
		}
	}
	if size > 0 {
		report(size) // resuming: these bytes are already held
	}
	if size == 0 {
		if err := dst.SetPartETag(key, se.ETag); err != nil {
			return vfs.Entry{}, err
		}
	}

	// 4. stream from the offset, pinned to the etag.
	rc, oe, err := src.Open(ctx, srcPath, size, se.ETag)
	if err != nil {
		return vfs.Entry{}, ctxOr(ctx, err)
	}
	defer rc.Close()
	if oe.ETag != se.ETag {
		return vfs.Entry{}, vfs.E(vfs.CodeChanged, "source changed")
	}
	if err := stream(ctx, f, rc, report); err != nil {
		return vfs.Entry{}, err
	}

	// 5. fsync.
	if err := f.Sync(); err != nil {
		return vfs.Entry{}, writeErr(err)
	}

	// 6. hash both sides, then re-check the source etag.
	sum, n, err := hashFile(ctx, f)
	if err != nil {
		return vfs.Entry{}, err
	}
	if err := closeF(); err != nil {
		return vfs.Entry{}, writeErr(err)
	}
	srcSum, err := src.Hash(ctx, srcPath)
	if err != nil {
		return vfs.Entry{}, ctxOr(ctx, err)
	}
	after, err := src.Stat(ctx, srcPath)
	if err != nil {
		return vfs.Entry{}, ctxOr(ctx, err)
	}
	if after.ETag != se.ETag {
		return vfs.Entry{}, vfs.E(vfs.CodeChanged, "source changed during transfer")
	}
	if n != se.Size || sum != srcSum {
		drop()
		dst.RemovePart(key)
		return vfs.Entry{}, vfs.E(vfs.CodeHashMismatch, "transferred data does not match the source")
	}
	return after, nil
}

func stream(ctx context.Context, f io.Writer, r io.Reader, progress func(int64)) error {
	buf := make([]byte, copyBuf)
	for {
		if err := ctx.Err(); err != nil {
			return canceled()
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return writeErr(err)
			}
			progress(int64(n))
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return ctxOr(ctx, rerr)
		}
	}
}

func hashFile(ctx context.Context, f interface {
	io.ReadSeeker
}) (string, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, writeErr(err)
	}
	h := sha256.New()
	buf := make([]byte, copyBuf)
	var total int64
	for {
		if ctx.Err() != nil {
			return "", 0, canceled()
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		total += int64(n)
		if err == io.EOF {
			return hex.EncodeToString(h.Sum(nil)), total, nil
		}
		if err != nil {
			return "", 0, writeErr(err)
		}
	}
}

func canceled() error { return vfs.E(vfs.CodeCanceled, "transfer canceled") }

// ctxOr reports cancellation when ctx is done, else the error unchanged
// (non-vfs errors become internal).
func ctxOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return canceled()
	}
	var ve *vfs.Error
	if errors.As(err, &ve) {
		return ve
	}
	return vfs.E(vfs.CodeInternal, "source read: %s", vfs.PathlessDetail(err))
}

// writeErr maps a local write error to a spec code.
func writeErr(err error) error {
	var ve *vfs.Error
	switch {
	case errors.As(err, &ve):
		return ve
	case shares.IsDiskFull(err):
		return vfs.E(vfs.CodeDiskFull, "no space left on device")
	}
	return vfs.E(vfs.CodeInternal, "write: %s", vfs.PathlessDetail(err))
}
