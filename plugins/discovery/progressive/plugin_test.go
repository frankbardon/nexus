package progressive

import (
	"context"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// testBus is a minimal event bus for unit tests.
type testBus struct {
	handlers map[string][]engine.HandlerFunc
	emitted  []emittedEvent
}

type emittedEvent struct {
	eventType string
	payload   any
}

func newTestBus() *testBus {
	return &testBus{handlers: make(map[string][]engine.HandlerFunc)}
}

func (b *testBus) Subscribe(eventType string, handler engine.HandlerFunc, _ ...engine.SubscribeOption) func() {
	b.handlers[eventType] = append(b.handlers[eventType], handler)
	return func() {}
}

func (b *testBus) SubscribeAll(handler engine.HandlerFunc) func() { return func() {} }

func (b *testBus) Emit(eventType string, payload any) error {
	b.emitted = append(b.emitted, emittedEvent{eventType, payload})
	return nil
}

func (b *testBus) EmitEvent(event engine.Event[any]) error {
	b.emitted = append(b.emitted, emittedEvent{event.Type, event.Payload})
	return nil
}

func (b *testBus) EmitAsync(eventType string, payload any) <-chan error {
	ch := make(chan error, 1)
	_ = b.Emit(eventType, payload)
	ch <- nil
	close(ch)
	return ch
}

func (b *testBus) EmitVetoable(eventType string, payload any) (engine.VetoResult, error) {
	return engine.VetoResult{}, nil
}

func (b *testBus) Drain(_ context.Context) error { return nil }

// newTestPlugin creates a plugin with default config for testing.
func newTestPlugin() *Plugin {
	p := New().(*Plugin)
	p.bus = newTestBus()
	p.logger = slog.Default()
	return p
}

func TestToolClassification(t *testing.T) {
	p := newTestPlugin()

	// Register classified tools.
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "read_file", Class: "filesystem", Subclass: "read"},
	})
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "write_file", Class: "filesystem", Subclass: "write"},
	})

	if len(p.classes) != 1 {
		t.Fatalf("expected 1 class, got %d", len(p.classes))
	}
	ci := p.classes["filesystem"]
	if ci == nil {
		t.Fatal("expected filesystem class")
	}
	if ci.toolCount() != 2 {
		t.Errorf("expected 2 tools, got %d", ci.toolCount())
	}
	if len(ci.Subclasses["read"]) != 1 {
		t.Errorf("expected 1 read tool, got %d", len(ci.Subclasses["read"]))
	}
	if len(ci.Subclasses["write"]) != 1 {
		t.Errorf("expected 1 write tool, got %d", len(ci.Subclasses["write"]))
	}
}

func TestSpecialTierTools(t *testing.T) {
	p := newTestPlugin()

	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "ask_user", Class: "communication"},
	})
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "spawn_subagent", Class: "agents"},
	})

	if len(p.specialTools) != 2 {
		t.Fatalf("expected 2 special tools, got %d", len(p.specialTools))
	}
	if len(p.classes) != 0 {
		t.Errorf("special tools should not create classes, got %d", len(p.classes))
	}
}

func TestClasslessTools(t *testing.T) {
	p := newTestPlugin()

	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "custom_tool"},
	})

	if len(p.classlessTools) != 1 {
		t.Fatalf("expected 1 classless tool, got %d", len(p.classlessTools))
	}
}

func TestBeforeLLMRequest_FullDepth(t *testing.T) {
	p := newTestPlugin()
	p.defaultDepth = "full"

	// Register a tool.
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "read_file", Class: "filesystem", Subclass: "read"},
	})

	// Build request with the original tool.
	req := &events.LLMRequest{
		Tools: []events.ToolDef{{Name: "read_file"}},
	}
	vp := &engine.VetoablePayload{Original: req}

	p.handleBeforeLLMRequest(engine.Event[any]{
		Type:    "before:llm.request",
		Payload: vp,
	})

	// full depth = no modification.
	if len(req.Tools) != 1 || req.Tools[0].Name != "read_file" {
		t.Errorf("full depth should not modify tools, got %v", req.Tools)
	}
}

func TestBeforeLLMRequest_ClassDepth(t *testing.T) {
	p := newTestPlugin()

	// Register tools across tiers.
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "ask_user", Class: "communication"},
	})
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "read_file", Description: "Read a file.", Class: "filesystem", Subclass: "read"},
	})
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "write_file", Description: "Write a file.", Class: "filesystem", Subclass: "write"},
	})
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "custom_tool"},
	})

	req := &events.LLMRequest{
		Messages: []events.Message{{Role: "system", Content: "You are helpful."}},
		Tools:    []events.ToolDef{{Name: "all_tools_placeholder"}},
	}
	vp := &engine.VetoablePayload{Original: req}

	p.handleBeforeLLMRequest(engine.Event[any]{
		Type:    "before:llm.request",
		Payload: vp,
	})

	// Should have: ask_user (special), custom_tool (classless+include), discover (meta-tool).
	// filesystem tools should NOT be present.
	toolNames := make(map[string]bool)
	for _, t := range req.Tools {
		toolNames[t.Name] = true
	}

	if !toolNames["ask_user"] {
		t.Error("special tool ask_user should be included")
	}
	if !toolNames["custom_tool"] {
		t.Error("classless tool should be included (default: include)")
	}
	if !toolNames["discover"] {
		t.Error("discover meta-tool should be included")
	}
	if toolNames["read_file"] || toolNames["write_file"] {
		t.Error("classified tools should NOT be included before discovery")
	}
}

func TestBeforeLLMRequest_ClasslessExclude(t *testing.T) {
	p := newTestPlugin()
	p.classlessBehavior = "exclude"

	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "custom_tool"},
	})
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "read_file", Description: "Read.", Class: "filesystem"},
	})

	req := &events.LLMRequest{
		Messages: []events.Message{{Role: "system", Content: "test"}},
		Tools:    []events.ToolDef{},
	}
	vp := &engine.VetoablePayload{Original: req}

	p.handleBeforeLLMRequest(engine.Event[any]{
		Type:    "before:llm.request",
		Payload: vp,
	})

	for _, tool := range req.Tools {
		if tool.Name == "custom_tool" {
			t.Error("classless tool should be excluded when classless_behavior=exclude")
		}
	}
}

func TestDiscoverRevealsTools(t *testing.T) {
	p := newTestPlugin()
	bus := p.bus.(*testBus)

	p.handleToolRegister(engine.Event[any]{
		Type: "tool.register",
		Payload: events.ToolDef{
			Name: "read_file", Description: "Read a file.", Class: "filesystem", Subclass: "read",
			Parameters: map[string]any{"type": "object"},
		},
	})
	p.handleToolRegister(engine.Event[any]{
		Type: "tool.register",
		Payload: events.ToolDef{
			Name: "write_file", Description: "Write a file.", Class: "filesystem", Subclass: "write",
			Parameters: map[string]any{"type": "object"},
		},
	})

	// Discover full class.
	tc := events.ToolCall{
		ID:        "call-1",
		Name:      "discover",
		Arguments: map[string]any{"class": "filesystem"},
	}
	p.handleToolInvoke(engine.Event[any]{Type: "tool.invoke", Payload: tc})

	// Should have emitted tool.result.
	if len(bus.emitted) < 1 {
		t.Fatal("expected tool.result emission")
	}

	// Check revealed state.
	if !p.revealed["filesystem"] {
		t.Error("filesystem class should be marked as revealed")
	}

	// Now a subsequent LLM request should include filesystem tools.
	req := &events.LLMRequest{
		Messages: []events.Message{{Role: "system", Content: "test"}},
		Tools:    []events.ToolDef{},
	}
	vp := &engine.VetoablePayload{Original: req}
	p.handleBeforeLLMRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	found := make(map[string]bool)
	for _, tool := range req.Tools {
		found[tool.Name] = true
	}
	if !found["read_file"] || !found["write_file"] {
		t.Errorf("revealed tools should be in request, got tools: %v", found)
	}
}

func TestDiscoverSubclass(t *testing.T) {
	p := newTestPlugin()

	p.handleToolRegister(engine.Event[any]{
		Type: "tool.register",
		Payload: events.ToolDef{
			Name: "read_file", Description: "Read a file.", Class: "filesystem", Subclass: "read",
			Parameters: map[string]any{"type": "object"},
		},
	})
	p.handleToolRegister(engine.Event[any]{
		Type: "tool.register",
		Payload: events.ToolDef{
			Name: "write_file", Description: "Write a file.", Class: "filesystem", Subclass: "write",
			Parameters: map[string]any{"type": "object"},
		},
	})

	// Discover only "read" subclass.
	tc := events.ToolCall{
		ID:        "call-1",
		Name:      "discover",
		Arguments: map[string]any{"class": "filesystem", "subclass": "read"},
	}
	p.handleToolInvoke(engine.Event[any]{Type: "tool.invoke", Payload: tc})

	if !p.revealed["filesystem.read"] {
		t.Error("filesystem.read should be marked as revealed")
	}

	// LLM request should include read_file but not write_file.
	req := &events.LLMRequest{
		Messages: []events.Message{{Role: "system", Content: "test"}},
		Tools:    []events.ToolDef{},
	}
	vp := &engine.VetoablePayload{Original: req}
	p.handleBeforeLLMRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	found := make(map[string]bool)
	for _, tool := range req.Tools {
		found[tool.Name] = true
	}
	if !found["read_file"] {
		t.Error("read_file should be in request after discover(filesystem.read)")
	}
	if found["write_file"] {
		t.Error("write_file should NOT be in request (only read subclass revealed)")
	}
}

func TestTurnScope(t *testing.T) {
	p := newTestPlugin()
	p.scope = "turn"

	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "read_file", Description: "Read.", Class: "filesystem", Subclass: "read"},
	})

	// Reveal filesystem.
	p.revealed["filesystem"] = true

	// First turn — revealed set gets reset.
	req := &events.LLMRequest{
		Messages: []events.Message{{Role: "system", Content: "test"}},
		Tools:    []events.ToolDef{},
	}
	vp := &engine.VetoablePayload{Original: req}
	p.handleBeforeLLMRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	// After turn scope reset, revealed should be empty.
	if p.revealed["filesystem"] {
		t.Error("turn scope should reset revealed set")
	}

	// Tool should not be in request.
	found := false
	for _, tool := range req.Tools {
		if tool.Name == "read_file" {
			found = true
		}
	}
	if found {
		t.Error("read_file should not be present after turn scope reset")
	}
}

func TestAlwaysInclude(t *testing.T) {
	p := newTestPlugin()
	p.alwaysInclude = []string{"memory"}

	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "memory_write", Description: "Write memory.", Class: "memory"},
	})
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "read_file", Description: "Read.", Class: "filesystem"},
	})

	req := &events.LLMRequest{
		Messages: []events.Message{{Role: "system", Content: "test"}},
		Tools:    []events.ToolDef{},
	}
	vp := &engine.VetoablePayload{Original: req}
	p.handleBeforeLLMRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	found := make(map[string]bool)
	for _, tool := range req.Tools {
		found[tool.Name] = true
	}
	if !found["memory_write"] {
		t.Error("always_include class tools should be in request")
	}
	if found["read_file"] {
		t.Error("non-always-include classified tools should not be in request")
	}
}

func TestSkipInternalRequests(t *testing.T) {
	p := newTestPlugin()

	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "read_file", Class: "filesystem"},
	})

	originalTools := []events.ToolDef{{Name: "read_file"}}
	req := &events.LLMRequest{
		Tools:    originalTools,
		Metadata: map[string]any{"_source": "nexus.planner.dynamic"},
	}
	vp := &engine.VetoablePayload{Original: req}
	p.handleBeforeLLMRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	// Internal request should not be modified.
	if len(req.Tools) != 1 || req.Tools[0].Name != "read_file" {
		t.Error("internal requests should not be modified")
	}
}

func TestToolChoiceForcesReveal(t *testing.T) {
	p := newTestPlugin()

	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "read_file", Description: "Read.", Class: "filesystem", Subclass: "read"},
	})
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "write_file", Description: "Write.", Class: "filesystem", Subclass: "write"},
	})
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "query_db", Description: "Query.", Class: "database"},
	})

	req := &events.LLMRequest{
		ToolChoice: &events.ToolChoice{Mode: "tool", Name: "write_file"},
	}
	vp := &engine.VetoablePayload{Original: req}
	p.handleBeforeLLMRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	names := make(map[string]bool)
	for _, tl := range req.Tools {
		names[tl.Name] = true
	}
	if !names["write_file"] {
		t.Fatal("mandated write_file must be present in tool list")
	}
	if !p.revealed["filesystem.write"] {
		t.Error("filesystem.write subclass should be marked revealed")
	}
	if names["query_db"] {
		t.Error("unrelated class should remain hidden")
	}
	if names["read_file"] {
		t.Error("only the mandated subclass should be revealed, not siblings")
	}
}

func TestToolChoiceNoOpWhenAlreadyVisible(t *testing.T) {
	p := newTestPlugin()

	// Classless tool is visible by default.
	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "custom_tool"},
	})

	req := &events.LLMRequest{
		ToolChoice: &events.ToolChoice{Mode: "tool", Name: "custom_tool"},
	}
	vp := &engine.VetoablePayload{Original: req}
	p.handleBeforeLLMRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	count := 0
	for _, tl := range req.Tools {
		if tl.Name == "custom_tool" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("custom_tool should appear exactly once, got %d", count)
	}
}

func TestToolChoiceUnknownToolIsNoOp(t *testing.T) {
	p := newTestPlugin()

	p.handleToolRegister(engine.Event[any]{
		Type:    "tool.register",
		Payload: events.ToolDef{Name: "read_file", Description: "Read.", Class: "filesystem"},
	})

	req := &events.LLMRequest{
		ToolChoice: &events.ToolChoice{Mode: "tool", Name: "nonexistent"},
	}
	vp := &engine.VetoablePayload{Original: req}
	p.handleBeforeLLMRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	for _, tl := range req.Tools {
		if tl.Name == "nonexistent" {
			t.Fatal("unknown tool should not be synthesized into the list")
		}
	}
	if p.revealed["filesystem"] {
		t.Error("unknown tool mandate must not reveal arbitrary classes")
	}
}

func TestClassSummaryXML(t *testing.T) {
	p := newTestPlugin()

	p.handleToolRegister(engine.Event[any]{
		Type: "tool.register",
		Payload: events.ToolDef{
			Name: "read_file", Description: "Read file contents.", Class: "filesystem", Subclass: "read",
		},
	})
	p.handleToolRegister(engine.Event[any]{
		Type: "tool.register",
		Payload: events.ToolDef{
			Name: "write_file", Description: "Write file contents.", Class: "filesystem", Subclass: "write",
		},
	})

	xml := p.buildClassSummaryXML()
	if xml == "" {
		t.Fatal("expected non-empty class summary XML")
	}
	if !contains(xml, `name="filesystem"`) {
		t.Error("XML should contain filesystem class")
	}
	if !contains(xml, `tools="2"`) {
		t.Error("XML should show 2 tools")
	}
	if !contains(xml, `subclasses="read, write"`) {
		t.Error("XML should list subclasses")
	}
}

func TestGenerateClassDescription(t *testing.T) {
	ci := &classInfo{
		Name: "filesystem",
		Subclasses: map[string][]events.ToolDef{
			"read":  {{Description: "Read file contents."}},
			"write": {{Description: "Write file contents."}},
		},
	}
	desc := generateClassDescription(ci)
	if desc == "" {
		t.Fatal("expected non-empty description")
	}
	if !contains(desc, "Read file contents.") || !contains(desc, "Write file contents.") {
		t.Errorf("description should contain tool descriptions, got: %s", desc)
	}
}

func TestFirstSentence(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"Read the contents of a file. Returns the content.", "Read the contents of a file."},
		{"Simple description", "Simple description."},
		{"Already has period.", "Already has period."},
	}
	for _, tt := range tests {
		got := firstSentence(tt.input)
		if got != tt.want {
			t.Errorf("firstSentence(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDiscoverUnknownClass(t *testing.T) {
	p := newTestPlugin()
	bus := p.bus.(*testBus)

	tc := events.ToolCall{
		ID:        "call-1",
		Name:      "discover",
		Arguments: map[string]any{"class": "nonexistent"},
	}
	p.handleToolInvoke(engine.Event[any]{Type: "tool.invoke", Payload: tc})

	// Should emit error result.
	found := false
	for _, e := range bus.emitted {
		if e.eventType == "tool.result" {
			if r, ok := e.payload.(events.ToolResult); ok && r.Error != "" {
				found = true
			}
		}
	}
	if !found {
		t.Error("discover of unknown class should emit error result")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
