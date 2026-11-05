package main

import (
	"context"
	"errors"
	"log"

	"github.com/wailsapp/wails/v2/pkg/options"
	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Tray labels are Go constants, not ko.ts entries: the front end's strings are
// not reachable from Go. This is the one exception to "GUI strings live in
// ko.ts" (spec section 10).
const (
	trayTooltip   = "sugar-it"
	trayOpenLabel = "열기"
	trayQuitLabel = "종료"
)

// singleInstanceID is the lock ID of options.SingleInstanceLock. It must never
// change, or two versions would run side by side; it is specific to this app.
const singleInstanceID = "sugar-it.5b0c7e2a-3f41-4d8e-9a6b-1c2d7e90f4a3"

// errNoTray is what Start returns on systems without a tray; it is not logged.
var errNoTray = errors.New("no tray on this system")

// Tray is the notification-area icon. Start returns once the icon is shown or
// it failed; the callbacks may run on any goroutine. onLost is called if the
// tray dies while running (not after Stop). Stop removes the icon, waits for
// it, and may be called more than once.
type Tray interface {
	Start(onOpen, onQuit, onLost func()) error
	Stop()
}

// closeAction decides what the window's close button does: true means prevent
// the close (the window is hidden instead). It hides only while a tray is there
// to bring the window back, and never once the app is quitting.
func closeAction(trayActive, quitting bool) (prevent bool) {
	return trayActive && !quitting
}

// windowOps are the Wails runtime calls, a seam for tests. They are safe from
// any goroutine: Wails marshals each onto the UI thread. They need the context
// that Wails passes to startup, so the App calls them only once it has it.
type windowOps struct {
	show, hide, unminimise, quit func(ctx context.Context)
}

func wailsWindowOps() windowOps {
	return windowOps{
		show:       wailsrt.WindowShow,
		hide:       wailsrt.WindowHide,
		unminimise: wailsrt.WindowUnminimise,
		quit:       wailsrt.Quit,
	}
}

// startTray starts the tray after startup has set the context. A failure is
// logged and leaves trayActive false, so closing the window quits.
func (a *App) startTray() {
	if a.tray == nil {
		return
	}
	if err := a.tray.Start(a.showWindow, a.quitFromTray, a.trayLost); err != nil {
		if !errors.Is(err, errNoTray) {
			log.Printf("sugar-it: tray unavailable, closing the window will quit: %v", err)
		}
		return
	}
	a.trayActive.Store(true)
}

// beforeClose is OnBeforeClose: true prevents the close.
func (a *App) beforeClose(ctx context.Context) bool {
	if !closeAction(a.trayActive.Load(), a.quitting.Load()) {
		return false
	}
	a.win.hide(ctx)
	return true
}

// showWindow shows, restores and focuses the window, also when it is hidden.
// Before startup there is no context and nothing to show.
func (a *App) showWindow() {
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if ctx == nil {
		return
	}
	a.win.unminimise(ctx)
	a.win.show(ctx)
}

// secondInstance is OnSecondInstanceLaunch.
func (a *App) secondInstance(options.SecondInstanceData) { a.showWindow() }

// quitFromTray is the tray's 종료. The flag is set before Quit so that the
// OnBeforeClose that Quit triggers lets it through, whatever a close in flight
// decides; a second click does nothing.
func (a *App) quitFromTray() {
	if !a.quitting.CompareAndSwap(false, true) {
		return
	}
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if ctx != nil {
		a.win.quit(ctx)
	}
}

// trayLost: the tray died while running. Closing must quit from now on, and the
// window may be hidden with nothing left to bring it back.
func (a *App) trayLost() {
	a.trayActive.Store(false)
	a.showWindow()
}
