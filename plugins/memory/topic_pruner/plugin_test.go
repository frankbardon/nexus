package topic_pruner

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func newPluginForTest(t *testing.T, embedAvailable bool) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:                  bus,
		logger:               slog.Default(),
		enabled:              true,
		similarityThreshold:  0.55,
		keepLastTopicFull:    true,
		phrases:              append([]string{}, defaultPhrases...),
		embeddingsCapability: embedAvailable,
		lastShiftTurn:        -1,
	}
	for i, ph := range p.phrases {
		p.phrases[i] = lower(ph)
	}
	p.unsubs = append(p.unsubs,
		bus.Subscribe("io.input", p.handleInput, engine.WithPriority(60)),
		bus.Subscribe("agent.turn.end", p.handleTurnEnd, engine.WithPriority(60)),
	)
	return p, bus
}

// lower is a tiny helper so we don't pull strings into the test header.
func lower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out[i] = c
	}
	return string(out)
}

func TestExplicitPhraseShift(t *testing.T) {
	_, bus := newPluginForTest(t, false)

	shifts := 0
	bus.Subscribe("memory.topic_shift_detected", func(_ engine.Event[any]) { shifts++ })

	bus.Emit("io.input", events.UserInput{SchemaVersion: events.UserInputVersion, Content: "Different question: how does Go GC work?"})

	if shifts != 1 {
		t.Fatalf("expected 1 shift event, got %d", shifts)
	}
}

func TestPhraseInMiddleStillCounts(t *testing.T) {
	_, bus := newPluginForTest(t, false)

	shifts := 0
	bus.Subscribe("memory.topic_shift_detected", func(_ engine.Event[any]) { shifts++ })

	bus.Emit("io.input", events.UserInput{SchemaVersion: events.UserInputVersion, Content: "OK got it. On a different note can you check the build?"})

	if shifts != 1 {
		t.Fatalf("expected shift via 'on a different note', got %d", shifts)
	}
}

func TestNoShiftOnTopicalContinuation(t *testing.T) {
	_, bus := newPluginForTest(t, false)

	shifts := 0
	bus.Subscribe("memory.topic_shift_detected", func(_ engine.Event[any]) { shifts++ })

	bus.Emit("io.input", events.UserInput{SchemaVersion: events.UserInputVersion, Content: "Continue with the previous task please."})

	if shifts != 0 {
		t.Fatalf("expected no shift, got %d", shifts)
	}
}

func TestSameTurnDebounce(t *testing.T) {
	_, bus := newPluginForTest(t, false)

	shifts := 0
	bus.Subscribe("memory.topic_shift_detected", func(_ engine.Event[any]) { shifts++ })

	// Two phrase-triggering inputs in the same turn — second should debounce.
	bus.Emit("io.input", events.UserInput{SchemaVersion: events.UserInputVersion, Content: "different question: x"})
	bus.Emit("io.input", events.UserInput{SchemaVersion: events.UserInputVersion, Content: "new topic: y"})

	if shifts != 1 {
		t.Fatalf("expected debounce to suppress duplicate shift, got %d", shifts)
	}
}

func TestSchemaVersionPropagated(t *testing.T) {
	_, bus := newPluginForTest(t, false)

	var captured events.MemoryTopicShiftDetected
	got := false
	bus.Subscribe("memory.topic_shift_detected", func(e engine.Event[any]) {
		if v, ok := e.Payload.(events.MemoryTopicShiftDetected); ok {
			captured = v
			got = true
		}
	})

	bus.Emit("io.input", events.UserInput{SchemaVersion: events.UserInputVersion, Content: "different topic: z"})

	if !got {
		t.Fatalf("did not capture shift event")
	}
	if captured.SchemaVersion != events.MemoryTopicShiftDetectedVersion {
		t.Fatalf("schema version mismatch: %d", captured.SchemaVersion)
	}
}
