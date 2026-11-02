package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"sugarintegration/internal/mesh"
	"sugarintegration/internal/shares"
)

func TestDefaults(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	if c.ListenAddr != ":7443" || c.ControlAddr != "127.0.0.1:7444" || c.DAVAddr != "127.0.0.1:7445" || c.MaxConcurrentJobs != 2 {
		t.Fatalf("%+v", c)
	}
	if !mesh.ValidName(c.Name) {
		t.Fatalf("invalid default name %q", c.Name)
	}
}

func TestDefaultNameNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"My_NAS.local": "my-nas-local",
		"nas":          "nas",
		"":             "host",
		"한글":           "host",
		"---":          "host",
		"local":        "host",
		"LOCAL":        "host",
		"a-very-long-hostname-that-goes-beyond-32-chars": "a-very-long-hostname-that-goes-b",
	} {
		got := defaultName(in)
		if got != want {
			t.Errorf("defaultName(%q) = %q, want %q", in, got, want)
		}
		if !mesh.ValidName(got) {
			t.Errorf("defaultName(%q) = %q invalid", in, got)
		}
	}
}

func TestDefaultDir(t *testing.T) {
	d, err := DefaultDir()
	if err != nil {
		t.Skip("no user config dir")
	}
	if filepath.Base(d) != "sugar-it" {
		t.Fatal(d)
	}
}

func TestUpdatePersists(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	err := s.Update(func(c *Config) error {
		c.Name = "box"
		c.Shares = append(c.Shares, shares.Spec{Name: "docs", Path: "/x"})
		c.Members = append(c.Members, mesh.Member{ID: "a", Name: "a", Addrs: []string{"1:1"}, Generation: 1})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := s2.Get()
	if c.Name != "box" || len(c.Shares) != 1 || c.Shares[0].Path != "/x" || len(c.Members) != 1 || c.Members[0].Addrs[0] != "1:1" {
		t.Fatalf("%+v", c)
	}
	if runtime.GOOS != "windows" {
		if s2.Dir() != dir {
			t.Fatal(s2.Dir())
		}
	}
}

func TestUpdateErrorDiscards(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	before, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	boom := errors.New("boom")
	err := s.Update(func(c *Config) error {
		c.Name = "changed"
		c.Shares = append(c.Shares, shares.Spec{Name: "x"})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if s.Get().Name == "changed" || len(s.Get().Shares) != 0 {
		t.Fatal("memory changed")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if string(before) != string(after) {
		t.Fatal("file changed")
	}
}

func TestGetIsDeepCopy(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Update(func(c *Config) error {
		c.Members = []mesh.Member{{ID: "a", Addrs: []string{"1:1"}}}
		c.AdvertiseAddrs = []string{"x:1"}
		return nil
	})
	c := s.Get()
	c.Members[0].Addrs[0] = "z"
	c.AdvertiseAddrs[0] = "z"
	d := s.Get()
	if d.Members[0].Addrs[0] != "1:1" || d.AdvertiseAddrs[0] != "x:1" {
		t.Fatal("Get leaked internal state")
	}
}

func TestCorruptConfigNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	os.WriteFile(p, []byte("{broken"), 0o600)
	if _, err := Open(dir); err == nil {
		t.Fatal("expected error")
	}
	if b, _ := os.ReadFile(p); string(b) != "{broken" {
		t.Fatal("overwritten")
	}
}

func TestControlTokenStable(t *testing.T) {
	dir := t.TempDir()
	a, err := ControlToken(dir)
	if err != nil || len(a) < 32 {
		t.Fatalf("%q %v", a, err)
	}
	b, _ := ControlToken(dir)
	if a != b {
		t.Fatal("changed")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "control.token"))
		if fi.Mode().Perm() != 0o600 {
			t.Fatal(fi.Mode())
		}
	}
}

func TestControlTokenReplacesStaleEmptyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "control.token")
	os.WriteFile(p, nil, 0o600)
	tok, err := ControlToken(dir)
	if err != nil || len(tok) < 32 {
		t.Fatalf("%q %v", tok, err)
	}
	if b, _ := os.ReadFile(p); string(b) != tok {
		t.Fatal("file does not hold the token")
	}
	if again, _ := ControlToken(dir); again != tok {
		t.Fatal("unstable")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatal(fi.Mode())
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("temp files left: %v", ents)
	}
}

func TestControlTokenConcurrentCallersAgree(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	toks := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); toks[i], errs[i] = ControlToken(dir) }()
	}
	wg.Wait()
	for i := range toks {
		if errs[i] != nil || toks[i] == "" || toks[i] != toks[0] {
			t.Fatalf("caller %d: %q %v (first %q)", i, toks[i], errs[i], toks[0])
		}
	}
}
