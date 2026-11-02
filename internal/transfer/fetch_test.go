package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sugarintegration/internal/shares"
	"sugarintegration/internal/vfs"
)

type env struct {
	srcDir, dstDir string
	src, dst       *shares.LocalFS
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{srcDir: t.TempDir(), dstDir: t.TempDir()}
	var err error
	if e.src, err = shares.Open(shares.Spec{Name: "s", Path: e.srcDir}); err != nil {
		t.Fatal(err)
	}
	if e.dst, err = shares.Open(shares.Spec{Name: "d", Path: e.dstDir}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.src.Close(); e.dst.Close() })
	return e
}

func (e *env) write(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o640); err != nil {
		t.Fatal(err)
	}
}

func (e *env) read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *env) exists(dir, name string) bool {
	_, err := os.Lstat(filepath.Join(dir, name))
	return err == nil
}

func (e *env) noParts(t *testing.T) {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(e.dstDir, vfs.PartDir))
	if len(ents) != 0 {
		t.Fatalf("part files left behind: %v", ents)
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if vfs.CodeOf(err) != code || err == nil {
		t.Fatalf("got %v, want code %s", err, code)
	}
}

func fetch(e *env, src vfs.FS, from, to string, c Conflict) (FetchResult, error) {
	return Fetch(context.Background(), src, from, e.dst, to, "k1", c, "linux", nil)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

func TestFetchCopiesAndPreservesMtime(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a.txt", []byte("hello"))
	mt := time.Unix(1700000000, 123456789)
	os.Chtimes(filepath.Join(e.srcDir, "a.txt"), mt, mt)
	var got int64
	res, err := Fetch(context.Background(), e.src, "a.txt", e.dst, "b.txt", "k1", ConflictFail, "linux", func(n int64) { got += n })
	if err != nil || res.Dst != "b.txt" || res.Skipped {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if e.read(t, e.dstDir, "b.txt") != "hello" || got != 5 {
		t.Fatalf("content/progress wrong: %d", got)
	}
	fi, _ := os.Stat(filepath.Join(e.dstDir, "b.txt"))
	if !fi.ModTime().Equal(mt) || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mtime=%v mode=%v", fi.ModTime(), fi.Mode())
	}
	e.noParts(t)
}

func TestFetchResumesAfterDrop(t *testing.T) {
	e := newEnv(t)
	data := randBytes(10 << 20)
	e.write(t, e.srcDir, "big", data)
	f := newFault(e.src)
	f.dropAfter = 4 << 20
	_, err := fetch(e, f, "big", "big", ConflictFail)
	wantCode(t, err, vfs.CodePeerOffline)
	fi, serr := os.Stat(filepath.Join(e.dstDir, vfs.PartDir, "k1.part"))
	if serr != nil || fi.Size() != 4<<20 {
		t.Fatalf("part: %v %v", fi, serr)
	}
	if e.exists(e.dstDir, "big") {
		t.Fatal("destination must not exist yet")
	}
	f.dropAfter = -1
	if _, err := fetch(e, f, "big", "big", ConflictFail); err != nil {
		t.Fatal(err)
	}
	if f.offsets[len(f.offsets)-1] != 4<<20 {
		t.Fatalf("offsets %v", f.offsets)
	}
	if sha256.Sum256([]byte(e.read(t, e.dstDir, "big"))) != sha256.Sum256(data) {
		t.Fatal("content differs")
	}
	e.noParts(t)
}

func TestFetchSourceChangedRestarts(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a", randBytes(1<<20))
	f := newFault(e.src)
	f.dropAfter = 512 << 10
	_, err := fetch(e, f, "a", "a", ConflictFail)
	wantCode(t, err, vfs.CodePeerOffline)
	f.dropAfter = -1
	e.write(t, e.srcDir, "a", []byte("brand new content"))
	if _, err := fetch(e, f, "a", "a", ConflictFail); err != nil {
		t.Fatal(err)
	}
	if e.read(t, e.dstDir, "a") != "brand new content" {
		t.Fatal("stale part leaked into the result")
	}
	if f.offsets[len(f.offsets)-1] != 0 {
		t.Fatalf("expected restart from 0, offsets %v", f.offsets)
	}
}

func TestFetchPartWithoutETagRestarts(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a", []byte("0123456789"))
	os.MkdirAll(filepath.Join(e.dstDir, vfs.PartDir), 0o700)
	e.write(t, filepath.Join(e.dstDir, vfs.PartDir), "k1.part", []byte("JUNK"))
	if _, err := fetch(e, e.src, "a", "a", ConflictFail); err != nil {
		t.Fatal(err)
	}
	if e.read(t, e.dstDir, "a") != "0123456789" {
		t.Fatal("part without etag was trusted")
	}
}

func TestFetchSourceChangedTwiceFails(t *testing.T) {
	e := newEnv(t)
	p := filepath.Join(e.srcDir, "a")
	e.write(t, e.srcDir, "a", randBytes(1<<20))
	f := newFault(e.src)
	n := 0
	f.hookAt = 256 << 10
	f.hook = func() {
		n++
		os.WriteFile(p, randBytes(1<<20+n), 0o640)
	}
	_, err := fetch(e, f, "a", "a", ConflictFail)
	wantCode(t, err, vfs.CodeChanged)
	if e.exists(e.dstDir, "a") {
		t.Fatal("destination must not exist")
	}
	e.noParts(t)
	if n != 2 {
		t.Fatalf("expected exactly one restart, hook ran %d times", n)
	}
}

func TestFetchSourceChangedOnceRecovers(t *testing.T) {
	e := newEnv(t)
	p := filepath.Join(e.srcDir, "a")
	e.write(t, e.srcDir, "a", randBytes(1<<20))
	f := newFault(e.src)
	n := 0
	f.hookAt = 256 << 10
	f.hook = func() {
		if n++; n == 1 {
			os.WriteFile(p, []byte("second version"), 0o640)
		}
	}
	if _, err := fetch(e, f, "a", "a", ConflictFail); err != nil {
		t.Fatal(err)
	}
	if e.read(t, e.dstDir, "a") != "second version" {
		t.Fatal("mixed content")
	}
}

func TestFetchHashMismatch(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a", []byte("data"))
	f := newFault(e.src)
	f.hash = "00ff"
	_, err := fetch(e, f, "a", "a", ConflictFail)
	wantCode(t, err, vfs.CodeHashMismatch)
	if e.exists(e.dstDir, "a") {
		t.Fatal("unverified file committed")
	}
	e.noParts(t)
}

func TestFetchConflictPolicies(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a.txt", []byte("new"))
	e.write(t, e.dstDir, "a.txt", []byte("old"))

	_, err := fetch(e, e.src, "a.txt", "a.txt", ConflictFail)
	wantCode(t, err, vfs.CodeConflict)
	if e.read(t, e.dstDir, "a.txt") != "old" {
		t.Fatal("fail overwrote")
	}
	res, err := fetch(e, e.src, "a.txt", "a.txt", ConflictSkip)
	if err != nil || !res.Skipped || e.read(t, e.dstDir, "a.txt") != "old" {
		t.Fatalf("skip: %+v %v", res, err)
	}
	res, err = fetch(e, e.src, "a.txt", "a.txt", ConflictRename)
	if err != nil || res.Dst != "a (1).txt" || e.read(t, e.dstDir, "a (1).txt") != "new" {
		t.Fatalf("rename1: %+v %v", res, err)
	}
	res, err = fetch(e, e.src, "a.txt", "a.txt", ConflictRename)
	if err != nil || res.Dst != "a (2).txt" {
		t.Fatalf("rename2: %+v %v", res, err)
	}
	res, err = fetch(e, e.src, "a.txt", "a.txt", ConflictOverwrite)
	if err != nil || res.Dst != "a.txt" || e.read(t, e.dstDir, "a.txt") != "new" {
		t.Fatalf("overwrite: %+v %v", res, err)
	}
	e.noParts(t)
}

func TestFetchOverwriteDirectoryConflicts(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a", []byte("x"))
	os.Mkdir(filepath.Join(e.dstDir, "a"), 0o755)
	_, err := fetch(e, e.src, "a", "a", ConflictOverwrite)
	wantCode(t, err, vfs.CodeConflict)
}

func TestFetchSkipsSymlink(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "real", []byte("x"))
	os.Symlink("real", filepath.Join(e.srcDir, "link"))
	res, err := fetch(e, e.src, "link", "link", ConflictFail)
	if err != nil || !res.Skipped || res.SkipReason != "skipped_symlink" {
		t.Fatalf("%+v %v", res, err)
	}
	if e.exists(e.dstDir, "link") {
		t.Fatal("symlink transferred")
	}
}

func TestFetchCaseOnlyCollision(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a.txt", []byte("new"))
	e.write(t, e.dstDir, "A.txt", []byte("old"))
	d := ciDest{e.dst}
	_, err := Fetch(context.Background(), e.src, "a.txt", d, "a.txt", "k1", ConflictFail, "linux", nil)
	wantCode(t, err, vfs.CodeConflict)
	if e.read(t, e.dstDir, "A.txt") != "old" || e.exists(e.dstDir, "a.txt") {
		t.Fatal("existing file disturbed")
	}
}

func TestFetchInvalidName(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a:b", []byte("x"))
	_, err := Fetch(context.Background(), e.src, "a:b", e.dst, "a:b", "k1", ConflictFail, "windows", nil)
	wantCode(t, err, vfs.CodeInvalidName)
	if !e.exists(e.srcDir, "a:b") {
		t.Fatal("source removed")
	}
}

func TestFetchDiskFull(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a", []byte("data"))
	d := failDest{LocalFS: e.dst, commitErr: vfs.E(vfs.CodeDiskFull, "full")}
	_, err := Fetch(context.Background(), e.src, "a", d, "a", "k1", ConflictFail, "linux", nil)
	wantCode(t, err, vfs.CodeDiskFull)
	if e.exists(e.dstDir, "a") {
		t.Fatal("destination exists")
	}
}

func TestFetchDiskFullOnWrite(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("no /dev/full")
	}
	e := newEnv(t)
	e.write(t, e.srcDir, "a", []byte("data"))
	d := failDest{LocalFS: e.dst, partFile: "/dev/full"}
	_, err := Fetch(context.Background(), e.src, "a", d, "a", "k1", ConflictFail, "linux", nil)
	wantCode(t, err, vfs.CodeDiskFull)
}

func TestFetchCanceled(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a", randBytes(2<<20))
	ctx, cancel := context.WithCancel(context.Background())
	f := newFault(e.src)
	f.hookAt = 1 << 20
	f.hook = cancel
	_, err := Fetch(ctx, f, "a", e.dst, "a", "k1", ConflictFail, "linux", nil)
	wantCode(t, err, vfs.CodeCanceled)
	fi, serr := os.Stat(filepath.Join(e.dstDir, vfs.PartDir, "k1.part"))
	if serr != nil || fi.Size() < 1<<20 {
		t.Fatalf("part not kept: %v %v", fi, serr)
	}
	if e.exists(e.dstDir, "a") {
		t.Fatal("destination exists")
	}
}

func TestFetchEmptyFile(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "empty", nil)
	if _, err := fetch(e, e.src, "empty", "empty", ConflictFail); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dstDir, "empty")); !bytes.Equal(b, nil) || !e.exists(e.dstDir, "empty") {
		t.Fatal("empty file missing")
	}
}

func TestFetchSourceNotFoundAndDir(t *testing.T) {
	e := newEnv(t)
	os.Mkdir(filepath.Join(e.srcDir, "d"), 0o755)
	_, err := fetch(e, e.src, "nope", "x", ConflictFail)
	wantCode(t, err, vfs.CodeNotFound)
	_, err = fetch(e, e.src, "d", "x", ConflictFail)
	wantCode(t, err, vfs.CodeInvalidRequest)
}

func TestFetchReadOnlyDest(t *testing.T) {
	e := newEnv(t)
	e.write(t, e.srcDir, "a", []byte("x"))
	ro, _ := shares.Open(shares.Spec{Name: "r", Path: e.dstDir, ReadOnly: true})
	defer ro.Close()
	_, err := Fetch(context.Background(), e.src, "a", ro, "a", "k1", ConflictFail, "linux", nil)
	wantCode(t, err, vfs.CodeReadOnly)
}
