package contentsafety

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func streamTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newStreamPlugin wires the gate for stream scanning against a real bus. These
// tests go through Init because the config wiring — scan_stream defaulting to
// scan_llm_responses, stream_hold_back parsing — is part of what they check.
func newStreamPlugin(t *testing.T, cfg map[string]any) engine.EventBus {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	if _, ok := cfg["scan_llm_responses"]; !ok {
		cfg["scan_llm_responses"] = true
	}
	if err := p.Init(engine.PluginContext{Bus: bus, Config: cfg, Logger: streamTestLogger()}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return bus
}

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

// TestStreamGate_BlocksSSNBeforeRelease is the headline property: a social
// security number never reaches the bus, so no transport has to retract it and
// no user reads it. Under the response-only gate this text was fully rendered
// before the veto fired.
func TestStreamGate_BlocksSSNBeforeRelease(t *testing.T) {
	bus := newStreamPlugin(t, map[string]any{"action": "block"})

	released, blocked := driveStream(bus, "the number is ", "123-45-6789", " ok")

	if !blocked {
		t.Fatal("stream was not blocked")
	}
	if strings.Contains(released, "123-45-6789") {
		t.Fatalf("released %q leaked the SSN", released)
	}
}

// TestStreamGate_CatchesPatternSplitAcrossDeltas is what the trailing-run hold
// is for. Providers chunk wherever their tokenizer lands, so a card number
// arrives in pieces; scanning each delta alone would match none of them.
func TestStreamGate_CatchesPatternSplitAcrossDeltas(t *testing.T) {
	bus := newStreamPlugin(t, map[string]any{"action": "block"})

	released, blocked := driveStream(bus, "card 4111", "1111", "1111", "1111 end")

	if !blocked {
		t.Fatal("a card number split across deltas escaped the gate")
	}
	if strings.Contains(released, "4111111111111111") {
		t.Fatalf("released %q leaked the card number", released)
	}
}

// TestStreamGate_RedactsInPlace covers the degraded-but-recovered path: the
// stream continues, with the match rewritten rather than the turn cut.
func TestStreamGate_RedactsInPlace(t *testing.T) {
	bus := newStreamPlugin(t, map[string]any{"action": "redact"})

	released, blocked := driveStream(bus, "mail me at ", "bob@example.com", " today")

	if blocked {
		t.Fatal("redact mode blocked the stream")
	}
	if strings.Contains(released, "bob@example.com") {
		t.Fatalf("released %q leaked the address", released)
	}
	if !strings.Contains(released, "[REDACTED]") {
		t.Fatalf("released %q carries no redaction marker", released)
	}
	if !strings.Contains(released, "mail me at") || !strings.Contains(released, "today") {
		t.Errorf("released %q lost the surrounding text", released)
	}
}

// TestStreamGate_CleanStreamShipsWhole guards the common case: a response with
// nothing to hide streams in full, and the gate costs it only the lag of one
// partial word.
func TestStreamGate_CleanStreamShipsWhole(t *testing.T) {
	bus := newStreamPlugin(t, map[string]any{"action": "block"})

	released, blocked := driveStream(bus, "a perfectly ", "ordinary ", "answer")

	if blocked {
		t.Fatal("clean stream was blocked")
	}
	if released != "a perfectly ordinary answer" {
		t.Errorf("released = %q, want the whole text", released)
	}
}

// TestStreamGate_HoldsOnlyTheTrailingRun pins the latency cost. A fixed buffer
// would lag the stream by a sentence or two; holding the trailing
// non-whitespace run lags it by a partial word, which is all a token-shaped
// pattern can straddle.
func TestStreamGate_HoldsOnlyTheTrailingRun(t *testing.T) {
	bus := newStreamPlugin(t, map[string]any{"action": "block"})

	var released strings.Builder
	bus.Subscribe("llm.stream.chunk", func(e engine.Event[any]) {
		if c, ok := e.Payload.(events.StreamChunk); ok {
			released.WriteString(c.Content)
		}
	})

	sp := engine.NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("hello wor")
	if got := released.String(); got != "hello " {
		t.Fatalf("released %q, want %q — only the partial word held", got, "hello ")
	}
	sp.Close()
	if got := released.String(); got != "hello wor" {
		t.Fatalf("final released = %q", got)
	}
}

// TestStreamGate_CatchesWhitespaceSpanningPattern pins that detection does
// not depend on the hold. A phrase spanning spaces begins in text already
// released, yet the match still completes inside the pending region and is
// still caught — because the scan runs over the cumulative turn and acts on
// where a match ENDS, not on where it starts.
func TestStreamGate_CatchesWhitespaceSpanningPattern(t *testing.T) {
	cfg := map[string]any{
		"action":          "block",
		"custom_patterns": []any{`launch\s+code\s+alpha`},
	}
	bus := newStreamPlugin(t, cfg)

	released, blocked := driveStream(bus, "the launch ", "code ", "alpha is set")

	if !blocked {
		t.Fatal("a whitespace-spanning pattern escaped the gate")
	}
	if strings.Contains(released, "alpha") {
		t.Errorf("released %q leaked the final token of the match", released)
	}
}

// TestStreamGate_HoldBackFloorShrinksTheResidue shows what stream_hold_back
// actually buys, which is not detection but a smaller leak. Without a floor
// the leading words of a whitespace-spanning phrase ship before the match
// completes; with one wide enough to cover the phrase, nothing does.
func TestStreamGate_HoldBackFloorShrinksTheResidue(t *testing.T) {
	deltas := []string{"the launch ", "code ", "alpha is set"}
	pattern := []any{`launch\s+code\s+alpha`}

	loose := newStreamPlugin(t, map[string]any{
		"action": "block", "custom_patterns": pattern,
	})
	looseText, looseBlocked := driveStream(loose, deltas...)

	tight := newStreamPlugin(t, map[string]any{
		"action": "block", "custom_patterns": pattern, "stream_hold_back": 64,
	})
	tightText, tightBlocked := driveStream(tight, deltas...)

	if !looseBlocked || !tightBlocked {
		t.Fatal("both configurations should block")
	}
	if len(tightText) >= len(looseText) {
		t.Errorf("hold-back floor released %d bytes, no better than the %d without it",
			len(tightText), len(looseText))
	}
	if strings.Contains(tightText, "launch") {
		t.Errorf("with a floor wide enough, nothing of the phrase should ship; got %q", tightText)
	}
}

// TestStreamGate_RedactEscalatesOnStraddle covers the case redaction cannot
// honor. When a match begins in text that already shipped, rewriting the
// remaining half would leave the user with a partly-leaked value labelled as
// redacted. The gate blocks instead.
func TestStreamGate_RedactEscalatesOnStraddle(t *testing.T) {
	cfg := map[string]any{
		"action":          "redact",
		"custom_patterns": []any{`launch\s+code\s+alpha`},
	}
	bus := newStreamPlugin(t, cfg)

	released, blocked := driveStream(bus, "the launch ", "code ", "alpha is set")

	if !blocked {
		t.Fatal("redact mode should escalate to a block when a match straddles the release boundary")
	}
	if strings.Contains(released, "alpha") {
		t.Errorf("released %q leaked the tail of an unredactable match", released)
	}
}

// TestStreamGate_RedactsEveryMatchInOneSegment guards the span-based rewrite
// against the leftmost-match trap: regexp.FindStringIndex returns one match,
// and acting only on it would leave later ones in the same delta untouched.
func TestStreamGate_RedactsEveryMatchInOneSegment(t *testing.T) {
	bus := newStreamPlugin(t, map[string]any{"action": "redact"})

	released, blocked := driveStream(bus, "mail a@b.com and c@d.com now")

	if blocked {
		t.Fatal("redact mode blocked the stream")
	}
	if strings.Contains(released, "a@b.com") || strings.Contains(released, "c@d.com") {
		t.Errorf("released %q left an address unredacted", released)
	}
	if n := strings.Count(released, "[REDACTED]"); n != 2 {
		t.Errorf("released %q has %d redaction markers, want 2", released, n)
	}
}

// TestStreamGate_IgnoresMatchesAlreadyReleased pins the other half of the
// leftmost-match fix. Scanning the cumulative turn means an earlier match sits
// in the window forever; acting on it again every segment would cut a clean
// stream over bytes the gate already allowed out.
func TestStreamGate_IgnoresMatchesAlreadyReleased(t *testing.T) {
	bus := newStreamPlugin(t, map[string]any{"action": "redact"})

	released, blocked := driveStream(bus,
		"first a@b.com ", "then some ", "more ", "ordinary ", "words here")

	if blocked {
		t.Fatal("stream was blocked over a match that had already been redacted")
	}
	if n := strings.Count(released, "[REDACTED]"); n != 1 {
		t.Errorf("released %q has %d markers, want exactly 1 — the match must not be re-redacted",
			released, n)
	}
	if !strings.Contains(released, "words here") {
		t.Errorf("released %q lost the clean tail", released)
	}
}

// TestStreamGate_ScanStreamFollowsScanLLMResponses pins the default, which is
// what keeps an operator who asked for response gating from being silently
// downgraded to non-streaming requests.
func TestStreamGate_ScanStreamFollowsScanLLMResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want bool
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
			if err := p.Init(engine.PluginContext{Bus: bus, Config: tc.cfg, Logger: streamTestLogger()}); err != nil {
				t.Fatalf("Init: %v", err)
			}
			defer func() { _ = p.Shutdown(context.Background()) }()

			var declared bool
			for _, sub := range p.Subscriptions() {
				if sub.EventType == engine.EventBeforeStreamChunk {
					declared = true
				}
			}
			if declared != tc.want {
				t.Errorf("declares before:llm.stream.chunk = %v, want %v", declared, tc.want)
			}
			if got := bus.HasSubscribers(engine.EventBeforeStreamChunk); got != tc.want {
				t.Errorf("subscribed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStreamGate_IsStreamSafe closes the loop: because the gate handles the
// stream hook, the engine leaves streaming on instead of downgrading every
// request to a blocking call.
func TestStreamGate_IsStreamSafe(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:    bus,
		Config: map[string]any{"scan_llm_responses": true},
		Logger: streamTestLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	if unsafe := engine.StreamUnsafePlugins([]engine.Plugin{p}); len(unsafe) != 0 {
		t.Errorf("gate reported stream-unsafe (%v); it handles the stream hook", unsafe)
	}
}

// TestStreamHoldFrom covers the hold rule directly, including the floor.
func TestStreamHoldFrom(t *testing.T) {
	tests := []struct {
		name    string
		content string
		floor   int
		want    int
	}{
		{"trailing word is held", "hello wor", 0, 6},
		{"a trailing space means nothing is pending", "hello ", 0, 6},
		{"no whitespace at all holds everything", "abcdef", 0, 0},
		{"empty content", "", 0, 0},
		{"floor widens the hold past the word boundary", "hello world", 8, 3},
		{"floor larger than content clamps to zero", "hi", 99, 0},
		{"floor narrower than the run does not shrink it", "hello worldwide", 2, 6},
		{"newline counts as a separator", "line\nabc", 0, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := streamHoldFrom(tt.content, tt.floor); got != tt.want {
				t.Errorf("streamHoldFrom(%q, %d) = %d, want %d",
					tt.content, tt.floor, got, tt.want)
			}
		})
	}
}
