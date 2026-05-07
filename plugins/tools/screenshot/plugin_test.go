package screenshot

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/events"
)

// minPNG is the 8-byte PNG signature plus a tiny trailing chunk — enough
// for tests that only care about routing, not pixels.
var minPNG = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x00}

// busCapture wraps a real engine.EventBus and records every tool.result so
// tests can assert what the plugin emitted without booting the full engine.
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
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = b.EventBus.Drain(ctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.results) == 0 {
		t.Fatal("no tool.result emitted")
	}
	return b.results[len(b.results)-1]
}

// stubRun returns a runFunc that writes payload to outPath. fail, when
// non-nil, is returned without writing.
func stubRun(payload []byte, fail error) runFunc {
	return func(_ context.Context, _ string, _ []string, outPath string) error {
		if fail != nil {
			return fail
		}
		return os.WriteFile(outPath, payload, 0o644)
	}
}

// makePlugin spins up a screenshot plugin with the given inline cutoff and
// stubbed exec runner. Returns the plugin and a busCapture for assertions.
func makePlugin(t *testing.T, inlineCutoff int64, run runFunc) (*Plugin, *busCapture) {
	t.Helper()
	dir := t.TempDir()
	bus := newBusCapture(t)
	store, err := blobs.New(filepath.Join(dir, "blobs"), 0)
	if err != nil {
		t.Fatalf("blobs.New: %v", err)
	}
	p := &Plugin{
		bus:              bus,
		blobStore:        store,
		blobInlineCutoff: inlineCutoff,
		timeout:          5 * time.Second,
		run:              run,
	}
	return p, bus
}

func TestScreenshot_Inline(t *testing.T) {
	p, bus := makePlugin(t, 1<<20, stubRun(minPNG, nil))

	p.handleScreenshot(events.ToolCall{ID: "shot-1", Name: toolName})

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

func TestScreenshot_Blob(t *testing.T) {
	bigData := append(append([]byte{}, minPNG...), bytes.Repeat([]byte{0xAA}, 4096)...)
	p, bus := makePlugin(t, 1024, stubRun(bigData, nil))

	p.handleScreenshot(events.ToolCall{ID: "shot-2", Name: toolName})

	r := bus.lastResult(t)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if len(r.OutputParts) != 1 {
		t.Fatalf("OutputParts len: got %d want 1", len(r.OutputParts))
	}
	part := r.OutputParts[0]
	if part.URI == "" || blobs.SHAFromURI(part.URI) == "" {
		t.Fatalf("expected nexus-blob URI; got %q", part.URI)
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

func TestScreenshot_NoBlobStore(t *testing.T) {
	bus := newBusCapture(t)
	p := &Plugin{
		bus:     bus,
		timeout: time.Second,
		run:     stubRun(minPNG, nil),
	}
	p.handleScreenshot(events.ToolCall{ID: "shot-3", Name: toolName})

	r := bus.lastResult(t)
	if r.Error == "" {
		t.Fatalf("expected error when blob store missing")
	}
	if len(r.OutputParts) != 0 {
		t.Errorf("expected no OutputParts on error; got %d", len(r.OutputParts))
	}
}

func TestScreenshot_CaptureFailure(t *testing.T) {
	p, bus := makePlugin(t, 1<<20, stubRun(nil, errors.New("boom")))

	p.handleScreenshot(events.ToolCall{ID: "shot-4", Name: toolName})

	r := bus.lastResult(t)
	if r.Error == "" {
		t.Fatalf("expected error from stubbed runner")
	}
	if len(r.OutputParts) != 0 {
		t.Errorf("expected no OutputParts on error; got %d", len(r.OutputParts))
	}
}
