package fileio

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/events"
)

// minPNG is the 8-byte PNG signature plus a tiny trailing chunk —
// readable by the fileio plugin's mime detection (extension-based) without
// pulling a real image library into the test path.
var minPNG = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x00}

// minPDF is the 5-byte PDF magic + EOF marker. fileio dispatches on
// extension, not content sniffing, but we still write byte-realistic bytes
// in case future code paths add content checks.
var minPDF = []byte{'%', 'P', 'D', 'F', '-', '1', '.', '4', '\n', '%', '%', 'E', 'O', 'F'}

// busCapture wraps a real engine.EventBus and records every tool.result so
// tests can assert what the plugin emitted without booting the full
// engine.
type busCapture struct {
	engine.EventBus

	mu      sync.Mutex
	results []events.ToolResult
}

func newBusCapture(t *testing.T) *busCapture {
	t.Helper()
	bus := engine.NewEventBus()
	bc := &busCapture{EventBus: bus}
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.ToolResult); ok {
			bc.mu.Lock()
			bc.results = append(bc.results, r)
			bc.mu.Unlock()
		}
	})
	return bc
}

func (b *busCapture) lastResult(t *testing.T) events.ToolResult {
	t.Helper()
	// Drain pending dispatch so async-flagged events flush before assert.
	_ = b.EventBus.Drain(contextWithDeadline(t, 200*time.Millisecond))
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.results) == 0 {
		t.Fatal("no tool.result emitted")
	}
	return b.results[len(b.results)-1]
}

func contextWithDeadline(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// makePlugin spins up a fileio plugin scoped to dir with both multimodal
// tools enabled and an explicit inline cutoff so tests can choose between
// inline and blob-stored paths.
func makePlugin(t *testing.T, dir string, inlineCutoff int64) (*Plugin, *busCapture) {
	t.Helper()
	bus := newBusCapture(t)
	store, err := blobs.New(filepath.Join(dir, "blobs"), 0)
	if err != nil {
		t.Fatalf("blobs.New: %v", err)
	}
	p := &Plugin{
		bus:              bus,
		baseDir:          dir,
		enabled:          map[string]bool{"read_image": true, "read_document": true},
		blobStore:        store,
		blobInlineCutoff: inlineCutoff,
	}
	return p, bus
}

func TestReadImage_Inline(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "small.png")
	if err := os.WriteFile(pngPath, minPNG, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Cutoff well above payload size — should inline.
	p, bus := makePlugin(t, dir, 1<<20)
	p.handleReadBinary(events.ToolCall{ID: "img-1", Name: "read_image", Arguments: map[string]any{"path": "small.png"}}, kindImage)

	r := bus.lastResult(t)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if len(r.OutputParts) != 1 {
		t.Fatalf("OutputParts len: got %d want 1", len(r.OutputParts))
	}
	part := r.OutputParts[0]
	if part.Type != "image" {
		t.Errorf("part.Type: got %q want image", part.Type)
	}
	if part.MimeType != "image/png" {
		t.Errorf("part.MimeType: got %q want image/png", part.MimeType)
	}
	if !bytes.Equal(part.Data, minPNG) {
		t.Errorf("part.Data does not match fixture")
	}
	if part.URI != "" {
		t.Errorf("expected inline (empty URI); got URI=%q", part.URI)
	}
}

func TestReadImage_Blob(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "big.png")
	bigData := append(append([]byte{}, minPNG...), bytes.Repeat([]byte{0xAA}, 4096)...)
	if err := os.WriteFile(pngPath, bigData, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Cutoff below payload size — must go through blob store.
	p, bus := makePlugin(t, dir, 1024)
	p.handleReadBinary(events.ToolCall{ID: "img-2", Name: "read_image", Arguments: map[string]any{"path": "big.png"}}, kindImage)

	r := bus.lastResult(t)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if len(r.OutputParts) != 1 {
		t.Fatalf("OutputParts len: got %d want 1", len(r.OutputParts))
	}
	part := r.OutputParts[0]
	if part.URI == "" || blobs.SHAFromURI(part.URI) == "" {
		t.Errorf("expected nexus-blob URI; got %q", part.URI)
	}
	if len(part.Data) != 0 {
		t.Errorf("expected blob path with no inline Data; got %d inline bytes", len(part.Data))
	}
	got, mt, err := p.blobStore.Get(blobs.SHAFromURI(part.URI))
	if err != nil {
		t.Fatalf("blob store Get: %v", err)
	}
	if !bytes.Equal(got, bigData) {
		t.Errorf("blob bytes do not match fixture")
	}
	if mt != "image/png" {
		t.Errorf("blob media type: got %q want image/png", mt)
	}
	bu, _ := r.OutputStructured["blob_uri"].(string)
	if bu != part.URI {
		t.Errorf("OutputStructured.blob_uri %q != part.URI %q", bu, part.URI)
	}
}

func TestReadImage_UnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), []byte{0, 1, 2}, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p, bus := makePlugin(t, dir, 1<<20)
	p.handleReadBinary(events.ToolCall{ID: "img-3", Name: "read_image", Arguments: map[string]any{"path": "data.bin"}}, kindImage)

	r := bus.lastResult(t)
	if r.Error == "" {
		t.Fatalf("expected error on unsupported extension")
	}
	if len(r.OutputParts) != 0 {
		t.Errorf("expected no OutputParts on error; got %d", len(r.OutputParts))
	}
}

func TestReadDocument_PDF(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spec.pdf"), minPDF, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p, bus := makePlugin(t, dir, 1<<20)
	p.handleReadBinary(events.ToolCall{ID: "doc-1", Name: "read_document", Arguments: map[string]any{"path": "spec.pdf"}}, kindDocument)

	r := bus.lastResult(t)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if len(r.OutputParts) != 1 {
		t.Fatalf("OutputParts len: got %d want 1", len(r.OutputParts))
	}
	part := r.OutputParts[0]
	if part.Type != "file" {
		t.Errorf("part.Type: got %q want file", part.Type)
	}
	if part.MimeType != "application/pdf" {
		t.Errorf("part.MimeType: got %q want application/pdf", part.MimeType)
	}
}

func TestReadBinary_NoBlobStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.png"), minPNG, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	bus := newBusCapture(t)
	// blobStore deliberately nil — simulates a non-session embedder that
	// disabled multimodal reads.
	p := &Plugin{
		bus:     bus,
		baseDir: dir,
		enabled: map[string]bool{"read_image": true},
	}
	p.handleReadBinary(events.ToolCall{ID: "img-4", Name: "read_image", Arguments: map[string]any{"path": "x.png"}}, kindImage)

	r := bus.lastResult(t)
	if r.Error == "" {
		t.Fatalf("expected error when blob store missing")
	}
}
