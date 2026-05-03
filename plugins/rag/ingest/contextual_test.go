package ingest

import (
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// stubProvider answers llm.request with a canned prefix while echoing the
// request metadata so the contextualizer's correlation works.
type stubProvider struct {
	bus      engine.EventBus
	prefix   string
	calls    atomic.Int32
	failNext atomic.Bool
}

func (s *stubProvider) install() func() {
	return s.bus.Subscribe("llm.request", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.LLMRequest)
		if !ok {
			return
		}
		s.calls.Add(1)
		if s.failNext.CompareAndSwap(true, false) {
			// Don't emit anything — exercise the timeout path.
			return
		}
		_ = s.bus.Emit("llm.response", events.LLMResponse{
			Content:  s.prefix,
			Metadata: req.Metadata,
		})
	}, engine.WithPriority(50))
}

func newCtxr(t *testing.T, enabled bool, prefix string) (*contextualizer, *stubProvider, func()) {
	t.Helper()
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cacheDir := t.TempDir()
	ctxr, err := newContextualizer(bus, logger, map[string]any{
		"enabled":              enabled,
		"max_chars_doc_window": 1000,
		"max_chars_prefix":     200,
		"timeout_ms":           500,
	}, cacheDir)
	if err != nil {
		t.Fatalf("newContextualizer: %v", err)
	}
	stub := &stubProvider{bus: bus, prefix: prefix}
	unsub := stub.install()
	return ctxr, stub, func() {
		unsub()
		ctxr.close()
	}
}

func TestContextualizerDisabledReturnsEmpty(t *testing.T) {
	ctxr, stub, cleanup := newCtxr(t, false, "any")
	t.Cleanup(cleanup)

	got := ctxr.Prefix("doc context here", "chunk text")
	if got != "" {
		t.Fatalf("disabled contextualizer should return empty, got %q", got)
	}
	if stub.calls.Load() != 0 {
		t.Fatalf("disabled contextualizer should not hit LLM, got %d calls", stub.calls.Load())
	}
}

func TestContextualizerGeneratesPrefix(t *testing.T) {
	ctxr, stub, cleanup := newCtxr(t, true, "  This is the situating context.\n")
	t.Cleanup(cleanup)

	got := ctxr.Prefix("doc context here", "chunk text")
	if got != "This is the situating context." {
		t.Fatalf("Prefix = %q, want trimmed paragraph", got)
	}
	if stub.calls.Load() != 1 {
		t.Fatalf("expected 1 LLM call, got %d", stub.calls.Load())
	}
}

func TestContextualizerCachesPrefix(t *testing.T) {
	ctxr, stub, cleanup := newCtxr(t, true, "cached prefix")
	t.Cleanup(cleanup)

	_ = ctxr.Prefix("doc", "chunk")
	_ = ctxr.Prefix("doc", "chunk")
	if stub.calls.Load() != 1 {
		t.Fatalf("expected 1 LLM call (second hits cache), got %d", stub.calls.Load())
	}
}

func TestContextualizerCacheKeyDistinctOnContext(t *testing.T) {
	ctxr, stub, cleanup := newCtxr(t, true, "prefix")
	t.Cleanup(cleanup)

	_ = ctxr.Prefix("doc-A", "chunk")
	_ = ctxr.Prefix("doc-B", "chunk")
	if stub.calls.Load() != 2 {
		t.Fatalf("different doc context should bypass cache, got %d calls", stub.calls.Load())
	}
}

func TestContextualizerHandlesTimeout(t *testing.T) {
	ctxr, stub, cleanup := newCtxr(t, true, "anything")
	t.Cleanup(cleanup)

	stub.failNext.Store(true)
	got := ctxr.Prefix("doc", "chunk")
	if got != "" {
		t.Fatalf("timeout should return empty, got %q", got)
	}
}

func TestContextualizerTruncatesPrefix(t *testing.T) {
	long := strings.Repeat("x", 1000)
	ctxr, _, cleanup := newCtxr(t, true, long)
	t.Cleanup(cleanup)

	got := ctxr.Prefix("doc", "chunk")
	if len(got) > 200 {
		t.Fatalf("prefix not truncated to max_chars_prefix=200, got len %d", len(got))
	}
}
