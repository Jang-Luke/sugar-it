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
	if !strings.Contains(u.Content, `ExecStart="/usr/local/bin/sugar-it" daemon`) {
		t.Fatalf("content:\n%s", u.Content)
	}
	if !strings.Contains(u.Content, "WantedBy=default.target") {
		t.Fatalf("no install section:\n%s", u.Content)
	}
	if !hasCmd(u.Commands, "loginctl", "enable-linger") || !hasCmd(u.Commands, "systemctl", "--user", "enable") {
		t.Fatalf("commands %v", u.Commands)
	}
	u, _ = service.Plan("linux", "/x/sugar-it", "/home/ann", "/home/ann/cfg dir")
	if !strings.Contains(u.Content, `ExecStart="/x/sugar-it" daemon --config-dir "/home/ann/cfg dir"`) {
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
	if !strings.Contains(joined, "/Applications/Sugar It/sugar-it|daemon") {
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

func TestWindowsUnit(t *testing.T) {
	u, err := service.Plan("windows", `C:\Program Files\Sugar It\sugar-it.exe`, `C:\Users\ann`, "")
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "" || len(u.Commands) == 0 {
		t.Fatalf("unit %+v", u)
	}
	c := u.Commands[0]
	if c[0] != "schtasks" || !hasAdjacent(c, "/Create") || !hasAdjacent(c, "/SC", "ONLOGON") {
		t.Fatalf("command %v", c)
	}
	var tr string
	for i, a := range c {
		if a == "/TR" {
			tr = c[i+1]
		}
	}
	if tr != `"C:\Program Files\Sugar It\sugar-it.exe" daemon` {
		t.Fatalf("/TR %q", tr)
	}
	for _, a := range c {
		if a == "SYSTEM" || a == "/RU" {
			t.Fatalf("task must run as the user: %v", c)
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
	if err != nil || !hasCmd(u.Commands, "schtasks", "/Delete") {
		t.Fatalf("%+v %v", u, err)
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

// The desktop binary only leaves GUI mode when daemon is its first argument.
func TestDaemonIsTheFirstArgument(t *testing.T) {
	u, err := service.Plan("darwin", "/a/sugar-it", "/Users/ann", "/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`<string>/a/sugar-it</string>\s*<string>daemon</string>\s*<string>--config-dir</string>\s*<string>/cfg</string>`).MatchString(u.Content) {
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
