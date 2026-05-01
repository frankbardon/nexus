//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestLongtermMemory_Boot validates the longterm memory plugin boots and
// emits its load event.
func TestLongtermMemory_Boot(t *testing.T) {
	dir := t.TempDir()
	cfg := copyConfig(t, "configs/test-longterm-memory.yaml", map[string]any{
		"nexus.memory.longterm": map[string]any{
			"scope":     "global",
			"auto_load": true,
			"path":      dir,
		},
	})
	h := testharness.New(t, cfg, testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertBooted("nexus.memory.longterm")
	h.AssertEventEmitted("memory.longterm.loaded")
}

// TestLongtermMemory_RegistersTools validates that all four CRUD tools are
// registered with the catalog.
func TestLongtermMemory_RegistersTools(t *testing.T) {
	dir := t.TempDir()
	cfg := copyConfig(t, "configs/test-longterm-memory.yaml", map[string]any{
		"nexus.memory.longterm": map[string]any{
			"scope":     "global",
			"auto_load": true,
			"path":      dir,
		},
	})
	h := testharness.New(t, cfg, testharness.WithTimeout(20*time.Second))
	h.Run()

	wantTools := map[string]bool{
		"memory_write":  false,
		"memory_read":   false,
		"memory_list":   false,
		"memory_delete": false,
	}
	for _, e := range h.Events() {
		if e.Type != "tool.register" {
			continue
		}
		td, ok := e.Payload.(events.ToolDef)
		if !ok {
			continue
		}
		if _, want := wantTools[td.Name]; want {
			wantTools[td.Name] = true
		}
	}
	for name, registered := range wantTools {
		if !registered {
			t.Errorf("expected tool %q to be registered", name)
		}
	}
}

// TestLongtermMemory_WriteCreatesArtifact validates the end-to-end memory_write
// flow: mock LLM tool call → memory.longterm handler stores → markdown file
// appears under the configured path.
func TestLongtermMemory_WriteCreatesArtifact(t *testing.T) {
	dir := t.TempDir()
	cfg := copyConfig(t, "configs/test-longterm-memory.yaml", map[string]any{
		"nexus.memory.longterm": map[string]any{
			"scope":     "global",
			"auto_load": true,
			"path":      dir,
		},
	})
	h := testharness.New(t, cfg, testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertToolCalled("memory_write")
	h.AssertEventEmitted("memory.longterm.stored")

	// Verify the markdown artifact landed at the configured path. Filename is
	// derived from the key passed by the mock (favorite-color → favorite-color.md
	// or similar normalised form).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read memory dir %s: %v", dir, err)
	}
	var md []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".md" {
			md = append(md, e.Name())
		}
	}
	if len(md) == 0 {
		t.Errorf("expected at least one .md file in %s after memory_write, got entries: %v", dir, entries)
	}
}
