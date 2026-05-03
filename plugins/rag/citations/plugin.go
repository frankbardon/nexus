// Package citations parses and structures source attributions in LLM
// responses that follow a retrieval pass. Two paths:
//
//   - "tag" (default): the system prompt instructs the model to wrap cited
//     spans with `<cite source="..." chunk="N"/>` markers. The plugin
//     parses the tags, validates each against the current turn's retrieval
//     context, strips tags from user-visible text, and emits a structured
//     CitedResponse on `llm.response.cited`.
//
//   - "anthropic_native": when the LLM provider already populates
//     LLMResponse.Citations[], reuse those structured spans instead of
//     parsing tags.
//
// Either way the user-visible Text on the emitted CitedResponse is free of
// any in-band citation markup so IO renderers can footnote without
// post-processing.
package citations

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.rag.citations"
	pluginName = "RAG Citations"
	version    = "0.1.0"

	defaultMode            = "auto"
	defaultSectionPriority = 60
)

// Plugin parses citation tags out of LLM responses and emits the structured
// counterpart event.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	mode            string // "tag" | "anthropic_native" | "auto"
	strict          bool
	sectionPriority int

	prompts *engine.PromptRegistry

	mu    sync.Mutex
	turns map[string]map[chunkKey]events.RetrievedChunk // per-turn retrieval index

	unsubs []func()
}

// chunkKey identifies a retrieved chunk for citation validation. Source is
// often empty when the retrieval emitter only carries DocID; we treat
// (Source, DocID, ChunkIdx) as the canonical key with empty fields as
// wildcards on lookup.
type chunkKey struct {
	source   string
	docID    string
	chunkIdx string
}

func New() engine.Plugin {
	return &Plugin{
		mode:            defaultMode,
		strict:          true,
		sectionPriority: defaultSectionPriority,
		turns:           make(map[string]map[chunkKey]events.RetrievedChunk),
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "rag.citations",
		Description: "Structured citation extraction from LLM responses (tag parsing + Anthropic native).",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.prompts = ctx.Prompts

	if v, ok := ctx.Config["mode"].(string); ok && v != "" {
		switch strings.ToLower(v) {
		case "tag", "anthropic_native", "auto":
			p.mode = strings.ToLower(v)
		default:
			return fmt.Errorf("rag/citations: unknown mode %q (want tag|anthropic_native|auto)", v)
		}
	}
	if v, ok := ctx.Config["strict"].(bool); ok {
		p.strict = v
	}
	if v, ok := ctx.Config["section_priority"].(int); ok {
		p.sectionPriority = v
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("rag.retrieved", p.handleRetrieved,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleResponse,
			engine.WithPriority(80), engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd,
			engine.WithPriority(99), engine.WithSource(pluginID)),
	)

	if p.prompts != nil && (p.mode == "tag" || p.mode == "auto") {
		p.prompts.Register(pluginID, p.sectionPriority, p.buildPromptSection)
	}
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.prompts != nil {
		p.prompts.Unregister(pluginID)
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "rag.retrieved", Priority: 50},
		{EventType: "llm.response", Priority: 80},
		{EventType: "agent.turn.end", Priority: 99},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"llm.response.cited"}
}

// handleRetrieved indexes the chunks each retrieval call returned for the
// turn. The citation parser consults this on llm.response to validate
// references — ids that point at non-retrieved chunks are stripped (in
// strict mode) or kept with a TrustTier="unverified" tag (otherwise).
func (p *Plugin) handleRetrieved(event engine.Event[any]) {
	rc, ok := event.Payload.(events.RetrievalContext)
	if !ok {
		return
	}
	if rc.TurnID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.turns[rc.TurnID]
	if !ok {
		idx = make(map[chunkKey]events.RetrievedChunk)
		p.turns[rc.TurnID] = idx
	}
	for _, c := range rc.Chunks {
		idx[chunkKey{source: c.Source, docID: c.DocID, chunkIdx: c.ChunkIdx}] = c
	}
}

// handleResponse runs after every llm.response. Tag-mode responses get
// regex-parsed; Anthropic-native responses get read directly. Either way
// llm.response.cited fires with the structured CitationRefs.
func (p *Plugin) handleResponse(event engine.Event[any]) {
	resp, ok := event.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	turnID, _ := resp.Metadata["turn_id"].(string)
	if turnID == "" {
		// Some upstream callers stash the turn under a different key. Be
		// permissive — emit anyway with empty TurnID so renderers can use
		// the cited text even when correlation is missing.
	}

	mode := p.mode
	if mode == "auto" {
		if len(resp.Citations) > 0 {
			mode = "anthropic_native"
		} else {
			mode = "tag"
		}
	}

	var cited events.CitedResponse
	switch mode {
	case "anthropic_native":
		cited = p.fromAnthropicNative(resp, turnID)
	default:
		cited = p.fromTags(resp, turnID)
	}

	if len(cited.Citations) == 0 && cited.Text == resp.Content {
		// Nothing parsed and no transformation — skip the redundant emit.
		return
	}
	_ = p.bus.Emit("llm.response.cited", cited)
}

// handleTurnEnd discards per-turn retrieval indices so long-running sessions
// do not leak memory.
func (p *Plugin) handleTurnEnd(event engine.Event[any]) {
	info, ok := event.Payload.(events.TurnInfo)
	if !ok {
		return
	}
	p.mu.Lock()
	delete(p.turns, info.TurnID)
	p.mu.Unlock()
}

// citePattern matches `<cite source="..." chunk="N"/>` and the equivalent
// non-self-closing variant. Attributes can appear in either order; chunk is
// optional (some uses cite a whole document). The non-greedy capture allows
// path-style sources containing `/` (only `>` is excluded so the closing
// bracket terminates the tag deterministically).
var citePattern = regexp.MustCompile(`<cite\s+([^>]*?)\s*/?>(?:</cite>)?`)
var attrPattern = regexp.MustCompile(`(\w+)\s*=\s*"([^"]*)"`)

func (p *Plugin) fromTags(resp events.LLMResponse, turnID string) events.CitedResponse {
	matches := citePattern.FindAllStringSubmatchIndex(resp.Content, -1)
	if len(matches) == 0 {
		return events.CitedResponse{TurnID: turnID, Text: resp.Content, Mode: "tag"}
	}

	allow := p.snapshot(turnID)
	var (
		out   strings.Builder
		cites []events.CitationRef
		last  int
	)
	for _, m := range matches {
		start, end := m[0], m[1]
		out.WriteString(resp.Content[last:start])
		attrsStart, attrsEnd := m[2], m[3]
		attrs := parseAttrs(resp.Content[attrsStart:attrsEnd])
		ref := events.CitationRef{
			Source:    attrs["source"],
			DocID:     attrs["doc"],
			ChunkIdx:  attrs["chunk"],
			Snippet:   attrs["snippet"],
			SpanStart: out.Len(),
			SpanEnd:   out.Len(),
		}
		if !p.allow(ref, allow) {
			if p.strict {
				// Strict: drop both the tag and the citation.
				last = end
				continue
			}
			ref.TrustTier = "unverified"
		}
		cites = append(cites, ref)
		last = end
	}
	out.WriteString(resp.Content[last:])

	return events.CitedResponse{
		TurnID:    turnID,
		Text:      out.String(),
		Citations: cites,
		Mode:      "tag",
	}
}

func (p *Plugin) fromAnthropicNative(resp events.LLMResponse, turnID string) events.CitedResponse {
	cites := make([]events.CitationRef, 0, len(resp.Citations))
	for _, c := range resp.Citations {
		cites = append(cites, events.CitationRef{
			Source:    c.DocumentTitle,
			Snippet:   c.CitedText,
			SpanStart: c.StartCharIndex,
			SpanEnd:   c.EndCharIndex,
		})
	}
	return events.CitedResponse{
		TurnID:    turnID,
		Text:      resp.Content,
		Citations: cites,
		Mode:      "anthropic_native",
	}
}

// allow validates a CitationRef against the per-turn retrieval index. A
// reference matches when (Source, DocID, ChunkIdx) all match a recorded
// chunk; missing fields on the recorded chunk are treated as wildcards so
// retrieval emitters that only carry one identifier still work.
func (p *Plugin) allow(ref events.CitationRef, allow map[chunkKey]events.RetrievedChunk) bool {
	if len(allow) == 0 {
		// No retrieval context recorded — we cannot validate. Tag-mode
		// strict still drops; non-strict keeps the citation untrusted.
		return false
	}
	for k, c := range allow {
		if ref.Source != "" && k.source != "" && ref.Source != k.source {
			continue
		}
		if ref.DocID != "" && k.docID != "" && ref.DocID != k.docID {
			continue
		}
		if ref.ChunkIdx != "" && k.chunkIdx != "" && ref.ChunkIdx != k.chunkIdx {
			continue
		}
		// At least one identifier must be supplied by the citation.
		if ref.Source == "" && ref.DocID == "" && ref.ChunkIdx == "" {
			return false
		}
		_ = c
		return true
	}
	return false
}

func (p *Plugin) snapshot(turnID string) map[chunkKey]events.RetrievedChunk {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.turns[turnID]
	if idx == nil {
		return nil
	}
	out := make(map[chunkKey]events.RetrievedChunk, len(idx))
	for k, v := range idx {
		out[k] = v
	}
	return out
}

func parseAttrs(s string) map[string]string {
	out := make(map[string]string)
	for _, m := range attrPattern.FindAllStringSubmatch(s, -1) {
		out[strings.ToLower(m[1])] = m[2]
	}
	return out
}

// buildPromptSection registers a system-prompt instruction telling the model
// to mark cited spans with the `<cite/>` tag format the parser expects.
// Attached at sectionPriority (60 by default) so it lands after most plugin
// sections but before any agent-specific overrides.
func (p *Plugin) buildPromptSection() string {
	return `<citation_contract>
When you use facts from retrieved knowledge chunks in your response, mark each
cited span with a self-closing XML tag immediately after the sentence that
relies on it:

  <cite source="<source path or url>" chunk="<chunk_idx>"/>

The source and chunk values must match the metadata of a chunk that was
returned by knowledge_search or memory recall during this turn. Do not
fabricate citations. Citations without retrieval evidence will be stripped.
</citation_contract>`
}
