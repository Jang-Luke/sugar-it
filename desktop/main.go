package main

import (
	"embed"
	"flag"
	"io"
	"log"
	"os"
	"runtime"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"sugarintegration/internal/cli"
)

//go:embed all:frontend/dist
var assets embed.FS

// runDaemon handles the first argument daemon: the desktop package has no
// sugar-it command, so its background service runs this program. It goes through
// the CLI and never reaches Wails, so it starts no GTK and needs no display.
func runDaemon(args []string, stdout, stderr io.Writer) (code int, handled bool) {
	if len(args) == 0 || args[0] != "daemon" {
		return 0, false
	}
	return cli.Execute(args, stdout, stderr), true
}

func main() {
	if code, handled := runDaemon(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	configDir := flag.String("config-dir", "", "config directory (default: the user config directory's sugar-it)")
	flag.Parse()
	app := NewApp(*configDir)
	// Also when Run fails: a tray icon must not outlive the process.
	defer app.stopTray()
	err := wails.Run(&options.App{
		Title:     "sugar-it",
		Width:     1180,
		Height:    760,
		MinWidth:  860,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:     app.startup,
		OnShutdown:    app.shutdown,
		OnBeforeClose: app.beforeClose,
		// macOS only: closing hides the app and a Dock click shows it. Windows
		// hides through OnBeforeClose, because it needs the tray; Linux quits.
		HideWindowOnClose: runtime.GOOS == "darwin",
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               singleInstanceID,
			OnSecondInstanceLaunch: app.secondInstance,
		},
		Bind: []any{app},
	})
	if err != nil {
		app.stopTray() // log.Fatal skips deferred calls
		log.Fatal(err)
	}
}
