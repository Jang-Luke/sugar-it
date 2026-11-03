package mount_test

import (
	"context"
	"errors"
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

func TestPlanWindowsConnect(t *testing.T) {
	ok := mount.WinLimits{Known: true, BasicAuthLevel: 2, FileSizeLimit: 4294967295}
	p, err := mount.MountPlan("windows", dav, token, "Z:", ok)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Argv) != 0 || p.Stdin != "" {
		t.Fatalf("windows runs no command: %v %q", p.Argv, p.Stdin)
	}
	w := p.Windows
	if w == nil || w.RemoteName != `\\127.0.0.1@7445\DavWWWRoot` || w.User != "sugar-it" || w.Drive != "Z:" {
		t.Fatalf("connect %+v", w)
	}
	p, err = mount.MountPlan("windows", dav, token, "", ok)
	if err != nil || p.Windows == nil || p.Windows.Drive != "" {
		t.Fatalf("auto drive: %+v %v", p.Windows, err)
	}
	if _, err := mount.MountPlan("windows", "[::1]:7445", token, "", ok); err == nil {
		t.Fatal("an IPv6 literal has no WebDAV UNC form")
	}
}

func TestConnectPassesArgsAndReturnsDrive(t *testing.T) {
	ok := mount.WinLimits{Known: true, BasicAuthLevel: 2, FileSizeLimit: 4294967295}
	p, _ := mount.MountPlan("windows", dav, token, "", ok)
	var got mount.ConnectArgs
	conn := func(a mount.ConnectArgs) (string, error) { got = a; return "Y:", nil }
	var out strings.Builder
	drive, err := mount.RunMount(context.Background(), &fakeRunner{}, conn, p, token, &out)
	if err != nil || drive != "Y:" {
		t.Fatalf("%q %v", drive, err)
	}
	if got.Password != token || got.User != "sugar-it" || got.RemoteName != p.Windows.RemoteName || got.Drive != "" {
		t.Fatalf("args %+v", got)
	}
	if !strings.Contains(out.String(), "Y:") || strings.Contains(out.String(), token) {
		t.Fatalf("output %q", out.String())
	}
}

func TestConnectErrorsNeverCarryToken(t *testing.T) {
	ok := mount.WinLimits{Known: true, BasicAuthLevel: 2, FileSizeLimit: 4294967295}
	p, _ := mount.MountPlan("windows", dav, token, "Z:", ok)
	conn := func(a mount.ConnectArgs) (string, error) { return "", errors.New("provider said " + a.Password) }
	var out strings.Builder
	_, err := mount.RunMount(context.Background(), &fakeRunner{}, conn, p, token, &out)
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(out.String(), token) {
		t.Fatalf("err %v out %q", err, out.String())
	}
}

func TestWinErrorMessages(t *testing.T) {
	cases := map[uint32]string{
		85:   "already in use",
		5:    "credentials",
		86:   "credentials",
		1326: "credentials",
		1244: "logon session",
		53:   "WebClient",
		67:   "WebClient",
		1203: "WebClient",
		1058: "WebClient",
		1068: "WebClient",
		259:  "free drive letter",
		1219: "different credentials",
	}
	for code, want := range cases {
		err := mount.WinError(code)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("code %d: %v, want %q", code, err, want)
		}
	}
	if err := mount.WinError(9999); err == nil || !strings.Contains(err.Error(), "9999") {
		t.Errorf("unknown code: %v", err)
	}
}

func TestBasicAuthBlockedError(t *testing.T) {
	_, err := mount.MountPlan("windows", dav, token, "Z:", mount.WinLimits{Known: true, BasicAuthLevel: 1, FileSizeLimit: 4294967295})
	var be *mount.BasicAuthBlockedError
	if !errors.As(err, &be) || be.Level != 1 || len(be.Commands) != 2 || !strings.HasPrefix(be.Commands[0], "reg add ") {
		t.Fatalf("%#v", err)
	}
	if !strings.Contains(err.Error(), "reg add") {
		t.Fatalf("CLI text must keep the commands: %v", err)
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
	if err != nil || strings.Join(p.Argv, " ") != "net use Z: /delete /y" {
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
