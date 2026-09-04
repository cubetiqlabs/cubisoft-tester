package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Progress is emitted on the "progress" event while a test runs, so the UI can
// show something during a run that takes minutes.
type Progress struct {
	Kind    string  `json:"kind"` // diagnose | latency | speedtest | trace
	Message string  `json:"message"`
	Pct     float64 `json:"pct"`
}

// Tester is the single service bound to the frontend. Only one test runs at a
// time: starting a new one cancels whatever was in flight.
type Tester struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	gen    uint64
}

func (t *Tester) begin(budget time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
	}
	t.gen++
	gen := t.gen
	t.cancel = cancel
	t.mu.Unlock()
	return ctx, func() {
		cancel()
		t.mu.Lock()
		// Only clear our own cancel: a newer run may already have replaced it.
		if t.gen == gen {
			t.cancel = nil
		}
		t.mu.Unlock()
	}
}

// Cancel stops the running test. Safe to call when nothing is running.
func (t *Tester) Cancel() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *Tester) progress(kind, message string, pct float64) {
	app := application.Get()
	if app == nil {
		return
	}
	app.Event.Emit("progress", Progress{Kind: kind, Message: message, Pct: pct})
}

// Version is the running version, or "dev" for a local build.
func (t *Tester) Version() string { return version }

// CheckForUpdates opens the framework's update window, which reports "up to
// date" or walks download, verify, install and restart. Released builds only.
func (t *Tester) CheckForUpdates() error {
	app := application.Get()
	if app == nil || !isRelease(version) {
		return errors.New("this is a local build; updates apply to releases downloaded from GitHub")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return app.Updater.CheckAndInstall(ctx)
}

// ListDatabases returns the schemas this account can see, for the UI dropdown.
func (t *Tester) ListDatabases(cfg Config) ([]string, error) {
	// Deliberately not registered as the cancellable run: clicking "List" while
	// a speed test is going must not kill it.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout()+10*time.Second)
	defer cancel()

	cfg.Database = "" // a bad database name must not block listing the good ones
	db, err := cfg.open(ctx, 1)
	if err != nil {
		return nil, errorWithHint(err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, errorWithHint(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// errorWithHint flattens an error plus its diagnosis into the single string the
// frontend receives, since Wails only carries the message across the bridge.
func errorWithHint(err error) error {
	msg := errString(err)
	if h := hint(err); h != "" {
		msg += " \u2014 " + h
	}
	return errors.New(msg)
}
