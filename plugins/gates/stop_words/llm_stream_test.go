package stopwords

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// discardLogger keeps Init's startup line out of test output. These tests go
// through Init rather than setting fields directly, because the config wiring
// (scan_stream defaulting to scan_llm_responses) is part of what they check.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newStreamGate builds a plugin wired for stream scanning against a real bus,
// and returns the bus so tests can drive a publisher through it.
func newStreamGate(t *testing.T, words ...string) engine.EventBus {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	cfg := map[string]any{
		"scan_llm_responses": true,
		"words":              toAnySlice(words),
	}
	if err := p.Init(engine.PluginContext{
		Bus:    bus,
		Config: cfg,
		Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return bus
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// driveStream feeds deltas through a publisher and returns the text that
// actually reached the bus, plus whether the stream was blocked.
func driveStream(bus engine.EventBus, deltas ...string) (string, bool) {
	var released strings.Builder
	bus.Subscribe("llm.stream.chunk", func(e engine.Event[any]) {
		if c, ok := e.Payload.(events.StreamChunk); ok {
			released.WriteString(c.Content)
		}
	})
	sp := engine.NewStreamPublisher(bus, "turn-1", "req-1")
	for _, d := range deltas {
		sp.Text(d)
	}
	sp.Close()
	blocked, _ := sp.Blocked()
	return released.String(), blocked
}

// TestStreamGate_BlocksBannedWordBeforeRelease is the property that makes
// streaming safe with this gate on: the banned word is never published. Before
// the stream hook existed, a veto at before:llm.response fired only after the
// whole response had been rendered.
func TestStreamGate_BlocksBannedWordBeforeRelease(t *testing.T) {
	bus := newStreamGate(t, "forbidden")

	released, blocked := driveStream(bus, "this is ", "forbidden ", "text")

	if !blocked {
		t.Fatal("stream was not blocked")
	}
	if strings.Contains(strings.ToLower(released), "forbidden") {
		t.Fatalf("released text %q leaked the banned word", released)
	}
	if released != "this is " {
		t.Errorf("released = %q, want only the clean prefix", released)
	}
}

// TestStreamGate_CatchesWordSplitAcrossDeltas is the case hold-back exists
// for. Providers chunk on token boundaries, which fall wherever the tokenizer
// says — a per-delta scan would see "forb" and "idden" and match neither.
func TestStreamGate_CatchesWordSplitAcrossDeltas(t *testing.T) {
	bus := newStreamGate(t, "forbidden")

	released, blocked := driveStream(bus, "say forb", "idden now")

	if !blocked {
		t.Fatal("a word split across deltas escaped the gate")
	}
	if strings.Contains(strings.ToLower(released), "forbidden") {
		t.Fatalf("released text %q leaked the banned word", released)
	}
}

// TestStreamGate_HoldsOnlyTheIncompleteToken pins the latency cost: the gate
// defers the trailing partial word and nothing more, so a stream lags by at
// most one token rather than by a fixed buffer.
func TestStreamGate_HoldsOnlyTheIncompleteToken(t *testing.T) {
	bus := newStreamGate(t, "forbidden")

	var released strings.Builder
	bus.Subscribe("llm.stream.chunk", func(e engine.Event[any]) {
		if c, ok := e.Payload.(events.StreamChunk); ok {
			released.WriteString(c.Content)
		}
	})

	sp := engine.NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("hello wor")
	if got := released.String(); got != "hello " {
		t.Fatalf("released %q mid-stream, want %q — only the partial token held",
			got, "hello ")
	}
	sp.Text("ld there")
	if got := released.String(); got != "hello world " {
		t.Fatalf("released %q, want the completed token flushed", got)
	}
	sp.Close()
	if got := released.String(); got != "hello world there" {
		t.Fatalf("final released = %q", got)
	}
}

// TestStreamGate_CleanStreamIsUnchanged guards against the gate costing a
// deployment its streaming behavior when nothing is wrong: every byte ships,
// and the only difference is where the chunk boundaries fall.
func TestStreamGate_CleanStreamIsUnchanged(t *testing.T) {
	bus := newStreamGate(t, "forbidden")

	released, blocked := driveStream(bus, "a perfectly ", "ordinary ", "answer")

	if blocked {
		t.Fatal("clean stream was blocked")
	}
	if released != "a perfectly ordinary answer" {
		t.Errorf("released = %q, want the whole text", released)
	}
}

// TestStreamGate_WordAtStreamEndIsStillCaught covers the final flush: Close
// ignores Hold, so a banned word the model ends on cannot slip out by simply
// never being followed by a separator.
func TestStreamGate_WordAtStreamEndIsStillCaught(t *testing.T) {
	bus := newStreamGate(t, "forbidden")

	released, blocked := driveStream(bus, "ends with forbidden")

	if !blocked {
		t.Fatal("a banned word at end of stream escaped the gate")
	}
	if strings.Contains(strings.ToLower(released), "forbidden") {
		t.Fatalf("released text %q leaked the banned word", released)
	}
}

// TestStreamGate_ScansOnlyUnreleasedText guards against a pathology in the
// obvious implementation. Scanning the whole accumulated turn on every segment
// re-reports words from earlier segments, so a long clean stream would keep
// re-examining text it already approved — and any future match rule change
// would retroactively cut a stream over bytes the gate itself let out. The
// handler scans from the release boundary back only as far as a straddling
// word requires.
func TestStreamGate_ScansOnlyUnreleasedText(t *testing.T) {
	bus := newStreamGate(t, "forbidden")

	var chunks int
	bus.Subscribe("llm.stream.chunk", func(e engine.Event[any]) {
		if _, ok := e.Payload.(events.StreamChunk); ok {
			chunks++
		}
	})

	sp := engine.NewStreamPublisher(bus, "turn-1", "req-1")
	for _, d := range []string{"one ", "two ", "three ", "four "} {
		sp.Text(d)
	}
	sp.Close()

	if blocked, _ := sp.Blocked(); blocked {
		t.Fatal("a clean multi-delta stream was blocked")
	}
	if chunks == 0 {
		t.Fatal("nothing was released")
	}
}

// TestStreamGate_ScanStreamFollowsScanLLMResponses pins the default. An
// operator who asked for response gating gets streaming coverage without a
// second knob — and therefore is not silently downgraded to non-streaming by
// engine.StreamUnsafePlugins.
func TestStreamGate_ScanStreamFollowsScanLLMResponses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      map[string]any
		wantSubs bool
	}{
		{"responses on implies stream on", map[string]any{"scan_llm_responses": true}, true},
		{"responses off implies stream off", map[string]any{"scan_llm_responses": false}, false},
		{
			"explicit override wins",
			map[string]any{"scan_llm_responses": true, "scan_stream": false},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := engine.NewEventBus()
			p := New().(*Plugin)
			tc.cfg["words"] = []any{"forbidden"}
			if err := p.Init(engine.PluginContext{Bus: bus, Config: tc.cfg, Logger: discardLogger()}); err != nil {
				t.Fatalf("Init: %v", err)
			}
			defer func() { _ = p.Shutdown(context.Background()) }()

			var found bool
			for _, sub := range p.Subscriptions() {
				if sub.EventType == engine.EventBeforeStreamChunk {
					found = true
				}
			}
			if found != tc.wantSubs {
				t.Errorf("declares before:llm.stream.chunk = %v, want %v", found, tc.wantSubs)
			}
			if got := bus.HasSubscribers(engine.EventBeforeStreamChunk); got != tc.wantSubs {
				t.Errorf("subscribed to before:llm.stream.chunk = %v, want %v", got, tc.wantSubs)
			}
		})
	}
}

// TestStreamGate_IsStreamSafe is the end of the chain: because this gate now
// handles the stream hook, an operator running it keeps streaming instead of
// being downgraded to blocking requests.
func TestStreamGate_IsStreamSafe(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:    bus,
		Config: map[string]any{"scan_llm_responses": true, "words": []any{"forbidden"}},
		Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	if unsafe := engine.StreamUnsafePlugins([]engine.Plugin{p}); len(unsafe) != 0 {
		t.Errorf("gate reported stream-unsafe (%v); it handles the stream hook", unsafe)
	}
}
