//go:build !windows

package main

// There is no tray outside Windows: Linux closes the app with the window, and
// macOS hides it through options.App.HideWindowOnClose. This file keeps the
// tray library out of those builds.

func newTray() Tray { return noTray{} }

type noTray struct{}

func (noTray) Start(_, _, _ func()) error { return errNoTray }
func (noTray) Stop()                      {}
