package main

import (
	"context"
	"embed"
	_ "embed"
	"log"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/github"
)

//go:embed all:frontend/dist
var assets embed.FS

// version.txt is the single source of truth: scripts/release.sh bumps it, the
// workflow names release assets after it, and the updater compares against it.
//
//go:embed version.txt
var versionFile string

var version = strings.TrimSpace(versionFile)

const repository = "cubetiqlabs/cubisoft-tester"

// isRelease reports whether this build came from a tagged release. Local builds
// carry a placeholder version and must not talk to the update feed.
func isRelease(v string) bool {
	return v != "" && v != "dev" && v[0] >= '0' && v[0] <= '9'
}

func init() {
	application.RegisterEvent[Progress]("progress")
	application.RegisterEvent[string]("menu")
	application.RegisterEvent[string]("files-dropped")
	application.RegisterEvent[Sample]("speed:sample")
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

	setupMenu(app)
	setupUpdater(app)

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
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
		// Windows and Linux only show the application menu when a window opts
		// in; without this the File menu simply does not exist there.
		UseApplicationMenu: true,
		// Lets an exported profile file be dropped straight onto the window.
		EnableFileDrop: true,
	})

	window.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		for _, path := range e.Context().DroppedFiles() {
			if strings.HasSuffix(strings.ToLower(path), ".json") {
				app.Event.Emit("files-dropped", path)
				return
			}
		}
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// setupUpdater points the framework updater at this repo's GitHub releases and
// does one quiet check at startup. Check alone opens no window — the frontend
// listens for wails:updater:update-available and offers the update instead.
func setupUpdater(app *application.App) {
	if !isRelease(version) {
		return
	}
	provider, err := github.New(github.Config{
		Repository:    repository,
		ChecksumAsset: "checksums.txt",
	})
	if err != nil {
		log.Printf("updater: %v", err)
		return
	}
	if err := app.Updater.Init(updater.Config{
		CurrentVersion: version,
		Providers:      []updater.Provider{provider},
	}); err != nil {
		log.Printf("updater: %v", err)
		return
	}
	go func() {
		time.Sleep(5 * time.Second) // let the window paint before touching the network
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = app.Updater.Check(ctx)
	}()
}
