package peerapi

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"sugarintegration/internal/vfs"
)

// A member's removal must stop what it already has in flight, not only its
// next request.

const revokeWait = 5 * time.Second

func sparse(t *testing.T, dir, name string, size int64) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeStopsDownload(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	const size = 512 << 20
	sparse(t, dir, "big", size)
	e.share("s", l, false)
	rc, _, err := e.c.FS("s").Open(bg, "big", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, 1<<20)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatal(err)
	}
	e.b.revoke(e.cliID.ID)
	var n int64
	for err == nil {
		var m int
		m, err = rc.Read(buf)
		n += int64(m)
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("the removed member read the file to EOF (%d more bytes)", n)
	}
	if n >= size/2 {
		t.Errorf("the removed member read %d more bytes", n)
	}
	// And the same connection is refused from now on.
	_, err = e.c.FS("s").List(bg, "")
	wantCode(t, err, vfs.CodePeerUnauthorized)
}

type closeWatch struct {
	io.ReadCloser
	closed chan struct{}
	once   atomic.Bool
}

func (c *closeWatch) Close() error {
	if c.once.CompareAndSwap(false, true) {
		close(c.closed)
	}
	return c.ReadCloser.Close()
}

// A client that stopped reading blocks the server in Write, where no context
// is looked at.
func TestRevokeStopsStalledDownload(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	sparse(t, dir, "big", 512<<20)
	closed := make(chan struct{})
	e.share("s", &hookFS{FS: l, open: func(ctx context.Context, p string, off int64, etag string) (io.ReadCloser, vfs.Entry, error) {
		rc, ent, err := l.Open(ctx, p, off, etag)
		if err != nil {
			return nil, ent, err
		}
		return &closeWatch{ReadCloser: rc, closed: closed}, ent, nil
	}}, false)
	rc, _, err := e.c.FS("s").Open(bg, "big", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	time.Sleep(100 * time.Millisecond) // the flow-control window fills up
	e.b.revoke(e.cliID.ID)
	select {
	case <-closed:
	case <-time.After(revokeWait):
		t.Fatal("the server is still sending to the removed member")
	}
}

func TestRevokeStopsUpload(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	e.share("s", l, false)
	pr, pw := io.Pipe()
	defer pw.Close()
	// Small enough that no flow control is involved: the sender always ends up
	// waiting in the source's Read, with no error.
	go pw.Write(make([]byte, 64<<10))
	done := make(chan error, 1)
	go func() {
		_, err := e.c.FS("s").Put(bg, "up", pr, vfs.PutOptions{})
		done <- err
	}()
	eventuallyPart(t, dir)
	e.b.revoke(e.cliID.ID)
	select {
	case err := <-done:
		wantCode(t, err, vfs.CodePeerUnauthorized)
	case <-time.After(revokeWait):
		t.Fatal("the removed member's upload has not ended")
	}
	if _, err := os.Stat(filepath.Join(dir, "up")); !os.IsNotExist(err) {
		t.Errorf("destination: %v", err)
	}
	waitNoTemp(t, dir)
}

// The same stalled source when the peer goes away instead of answering.
func TestPutReturnsWhenPeerGoesWithStalledSource(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	e.share("s", l, false)
	pr, pw := io.Pipe()
	defer pw.Close()
	go pw.Write(make([]byte, 64<<10))
	done := make(chan error, 1)
	go func() {
		_, err := e.c.FS("s").Put(bg, "up", pr, vfs.PutOptions{})
		done <- err
	}()
	eventuallyPart(t, dir)
	e.srv.Close()
	select {
	case err := <-done:
		wantCode(t, err, vfs.CodePeerOffline)
	case <-time.After(revokeWait):
		t.Fatal("Put waited for its source after the connection was lost")
	}
}

func partFiles(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, vfs.PartDir, "put-*"))
	return m
}

func eventuallyPart(t *testing.T, dir string) {
	t.Helper()
	for end := time.Now().Add(revokeWait); len(partFiles(dir)) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the upload never started")
		}
	}
}

func waitNoTemp(t *testing.T, dir string) {
	t.Helper()
	for end := time.Now().Add(revokeWait); len(partFiles(dir)) != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("temporary files left: %v", partFiles(dir))
		}
	}
}

func TestRevokeStopsHash(t *testing.T) {
	e := start(t, "linux")
	l, _ := localShare(t, false)
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	e.share("s", &hookFS{FS: l, hash: func(ctx context.Context, p string) (string, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return "", vfs.E(vfs.CodeCanceled, "canceled")
		case <-release:
			return "sum", nil
		}
	}}, false)
	done := make(chan error, 1)
	go func() {
		_, err := e.c.FS("s").Hash(bg, "f")
		done <- err
	}()
	<-entered
	e.b.revoke(e.cliID.ID)
	select {
	case err := <-done:
		wantCode(t, err, vfs.CodePeerUnauthorized)
	case <-time.After(revokeWait):
		t.Fatal("the server is still hashing for the removed member")
	}
}

// Only the removed member is affected.
func TestRevokeLeavesOtherMembersAlone(t *testing.T) {
	e := start(t, "linux")
	other := newID(t)
	e.b.setMember(other.ID, true)
	oc := NewClient(other, e.srvID.ID, []string{e.addr})
	l, dir := localShare(t, false)
	const size = 8 << 20
	sparse(t, dir, "big", size)
	e.share("s", l, false)
	rc, _, err := oc.FS("s").Open(bg, "big", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	e.b.revoke(e.cliID.ID)
	n, err := io.Copy(io.Discard, rc)
	if err != nil || n != size {
		t.Fatalf("read %d, %v", n, err)
	}
}
