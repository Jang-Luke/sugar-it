package detach

import (
	"os/exec"
	"testing"
)

func TestStartRetriesWithoutBreakaway(t *testing.T) {
	var made int
	_, err := Start(func() *exec.Cmd {
		made++
		return exec.Command("/no/such/program")
	})
	if err == nil {
		t.Fatal("started a missing program")
	}
	if made != 2 {
		t.Errorf("made %d commands, want a first try and a retry", made)
	}
}

func TestStartRunsTheCommand(t *testing.T) {
	sh, err := exec.LookPath("go")
	if err != nil {
		t.Skip(err)
	}
	for name, start := range map[string]func(func() *exec.Cmd) (*exec.Cmd, error){"Start": Start, "StartGUI": StartGUI} {
		cmd, err := start(func() *exec.Cmd { return exec.Command(sh, "version") })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
