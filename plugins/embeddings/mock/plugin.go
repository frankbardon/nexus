// Package mock implements a deterministic, zero-I/O embeddings.provider for
// integration tests and offline development. Vectors are derived from a
// SHA-256 hash of the input text so the same text always maps to the same
// vector, and different texts map to stable-but-distinct vectors. No API
// calls, no key required.
//
// Active only when listed in plugins.active (opt-in). Register the factory
// under the id "nexus.embeddings.mock". Mirrors nexus.io.test in being a
// test-only plugin that ships with the main binary.
package mock

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"math"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID       = "nexus.embeddings.mock"
	pluginName     = "Mock Embeddings Provider"
	version        = "0.1.0"
	defaultDim     = 128
	defaultModelID = "mock-embedding"
)

type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	dim    int
	model  string
	unsubs []func()
}

func New() engine.Plugin { return &Plugin{dim: defaultDim, model: defaultModelID} }

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "embeddings.provider",
		Description: "Deterministic mock embeddings for tests (hash-based, no network).",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["dimensions"].(int); ok && v > 0 {
		p.dim = v
	}
	if v, ok := ctx.Config["dimensions"].(float64); ok && v > 0 {
		p.dim = int(v)
	}
	if v, ok := ctx.Config["model"].(string); ok && v != "" {
		p.model = v
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("embeddings.request", p.handle,
			engine.WithPriority(50), engine.WithSource(pluginID)),
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

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{{EventType: "embeddings.request", Priority: 50}}
}

func (p *Plugin) Emissions() []string { return nil }

func (p *Plugin) handle(event engine.Event[any]) {
	req, ok := event.Payload.(*events.EmbeddingsRequest)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID
	req.Model = p.model

	dim := p.dim
	if req.Dimensions > 0 {
		dim = req.Dimensions
	}
	out := make([][]float32, len(req.Texts))
	for i, t := range req.Texts {
		out[i] = hashVector(t, dim)
	}
	req.Vectors = out
	req.Usage = events.EmbeddingsUsage{PromptTokens: 0, TotalTokens: 0}
}

// hashVector produces a deterministic normalized vector of the given
// dimensionality from the SHA-256 hash of text. Expanding the 32-byte hash
// to an arbitrary dim uses HKDF-style chunk extension: each 4-byte block of
// the output is derived from sha256(hash || index). The resulting vector is
// unit-normalized so chromem-go (which requires normalized inputs) accepts it.
func hashVector(text string, dim int) []float32 {
	if dim <= 0 {
		dim = defaultDim
	}
	base := sha256.Sum256([]byte(text))
	vec := make([]float32, dim)
	buf := make([]byte, 4)
	for i := 0; i < dim; i++ {
		// Mix the base hash with the index to extend the stream.
		h := sha256.New()
		h.Write(base[:])
		binary.LittleEndian.PutUint32(buf, uint32(i))
		h.Write(buf)
		sum := h.Sum(nil)
		// Interpret the first 4 bytes as a uint32, map to [-1, 1].
		u := binary.LittleEndian.Uint32(sum[:4])
		vec[i] = float32(u)/float32(math.MaxUint32)*2 - 1
	}
	// Normalize.
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return vec
	}
	inv := float32(1.0 / norm)
	for i := range vec {
		vec[i] *= inv
	}
	return vec
}
