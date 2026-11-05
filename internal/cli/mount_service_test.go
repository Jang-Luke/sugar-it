package cli_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"sugarintegration/internal/cli"
	"sugarintegration/internal/control"
	"sugarintegration/internal/daemon"
)

type rec struct {
	calls [][]string
	stdin []string
}

func (r *rec) Run(_ context.Context, argv []string, stdin string, _ io.Writer) error {
	r.calls = append(r.calls, argv)
	r.stdin = append(r.stdin, stdin)
	return nil
}

type svcRec struct{ calls [][]string }

func (r *svcRec) Run(_ context.Context, argv []string, _ io.Writer) error {
	r.calls = append(r.calls, argv)
	return nil
}

func TestMountCommandKeepsTokenOutOfArgv(t *testing.T) {
	h := newHost(t, "mt")
	c, err := control.Discover(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &rec{}
	var out, errb bytes.Buffer
	code := cli.ExecuteWithRunners([]string{"--config-dir", h.dir, "mount"}, &out, &errb, r, &svcRec{})
	if runtime.GOOS == "windows" {
		return // WebClient settings decide; covered by the mount package tests
	}
	if code != 0 || len(r.calls) != 1 {
		t.Fatalf("exit %d calls %v stderr %q", code, r.calls, errb.String())
	}
	for _, a := range r.calls[0] {
		if strings.Contains(a, c.Token()) {
			t.Fatalf("token in argv %v", r.calls[0])
		}
	}
	if !strings.Contains(strings.Join(r.calls[0], " "), c.DAVAddr()) {
		t.Fatalf("dav address missing: %v", r.calls[0])
	}
	if strings.Contains(errb.String(), c.Token()) {
		t.Fatal("token on stderr")
	}
	// Only a terminal-prompt OS shows the token, and only on stdout.
	if (r.stdin[0] == "") != strings.Contains(out.String(), c.Token()) {
		t.Fatalf("stdin %q, stdout %q", r.stdin[0], out.String())
	}
}

func TestMountRejectsBadDrive(t *testing.T) {
	h := newHost(t, "md")
	r := &rec{}
	var out, errb bytes.Buffer
	code := cli.ExecuteWithRunners([]string{"--config-dir", h.dir, "mount", "--drive", "Z:;del"}, &out, &errb, r, &svcRec{})
	if code != 1 || len(r.calls) != 0 || !strings.Contains(errb.String(), "invalid drive") {
		t.Fatalf("exit %d calls %v stderr %q", code, r.calls, errb.String())
	}
}

func TestMountWithoutDaemon(t *testing.T) {
	var out, errb bytes.Buffer
	r := &rec{}
	code := cli.ExecuteWithRunners([]string{"--config-dir", t.TempDir(), "mount"}, &out, &errb, r, &svcRec{})
	if code != 1 || len(r.calls) != 0 || !strings.Contains(errb.String(), "no sugar-it daemon") {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}

func TestServiceInstallAndUninstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME is not the home directory on Windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := &svcRec{}
	var out, errb bytes.Buffer
	if code := cli.ExecuteWithRunners([]string{"--config-dir", "rel-cfg", "service", "install"}, &out, &errb, &rec{}, s); code != 0 {
		t.Fatalf("install exit %d: %s", code, errb.String())
	}
	if len(s.calls) == 0 {
		t.Fatal("no command ran")
	}
	var unit string
	switch runtime.GOOS {
	case "linux":
		unit = home + "/.config/systemd/user/sugar-it.service"
	case "darwin":
		unit = home + "/Library/LaunchAgents/io.sugar-it.daemon.plist"
	}
	b, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"rel-cfg"`) || !strings.Contains(string(b), "rel-cfg") {
		t.Fatalf("config dir must be written absolute:\n%s", b)
	}
	s.calls = nil
	if code := cli.ExecuteWithRunners([]string{"service", "uninstall"}, &out, &errb, &rec{}, s); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) || len(s.calls) == 0 {
		t.Fatalf("unit still there (%v) or no command ran %v", err, s.calls)
	}
}

func TestWindowsUninstallStopsTheDaemon(t *testing.T) {
	defer cli.SetServiceOS("windows")()
	h := newHost(t, "wu")
	s := &svcRec{}
	var out, errb bytes.Buffer
	if code := cli.ExecuteWithRunners([]string{"--config-dir", h.dir, "service", "uninstall"}, &out, &errb, &rec{}, s); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Stopped the running daemon") {
		t.Fatalf("output %q", out.String())
	}
	if _, err := control.Discover(h.dir); err == nil {
		t.Fatal("the daemon still answers")
	}
}

func TestWindowsUninstallWithoutDaemon(t *testing.T) {
	defer cli.SetServiceOS("windows")()
	var out, errb bytes.Buffer
	if code := cli.ExecuteWithRunners([]string{"--config-dir", t.TempDir(), "service", "uninstall"}, &out, &errb, &rec{}, &svcRec{}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "nothing to stop") {
		t.Fatalf("output %q", out.String())
	}
}

func TestWindowsUninstallLeavesAnInProcessDaemon(t *testing.T) {
	defer cli.SetServiceOS("windows")()
	h := newConfig(t, "wi")
	d, err := daemon.Start(bg, daemon.Options{ConfigDir: h.dir, InProcess: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Shutdown(bg)
	var out, errb bytes.Buffer
	if code := cli.ExecuteWithRunners([]string{"--config-dir", h.dir, "service", "uninstall"}, &out, &errb, &rec{}, &svcRec{}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "when the app quits") {
		t.Fatalf("output %q", out.String())
	}
	if _, err := control.Discover(h.dir); err != nil {
		t.Fatalf("the in-process daemon must keep running: %v", err)
	}
}
