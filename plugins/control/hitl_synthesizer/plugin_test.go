package hitlsynthesizer

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// stubProvider is a minimal LLM provider stand-in. It subscribes to
// llm.request and emits an llm.response carrying the configured
// content (and copying back the synthesizer correlation metadata so
// the synthesizer's response handler can match it). When `fail` is
// true it skips the emit, simulating the core.error path.
type stubProvider struct {
	bus     engine.EventBus
	content string
	fail    bool
	mu      sync.Mutex
	seen    []events.LLMRequest
}

func newStubProvider(bus engine.EventBus, content string) *stubProvider {
	sp := &stubProvider{bus: bus, content: content}
	bus.Subscribe("llm.request", sp.handle)
	return sp
}

func (s *stubProvider) handle(e engine.Event[any]) {
	req, ok := e.Payload.(events.LLMRequest)
	if !ok {
		return
	}
	source, _ := req.Metadata["_source"].(string)
	if source != llmSource {
		return
	}
	s.mu.Lock()
	s.seen = append(s.seen, req)
	fail := s.fail
	content := s.content
	s.mu.Unlock()
	if fail {
		return
	}
	corrID, _ := req.Metadata["_synth_id"].(string)
	_ = s.bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: content,
		Metadata: map[string]any{
			"_source":   llmSource,
			"_synth_id": corrID,
		},
	})
}

// newTestPlugin constructs a Plugin wired to an in-process bus with
// the synthesizer's three subscriptions registered. We bypass Init so
// the test can drive the plugin without a full PluginContext (no
// session, no config — defaults applied directly).
func newTestPlugin(t *testing.T) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.modelRole = "haiku"
	p.maxActionRefChars = defaultMaxActionRefChars
	p.cacheEnabled = true
	p.fallbackTemplateRaw = defaultFallbackTemplate
	tmpl, err := template.New("fallback").Parse(p.fallbackTemplateRaw)
	if err != nil {
		t.Fatalf("parse fallback: %v", err)
	}
	p.fallbackTemplate = tmpl

	bus.Subscribe("hitl.requested", p.handleHITLRequested,
		engine.WithPriority(-100))
	bus.Subscribe("before:hitl.requested", p.handleBeforeHITLRequested,
		engine.WithPriority(-100))
	bus.Subscribe("llm.response", p.handleLLMResponse,
		engine.WithPriority(50))
	return p, bus
}

func TestSynthesize_CacheMiss_LLMRender(t *testing.T) {
	_, bus := newTestPlugin(t)
	stub := newStubProvider(bus, "Approve running ls in /tmp?")

	req := &events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "h1",
		ActionKind:        "tool.invoke",
		PromptSynthesizer: CapabilityName,
		ActionRef: map[string]any{
			"tool":    "shell",
			"command": "ls /tmp",
		},
	}
	if err := bus.Emit("hitl.requested", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Prompt != "Approve running ls in /tmp?" {
		t.Fatalf("expected Prompt to be set from LLM, got %q", req.Prompt)
	}
	if len(stub.seen) != 1 {
		t.Fatalf("expected one llm.request, got %d", len(stub.seen))
	}
	// Verify the synthesizer's user message embedded the action ref.
	if got := stub.seen[0].Messages[1].Content; !strings.Contains(got, "ls /tmp") {
		t.Errorf("user prompt missing action ref detail: %q", got)
	}
}

func TestSynthesize_CacheHit_NoLLMCall(t *testing.T) {
	p, bus := newTestPlugin(t)
	stub := newStubProvider(bus, "should not be used")

	// Pre-seed the cache for the same (kind, ref) shape.
	actionKind := "tool.invoke"
	actionRef := map[string]any{"tool": "shell", "command": "ls /tmp"}
	key := buildCacheKey(actionKind, actionRef)
	p.cacheMu.Lock()
	p.cache[key] = "Cached: ok to ls?"
	p.cacheMu.Unlock()

	req := &events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "h2",
		ActionKind:        actionKind,
		PromptSynthesizer: CapabilityName,
		ActionRef:         actionRef,
	}
	if err := bus.Emit("hitl.requested", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Prompt != "Cached: ok to ls?" {
		t.Fatalf("expected cached prompt, got %q", req.Prompt)
	}
	if len(stub.seen) != 0 {
		t.Fatalf("expected no llm.request on cache hit, got %d", len(stub.seen))
	}
}

func TestSynthesize_LLMFailure_FallbackTemplate(t *testing.T) {
	p, bus := newTestPlugin(t)
	// Override fallback so the test asserts a custom message + template
	// substitution, not the default.
	tmpl, err := template.New("fallback").Parse("FB: {{.action_kind}}")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p.fallbackTemplate = tmpl
	p.fallbackTemplateRaw = "FB: {{.action_kind}}"

	stub := newStubProvider(bus, "")
	stub.fail = true

	req := &events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "h3",
		ActionKind:        "memory.longterm.write",
		PromptSynthesizer: CapabilityName,
		ActionRef:         map[string]any{"key": "user_pref/theme"},
	}
	if err := bus.Emit("hitl.requested", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Prompt != "FB: memory.longterm.write" {
		t.Fatalf("expected fallback prompt, got %q", req.Prompt)
	}
	// Fallback responses are not cached (synthesis returned err).
	p.cacheMu.RLock()
	gotEntries := len(p.cache)
	p.cacheMu.RUnlock()
	if gotEntries != 0 {
		t.Errorf("fallback path should not populate cache, got %d entries", gotEntries)
	}
}

func TestSynthesize_NoSynthesizerSet_NoOp(t *testing.T) {
	_, bus := newTestPlugin(t)
	stub := newStubProvider(bus, "should not run")

	req := &events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "h4",
		ActionKind: "tool.invoke",
		// PromptSynthesizer intentionally empty.
		ActionRef: map[string]any{"tool": "shell"},
	}
	if err := bus.Emit("hitl.requested", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Prompt != "" {
		t.Errorf("expected Prompt to remain empty, got %q", req.Prompt)
	}
	if len(stub.seen) != 0 {
		t.Errorf("synthesizer should not call LLM when opt-out, got %d", len(stub.seen))
	}
}

func TestSynthesize_PromptAlreadySet_NoOp(t *testing.T) {
	_, bus := newTestPlugin(t)
	stub := newStubProvider(bus, "should not run")

	req := &events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "h5",
		ActionKind:        "tool.invoke",
		PromptSynthesizer: CapabilityName,
		Prompt:            "Already set by emitter",
		ActionRef:         map[string]any{"tool": "shell"},
	}
	if err := bus.Emit("hitl.requested", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Prompt != "Already set by emitter" {
		t.Errorf("prompt should remain unchanged, got %q", req.Prompt)
	}
	if len(stub.seen) != 0 {
		t.Errorf("synthesizer should not call LLM when prompt is set, got %d", len(stub.seen))
	}
}

func TestSynthesize_BeforeVetoablePath(t *testing.T) {
	_, bus := newTestPlugin(t)
	stub := newStubProvider(bus, "Vetoable path prompt")

	req := &events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "h6",
		ActionKind:        "tool.invoke",
		PromptSynthesizer: CapabilityName,
		ActionRef:         map[string]any{"tool": "shell", "command": "rm -rf /"},
	}
	veto, err := bus.EmitVetoable("before:hitl.requested", req)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if veto.Vetoed {
		t.Fatalf("synthesizer must never veto, got: %s", veto.Reason)
	}
	if req.Prompt != "Vetoable path prompt" {
		t.Fatalf("expected vetoable mutation, got %q", req.Prompt)
	}
	if len(stub.seen) != 1 {
		t.Errorf("expected 1 llm.request from vetoable path, got %d", len(stub.seen))
	}
}

func TestSynthesize_ValuePayloadIgnored(t *testing.T) {
	_, bus := newTestPlugin(t)
	stub := newStubProvider(bus, "should not run")

	// Emit by VALUE — the synthesizer can't carry mutations forward
	// through a value payload, so it must skip cleanly without making
	// LLM calls.
	req := events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "h7",
		ActionKind:        "tool.invoke",
		PromptSynthesizer: CapabilityName,
		ActionRef:         map[string]any{"tool": "shell"},
	}
	if err := bus.Emit("hitl.requested", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(stub.seen) != 0 {
		t.Errorf("synthesizer should skip value payloads, got %d llm.request emits", len(stub.seen))
	}
}

// TestSynthesize_NonToolEmitter_BeforePath simulates the canonical
// Option B emission flow used by the approval_policy gate and the
// memory/internal/approval helper: the emitter calls EmitVetoable on
// before:hitl.requested first (so the synthesizer can mutate Prompt),
// then the value-payload Emit on hitl.requested for IO consumers. The
// test asserts synthesis fires for an approval-gate-shaped action even
// though the emitter is not the ask_user tool.
func TestSynthesize_NonToolEmitter_BeforePath(t *testing.T) {
	_, bus := newTestPlugin(t)
	stub := newStubProvider(bus, "Approve memory write to user_pref/theme?")

	// IO-side stand-in: subscribe to the non-vetoable hitl.requested form
	// so we can assert the value emission carries the synthesized prompt.
	var ioSeen events.HITLRequest
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.HITLRequest); ok {
			ioSeen = r
		}
	}, engine.WithPriority(50))

	req := events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "approval-1",
		RequesterPlugin:   "nexus.gate.approval_policy",
		ActionKind:        "memory.longterm.write",
		PromptSynthesizer: CapabilityName,
		ActionRef: map[string]any{
			"key":   "user_pref/theme",
			"value": "dark",
		},
	}

	veto, err := bus.EmitVetoable("before:hitl.requested", &req)
	if err != nil {
		t.Fatalf("emit before:hitl.requested: %v", err)
	}
	if veto.Vetoed {
		t.Fatalf("synthesizer must never veto, got reason=%q", veto.Reason)
	}
	if req.Prompt != "Approve memory write to user_pref/theme?" {
		t.Fatalf("expected synthesized prompt on req, got %q", req.Prompt)
	}

	if err := bus.Emit("hitl.requested", req); err != nil {
		t.Fatalf("emit hitl.requested: %v", err)
	}
	if ioSeen.Prompt != "Approve memory write to user_pref/theme?" {
		t.Fatalf("expected IO subscriber to see synthesized prompt, got %q", ioSeen.Prompt)
	}
	if len(stub.seen) != 1 {
		t.Errorf("expected 1 llm.request from non-tool emitter, got %d", len(stub.seen))
	}
}

func TestBuildCacheKey_DeterministicAndPartitioned(t *testing.T) {
	a := buildCacheKey("tool.invoke", map[string]any{"a": 1, "b": 2})
	b := buildCacheKey("tool.invoke", map[string]any{"b": 2, "a": 1}) // same content, different literal order
	if a != b {
		t.Fatalf("cache key not deterministic across map iteration order: %s vs %s", a, b)
	}
	c := buildCacheKey("memory.longterm.write", map[string]any{"a": 1, "b": 2})
	if a == c {
		t.Fatalf("different action kinds must produce different keys, got %s for both", a)
	}
	if !strings.HasPrefix(a, "tool.invoke/") {
		t.Errorf("expected key to be partitioned by action kind, got %q", a)
	}
}

// interceptor stands in for a before:llm.request handler that owns the
// answer (fanout role, test IO mock): it intercepts the synthesis request
// and, when answer is set, publishes the response from another goroutine
// after the publish has returned — first a fanout leg sharing the
// RequestID, then the merged response, whose _source is the interceptor's
// own (as fanout's is), so only RequestID correlation can match it.
func interceptor(bus engine.EventBus, answer string, block bool) {
	bus.Subscribe("before:llm.request", func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		req, _ := vp.Original.(*events.LLMRequest)
		if req == nil {
			return
		}
		if block {
			vp.Veto = engine.VetoResult{Vetoed: true, Reason: "blocked by test"}
			return
		}
		engine.InterceptLLMRequest(vp, req, "test.interceptor", "intercepted")
		if answer == "" {
			return
		}
		id := req.RequestID
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion,
				RequestID: id, Content: "leg answer",
				Metadata: map[string]any{"_source": "test.interceptor", engine.MetaFanoutProvider: "a"},
			})
			_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion,
				RequestID: id, Content: answer,
				Metadata: map[string]any{"_source": "test.interceptor"},
			})
		}()
	})
}

func synthRequest(id string) *events.HITLRequest {
	return &events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: id,
		ActionKind:        "tool.invoke",
		PromptSynthesizer: CapabilityName,
		ActionRef:         map[string]any{"tool": "shell", "command": "rm -rf /tmp/" + id},
	}
}

func TestSynthesize_InterceptedRequest_WaitsForAsyncResponse(t *testing.T) {
	_, bus := newTestPlugin(t)
	interceptor(bus, "Delete the tmp dir?", false)

	req := synthRequest("h-int")
	if err := bus.Emit("hitl.requested", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Prompt != "Delete the tmp dir?" {
		t.Fatalf("expected the intercepted answer, got %q", req.Prompt)
	}
}

func TestSynthesize_InterceptedRequest_ShutdownStopsWait(t *testing.T) {
	p, bus := newTestPlugin(t)
	interceptor(bus, "", false) // intercepts, never answers

	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = p.Shutdown(context.Background())
	}()
	req := synthRequest("h-cancel")
	done := make(chan struct{})
	go func() {
		_ = bus.Emit("hitl.requested", req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("synthesis kept waiting after Shutdown cancelled its context")
	}
	if req.Prompt != "Approve action: tool.invoke" {
		t.Fatalf("expected fallback prompt, got %q", req.Prompt)
	}
}

func TestSynthesize_BlockedRequest_FallsBackAtOnce(t *testing.T) {
	_, bus := newTestPlugin(t)
	interceptor(bus, "", true)

	req := synthRequest("h-block")
	start := time.Now()
	if err := bus.Emit("hitl.requested", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("blocked request waited %s; a block veto must fail at once", time.Since(start))
	}
	if req.Prompt != "Approve action: tool.invoke" {
		t.Fatalf("expected fallback prompt, got %q", req.Prompt)
	}
}
