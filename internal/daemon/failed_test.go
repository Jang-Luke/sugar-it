package daemon

import (
	"context"
	"testing"
	"time"

	"sugarintegration/internal/config"
)

func TestListenerFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.ListenAddr, c.ControlAddr, c.DAVAddr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
		c.AdvertiseAddrs = []string{"127.0.0.1:1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	d, err := Start(context.Background(), Options{ConfigDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	d.lns[2].Close() // the WebDAV listener dies under the server
	select {
	case err := <-d.Failed():
		if err == nil {
			t.Fatal("nil failure")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no failure reported")
	}
	waited := make(chan error, 1)
	go func() { waited <- d.Wait() }()
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("Wait returned nil after a failure")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not stop itself")
	}
}
