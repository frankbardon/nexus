package pdf

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// minPDF is a minimal PDF magic + EOF marker — enough for tests that
// only check routing (extension and mode dispatch), not parsing.
var minPDF = []byte{'%', 'P', 'D', 'F', '-', '1', '.', '4', '\n', '%', '%', 'E', 'O', 'F'}

// busCapture records every tool.result so tests can assert what the
// plugin emitted without booting the full engine.
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

// TestReadPDF_DocumentMode_NoPdftotext verifies the document-mode path
// works without a pdftotext binary. Writes a fixture PDF, asserts the
// raw bytes ride on a file MessagePart, and that no shell-out happened
// (we never set p.pdftotext).
func TestReadPDF_DocumentMode_NoPdftotext(t *testing.T) {
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "spec.pdf")
	if err := os.WriteFile(pdfPath, minPDF, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	bus := newBusCapture(t)
	p := &Plugin{
		bus:         bus,
		defaultMode: modeText, // override default in args
	}
	p.handleReadPDF(events.ToolCall{
		ID:        "pdf-1",
		Name:      "read_pdf",
		Arguments: map[string]any{"path": pdfPath, "mode": modeDocument},
	})

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
	if !bytes.Equal(part.Data, minPDF) {
		t.Errorf("part.Data does not match fixture")
	}
	if got, _ := r.OutputStructured["mode"].(string); got != modeDocument {
		t.Errorf("OutputStructured.mode: got %q want %q", got, modeDocument)
	}
}

// TestReadPDF_DefaultMode_Document verifies that setting default_mode at
// plugin level routes calls to document mode without an explicit arg.
func TestReadPDF_DefaultMode_Document(t *testing.T) {
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "spec.pdf")
	if err := os.WriteFile(pdfPath, minPDF, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	bus := newBusCapture(t)
	p := &Plugin{
		bus:         bus,
		defaultMode: modeDocument,
	}
	p.handleReadPDF(events.ToolCall{
		ID:        "pdf-2",
		Name:      "read_pdf",
		Arguments: map[string]any{"path": pdfPath},
	})

	r := bus.lastResult(t)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if len(r.OutputParts) != 1 {
		t.Fatalf("OutputParts len: got %d want 1", len(r.OutputParts))
	}
	if r.OutputParts[0].MimeType != "application/pdf" {
		t.Errorf("MimeType: got %q want application/pdf", r.OutputParts[0].MimeType)
	}
}

// TestReadPDF_TextMode_NoBinary surfaces a clear error when text mode is
// requested but pdftotext isn't configured. Without this guard the
// plugin would pass an empty path to exec.Command.
func TestReadPDF_TextMode_NoBinary(t *testing.T) {
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "spec.pdf")
	if err := os.WriteFile(pdfPath, minPDF, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	bus := newBusCapture(t)
	p := &Plugin{
		bus:         bus,
		defaultMode: modeText,
	}
	p.handleReadPDF(events.ToolCall{
		ID:        "pdf-3",
		Name:      "read_pdf",
		Arguments: map[string]any{"path": pdfPath, "mode": modeText},
	})

	r := bus.lastResult(t)
	if r.Error == "" {
		t.Fatalf("expected error when pdftotext binary missing")
	}
	if len(r.OutputParts) != 0 {
		t.Errorf("expected no OutputParts on error; got %d", len(r.OutputParts))
	}
}

// TestReadPDF_InvalidMode rejects unknown mode values up front.
func TestReadPDF_InvalidMode(t *testing.T) {
	bus := newBusCapture(t)
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "spec.pdf")
	if err := os.WriteFile(pdfPath, minPDF, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	p := &Plugin{
		bus:         bus,
		defaultMode: modeText,
	}
	p.handleReadPDF(events.ToolCall{
		ID:        "pdf-4",
		Name:      "read_pdf",
		Arguments: map[string]any{"path": pdfPath, "mode": "screenshot"},
	})

	r := bus.lastResult(t)
	if r.Error == "" {
		t.Fatalf("expected error on invalid mode")
	}
}
