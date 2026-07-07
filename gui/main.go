// SuperSpeedySearch GUI — a Wails desktop client that discovers search nodes
// and fans out queries. Run `wails dev` in this directory for live-reload
// development, `wails build` for a distributable app bundle.
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app, err := NewApp()
	if err != nil {
		log.Fatal("load settings: ", err)
	}
	err = wails.Run(&options.App{
		Title:     "Super Speedy Search",
		Width:     1100,
		Height:    720,
		MinWidth:  760,
		MinHeight: 480,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup: app.startup,
		Bind:      []interface{}{app},
		Mac: &mac.Options{
			TitleBar:             mac.TitleBarDefault(),
			Appearance:           mac.DefaultAppearance,
			WebviewIsTransparent: false,
			About: &mac.AboutInfo{
				Title:   "Super Speedy Search",
				Message: "Search across your computers and NAS devices.",
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
