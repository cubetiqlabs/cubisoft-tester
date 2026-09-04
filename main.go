package main

import (
	"embed"
	"log"

	_ "github.com/go-sql-driver/mysql"
	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

func init() {
	// Registering the event gives the generated TS bindings a typed payload.
	application.RegisterEvent[Progress]("progress")
}

func main() {
	app := application.New(application.Options{
		Name:        "MySQL Tester",
		Description: "MySQL connection diagnostics, latency and throughput testing",
		Services: []application.Service{
			application.NewService(&Tester{}),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "MySQL Tester",
		Width:     1180,
		Height:    820,
		MinWidth:  900,
		MinHeight: 620,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 42,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(15, 17, 22),
		URL:              "/",
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
