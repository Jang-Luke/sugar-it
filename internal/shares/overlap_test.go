package shares

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"sugarintegration/internal/vfs"
)

func TestCheckOverlap(t *testing.T) {
	ex := []Spec{{Name: "a", Path: "/data/a"}}
	cases := []struct {
		cand string
		bad  bool
	}{
		{"/data/a", true}, {"/data", true}, {"/data/a/b", true}, {"/data/a/", true},
		{"/data/b", false}, {"/data/ab", false}, {"/other", false},
	}
	for _, c := range cases {
		err := CheckOverlap(ex, c.cand, "linux")
		if c.bad {
			wantCode(t, err, vfs.CodeShareOverlap)
		} else if err != nil {
			t.Errorf("%s: %v", c.cand, err)
		}
	}
	if err := CheckOverlap(nil, "/x", "linux"); err != nil {
		t.Fatal(err)
	}
	// case sensitivity
	if err := CheckOverlap(ex, "/data/A", "linux"); err != nil {
		t.Errorf("linux is case sensitive: %v", err)
	}
	wantCode(t, CheckOverlap(ex, "/Data/A/x", "darwin"), vfs.CodeShareOverlap)
	// root contains everything
	wantCode(t, CheckOverlap(ex, "/", "linux"), vfs.CodeShareOverlap)
}

func TestCheckOverlapWindowsCase(t *testing.T) {
	ex := []Spec{{Name: "w", Path: `C:\Data`}}
	wantCode(t, CheckOverlap(ex, `c:\data\x`, "windows"), vfs.CodeShareOverlap)
	wantCode(t, CheckOverlap(ex, `c:\DATA`, "windows"), vfs.CodeShareOverlap)
	wantCode(t, CheckOverlap(ex, `C:/Data/`, "windows"), vfs.CodeShareOverlap)
	if err := CheckOverlap(ex, `C:\Database`, "windows"); err != nil {
		t.Fatal(err)
	}
	if err := CheckOverlap(ex, `D:\Data`, "windows"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckOverlapSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	os.MkdirAll(filepath.Join(real, "sub"), 0o755)
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	ex := []Spec{{Name: "r", Path: real}}
	wantCode(t, CheckOverlap(ex, link, runtime.GOOS), vfs.CodeShareOverlap)
	wantCode(t, CheckOverlap(ex, filepath.Join(link, "sub"), runtime.GOOS), vfs.CodeShareOverlap)
	// not-yet-existing child below a symlinked parent
	wantCode(t, CheckOverlap(ex, filepath.Join(link, "new"), runtime.GOOS), vfs.CodeShareOverlap)
	sib := filepath.Join(base, "sib")
	os.Mkdir(sib, 0o755)
	if err := CheckOverlap(ex, sib, runtime.GOOS); err != nil {
		t.Fatal(err)
	}
	// existing share registered through a link
	ex2 := []Spec{{Name: "l", Path: link}}
	wantCode(t, CheckOverlap(ex2, filepath.Join(real, "sub"), runtime.GOOS), vfs.CodeShareOverlap)
}
