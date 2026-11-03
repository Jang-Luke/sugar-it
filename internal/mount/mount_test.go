package mount_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"sugarintegration/internal/mount"
)

const (
	dav   = "127.0.0.1:7445"
	token = "s3cr3t-token-value"
)

func TestPlanLinux(t *testing.T) {
	p, err := mount.MountPlan("linux", dav, token, "", mount.WinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Argv) < 3 || p.Argv[0] != "gio" || p.Argv[1] != "mount" || p.Argv[len(p.Argv)-1] != "dav://sugar-it@127.0.0.1:7445/" {
		t.Fatalf("argv %v", p.Argv)
	}
	if !strings.Contains(p.Stdin, token) {
		t.Fatalf("token must go through stdin, got %q", p.Stdin)
	}
}

func TestPlanDarwin(t *testing.T) {
	p, err := mount.MountPlan("darwin", dav, token, "", mount.WinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Argv[0] != "osascript" || !strings.Contains(strings.Join(p.Argv, " "), "mount volume") || !strings.Contains(strings.Join(p.Argv, " "), "http://127.0.0.1:7445/") {
		t.Fatalf("argv %v", p.Argv)
	}
	if p.Stdin != "" {
		t.Fatalf("stdin %q", p.Stdin)
	}
	if !strings.Contains(strings.Join(p.Notes, "\n"), "sugar-it") {
		t.Fatalf("notes must name the user: %v", p.Notes)
	}
}

func TestPlanWindowsPrompt(t *testing.T) {
	ok := mount.WinLimits{Known: true, BasicAuthLevel: 2, FileSizeLimit: 4294967295}
	p, err := mount.MountPlan("windows", dav, token, "Z:", ok)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"net", "use", "Z:", "http://127.0.0.1:7445/", "/user:sugar-it", "*"}
	if strings.Join(p.Argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv %v, want %v", p.Argv, want)
	}
	p, err = mount.MountPlan("windows", dav, token, "", ok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Argv[2] != "*" || p.Argv[len(p.Argv)-1] != "*" {
		t.Fatalf("auto drive: %v", p.Argv)
	}
}

func TestTokenNeverInArgv(t *testing.T) {
	ok := mount.WinLimits{Known: true, BasicAuthLevel: 2, FileSizeLimit: 4294967295}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		p, err := mount.MountPlan(goos, dav, token, "Z:", ok)
		if err != nil {
			t.Fatal(goos, err)
		}
		for _, a := range p.Argv {
			if strings.Contains(a, token) {
				t.Errorf("%s: token in argv %v", goos, p.Argv)
			}
		}
		for _, n := range p.Notes {
			if strings.Contains(n, token) {
				t.Errorf("%s: token in notes", goos)
			}
		}
	}
}

func TestWindowsLimitsGuidance(t *testing.T) {
	_, err := mount.MountPlan("windows", dav, token, "Z:", mount.WinLimits{Known: true, BasicAuthLevel: 1, FileSizeLimit: 4294967295})
	if err == nil || !strings.Contains(err.Error(), "reg add") || !strings.Contains(err.Error(), "BasicAuthLevel") {
		t.Fatalf("want reg add guidance, got %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("token in error")
	}
	p, err := mount.MountPlan("windows", dav, token, "Z:", mount.WinLimits{Known: true, BasicAuthLevel: 2, FileSizeLimit: 50000000})
	if err != nil {
		t.Fatalf("a small size limit must not block: %v", err)
	}
	if !strings.Contains(strings.Join(p.Notes, "\n"), "FileSizeLimitInBytes") {
		t.Fatalf("want a size warning: %v", p.Notes)
	}
	p, err = mount.MountPlan("windows", dav, token, "Z:", mount.WinLimits{})
	if err != nil || len(p.Notes) == 0 {
		t.Fatalf("unknown limits still mount, with a note: %v %v", p.Notes, err)
	}
}

func TestInvalidDriveRejected(t *testing.T) {
	for _, d := range []string{"Z:;del", "ZZ:", "Z", "1:", ":", "Z:\\", "Z: ", "*", "Z:/x"} {
		for _, goos := range []string{"linux", "darwin", "windows"} {
			if _, err := mount.MountPlan(goos, dav, token, d, mount.WinLimits{Known: true, BasicAuthLevel: 2}); err == nil {
				t.Errorf("%s: drive %q accepted", goos, d)
			}
		}
		if _, err := mount.UnmountPlan("windows", dav, d); err == nil {
			t.Errorf("unmount: drive %q accepted", d)
		}
	}
	if _, err := mount.MountPlan("windows", dav, token, "z:", mount.WinLimits{Known: true, BasicAuthLevel: 2}); err != nil {
		t.Errorf("lowercase drive: %v", err)
	}
}

func TestBadAddressRejected(t *testing.T) {
	for _, a := range []string{"", "no-port", "127.0.0.1:7445/x y", "a b:1"} {
		if _, err := mount.MountPlan("linux", a, token, "", mount.WinLimits{}); err == nil {
			t.Errorf("address %q accepted", a)
		}
	}
	if _, err := mount.MountPlan("plan9", dav, token, "", mount.WinLimits{}); err == nil {
		t.Error("unsupported OS accepted")
	}
}

func TestUnmountPlan(t *testing.T) {
	p, err := mount.UnmountPlan("linux", dav, "")
	if err != nil || strings.Join(p.Argv, " ") != "gio mount -u dav://sugar-it@127.0.0.1:7445/" {
		t.Fatalf("%v %v", p.Argv, err)
	}
	p, err = mount.UnmountPlan("windows", dav, "Z:")
	if err != nil || strings.Join(p.Argv, " ") != "net use Z: /delete" {
		t.Fatalf("%v %v", p.Argv, err)
	}
	if _, err := mount.UnmountPlan("windows", dav, ""); err == nil {
		t.Fatal("windows unmount needs a drive")
	}
	if p, err := mount.UnmountPlan("darwin", dav, ""); err != nil || len(p.Argv) == 0 {
		t.Fatalf("%v %v", p, err)
	}
}

type fakeRunner struct {
	argv  []string
	stdin string
	err   error
}

func (f *fakeRunner) Run(_ context.Context, argv []string, stdin string, _ io.Writer) error {
	f.argv, f.stdin = argv, stdin
	return f.err
}

func TestRunUsesRunnerAndPrintsNotes(t *testing.T) {
	p, _ := mount.MountPlan("linux", dav, token, "", mount.WinLimits{})
	p.Notes = []string{"hello note"}
	f := &fakeRunner{}
	var out strings.Builder
	if err := mount.RunWith(context.Background(), f, p, &out); err != nil {
		t.Fatal(err)
	}
	if f.stdin != p.Stdin || strings.Join(f.argv, " ") != strings.Join(p.Argv, " ") {
		t.Fatalf("runner got %v %q", f.argv, f.stdin)
	}
	if !strings.Contains(out.String(), "hello note") || strings.Contains(out.String(), token) {
		t.Fatalf("output %q", out.String())
	}
}
