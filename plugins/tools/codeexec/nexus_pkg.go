package codeexec

import (
	"reflect"
	"sync"

	"github.com/traefik/yaegi/interp"
)

// imageBuffer collects images emitted by a script via nexus.ReturnImage.
// One buffer per run_code invocation; appended via the script-callable
// closure built in buildNexusExports. The plugin's runScript flushes
// this buffer onto the resulting ToolResult.OutputParts after Run
// completes (success or failure — emits whatever was already captured).
//
// Mutex-protected because parallel.* primitives may legitimately call
// nexus.ReturnImage from multiple goroutines spawned by the script.
type imageBuffer struct {
	mu     sync.Mutex
	images []capturedImage
}

// capturedImage is the in-memory record of a single nexus.ReturnImage
// call. We hold the raw bytes and the script-supplied mimeType until
// runScript's tail decides whether to inline or route through the blob
// store; doing the routing here would couple nexus_pkg.go to the blob
// store and force tests that exercise the buffer to also configure a
// blob store.
type capturedImage struct {
	Data     []byte
	MimeType string
}

func (b *imageBuffer) Add(data []byte, mimeType string) {
	if len(data) == 0 {
		return
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	// Defensive copy — the script might mutate its slice after the call.
	dup := make([]byte, len(data))
	copy(dup, data)
	b.mu.Lock()
	b.images = append(b.images, capturedImage{Data: dup, MimeType: mimeType})
	b.mu.Unlock()
}

func (b *imageBuffer) Snapshot() []capturedImage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]capturedImage, len(b.images))
	copy(out, b.images)
	return out
}

// buildNexusExports returns a Yaegi package at import path "nexus" that
// exposes a single helper:
//
//	nexus.ReturnImage(data []byte, mimeType string)
//
// Scripts call it to attach an image to the resulting ToolResult.OutputParts
// (alongside the JSON return value from main.Run). One typical use case:
// a script reads a chart-rendering tool's bytes and wants the next LLM turn
// to see the rendered chart, not just a description.
//
// The function is wired to the per-invocation buffer so multiple calls
// stack in script order, and so a panic mid-script doesn't lose previous
// captures (runScript still reads buf.Snapshot() in its tail).
func buildNexusExports(buf *imageBuffer) interp.Exports {
	pkg := map[string]reflect.Value{
		"ReturnImage": reflect.ValueOf(func(data []byte, mimeType string) {
			buf.Add(data, mimeType)
		}),
	}
	return interp.Exports{"nexus/nexus": pkg}
}
