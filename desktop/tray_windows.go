//go:build windows

package main

import (
	_ "embed"
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/energye/systray"
)

// trayIcon is the notification-area icon. It is committed here, not taken from
// build/windows/icon.ico, which `wails build` generates and may rewrite.
//
//go:embed tray.ico
var trayIcon []byte

const (
	trayStartTimeout = 5 * time.Second
	trayStopTimeout  = 3 * time.Second
)

// winTray runs github.com/energye/systray. Its message loop must run on the
// thread that created the tray window, and must not block Wails, so Start gives
// it a goroutine locked to its own OS thread (Wails' loop is on the main one).
// Menu clicks arrive on that thread; the callbacks are run on new goroutines so
// that the tray thread never waits on the UI thread.
type winTray struct {
	mu        sync.Mutex
	started   bool
	abandoned bool // Start gave up; a late onReady must do nothing
	stopping  bool
	done      chan struct{} // closed when the loop has ended
}

func newTray() Tray { return &winTray{} }

func (t *winTray) Start(onOpen, onQuit, onLost func()) error {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return errors.New("tray already started")
	}
	t.started = true
	t.done = make(chan struct{})
	done := t.done
	t.mu.Unlock()

	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread() // never unlocked: the thread ends with the loop
		defer close(done)
		show := func(menu systray.IMenu) { go onOpen() }
		systray.SetOnClick(show)
		systray.SetOnDClick(show)
		systray.Run(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.abandoned {
				return
			}
			systray.SetIcon(trayIcon)
			systray.SetTooltip(trayTooltip)
			open := systray.AddMenuItem(trayOpenLabel, trayOpenLabel)
			open.Click(func() { go onOpen() })
			systray.AddSeparator()
			quit := systray.AddMenuItem(trayQuitLabel, trayQuitLabel)
			quit.Click(func() { go onQuit() })
			close(ready)
		}, func() {
			t.mu.Lock()
			stopping := t.stopping
			t.mu.Unlock()
			if !stopping {
				go onLost()
			}
		})
	}()

	select {
	case <-ready:
		return nil
	case <-time.After(trayStartTimeout):
		// If the tray window could not be created the library logs and never
		// calls onReady; the loop then idles until the process ends.
		t.mu.Lock()
		t.abandoned = true
		t.stopping = true
		t.mu.Unlock()
		systray.Quit()
		return errors.New("the tray did not start in time")
	}
}

// Stop deletes the icon: Quit closes the tray window, whose destruction runs
// Shell_NotifyIcon(NIM_DELETE) before the loop ends. Waiting for that keeps the
// process from exiting first and leaving a ghost icon.
func (t *winTray) Stop() {
	t.mu.Lock()
	if !t.started || t.stopping {
		done := t.done
		t.mu.Unlock()
		if done != nil {
			waitFor(done)
		}
		return
	}
	t.stopping = true
	done := t.done
	t.mu.Unlock()
	systray.Quit()
	waitFor(done)
}

func waitFor(done chan struct{}) {
	select {
	case <-done:
	case <-time.After(trayStopTimeout):
	}
}
