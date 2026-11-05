package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wailsapp/wails/v2/pkg/options"
)

func TestCloseAction(t *testing.T) {
	for _, c := range []struct {
		name                 string
		trayActive, quitting bool
		wantPrevent          bool
	}{
		{"tray active, not quitting: hide", true, false, true},
		{"quitting: quit", true, true, false},
		{"no tray: quit", false, false, false},
		{"no tray and quitting: quit", false, true, false},
	} {
		if got := closeAction(c.trayActive, c.quitting); got != c.wantPrevent {
			t.Errorf("%s: closeAction = %v, want %v", c.name, got, c.wantPrevent)
		}
	}
}

// fakeTray records calls and lets a test click the menu.
type fakeTray struct {
	mu                   sync.Mutex
	startErr             error
	started, stopped     int
	onOpen, onQuit, lost func()
}

func (f *fakeTray) Start(onOpen, onQuit, onLost func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	if f.startErr != nil {
		return f.startErr
	}
	f.onOpen, f.onQuit, f.lost = onOpen, onQuit, onLost
	return nil
}

func (f *fakeTray) Stop() { f.mu.Lock(); f.stopped++; f.mu.Unlock() }

// windowSpy replaces the Wails window calls.
type windowSpy struct {
	shows, hides, quits, unmins atomic.Int32
}

func trayApp(t *testing.T, tr Tray) (*App, *windowSpy) {
	t.Helper()
	a := newApp(t, t.TempDir())
	a.tray = tr
	s := &windowSpy{}
	a.win = windowOps{
		show:       func(context.Context) { s.shows.Add(1) },
		hide:       func(context.Context) { s.hides.Add(1) },
		unminimise: func(context.Context) { s.unmins.Add(1) },
		quit:       func(context.Context) { s.quits.Add(1) },
	}
	a.startup(context.Background())
	t.Cleanup(func() { a.shutdown(context.Background()) })
	return a, s
}

func TestCloseHidesWhileTrayRuns(t *testing.T) {
	ft := &fakeTray{}
	a, s := trayApp(t, ft)
	if ft.started != 1 {
		t.Fatalf("tray started %d times", ft.started)
	}
	if !a.beforeClose(context.Background()) {
		t.Fatal("close was not prevented")
	}
	if s.hides.Load() != 1 {
		t.Errorf("hides = %d", s.hides.Load())
	}
}

func TestCloseQuitsWhenTrayFailedToStart(t *testing.T) {
	a, s := trayApp(t, &fakeTray{startErr: errors.New("no shell")})
	if a.beforeClose(context.Background()) {
		t.Error("close prevented with no tray: the window would be hidden for good")
	}
	if s.hides.Load() != 0 {
		t.Errorf("hides = %d", s.hides.Load())
	}
}

func TestCloseQuitsWithNoTray(t *testing.T) {
	a, _ := trayApp(t, nil)
	if a.beforeClose(context.Background()) {
		t.Error("close prevented without a tray")
	}
}

func TestTrayQuitQuitsOnceAndLetsCloseThrough(t *testing.T) {
	ft := &fakeTray{}
	a, s := trayApp(t, ft)
	ft.onQuit()
	ft.onQuit() // a second click is harmless
	if s.quits.Load() != 1 {
		t.Errorf("quit called %d times", s.quits.Load())
	}
	// Wails asks OnBeforeClose when the quit arrives: it must let it pass.
	if a.beforeClose(context.Background()) {
		t.Error("close prevented while quitting")
	}
	if s.hides.Load() != 0 {
		t.Errorf("hides = %d", s.hides.Load())
	}
}

func TestTrayQuitRacesWithClose(t *testing.T) {
	ft := &fakeTray{}
	a, s := trayApp(t, ft)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); ft.onQuit() }()
		go func() { defer wg.Done(); a.beforeClose(context.Background()) }()
	}
	wg.Wait()
	if s.quits.Load() != 1 {
		t.Errorf("quit called %d times", s.quits.Load())
	}
	if a.beforeClose(context.Background()) {
		t.Error("close prevented after quit")
	}
}

func TestTrayOpenShowsAndFocuses(t *testing.T) {
	ft := &fakeTray{}
	_, s := trayApp(t, ft)
	ft.onOpen()
	if s.shows.Load() != 1 || s.unmins.Load() != 1 {
		t.Errorf("shows = %d, unminimise = %d", s.shows.Load(), s.unmins.Load())
	}
}

func TestSecondLaunchShowsTheWindow(t *testing.T) {
	a, s := trayApp(t, &fakeTray{})
	a.secondInstance(options.SecondInstanceData{})
	if s.shows.Load() != 1 || s.unmins.Load() != 1 {
		t.Errorf("shows = %d, unminimise = %d", s.shows.Load(), s.unmins.Load())
	}
}

func TestShowBeforeStartupIsIgnored(t *testing.T) {
	a := newApp(t, t.TempDir())
	s := &windowSpy{}
	a.win.show = func(context.Context) { s.shows.Add(1) }
	a.win.unminimise = func(context.Context) {}
	a.secondInstance(options.SecondInstanceData{}) // no context yet: Wails calls would panic
	if s.shows.Load() != 0 {
		t.Error("window calls made before startup set the context")
	}
}

func TestLostTrayShowsWindowAndQuitsOnClose(t *testing.T) {
	ft := &fakeTray{}
	a, s := trayApp(t, ft)
	ft.lost()
	if s.shows.Load() != 1 {
		t.Errorf("shows = %d", s.shows.Load())
	}
	if a.beforeClose(context.Background()) {
		t.Error("close prevented after the tray was lost")
	}
}

func TestShutdownStopsTrayAfterTheDaemon(t *testing.T) {
	ft := &fakeTray{}
	a, _ := trayApp(t, ft)
	if a.d == nil {
		t.Fatal("no in-process daemon")
	}
	a.shutdown(context.Background())
	a.shutdown(context.Background()) // twice is harmless
	if ft.stopped < 1 {
		t.Error("tray was not stopped")
	}
}

func TestSingleInstanceID(t *testing.T) {
	if singleInstanceID == "" || len(singleInstanceID) < 16 {
		t.Errorf("singleInstanceID = %q", singleInstanceID)
	}
	const want = "sugar-it.5b0c7e2a-3f41-4d8e-9a6b-1c2d7e90f4a3"
	if singleInstanceID != want {
		t.Errorf("the single-instance ID changed to %q: that lets two versions run side by side", singleInstanceID)
	}
}

func TestAppStateSaysWhetherCloseHides(t *testing.T) {
	ft := &fakeTray{}
	a, _ := trayApp(t, ft)
	if !a.AppState().CloseHides {
		t.Error("CloseHides = false with a tray")
	}
	ft.lost()
	if a.AppState().CloseHides {
		t.Error("CloseHides = true after the tray was lost")
	}
	b, _ := trayApp(t, &fakeTray{startErr: errors.New("no shell")})
	if b.AppState().CloseHides {
		t.Error("CloseHides = true without a tray")
	}
	b.goos = "darwin" // HideWindowOnClose
	if !b.AppState().CloseHides {
		t.Error("CloseHides = false on macOS")
	}
}
