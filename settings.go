package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Settings are the small preferences that outlive a session. Kept beside the
// profiles, in their own file, so a corrupt one cannot take the profiles down.
type Settings struct {
	// Theme is "system", "light" or "dark".
	Theme string `json:"theme"`
}

func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "MySQLTester", "settings.json"), nil
}

func readSettings() Settings {
	s := Settings{Theme: "system"}
	path, err := settingsPath()
	if err != nil {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s // missing or unreadable: the defaults are fine
	}
	_ = json.Unmarshal(data, &s)
	if s.Theme != "light" && s.Theme != "dark" {
		s.Theme = "system"
	}
	return s
}

// Settings returns the stored preferences for the frontend to apply at startup.
func (t *Tester) Settings() Settings { return readSettings() }

// SetTheme stores the appearance choice. The frontend applies it; this only
// remembers it for the next launch and for the menu's checkmark.
func (t *Tester) SetTheme(theme string) error {
	switch theme {
	case "system", "light", "dark":
	default:
		return errors.New("unknown theme: " + theme)
	}
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(Settings{Theme: theme}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
