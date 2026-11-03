package shares

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"sugarintegration/internal/vfs"
)

var ctx = context.Background()

func newShare(t *testing.T, ro bool) (*LocalFS, string) {
	t.Helper()
	dir := t.TempDir()
	fs, err := Open(Spec{Name: "s", Path: dir, ReadOnly: ro})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs, dir
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := vfs.CodeOf(err); got != code {
		t.Fatalf("code = %q (%v), want %q", got, err, code)
	}
}

func readAll(t *testing.T, fs *LocalFS, p string) string {
	t.Helper()
	rc, _, err := fs.Open(ctx, p, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestListHidesPartDir(t *testing.T) {
	fs, dir := newShare(t, false)
	os.Mkdir(filepath.Join(dir, vfs.PartDir), 0o700)
	write(t, filepath.Join(dir, "a.txt"), "x")
	es, err := fs.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].Name != "a.txt" || es[0].Type != vfs.TypeFile || es[0].Size != 1 || es[0].ETag == "" {
		t.Fatalf("got %+v", es)
	}
}

func TestSymlinkEscapeBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	fs, dir := newShare(t, false)
	outside := filepath.Join(t.TempDir(), "secret")
	write(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
		t.Fatal(err)
	}
	es, err := fs.List(ctx, "")
	if err != nil || len(es) != 1 || es[0].Type != vfs.TypeSymlink {
		t.Fatalf("list: %+v %v", es, err)
	}
	rc, _, err := fs.Open(ctx, "evil", 0, "")
	if err == nil {
		b, _ := io.ReadAll(rc)
		rc.Close()
		t.Fatalf("opened symlink, read %q", b)
	}
	if _, err := fs.Hash(ctx, "evil"); err == nil {
		t.Fatal("hash followed symlink")
	}
	// a symlinked directory pointing outside must not be traversable either
	os.Symlink(filepath.Dir(outside), filepath.Join(dir, "evildir"))
	if _, err := fs.Stat(ctx, "evildir/secret"); err == nil {
		t.Fatal("traversed escaping dir symlink")
	}
	if _, err := fs.List(ctx, "evildir"); err == nil {
		t.Fatal("listed escaping dir symlink")
	}
}

func TestSymlinkLoopListed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	fs, dir := newShare(t, false)
	if err := os.Symlink("a", filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	es, err := fs.List(ctx, "")
	if err != nil || len(es) != 1 || es[0].Name != "a" || es[0].Type != vfs.TypeSymlink {
		t.Fatalf("got %+v %v", es, err)
	}
	if _, _, err := fs.Open(ctx, "a", 0, ""); err == nil {
		t.Fatal("opened loop")
	}
}

func TestParentEscapeBlocked(t *testing.T) {
	fs, _ := newShare(t, false)
	_, err := fs.Stat(ctx, "../x")
	wantCode(t, err, vfs.CodeInvalidPath)
	_, err = fs.Stat(ctx, vfs.PartDir+"/x")
	wantCode(t, err, vfs.CodeInvalidPath)
}

func TestReadOnlyRejectsWrites(t *testing.T) {
	fs, dir := newShare(t, true)
	write(t, filepath.Join(dir, "a"), "x")
	if !fs.ReadOnly() {
		t.Fatal("ReadOnly false")
	}
	_, err := fs.Put(ctx, "b", strings.NewReader("y"), vfs.PutOptions{})
	wantCode(t, err, vfs.CodeReadOnly)
	wantCode(t, fs.Mkdir(ctx, "d"), vfs.CodeReadOnly)
	wantCode(t, fs.Remove(ctx, "a", false), vfs.CodeReadOnly)
	wantCode(t, fs.Rename(ctx, "a", "c"), vfs.CodeReadOnly)
	wantCode(t, fs.CommitPart("k", "z", time.Now(), 0o644), vfs.CodeReadOnly)
	if _, err := os.Stat(filepath.Join(dir, "a")); err != nil {
		t.Fatal("file vanished")
	}
}

func TestPutOverwriteRules(t *testing.T) {
	fs, dir := newShare(t, false)
	write(t, filepath.Join(dir, "a"), "old")
	_, err := fs.Put(ctx, "a", strings.NewReader("new"), vfs.PutOptions{})
	wantCode(t, err, vfs.CodeExists)
	if got := readAll(t, fs, "a"); got != "old" {
		t.Fatalf("content %q", got)
	}
	mt := time.Unix(1700000000, 0)
	e, err := fs.Put(ctx, "a", strings.NewReader("new"), vfs.PutOptions{Overwrite: true, ModTime: mt})
	if err != nil || !e.ModTime.Equal(mt) || e.Size != 3 {
		t.Fatalf("overwrite: %+v %v", e, err)
	}
	if got := readAll(t, fs, "a"); got != "new" {
		t.Fatalf("content %q", got)
	}
	_, err = fs.Put(ctx, "a", strings.NewReader("zzz"), vfs.PutOptions{Overwrite: true, SHA256: strings.Repeat("0", 64)})
	wantCode(t, err, vfs.CodeHashMismatch)
	if got := readAll(t, fs, "a"); got != "new" {
		t.Fatalf("content after mismatch %q", got)
	}
	sum := sha256.Sum256([]byte("good"))
	if _, err := fs.Put(ctx, "b", strings.NewReader("good"), vfs.PutOptions{SHA256: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatal(err)
	}
	_, err = fs.Put(ctx, "nodir/c", strings.NewReader("x"), vfs.PutOptions{})
	wantCode(t, err, vfs.CodeNotFound)
	// no temp leftovers
	ents, _ := os.ReadDir(filepath.Join(dir, vfs.PartDir))
	if len(ents) != 0 {
		t.Fatalf("leftover temp files: %v", ents)
	}
}

func TestOpenOffsetAndETag(t *testing.T) {
	fs, dir := newShare(t, false)
	write(t, filepath.Join(dir, "h"), "hello")
	rc, e, err := fs.Open(ctx, "h", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "llo" || e.Size != 5 {
		t.Fatalf("got %q %+v", b, e)
	}
	rc, _, err = fs.Open(ctx, "h", 0, e.ETag)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	_, _, err = fs.Open(ctx, "h", 0, "bogus")
	wantCode(t, err, vfs.CodeChanged)
	_, _, err = fs.Open(ctx, "missing", 0, "")
	wantCode(t, err, vfs.CodeNotFound)
}

func TestRemoveNonEmpty(t *testing.T) {
	fs, dir := newShare(t, false)
	os.MkdirAll(filepath.Join(dir, "d", "e"), 0o755)
	write(t, filepath.Join(dir, "d", "f"), "x")
	wantCode(t, fs.Remove(ctx, "d", false), vfs.CodeNotEmpty)
	if err := fs.Remove(ctx, "d", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d")); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}
	wantCode(t, fs.Remove(ctx, "d", true), vfs.CodeNotFound)
	wantCode(t, fs.Remove(ctx, "", true), vfs.CodeInvalidPath)
}

func TestRenameRules(t *testing.T) {
	fs, dir := newShare(t, false)
	write(t, filepath.Join(dir, "a"), "A")
	write(t, filepath.Join(dir, "b"), "B")
	os.Mkdir(filepath.Join(dir, "d"), 0o755)
	wantCode(t, fs.Rename(ctx, "a", "b"), vfs.CodeExists)
	wantCode(t, fs.Rename(ctx, "d", "d/x"), vfs.CodeInvalidRequest)
	wantCode(t, fs.Rename(ctx, "nope", "z"), vfs.CodeNotFound)
	if err := fs.Rename(ctx, "a", "d/a2"); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fs, "d/a2"); got != "A" || readAll(t, fs, "b") != "B" {
		t.Fatal("content wrong")
	}
}

func TestPartLifecycle(t *testing.T) {
	fs, dir := newShare(t, false)
	if et, err := fs.PartETag("k1"); err != nil || et != "" {
		t.Fatalf("etag %q %v", et, err)
	}
	f, size, err := fs.OpenPart("k1")
	if err != nil || size != 0 {
		t.Fatal(size, err)
	}
	f.WriteString("abc")
	f.Close()
	if err := fs.SetPartETag("k1", "e1"); err != nil {
		t.Fatal(err)
	}
	if et, _ := fs.PartETag("k1"); et != "e1" {
		t.Fatalf("etag %q", et)
	}
	f, size, _ = fs.OpenPart("k1")
	if size != 3 {
		t.Fatalf("size %d", size)
	}
	f.WriteString("def") // positioned at end: append
	f.Close()
	if err := fs.ResetPart("k1"); err != nil {
		t.Fatal(err)
	}
	if et, _ := fs.PartETag("k1"); et != "" {
		t.Fatal("etag survived reset")
	}
	f, size, _ = fs.OpenPart("k1")
	f.Close()
	if size != 0 {
		t.Fatalf("size after reset %d", size)
	}
	if err := fs.RemovePart("k1"); err != nil {
		t.Fatal(err)
	}
	if err := fs.RemovePart("k1"); err != nil {
		t.Fatal("remove should be idempotent:", err)
	}
	ents, _ := os.ReadDir(filepath.Join(dir, vfs.PartDir))
	if len(ents) != 0 {
		t.Fatalf("leftovers %v", ents)
	}
	for _, k := range []string{"", "../x", "a/b", "a b", "a.part"} {
		if _, _, err := fs.OpenPart(k); vfs.CodeOf(err) != vfs.CodeInvalidRequest {
			t.Errorf("key %q accepted: %v", k, err)
		}
	}
}

func TestCommitPartPreservesMtime(t *testing.T) {
	fs, dir := newShare(t, false)
	f, _, _ := fs.OpenPart("k")
	f.WriteString("payload")
	f.Close()
	fs.SetPartETag("k", "e")
	mt := time.Unix(1600000000, 0)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	if err := fs.CommitPart("k", "sub/out", mt, 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "sub", "out"))
	if err != nil || !st.ModTime().Equal(mt) {
		t.Fatalf("stat %v %v", st, err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", st.Mode())
	}
	ents, _ := os.ReadDir(filepath.Join(dir, vfs.PartDir))
	if len(ents) != 0 {
		t.Fatalf("part not removed: %v", ents)
	}
	wantCode(t, fs.CommitPart("k", "x", mt, 0o644), vfs.CodeNotFound)
	wantCode(t, fs.CommitPart("k", "../x", mt, 0o644), vfs.CodeInvalidPath)
}

func TestGCPartsSkipsActive(t *testing.T) {
	fs, dir := newShare(t, false)
	for _, k := range []string{"old1", "old2", "fresh"} {
		f, _, _ := fs.OpenPart(k)
		f.Close()
		fs.SetPartETag(k, "e")
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	pd := filepath.Join(dir, vfs.PartDir)
	for _, k := range []string{"old1", "old2"} {
		for _, ext := range []string{".part", ".etag"} {
			if err := os.Chtimes(filepath.Join(pd, k+ext), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	active := func(k string) bool { return k == "old1" }
	if err := fs.GCParts(7*24*time.Hour, active); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(pd)
	got := map[string]bool{}
	for _, e := range ents {
		got[e.Name()] = true
	}
	if !got["old1.part"] || !got["old1.etag"] || !got["fresh.part"] || got["old2.part"] || got["old2.etag"] {
		t.Fatalf("after gc: %v", got)
	}
}

func TestIntermediateSymlinkRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	fs, dir := newShare(t, false)
	os.Mkdir(filepath.Join(dir, "d"), 0o755)
	write(t, filepath.Join(dir, "d", "file"), "orig")
	os.Symlink("d", filepath.Join(dir, "link"))
	os.Mkdir(filepath.Join(dir, vfs.PartDir), 0o700)
	write(t, filepath.Join(dir, vfs.PartDir, "secret.part"), "s")
	os.Symlink(vfs.PartDir, filepath.Join(dir, "pl"))

	// move-onto-itself data loss scenario
	f, _, err := fs.OpenPart("k")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("new")
	f.Close()
	wantCode(t, fs.CommitPart("k", "link/file", time.Now(), 0o644), vfs.CodeInvalidPath)
	if got := readAll(t, fs, "d/file"); got != "orig" {
		t.Fatalf("d/file = %q", got)
	}
	// part dir exposure
	_, _, err = fs.Open(ctx, "pl/secret.part", 0, "")
	wantCode(t, err, vfs.CodeInvalidPath)
	_, err = fs.Stat(ctx, "pl/secret.part")
	wantCode(t, err, vfs.CodeInvalidPath)
	_, err = fs.List(ctx, "pl")
	wantCode(t, err, vfs.CodeInvalidRequest) // final component symlink is not a directory
	_, err = fs.Hash(ctx, "link/file")
	wantCode(t, err, vfs.CodeInvalidPath)
	_, err = fs.Put(ctx, "link/new", strings.NewReader("x"), vfs.PutOptions{})
	wantCode(t, err, vfs.CodeInvalidPath)
	wantCode(t, fs.Mkdir(ctx, "link/m"), vfs.CodeInvalidPath)
	wantCode(t, fs.Remove(ctx, "link/file", false), vfs.CodeInvalidPath)
	wantCode(t, fs.Rename(ctx, "link/file", "z"), vfs.CodeInvalidPath)
	wantCode(t, fs.Rename(ctx, "d/file", "link/z"), vfs.CodeInvalidPath)
	if got := readAll(t, fs, "d/file"); got != "orig" {
		t.Fatalf("d/file = %q", got)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestPutReaderErrors(t *testing.T) {
	fs, dir := newShare(t, false)
	_, err := fs.Put(ctx, "a", errReader{io.ErrUnexpectedEOF}, vfs.PutOptions{})
	wantCode(t, err, vfs.CodeInternal)
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("message lost: %v", err)
	}
	// a source-side ENOENT/ENOSPC must not be classified as a local condition
	_, err = fs.Put(ctx, "a", errReader{&os.PathError{Op: "read", Path: "x", Err: os.ErrNotExist}}, vfs.PutOptions{})
	wantCode(t, err, vfs.CodeInternal)
	_, err = fs.Put(ctx, "a", errReader{vfs.E(vfs.CodePeerOffline, "gone")}, vfs.PutOptions{})
	wantCode(t, err, vfs.CodePeerOffline)
	if _, err := os.Stat(filepath.Join(dir, "a")); err == nil {
		t.Fatal("partial file committed")
	}
}

func TestCommitPartRetryAfterFailure(t *testing.T) {
	fs, dir := newShare(t, false)
	f, _, _ := fs.OpenPart("k")
	f.WriteString("data")
	f.Close()
	wantCode(t, fs.CommitPart("k", "missing/out", time.Now(), 0o444), vfs.CodeNotFound)
	os.Mkdir(filepath.Join(dir, "missing"), 0o755)
	if err := fs.CommitPart("k", "missing/out", time.Now(), 0o444); err != nil {
		t.Fatalf("retry: %v", err)
	}
	// a part left read-only (kill between chmod and rename) can be reopened
	f, _, _ = fs.OpenPart("k2")
	f.WriteString("ab")
	f.Close()
	os.Chmod(filepath.Join(dir, vfs.PartDir, "k2.part"), 0o444)
	f, size, err := fs.OpenPart("k2")
	if err != nil || size != 2 {
		t.Fatalf("reopen: %d %v", size, err)
	}
	f.Close()
}

func TestIsDiskFull(t *testing.T) {
	if !IsDiskFull(&os.PathError{Op: "write", Path: "x", Err: diskFullErrno}) {
		t.Fatal("native disk-full errno not recognised")
	}
	if IsDiskFull(errors.New("other")) || IsDiskFull(nil) {
		t.Fatal("false positive")
	}
}

// recordDirSyncs makes l report each directory sync, and the state of the
// world at that moment, to fn; fn's result is what the sync returns.
func recordDirSyncs(l *LocalFS, fn func(dir string) error) { l.syncDir = fn }

func TestCommitPartSyncsParentAfterRename(t *testing.T) {
	for _, c := range []struct{ dst, parent string }{{"f", "."}, {"sub/deep/f", "sub/deep"}} {
		t.Run(c.dst, func(t *testing.T) {
			dir := t.TempDir()
			l, err := Open(Spec{Name: "s", Path: dir})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			if err := os.MkdirAll(filepath.Join(dir, "sub/deep"), 0o755); err != nil {
				t.Fatal(err)
			}
			f, _, err := l.OpenPart("k")
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString("data")
			f.Close()
			var synced []string
			recordDirSyncs(l, func(d string) error {
				synced = append(synced, d)
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(c.dst))); err != nil {
					t.Errorf("synced %q before the rename: %v", d, err)
				}
				return nil
			})
			if err := l.CommitPart("k", c.dst, time.Now(), 0o644); err != nil {
				t.Fatal(err)
			}
			if len(synced) != 1 || synced[0] != c.parent {
				t.Errorf("synced %q, want [%q]", synced, c.parent)
			}
		})
	}
}

// The documented state after a failed sync: the file is at its destination,
// complete, and the part and etag are gone, but it is not known to be durable.
func TestCommitPartSyncFailure(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(Spec{Name: "s", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f, _, err := l.OpenPart("k")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("data")
	f.Close()
	if err := l.SetPartETag("k", "e"); err != nil {
		t.Fatal(err)
	}
	recordDirSyncs(l, func(string) error { return errors.New("dir sync failed") })
	err = l.CommitPart("k", "f", time.Now(), 0o644)
	if vfs.CodeOf(err) != vfs.CodeInternal {
		t.Fatalf("got %v, want internal", err)
	}
	if b, rerr := os.ReadFile(filepath.Join(dir, "f")); rerr != nil || string(b) != "data" {
		t.Errorf("destination: %q, %v", b, rerr)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, vfs.PartDir)); len(ents) != 0 {
		t.Errorf("part dir: %v", ents)
	}
	recordDirSyncs(l, func(string) error { return diskFullErrno })
	f, _, _ = l.OpenPart("k")
	f.Close()
	if vfs.CodeOf(l.CommitPart("k", "g", time.Now(), 0o644)) != vfs.CodeDiskFull {
		t.Error("ENOSPC from the sync must be disk_full")
	}
}

func TestPutSyncsParent(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		dir := t.TempDir()
		l, err := Open(Spec{Name: "s", Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		var synced []string
		recordDirSyncs(l, func(d string) error {
			synced = append(synced, d)
			if _, err := os.Stat(filepath.Join(dir, "sub/f")); err != nil {
				t.Errorf("synced before the rename: %v", err)
			}
			return nil
		})
		o := vfs.PutOptions{Overwrite: overwrite}
		if _, err := l.Put(context.Background(), "sub/f", strings.NewReader("x"), o); err != nil {
			t.Fatal(err)
		}
		if len(synced) != 1 || synced[0] != "sub" {
			t.Errorf("overwrite=%v: synced %q", overwrite, synced)
		}
		recordDirSyncs(l, func(string) error { return errors.New("dir sync failed") })
		o.Overwrite = true
		if _, err := l.Put(context.Background(), "sub/f", strings.NewReader("y"), o); vfs.CodeOf(err) != vfs.CodeInternal {
			t.Errorf("got %v, want internal", err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "sub/f")); string(b) != "y" {
			t.Errorf("destination %q: the file stays in place on a sync failure", b)
		}
	}
}

// A file's parent must itself be durable, or syncing it saves nothing.
func TestMkdirSyncsParent(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(Spec{Name: "s", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var synced []string
	recordDirSyncs(l, func(d string) error { synced = append(synced, d); return nil })
	if err := l.Mkdir(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := l.Mkdir(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 2 || synced[0] != "." || synced[1] != "a" {
		t.Errorf("synced %q", synced)
	}
	recordDirSyncs(l, func(string) error { return errors.New("dir sync failed") })
	if vfs.CodeOf(l.Mkdir(context.Background(), "c")) != vfs.CodeInternal {
		t.Error("sync failure not returned")
	}
}

func TestSyncParentStaysInShare(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(Spec{Name: "s", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "out")); err != nil {
		t.Skip(err)
	}
	var synced []string
	recordDirSyncs(l, func(d string) error { synced = append(synced, d); return nil })
	if vfs.CodeOf(l.SyncParent("out/f")) != vfs.CodeInvalidPath {
		t.Error("synced through a link")
	}
	if vfs.CodeOf(l.SyncParent("../f")) != vfs.CodeInvalidPath {
		t.Error("synced outside the share")
	}
	if len(synced) != 0 {
		t.Errorf("synced %q", synced)
	}
	if err := l.SyncParent("f"); err != nil || len(synced) != 1 || synced[0] != "." {
		t.Errorf("%v, %q", err, synced)
	}
}

// Network and FAT-style mounts reject directory fsync. That must not fail every
// commit; only those two errors are forgiven, however they are wrapped.
func TestDirSyncUnsupportedIsNoOp(t *testing.T) {
	for _, unsupported := range []error{
		syscall.EINVAL,
		syscall.ENOTSUP,
		&fs.PathError{Op: "sync", Path: "d", Err: syscall.EINVAL},
		&fs.PathError{Op: "sync", Path: "d", Err: syscall.ENOTSUP},
	} {
		dir := t.TempDir()
		l, err := Open(Spec{Name: "s", Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		calls := 0
		recordDirSyncs(l, func(string) error { calls++; return unsupported })
		f, _, err := l.OpenPart("k")
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString("data")
		f.Close()
		if err := l.CommitPart("k", "f", time.Now(), 0o644); err != nil {
			t.Errorf("%v: CommitPart: %v", unsupported, err)
		}
		if _, err := l.Put(ctx, "g", strings.NewReader("x"), vfs.PutOptions{}); err != nil {
			t.Errorf("%v: Put: %v", unsupported, err)
		}
		if err := l.Mkdir(ctx, "d"); err != nil {
			t.Errorf("%v: Mkdir: %v", unsupported, err)
		}
		if err := l.SyncParent("f"); err != nil {
			t.Errorf("%v: SyncParent: %v", unsupported, err)
		}
		if calls != 4 {
			t.Errorf("%v: the sync must still be tried each time: %d calls", unsupported, calls)
		}
	}
}

func TestDirSyncOtherErrorsStillFail(t *testing.T) {
	for _, other := range []error{syscall.EIO, syscall.EACCES, &fs.PathError{Op: "sync", Path: "d", Err: syscall.EIO}} {
		dir := t.TempDir()
		l, err := Open(Spec{Name: "s", Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		recordDirSyncs(l, func(string) error { return other })
		f, _, err := l.OpenPart("k")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if err := l.CommitPart("k", "f", time.Now(), 0o644); err == nil {
			t.Errorf("%v: CommitPart succeeded", other)
		}
		if err := l.SyncParent("f"); err == nil {
			t.Errorf("%v: SyncParent succeeded", other)
		}
	}
}

type pathErrReader struct{}

func (pathErrReader) Read([]byte) (int, error) {
	return 0, &os.PathError{Op: "read", Path: "/secret/share/file", Err: syscall.EIO}
}

// os.Root's files report absolute paths, and these messages reach other hosts.
func TestSourceReadErrorDoesNotCarryLocalPath(t *testing.T) {
	l, err := Open(Spec{Name: "s", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, err = l.Put(context.Background(), "f", pathErrReader{}, vfs.PutOptions{})
	if err == nil || strings.Contains(err.Error(), "/secret") {
		t.Fatalf("Put = %v", err)
	}
}

// On Windows os.Root calls "a:b" an escape, drops the end of "f." so that it
// means f, and splits at a backslash. None of that may show: looked up, such a
// name is not there; created, it is invalid_name (spec 7), not invalid_path.
func TestNamesThisOSCannotHave(t *testing.T) {
	l, dir := newShare(t, false)
	l.goos = "windows"
	write(t, filepath.Join(dir, "f"), "x")
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "d", "g"), "y")
	if runtime.GOOS != "windows" {
		write(t, filepath.Join(dir, "a:b"), "z") // the refusal comes from the rules, not the disk
	}
	part, _, err := l.OpenPart("k")
	if err != nil {
		t.Fatal(err)
	}
	part.Close()
	for _, name := range []string{"a:b", "CON", "CON/f", "nul.txt", "f.", "f ", `d\g`, "q?", "d/a:b", "COM\u00b9", "CONIN$"} {
		t.Run(name, func(t *testing.T) {
			_, err := l.Stat(ctx, name)
			wantCode(t, err, vfs.CodeNotFound)
			_, err = l.List(ctx, name)
			wantCode(t, err, vfs.CodeNotFound)
			_, _, err = l.Open(ctx, name, 0, "")
			wantCode(t, err, vfs.CodeNotFound)
			_, err = l.Hash(ctx, name)
			wantCode(t, err, vfs.CodeNotFound)
			wantCode(t, l.Remove(ctx, name, true), vfs.CodeNotFound)
			wantCode(t, l.Rename(ctx, name, "elsewhere"), vfs.CodeNotFound)

			_, err = l.Put(ctx, name, strings.NewReader("d"), vfs.PutOptions{Overwrite: true})
			wantCode(t, err, vfs.CodeInvalidName)
			wantCode(t, l.Mkdir(ctx, name), vfs.CodeInvalidName)
			wantCode(t, l.Rename(ctx, "f", name), vfs.CodeInvalidName)
			wantCode(t, l.CommitPart("k", name, time.Time{}, 0o644), vfs.CodeInvalidName)
		})
	}
	if got := readAll(t, l, "f"); got != "x" {
		t.Errorf("f = %q", got)
	}
	if got := readAll(t, l, "d/g"); got != "y" {
		t.Errorf("d/g = %q", got)
	}
}

// Elsewhere every clean component is one name, whatever its bytes: a file with
// a name that is not UTF-8 stays reachable.
func TestNamesLeftAloneOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("these names cannot exist on Windows; TestNamesThisOSCannotHave covers it")
	}
	l, dir := newShare(t, false)
	for _, name := range []string{"a:b", "CON", "f.", "\xff\xfe"} {
		write(t, filepath.Join(dir, name), "x")
		if got := readAll(t, l, name); got != "x" {
			t.Errorf("%q = %q", name, got)
		}
	}
	if _, err := l.Put(ctx, "c:d", strings.NewReader("d"), vfs.PutOptions{}); err != nil {
		t.Error(err)
	}
}
