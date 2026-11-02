package transfer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"sugarintegration/internal/vfs"
)

func TestResolveDstCpRules(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Join(e.dstDir, "dir", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(t, e.dstDir, "file", []byte("x"))
	src := vfs.Entry{Name: "name", Path: "some/name", Type: vfs.TypeFile}
	for _, c := range []struct{ in, want string }{
		{"dir", "dir/name"},
		{"dir/", "dir/name"},
		{"dir/sub", "dir/sub/name"},
		{"missing/", "missing/name"},
		{"missing/deeper/", "missing/deeper/name"},
		{"missing", "missing"},
		{"dir/other", "dir/other"},
		{"file", "file"},
		{"", "name"},
		{".", "name"},
	} {
		got, err := ResolveDst(context.Background(), src, e.dst, c.in)
		if err != nil || got != c.want {
			t.Errorf("ResolveDst(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestResolveDstRejectsEscapes(t *testing.T) {
	e := newEnv(t)
	src := vfs.Entry{Name: "name", Type: vfs.TypeFile}
	for _, in := range []string{"../x", "/abs", vfs.PartDir + "/x"} {
		if _, err := ResolveDst(context.Background(), src, e.dst, in); vfs.CodeOf(err) != vfs.CodeInvalidPath {
			t.Errorf("ResolveDst(%q) = %v, want invalid_path", in, err)
		}
	}
}

func TestResolveDstThroughSymlinkFails(t *testing.T) {
	e := newEnv(t)
	if err := os.Mkdir(filepath.Join(e.dstDir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(e.dstDir, "link")); err != nil {
		t.Skip(err)
	}
	src := vfs.Entry{Name: "name", Type: vfs.TypeFile}
	if _, err := ResolveDst(context.Background(), src, e.dst, "link/x"); vfs.CodeOf(err) != vfs.CodeInvalidPath {
		t.Errorf("got %v, want invalid_path", err)
	}
}

func spec(op, from, to string) Spec {
	return Spec{Op: op,
		Src: Endpoint{Peer: "p", Share: "s", Path: from},
		Dst: Endpoint{Peer: "p", Share: "s", Path: to}}
}

func TestValidateRejectsSelf(t *testing.T) {
	dir := vfs.Entry{Name: "a", Path: "a", Type: vfs.TypeDir}
	for _, op := range []string{"copy", "move"} {
		for _, c := range [][2]string{
			{"a", "a"}, {"a", "a/b"}, {"a/", "a"}, {"a", "a/"}, {"./a", "a/./b"}, {"", "x"}, {"", ""},
		} {
			if err := Validate(spec(op, c[0], c[1]), dir, false); vfs.CodeOf(err) != vfs.CodeInvalidRequest {
				t.Errorf("%s %q -> %q: got %v, want invalid_request", op, c[0], c[1], err)
			}
		}
		if err := Validate(spec(op, "a", "ab"), dir, false); err != nil {
			t.Errorf("%s a -> ab: %v", op, err)
		}
		// The same paths on another share or peer are not the same place.
		other := spec(op, "a", "a")
		other.Dst.Share = "t"
		if err := Validate(other, dir, false); err != nil {
			t.Errorf("%s across shares: %v", op, err)
		}
		other = spec(op, "a", "a/b")
		other.Src.Peer = "q"
		if err := Validate(other, dir, false); err != nil {
			t.Errorf("%s across peers: %v", op, err)
		}
	}
}

func TestValidateMoveFromReadOnly(t *testing.T) {
	s := spec("move", "a", "b")
	e := vfs.Entry{Name: "a", Type: vfs.TypeFile}
	if err := Validate(s, e, true); vfs.CodeOf(err) != vfs.CodeReadOnly {
		t.Errorf("move: got %v, want read_only", err)
	}
	s.Op = "copy"
	if err := Validate(s, e, true); err != nil {
		t.Errorf("copy from a read-only share: %v", err)
	}
}

func TestValidateRejectsMalformed(t *testing.T) {
	e := vfs.Entry{Name: "a", Type: vfs.TypeFile}
	s := spec("rename", "a", "b")
	if err := Validate(s, e, false); vfs.CodeOf(err) != vfs.CodeInvalidRequest {
		t.Errorf("op: got %v", err)
	}
	s = spec("copy", "a", "b")
	s.OnConflict = "merge"
	if err := Validate(s, e, false); vfs.CodeOf(err) != vfs.CodeInvalidRequest {
		t.Errorf("conflict: got %v", err)
	}
	if err := Validate(spec("copy", "../a", "b"), e, false); vfs.CodeOf(err) != vfs.CodeInvalidPath {
		t.Errorf("src path: got %v", err)
	}
	if err := Validate(spec("copy", "a", "../b"), e, false); vfs.CodeOf(err) != vfs.CodeInvalidPath {
		t.Errorf("dst path: got %v", err)
	}
}

func TestPartKeyFormat(t *testing.T) {
	k := partKey("job", "a/b")
	if len(k) != 32 {
		t.Fatalf("len = %d", len(k))
	}
	if k == partKey("job", "a/c") || k == partKey("jo", "ba/b") {
		t.Error("keys collide")
	}
}
