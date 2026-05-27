package postures

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
)

func TestPlugin_LoadsScanDirOnInit(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "summarizer.yaml"), []byte(`name: summarizer
description: short tldr
system_prompt: be brief
allowed_tools: [web_fetch]
model:
  model_role: quick
default_budget:
  timeout: 10s
`))

	p := New().(*Plugin)
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := p.Init(engine.PluginContext{
		Bus:    bus,
		Logger: logger,
		Config: map[string]any{"scan_dirs": []any{dir}},
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	got, err := p.Registry().Get("summarizer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Description != "short tldr" {
		t.Errorf("description = %q", got.Description)
	}
	if got.Version == "" {
		t.Errorf("Version missing")
	}
}

func TestPlugin_WatchPicksUpEditsAfterReady(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "live.yaml")
	mustWrite(t, target, []byte("name: live\nsystem_prompt: v1\n"))

	p := New().(*Plugin)
	p.debounce = 50 * time.Millisecond
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := p.Init(engine.PluginContext{
		Bus:    bus,
		Logger: logger,
		Config: map[string]any{"scan_dirs": []any{dir}},
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	got, err := p.Registry().Get("live")
	if err != nil {
		t.Fatalf("initial get: %v", err)
	}
	initialVersion := got.Version

	// Subscribe to detect re-registration.
	changed := make(chan struct{}, 1)
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	ch := p.Registry().Watch(watchCtx)
	go func() {
		for c := range ch {
			if c.Name == "live" && c.Posture != nil && c.Posture.Version != initialVersion {
				select {
				case changed <- struct{}{}:
				default:
				}
				return
			}
		}
	}()

	// Edit the file. Use rewrite (not truncate-then-write split) so fsnotify
	// sees a single coherent change.
	mustWrite(t, target, []byte("name: live\nsystem_prompt: v2\n"))

	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe edit-driven reload")
	}
}

func mustWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
