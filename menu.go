package main

import (
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// setupMenu builds the application menu. Items emit an action rather than
// acting, so the menu and the UI run exactly the same code, prompts included.
func setupMenu(app *application.App) {
	menu := app.Menu.New()
	if runtime.GOOS == "darwin" {
		menu.AddRole(application.AppMenu)
	}

	send := func(action string) func(*application.Context) {
		return func(*application.Context) { app.Event.Emit("menu", action) }
	}

	file := menu.AddSubmenu("File")
	file.Add("Export Profiles…").SetAccelerator("cmdorctrl+shift+e").OnClick(send("profiles.export"))
	file.Add("Import Profiles…").SetAccelerator("cmdorctrl+shift+i").OnClick(send("profiles.import"))
	file.AddSeparator()
	file.Add("Check for Updates…").OnClick(send("app.update"))
	file.AddSeparator()
	file.AddRole(application.Quit)

	// Without an Edit menu the webview loses cut, copy, paste and select-all,
	// which would break every text field in the app.
	menu.AddRole(application.EditMenu)
	menu.AddRole(application.WindowMenu)

	app.Menu.SetApplicationMenu(menu)
}
