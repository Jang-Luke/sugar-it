package main

import (
	"embed"
	"flag"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	configDir := flag.String("config-dir", "", "config directory (default: the user config directory's sugar-it)")
	flag.Parse()
	app := NewApp(*configDir)
	err := wails.Run(&options.App{
		Title:     "sugar-it",
		Width:     1180,
		Height:    760,
		MinWidth:  860,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind:       []any{app},
	})
	if err != nil {
		log.Fatal(err)
	}
}
