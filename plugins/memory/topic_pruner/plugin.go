// Package topic_pruner detects topic shifts in the conversation and emits
// MemoryTopicShiftDetected when the user appears to have changed subjects.
//
// Two signals are combined:
//
//   - Explicit phrase matching ("different question", "new topic",
//     "let's move on", "moving on", etc.) — cheap, deterministic.
//   - Embedding similarity drop — runs only when the embeddings.provider
//     capability is active. Compares the latest user input against a
//     rolling centroid of the current topic's user inputs; a similarity
//     below the configured threshold flags a shift.
//
// Both signals are journalled via the bus event so replay is deterministic
// even though the classifier itself is heuristic. The pruner does not
// itself rewrite history — it surfaces the shift so other plugins
// (summary buffer, compaction) can react. This separation keeps the
// pruner cheap and avoids stepping on the existing summary_buffer
// machinery.
package topic_pruner

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.memory.topic_pruner"
	pluginName = "Topic-Aware Pruner"
	version    = "0.1.0"

	signalPhrase   = "phrase"
	signalEmbed    = "embedding"
	signalExplicit = "user_explicit"
)

// defaultPhrases captures common topic-shift cues. Lowercased; matched via
// case-insensitive substring contains. Operators can replace via config.
var defaultPhrases = []string{
	"different question",
	"different topic",
	"new topic",
	"new question",
	"let's move on",
	"moving on",
	"change of subject",
	"switching gears",
	"unrelated:",
	"separately,",
	"on a different note",
}

// Plugin implements the topic-aware pruner.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	enabled              bool
	similarityThreshold  float64
	keepLastTopicFull    bool
	phrases              []string
	embeddingsCapability bool

	mu               sync.Mutex
	turn             int
	currentTopicTurn int
	// centroid is the rolling mean embedding of the current topic's user
	// turns. Reset whenever a shift is detected.
	centroid     []float32
	centroidSize int
	// lastShiftTurn debounces consecutive shift signals — back-to-back
	// shifts within the same turn batch are noise.
	lastShiftTurn int

	unsubs []func()
}

// New creates a new topic_pruner plugin.
func New() engine.Plugin {
	return &Plugin{
		enabled:             true,
		similarityThreshold: 0.55,
		keepLastTopicFull:   true,
		phrases:             append([]string{}, defaultPhrases...),
		lastShiftTurn:       -1,
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }
func (p *Plugin) Capabilities() []engine.Capability {
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.input", Priority: 60},
		{EventType: "agent.turn.end", Priority: 60},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"memory.topic_shift_detected",
		"memory.curated",
		"embeddings.request",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["enabled"].(bool); ok {
		p.enabled = v
	}
	if v, ok := ctx.Config["similarity_threshold"].(float64); ok {
		p.similarityThreshold = v
	}
	if v, ok := ctx.Config["keep_last_topic_full"].(bool); ok {
		p.keepLastTopicFull = v
	}
	if list, ok := ctx.Config["explicit_phrases"].([]any); ok {
		phrases := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok && s != "" {
				phrases = append(phrases, strings.ToLower(s))
			}
		}
		p.phrases = phrases
	} else {
		// Normalise defaults to lowercase up front.
		for i, ph := range p.phrases {
			p.phrases[i] = strings.ToLower(ph)
		}
	}

	// Detect whether an embeddings.provider is registered. The capability
	// snapshot is point-in-time at boot; if no provider is active the
	// pruner falls back to phrase-only signal.
	if providers := ctx.Capabilities["embeddings.provider"]; len(providers) > 0 {
		p.embeddingsCapability = true
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.input", p.handleInput,
			engine.WithPriority(60), engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd,
			engine.WithPriority(60), engine.WithSource(pluginID)),
	)

	p.logger.Info("topic_pruner initialized",
		"enabled", p.enabled,
		"similarity_threshold", p.similarityThreshold,
		"embeddings_capability", p.embeddingsCapability,
	)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, u := range p.unsubs {
		u()
	}
	return nil
}

// handleInput runs the classifier on every user input.
func (p *Plugin) handleInput(e engine.Event[any]) {
	if !p.enabled {
		return
	}
	in, ok := e.Payload.(events.UserInput)
	if !ok {
		return
	}
	text := strings.TrimSpace(in.Content)
	if text == "" {
		return
	}

	// Phrase signal — cheapest first.
	if signal := p.matchPhrase(text); signal != "" {
		p.recordShift(signal, 0)
		return
	}

	// Embedding signal — only when a provider is registered.
	if !p.embeddingsCapability {
		// Still track the input so when an embeddings provider does
		// arrive (hot reload), we have something to compare against.
		return
	}

	vec, ok := p.embed(text)
	if !ok {
		return
	}

	p.mu.Lock()
	if p.centroid == nil {
		p.centroid = append([]float32{}, vec...)
		p.centroidSize = 1
		p.mu.Unlock()
		return
	}
	sim := cosine(vec, p.centroid)
	if sim < p.similarityThreshold {
		// Reset centroid for the new topic.
		p.centroid = append([]float32{}, vec...)
		p.centroidSize = 1
		p.mu.Unlock()
		p.recordShift(signalEmbed, sim)
		return
	}
	// Update rolling mean.
	for i := range p.centroid {
		p.centroid[i] = (p.centroid[i]*float32(p.centroidSize) + vec[i]) / float32(p.centroidSize+1)
	}
	p.centroidSize++
	p.mu.Unlock()
}

func (p *Plugin) handleTurnEnd(e engine.Event[any]) {
	if !p.enabled {
		return
	}
	if _, ok := e.Payload.(events.TurnInfo); !ok {
		return
	}
	p.mu.Lock()
	p.turn++
	p.mu.Unlock()
}

// matchPhrase returns the signal kind ("phrase" or "user_explicit") when
// the text contains any configured cue. We treat the leading-anchor
// variants ("unrelated:", "separately,") as user_explicit since they're
// stronger signals; the rest are "phrase".
func (p *Plugin) matchPhrase(text string) string {
	low := strings.ToLower(text)
	for _, ph := range p.phrases {
		if strings.HasPrefix(low, ph) {
			return signalExplicit
		}
		if strings.Contains(low, ph) {
			return signalPhrase
		}
	}
	return ""
}

// recordShift emits the bus events for an observed topic boundary,
// debouncing same-turn duplicate signals.
func (p *Plugin) recordShift(signal string, similarity float64) {
	p.mu.Lock()
	if p.turn == p.lastShiftTurn {
		p.mu.Unlock()
		return
	}
	from := p.currentTopicTurn
	p.lastShiftTurn = p.turn
	p.currentTopicTurn = p.turn
	now := p.turn
	p.mu.Unlock()

	_ = p.bus.Emit("memory.topic_shift_detected", events.MemoryTopicShiftDetected{
		SchemaVersion: events.MemoryTopicShiftDetectedVersion,
		FromTurn:      from,
		ToTurn:        now,
		Similarity:    similarity,
		Signal:        signal,
	})
	_ = p.bus.Emit("memory.curated", events.MemoryCurated{
		SchemaVersion: events.MemoryCuratedVersion,
		Layer:         "topic_pruner",
		SectionsTouched: []events.CurationSection{{
			SectionID:   pluginID + "/shift",
			Kind:        "session",
			TokensDelta: 0,
		}},
		CacheInvalidates: false,
		AtTurn:           now,
	})
}

// embed asks the resolved embeddings provider to encode a single text.
// Returns false when the provider failed or refused so the caller can
// fall back to phrase-only behaviour.
func (p *Plugin) embed(text string) ([]float32, bool) {
	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Texts:         []string{text},
	}
	if err := p.bus.Emit("embeddings.request", req); err != nil {
		return nil, false
	}
	if req.Error != "" || len(req.Vectors) == 0 || len(req.Vectors[0]) == 0 {
		return nil, false
	}
	return req.Vectors[0], true
}

// cosine returns the cosine similarity of two equal-length vectors.
// Returns 0 when a vector is zero-length or all-zero (caller should treat
// as "below threshold" — same as far apart).
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x := float64(a[i])
		y := float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
