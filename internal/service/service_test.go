package service_test

import (
	"context"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sugarintegration/internal/service"
)

func TestLinuxUnit(t *testing.T) {
	u, err := service.Plan("linux", "/usr/local/bin/sugar-it", "/home/ann", "")
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/home/ann/.config/systemd/user/sugar-it.service" {
		t.Fatalf("path %q", u.Path)
	}
	if !strings.Contains(u.Content, `ExecStart="/usr/local/bin/sugar-it" daemon --foreground`) {
		t.Fatalf("content:\n%s", u.Content)
	}
	if !strings.Contains(u.Content, "WantedBy=default.target") {
		t.Fatalf("no install section:\n%s", u.Content)
	}
	if !hasCmd(u.Commands, "loginctl", "enable-linger") || !hasCmd(u.Commands, "systemctl", "--user", "enable") {
		t.Fatalf("commands %v", u.Commands)
	}
	u, _ = service.Plan("linux", "/x/sugar-it", "/home/ann", "/home/ann/cfg dir")
	if !strings.Contains(u.Content, `ExecStart="/x/sugar-it" daemon --foreground --config-dir "/home/ann/cfg dir"`) {
		t.Fatalf("config dir missing:\n%s", u.Content)
	}
}

func TestDarwinUnit(t *testing.T) {
	u, err := service.Plan("darwin", "/Applications/Sugar It/sugar-it", "/Users/ann", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.Path, "/Users/ann/Library/LaunchAgents/") || !strings.HasSuffix(u.Path, ".plist") {
		t.Fatalf("path %q", u.Path)
	}
	dec := xml.NewDecoder(strings.NewReader(u.Content))
	var strs []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("plist is not well-formed: %v\n%s", err, u.Content)
		}
		if cd, ok := tok.(xml.CharData); ok {
			if s := strings.TrimSpace(string(cd)); s != "" {
				strs = append(strs, s)
			}
		}
	}
	joined := strings.Join(strs, "|")
	if !strings.Contains(joined, "/Applications/Sugar It/sugar-it|daemon|--foreground") {
		t.Fatalf("program arguments missing: %s", joined)
	}
	if len(u.Commands) == 0 || u.Commands[0][0] != "launchctl" {
		t.Fatalf("commands %v", u.Commands)
	}
}

func TestPlistEscapesXML(t *testing.T) {
	u, err := service.Plan("darwin", "/a&b/<x>/sugar-it", "/Users/ann", "")
	if err != nil {
		t.Fatal(err)
	}
	dec := xml.NewDecoder(strings.NewReader(u.Content))
	for {
		if _, err := dec.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("not well-formed: %v", err)
		}
	}
}

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func TestWindowsUnit(t *testing.T) {
	u, err := service.Plan("windows", `C:\Program Files\Sugar It\sugar-it.exe`, `C:\Users\ann`, "")
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "" || len(u.Commands) != 1 {
		t.Fatalf("unit %+v", u)
	}
	want := []string{"reg", "add", runKey, "/v", "sugar-it", "/t", "REG_SZ", "/d", `"C:\Program Files\Sugar It\sugar-it.exe" daemon`, "/f"}
	if !slices.Equal(u.Commands[0], want) {
		t.Fatalf("command %q, want %q", u.Commands[0], want)
	}
	for _, c := range u.Commands {
		if c[0] == "schtasks" {
			t.Fatalf("a scheduled task needs elevation: %v", c)
		}
	}
	if !strings.Contains(strings.Join(u.Notes, "\n"), "next logon") {
		t.Fatalf("install must say it starts at the next logon: %v", u.Notes)
	}
}

// escapeArg is syscall.EscapeArg, which os/exec uses to build a Windows command line.
func escapeArg(s string) string {
	if len(s) == 0 {
		return `""`
	}
	n := len(s)
	hasSpace := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			n++
		case ' ', '\t':
			hasSpace = true
		}
	}
	if hasSpace {
		n += 2
	}
	if n == len(s) {
		return s
	}
	qs := make([]byte, 0, n)
	if hasSpace {
		qs = append(qs, '"')
	}
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			slashes++
			qs = append(qs, '\\')
		case c == '"':
			for ; slashes > 0; slashes-- {
				qs = append(qs, '\\')
			}
			qs = append(qs, '\\', '"')
		default:
			slashes = 0
			qs = append(qs, c)
		}
	}
	if hasSpace {
		for ; slashes > 0; slashes-- {
			qs = append(qs, '\\')
		}
		qs = append(qs, '"')
	}
	return string(qs)
}

// parseCmdLine splits a command line the way the Microsoft C runtime does,
// which is how reg.exe and a Run-key program see their arguments.
func parseCmdLine(s string) []string {
	var args []string
	i := 0
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			return args
		}
		var arg []byte
		inQuote := false
		for i < len(s) && (inQuote || (s[i] != ' ' && s[i] != '\t')) {
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				arg = append(arg, strings.Repeat(`\`, n/2)...)
				if n%2 == 1 {
					arg = append(arg, '"')
				} else {
					inQuote = !inQuote
				}
				i++
				continue
			}
			arg = append(arg, strings.Repeat(`\`, n)...)
			if i < len(s) {
				arg = append(arg, s[i])
				i++
			}
		}
		args = append(args, string(arg))
	}
}

// What reg receives and what Windows later runs must both come out as the
// intended words, including a path with spaces and a trailing backslash.
func TestWindowsRunValueSurvivesQuoting(t *testing.T) {
	for _, cfg := range []string{"", `C:\Users\ann\My Config`, `D:\my cfg\`, `E:\`} {
		exe := `C:\Program Files\sugar-it-desktop\sugar-it-desktop.exe`
		u, err := service.Plan("windows", exe, `C:\Users\ann`, cfg)
		if err != nil {
			t.Fatal(err)
		}
		var line []string
		for _, a := range u.Commands[0] {
			line = append(line, escapeArg(a))
		}
		got := parseCmdLine(strings.Join(line, " "))
		if !slices.Equal(got, u.Commands[0]) {
			t.Fatalf("reg receives %q, want %q (line %s)", got, u.Commands[0], strings.Join(line, " "))
		}
		value := got[slices.Index(got, "/d")+1]
		run := parseCmdLine(value)
		want := []string{exe, "daemon"}
		if cfg != "" {
			want = append(want, "--config-dir", cfg)
		}
		if !slices.Equal(run, want) {
			t.Fatalf("logon runs %q, want %q (value %s)", run, want, value)
		}
	}
}

func TestPathsWithSpacesQuoted(t *testing.T) {
	exe := "/opt/Sugar It/sugar-it"
	u, _ := service.Plan("linux", exe, "/home/a b", "/data/my cfg")
	if !strings.Contains(u.Content, `"/opt/Sugar It/sugar-it"`) || !strings.Contains(u.Content, `"/data/my cfg"`) {
		t.Fatalf("not quoted:\n%s", u.Content)
	}
	if u.Path != "/home/a b/.config/systemd/user/sugar-it.service" {
		t.Fatalf("path %q", u.Path)
	}
	// systemd expands % specifiers and $ variables even inside quotes.
	u, _ = service.Plan("linux", "/opt/50%/sugar-it", "/home/a", "")
	if !strings.Contains(u.Content, `"/opt/50%%/sugar-it"`) {
		t.Fatalf("%% not doubled:\n%s", u.Content)
	}
	u, _ = service.Plan("linux", `/opt/a"b/sugar-it`, "/home/a", "")
	if !strings.Contains(u.Content, `"/opt/a\"b/sugar-it"`) {
		t.Fatalf("quote not escaped:\n%s", u.Content)
	}
}

func TestUninstallPlans(t *testing.T) {
	u, err := service.UninstallPlan("linux", "/home/ann")
	if err != nil || u.Path != "/home/ann/.config/systemd/user/sugar-it.service" || !hasCmd(u.Commands, "systemctl", "--user", "disable") {
		t.Fatalf("%+v %v", u, err)
	}
	u, err = service.UninstallPlan("darwin", "/Users/ann")
	if err != nil || !strings.HasSuffix(u.Path, ".plist") || u.Commands[0][0] != "launchctl" {
		t.Fatalf("%+v %v", u, err)
	}
	u, err = service.UninstallPlan("windows", `C:\Users\ann`)
	if err != nil || len(u.Commands) != 1 || !slices.Equal(u.Commands[0], []string{"reg", "delete", runKey, "/v", "sugar-it", "/f"}) {
		t.Fatalf("%+v %v", u, err)
	}
	if len(u.Probe) == 0 || u.Probe[0] != "reg" || u.Probe[1] != "query" {
		t.Fatalf("a missing value must be detected first: %v", u.Probe)
	}
	if _, err := service.UninstallPlan("plan9", "/"); err == nil {
		t.Fatal("unsupported OS accepted")
	}
	if _, err := service.Plan("plan9", "/x", "/", ""); err == nil {
		t.Fatal("unsupported OS accepted")
	}
	if _, err := service.Plan("linux", "", "/", ""); err == nil {
		t.Fatal("empty exe accepted")
	}
}

type recorder struct {
	cmds    [][]string
	failing string
}

func (r *recorder) Run(_ context.Context, argv []string, _ io.Writer) error {
	r.cmds = append(r.cmds, argv)
	if argv[0] == r.failing {
		return os.ErrInvalid
	}
	return nil
}

func TestInstallWritesFileThenRunsCommands(t *testing.T) {
	home := t.TempDir()
	u, _ := service.Plan("linux", "/x/sugar-it", home, "")
	r := &recorder{}
	if err := service.InstallWith(context.Background(), r, u, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(u.Path)
	if err != nil || string(b) != u.Content {
		t.Fatalf("unit file: %v", err)
	}
	if len(r.cmds) != len(u.Commands) {
		t.Fatalf("ran %v", r.cmds)
	}
	un, _ := service.UninstallPlan("linux", home)
	r2 := &recorder{failing: "systemctl"}
	if err := service.UninstallWith(context.Background(), r2, un, io.Discard); err == nil {
		t.Fatal("a failing command must be reported")
	}
	if _, err := os.Stat(filepath.Join(u.Path)); !os.IsNotExist(err) {
		t.Fatal("the unit file must be removed even when a command fails")
	}
}

func hasCmd(cmds [][]string, words ...string) bool {
	for _, c := range cmds {
		if hasAdjacent(c, words...) {
			return true
		}
	}
	return false
}

// hasAdjacent reports whether every word appears in c, in order.
func hasAdjacent(c []string, words ...string) bool {
	i := 0
	for _, a := range c {
		if i < len(words) && a == words[i] {
			i++
		}
	}
	return i == len(words)
}

// systemd and launchd need a foreground process; the Windows Run key value
// starts the detaching form so no console stays. The desktop binary only leaves GUI mode when daemon is its first argument.
func TestDaemonIsTheFirstArgument(t *testing.T) {
	u, err := service.Plan("darwin", "/a/sugar-it", "/Users/ann", "/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`<string>/a/sugar-it</string>\s*<string>daemon</string>\s*<string>--foreground</string>\s*<string>--config-dir</string>\s*<string>/cfg</string>`).MatchString(u.Content) {
		t.Errorf("darwin arguments:\n%s", u.Content)
	}
	u, err = service.Plan("windows", `C:\a\sugar-it.exe`, `C:\Users\ann`, `C:\cfg`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"C:\a\sugar-it.exe" daemon --config-dir "C:\cfg"`; !slices.Contains(u.Commands[0], want) {
		t.Errorf("windows command %v, want %s", u.Commands[0], want)
	}
}

func TestWindowsUninstallOfMissingValueIsFine(t *testing.T) {
	u, _ := service.UninstallPlan("windows", `C:\Users\ann`)
	r := &recorder{failing: "reg"} // the probe fails: nothing is registered
	var out strings.Builder
	if err := service.UninstallWith(context.Background(), r, u, &out); err != nil {
		t.Fatalf("already uninstalled must not fail: %v", err)
	}
	for _, c := range r.cmds {
		if c[1] == "delete" {
			t.Fatalf("delete ran for a missing value: %v", r.cmds)
		}
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Fatalf("output %q", out.String())
	}
	// With the value present the delete runs, and its failure is reported.
	r2 := &failOn{sub: "delete"}
	if err := service.UninstallWith(context.Background(), r2, u, io.Discard); err == nil {
		t.Fatal("a failing delete must be reported")
	}
}

type failOn struct{ sub string }

func (f *failOn) Run(_ context.Context, argv []string, _ io.Writer) error {
	if len(argv) > 1 && argv[1] == f.sub {
		return os.ErrInvalid
	}
	return nil
}

func TestInstallPrintsNotes(t *testing.T) {
	u, _ := service.Plan("windows", `C:\a\sugar-it.exe`, `C:\Users\ann`, "")
	var out strings.Builder
	if err := service.InstallWith(context.Background(), &recorder{}, u, &out); err != nil || !strings.Contains(out.String(), "next logon") {
		t.Fatalf("%v %q", err, out.String())
	}
}
