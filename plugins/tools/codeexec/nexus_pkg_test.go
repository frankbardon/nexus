package codeexec

import (
	"bytes"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/blobs"
)

// minPNG is the 8-byte PNG signature plus a tiny trailing chunk — enough
// for tests that only care about routing, not pixels.
var nexusMinPNG = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x00}

func TestReturnImage_Inline(t *testing.T) {
	h := newHarness(t, nil)

	// Yaegi gets the PNG bytes from a string literal — Go source for a
	// short script doesn't have a clean way to embed binary, so we
	// `make` a small []byte and call ReturnImage with it.
	res := h.runCode(`
package main

import (
	"context"
	"nexus"
)

func Run(ctx context.Context) (any, error) {
	nexus.ReturnImage([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x00}, "image/png")
	return "ok", nil
}
`)

	if res.Error != "" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
	if len(res.OutputParts) != 1 {
		t.Fatalf("OutputParts len: got %d want 1", len(res.OutputParts))
	}
	part := res.OutputParts[0]
	if part.Type != "image" {
		t.Errorf("part.Type: got %q want image", part.Type)
	}
	if part.MimeType != "image/png" {
		t.Errorf("part.MimeType: got %q want image/png", part.MimeType)
	}
	if !bytes.Equal(part.Data, nexusMinPNG) {
		t.Errorf("part.Data does not match fixture")
	}
	// Default cutoff is 256 KiB; 12-byte payload should ride inline.
	if part.URI != "" {
		t.Errorf("expected inline (empty URI); got URI=%q", part.URI)
	}
}

func TestReturnImage_Blob(t *testing.T) {
	h := newHarness(t, map[string]any{
		"blob_store": map[string]any{
			"inline_threshold": 16,
		},
	})

	// Script generates a payload bigger than the cutoff. Use bytes.Repeat
	// inside the script so the test data path exercises the host
	// runtime end-to-end.
	res := h.runCode(`
package main

import (
	"bytes"
	"context"
	"nexus"
)

func Run(ctx context.Context) (any, error) {
	prefix := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x00}
	body := append(prefix, bytes.Repeat([]byte{0xAA}, 4096)...)
	nexus.ReturnImage(body, "image/png")
	return "ok", nil
}
`)
	if res.Error != "" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
	if len(res.OutputParts) != 1 {
		t.Fatalf("OutputParts len: got %d want 1", len(res.OutputParts))
	}
	part := res.OutputParts[0]
	if part.URI == "" || blobs.SHAFromURI(part.URI) == "" {
		t.Fatalf("expected nexus-blob URI; got %q", part.URI)
	}
	if len(part.Data) != 0 {
		t.Errorf("expected blob path with no inline Data; got %d inline bytes", len(part.Data))
	}
	got, mt, err := h.plugin.blobStore.Get(blobs.SHAFromURI(part.URI))
	if err != nil {
		t.Fatalf("blob store Get: %v", err)
	}
	if mt != "image/png" {
		t.Errorf("blob media type: got %q want image/png", mt)
	}
	if len(got) != 4096+12 {
		t.Errorf("blob bytes len: got %d want %d", len(got), 4096+12)
	}
}

func TestReturnImage_Multiple(t *testing.T) {
	h := newHarness(t, nil)
	res := h.runCode(`
package main

import (
	"context"
	"nexus"
)

func Run(ctx context.Context) (any, error) {
	nexus.ReturnImage([]byte{0x01, 0x02}, "image/png")
	nexus.ReturnImage([]byte{0x03, 0x04}, "image/jpeg")
	return nil, nil
}
`)
	if res.Error != "" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
	if len(res.OutputParts) != 2 {
		t.Fatalf("OutputParts len: got %d want 2", len(res.OutputParts))
	}
	if res.OutputParts[0].MimeType != "image/png" {
		t.Errorf("first MimeType: got %q want image/png", res.OutputParts[0].MimeType)
	}
	if res.OutputParts[1].MimeType != "image/jpeg" {
		t.Errorf("second MimeType: got %q want image/jpeg", res.OutputParts[1].MimeType)
	}
}
