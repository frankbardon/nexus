package fanout

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// mergedResponse finalizes a fanout whose intercepted request carried reqMeta
// and returns the merged llm.response it published.
func mergedResponse(t *testing.T, reqMeta map[string]any) events.LLMResponse {
	t.Helper()
	p, bus := newTestPlugin()

	a := makeResponse("nexus.llm.anthropic", "claude", "A", 0.01)
	b := makeResponse("nexus.llm.openai", "gpt", "B", 0.02)
	// Providers echo the leg's request Metadata, so a leg response carries the
	// original request's keys alongside the fanout markers, plus whatever the
	// provider adds itself.
	for k, v := range reqMeta {
		if k == engine.MetaInterceptedBy || k == engine.MetaSource {
			continue
		}
		a.Metadata[k] = v
		b.Metadata[k] = v
	}
	a.Metadata["thinking_blocks"] = "provider-added"

	state := &fanoutState{
		role:      "compare",
		strategy:  StrategyAll,
		request:   events.LLMRequest{Role: "compare", Metadata: reqMeta},
		responses: []events.LLMResponse{a, b},
		done:      make(chan struct{}),
	}
	p.inflight["test-fanout"] = state
	p.finalize("test-fanout", state)

	for _, e := range bus.emitted() {
		if e.Type != "llm.response" {
			continue
		}
		if r, ok := e.Payload.(events.LLMResponse); ok {
			return r
		}
	}
	t.Fatal("no merged llm.response emitted")
	return events.LLMResponse{}
}

func TestMergedResponse_RestoresOriginalSource(t *testing.T) {
	resp := mergedResponse(t, map[string]any{
		engine.MetaSource:        "nexus.agent.react",
		"turn_id":                "turn-7",
		engine.MetaInterceptedBy: pluginID,
	})

	if got := resp.Metadata[engine.MetaSource]; got != "nexus.agent.react" {
		t.Errorf("_source = %v, want nexus.agent.react", got)
	}
	if got := resp.Metadata["turn_id"]; got != "turn-7" {
		t.Errorf("turn_id = %v, want turn-7", got)
	}
	if got := resp.Metadata["thinking_blocks"]; got != "provider-added" {
		t.Errorf("provider-added key lost: thinking_blocks = %v", got)
	}
	if engine.IsFanoutLegResponse(resp.Metadata) {
		t.Error("merged response still carries the leg marker")
	}
	for _, k := range []string{"_target_provider", engine.MetaInterceptedBy} {
		if _, ok := resp.Metadata[k]; ok {
			t.Errorf("merged response carries %s", k)
		}
	}
	if resp.Metadata["_fanout"] != true || resp.Metadata["_fanout_id"] != "test-fanout" {
		t.Errorf("merged markers missing: %v", resp.Metadata)
	}
	if len(resp.Alternatives) != 1 || !engine.IsFanoutLegResponse(resp.Alternatives[0].Metadata) {
		t.Errorf("alternatives should keep their leg markers: %+v", resp.Alternatives)
	}
}

func TestMergedResponse_NoSourceWhenRequestHadNone(t *testing.T) {
	resp := mergedResponse(t, map[string]any{engine.MetaInterceptedBy: pluginID})
	if v, ok := resp.Metadata[engine.MetaSource]; ok {
		t.Errorf("_source = %v, want absent", v)
	}

	resp = mergedResponse(t, nil)
	if v, ok := resp.Metadata[engine.MetaSource]; ok {
		t.Errorf("nil request metadata: _source = %v, want absent", v)
	}
}
