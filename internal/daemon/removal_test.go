package daemon_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sugarintegration/internal/config"
	"sugarintegration/internal/control"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

const (
	removalFiles    = 30
	removalFileSize = 8 << 20
)

// sparseTree makes the files of a job that takes long enough to act on.
func sparseTree(t *testing.T, dir string) {
	t.Helper()
	for i := range removalFiles {
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("f%02d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(removalFileSize); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}

func regularFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && !strings.Contains(p, vfs.PartDir) {
			n++
		}
		return nil
	})
	return n
}

func waitJob(t *testing.T, c *control.Client, j transfer.Job, what string, f func(transfer.Job) bool) transfer.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		got, err := c.Job(bg, j.Host, j.ID)
		if err == nil && f(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; last %+v, %v", what, got, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func started(j transfer.Job) bool {
	return j.DoneFiles >= 2 || j.State == transfer.StateDone
}

func terminal(j transfer.Job) bool {
	switch j.State {
	case transfer.StateDone, transfer.StatePartial, transfer.StateFailed, transfer.StateCanceled:
		return true
	}
	return false
}

func TestRemovingSourceHostFailsRunningMove(t *testing.T) {
	_, _, adir, bdir, _, _ := twoDaemons(t)
	src := addShare(t, bdir, "s", nil)
	sparseTree(t, src)
	dst := addShare(t, adir, "d", nil)
	ac, _ := control.Discover(adir)
	bc, _ := control.Discover(bdir)

	j, err := ac.StartJob(bg, control.TransferRequest{Op: "move", Src: "b:s/", Dst: "local:d/out/"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, ac, j, "progress", started)
	if err := ac.RemovePeer(bg, "b"); err != nil {
		t.Fatal(err)
	}
	got := waitJob(t, ac, j, "the job to end", terminal)
	if got.State != transfer.StateFailed || got.Error == nil || got.Error.Code != vfs.CodePeerNotFound {
		t.Fatalf("job ended %s (%+v) after %d/%d files, want failed with peer_not_found", got.State, got.Error, got.DoneFiles, got.TotalFiles)
	}
	left := regularFiles(t, src)
	if left == 0 {
		t.Fatal("every source on the removed host was deleted")
	}
	moved := regularFiles(t, dst)
	if left+moved != removalFiles {
		t.Fatalf("%d left and %d moved of %d", left, moved, removalFiles)
	}

	// Retryable once paired again.
	inv, err := ac.Invite(bg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bc.JoinPeer(bg, inv); err != nil {
		t.Fatal(err)
	}
	if err := ac.RetryJob(bg, j.Host, j.ID); err != nil {
		t.Fatal(err)
	}
	got = waitJob(t, ac, j, "the retry to end", terminal)
	if got.State != transfer.StateDone || regularFiles(t, dst) != removalFiles || regularFiles(t, src) != 0 {
		t.Fatalf("after retry: %s (%+v), %d at the destination, %d at the source", got.State, got.Error, regularFiles(t, dst), regularFiles(t, src))
	}
}

// originJob starts on a, at b's request, a move between two of a's shares.
func originJob(t *testing.T, adir, bdir string) (j transfer.Job, ac *control.Client, src, dst string) {
	t.Helper()
	src = addShare(t, adir, "s", nil)
	sparseTree(t, src)
	dst = addShare(t, adir, "d", nil)
	ac, _ = control.Discover(adir)
	bc, _ := control.Discover(bdir)
	j, err := bc.StartJob(bg, control.TransferRequest{Op: "move", Src: "a:s/", Dst: "a:d/out/"})
	if err != nil {
		t.Fatal(err)
	}
	return j, ac, src, dst
}

func noParts(t *testing.T, share string) {
	t.Helper()
	parts, _ := filepath.Glob(filepath.Join(share, vfs.PartDir, "*"))
	if len(parts) != 0 {
		t.Errorf("parts left: %v", parts)
	}
}

func TestRemovingOriginHostCancelsJob(t *testing.T) {
	_, _, adir, bdir, _, _ := twoDaemons(t)
	j, ac, src, dst := originJob(t, adir, bdir)
	waitJob(t, ac, j, "progress", started)
	if err := ac.RemovePeer(bg, "b"); err != nil {
		t.Fatal(err)
	}
	got := waitJob(t, ac, j, "the job to end", terminal)
	if got.State != transfer.StateCanceled {
		t.Fatalf("job ended %s after %d/%d files, want canceled", got.State, got.DoneFiles, got.TotalFiles)
	}
	left, moved := regularFiles(t, src), regularFiles(t, dst)
	if left == 0 || moved == 0 || left+moved != removalFiles {
		t.Fatalf("%d left and %d moved of %d", left, moved, removalFiles)
	}
	noParts(t, dst)
}

// markRemoved records in a stopped host's config that it removed id.
func markRemoved(t *testing.T, dir, id string) {
	t.Helper()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		for i := range c.Members {
			if c.Members[i].ID == id {
				c.Members[i].Removed = true
				c.Members[i].Epoch++
				c.Members[i].Generation++
				return nil
			}
		}
		t.Fatalf("no member %s", id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRestartCancelsJobOfRemovedOrigin(t *testing.T) {
	a, b, adir, bdir, ao, _ := twoDaemons(t)
	j, ac, src, dst := originJob(t, adir, bdir)
	waitJob(t, ac, j, "progress", started)
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	markRemoved(t, adir, b.Node().ID())
	start(t, ao)
	ac, err := control.Discover(adir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ac.Job(bg, j.Host, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == transfer.StateDone {
		t.Skip("the job finished before the shutdown")
	}
	if got.State != transfer.StateCanceled {
		t.Fatalf("the job is %s after the restart, want canceled", got.State)
	}
	if left := regularFiles(t, src); left == 0 || left+regularFiles(t, dst) != removalFiles {
		t.Fatalf("%d left, %d moved", left, regularFiles(t, dst))
	}
	noParts(t, dst)
}

func TestRestartFailsJobFromRemovedSource(t *testing.T) {
	a, b, adir, bdir, ao, _ := twoDaemons(t)
	src := addShare(t, bdir, "s", nil)
	sparseTree(t, src)
	addShare(t, adir, "d", nil)
	ac, _ := control.Discover(adir)
	j, err := ac.StartJob(bg, control.TransferRequest{Op: "move", Src: "b:s/", Dst: "local:d/out/"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, ac, j, "progress", started)
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	markRemoved(t, adir, b.Node().ID())
	start(t, ao)
	ac, err = control.Discover(adir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ac.Job(bg, j.Host, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == transfer.StateDone {
		t.Skip("the job finished before the shutdown")
	}
	if got.State != transfer.StateFailed || got.Error == nil || got.Error.Code != vfs.CodePeerNotFound {
		t.Fatalf("the job is %s (%+v) after the restart, want failed with peer_not_found", got.State, got.Error)
	}
	if regularFiles(t, src) == 0 {
		t.Fatal("sources gone")
	}
}
