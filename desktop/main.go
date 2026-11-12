//go:build linux && !server

package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/zendext/scrape-fs/desktop/internal/gui"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	socket := flag.String("socket", "", "Use another local daemon socket (disables system management).")
	configuration := flag.String("config", "", "Read another configuration file (disables system management).")
	flag.Parse()
	frontend, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		log.Fatal(err)
	}
	var app *application.App
	service := gui.New(gui.Options{
		SocketPath: *socket, ConfigPath: *configuration,
		OpenPath: func(path string, folder bool) error {
			if folder {
				return app.Env.OpenFileManager(path, false)
			}
			return app.Browser.OpenFile(path)
		},
		ChooseSource: func() (string, error) {
			return app.Dialog.OpenFile().CanChooseFiles(true).CanChooseDirectories(false).PromptForSingleSelection()
		},
	})
	app = application.New(application.Options{
		Name: "ScrapeFS", Description: "Local virtual media library manager",
		Services: []application.Service{application.NewService(service)},
		Assets:   application.AssetOptions{Handler: application.AssetFileServerFS(frontend), DisableLogging: true},
	})
	app.Window.NewWithOptions(application.WebviewWindowOptions{Title: "ScrapeFS", Width: 1240, Height: 820, MinWidth: 800, MinHeight: 520, URL: "/"})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
