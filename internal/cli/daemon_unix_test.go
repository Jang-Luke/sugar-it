//go:build unix

package cli_test

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"sugarintegration/internal/cli"
	"sugarintegration/internal/config"
	"sugarintegration/internal/control"
)

func TestDaemonCommand(t *testing.T) {
	h := newConfig(t, "dm")
	adv := freeAddr(t)
	done := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() {
		done <- cli.Execute([]string{"--config-dir", h.dir, "daemon", "--advertise", adv,
			"--listen", h.peer, "--control", freeAddr(t), "--dav", freeAddr(t)}, &out, &errb)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := control.Discover(h.dir); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never came up: %s", errb.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// A second daemon on the same config dir is refused.
	var o2, e2 bytes.Buffer
	if code := cli.Execute([]string{"--config-dir", h.dir, "daemon", "--control", freeAddr(t),
		"--listen", freeAddr(t), "--dav", freeAddr(t)}, &o2, &e2); code != 1 || !strings.Contains(e2.String(), "already running") {
		t.Errorf("second daemon: exit %d stderr %q", code, e2.String())
	}
	syscall.Kill(os.Getpid(), syscall.SIGTERM) // Execute turns it into a graceful shutdown
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("daemon exit %d: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not stop on SIGTERM")
	}
	cfg, err := config.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Get().AdvertiseAddrs; len(got) != 1 || got[0] != adv {
		t.Fatalf("advertise not persisted: %v", got)
	}
}
