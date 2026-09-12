package dynvars

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newRequestTestPlugin boots a plugin against a real bus and returns a func
// that runs one request through the real before:llm.request path.
func newRequestTestPlugin(t *testing.T, cfg map[string]any) (*Plugin, func(*events.LLMRequest)) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config: cfg,
		Bus:    bus,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })
	return p, func(req *events.LLMRequest) {
		t.Helper()
		if _, err := bus.EmitVetoable("before:llm.request", req); err != nil {
			t.Fatalf("EmitVetoable: %v", err)
		}
	}
}

func userReq(contents ...string) *events.LLMRequest {
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion}
	for i, c := range contents {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		req.Messages = append(req.Messages, events.Message{Role: role, Content: c})
	}
	return req
}

// The headline change: the block rides the user message, not the system prompt.
func TestBlockIsPrependedToTheLastUserMessage(t *testing.T) {
	_, run := newRequestTestPlugin(t, map[string]any{"date": true})

	req := userReq("first", "answer", "second")
	run(req)

	last := req.Messages[2].Content
	if !strings.HasPrefix(last, "<runtime_context>") {
		t.Fatalf("last user message does not open with the block:\n%s", last)
	}
	if !strings.HasSuffix(last, "second") {
		t.Errorf("the user's own text was lost:\n%s", last)
	}
	if strings.Contains(req.Messages[0].Content, "runtime_context") {
		t.Errorf("an earlier user message was decorated:\n%s", req.Messages[0].Content)
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			t.Error("a system message was added; the block belongs on the user message")
		}
	}
}

// The property that makes stale-block cleanup unnecessary: the request is
// decorated, the caller's slice is not, so nothing a memory provider handed us
// can come back carrying the block.
func TestDecorationNeverTouchesTheCallersMessages(t *testing.T) {
	_, run := newRequestTestPlugin(t, map[string]any{"date": true})

	history := []events.Message{
		{Role: "user", Content: "hello"},
	}
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: history}
	run(req)

	if history[0].Content != "hello" {
		t.Fatalf("the caller's message was mutated to %q; a shared backing array would put the block in history", history[0].Content)
	}
	if !strings.Contains(req.Messages[0].Content, "<runtime_context>") {
		t.Error("the request was not decorated")
	}
}

// Ten turns of the same history must never accumulate ten blocks, which is the
// failure mode per-turn injection is usually accused of.
func TestRepeatedTurnsDoNotAccumulateBlocks(t *testing.T) {
	_, run := newRequestTestPlugin(t, map[string]any{"date": true})

	history := []events.Message{{Role: "user", Content: "hello"}}
	for i := 0; i < 10; i++ {
		req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: history}
		run(req)
		if n := strings.Count(req.Messages[0].Content, "<runtime_context>"); n != 1 {
			t.Fatalf("turn %d carried %d blocks, want exactly 1", i, n)
		}
	}
	if strings.Contains(history[0].Content, "runtime_context") {
		t.Fatalf("history accumulated a block: %q", history[0].Content)
	}
}

// A request re-emitted through the hook must not stack a second block.
func TestDecorationIsIdempotentOnOneRequest(t *testing.T) {
	_, run := newRequestTestPlugin(t, map[string]any{"date": true})

	req := userReq("hello")
	run(req)
	run(req)

	if n := strings.Count(req.Messages[0].Content, "<runtime_context>"); n != 1 {
		t.Errorf("re-emitting stacked %d blocks, want 1", n)
	}
}

// A planner or gate repair prompt has no user turn; inventing one would put
// words in the conversation nobody said.
func TestRequestWithNoUserMessageIsLeftAlone(t *testing.T) {
	_, run := newRequestTestPlugin(t, map[string]any{"date": true})

	req := &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Messages: []events.Message{
			{Role: "system", Content: "you are a planner"},
			{Role: "assistant", Content: "ok"},
		},
	}
	run(req)

	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want the original 2", len(req.Messages))
	}
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "runtime_context") {
			t.Errorf("a non-user message was decorated: %+v", m)
		}
	}
}

// Nothing enabled means nothing injected — no empty block.
func TestNoEnabledVariablesInjectsNothing(t *testing.T) {
	_, run := newRequestTestPlugin(t, map[string]any{})

	req := userReq("hello")
	run(req)

	if req.Messages[0].Content != "hello" {
		t.Errorf("content = %q, want it untouched", req.Messages[0].Content)
	}
}

// A multimodal message may be serialized from Parts instead of Content, so the
// block has to be visible on both paths.
func TestMultimodalMessageGetsALeadingTextPart(t *testing.T) {
	_, run := newRequestTestPlugin(t, map[string]any{"date": true})

	original := []events.MessagePart{
		{Type: "image", MimeType: "image/png", Data: []byte{1, 2, 3}},
	}
	req := &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Messages: []events.Message{
			{Role: "user", Content: "what is this?", Parts: original},
		},
	}
	run(req)

	parts := req.Messages[0].Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want the block plus the original", len(parts))
	}
	if parts[0].Type != "text" || !strings.Contains(parts[0].Text, "<runtime_context>") {
		t.Errorf("first part = %+v, want the block as leading text", parts[0])
	}
	if parts[1].Type != "image" {
		t.Errorf("the original part was displaced: %+v", parts[1])
	}
	if len(original) != 1 {
		t.Error("the caller's Parts slice was mutated")
	}
}

// The request headers allowlist still works from its new position.
func TestRequestHeadersReachTheUserMessage(t *testing.T) {
	bus := engine.NewEventBus()
	session, err := engine.NewSessionWorkspace(t.TempDir(), bus)
	if err != nil {
		t.Fatalf("new session workspace: %v", err)
	}
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config:  map[string]any{"request_headers": []any{"timezone"}},
		Bus:     bus,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: session,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })

	if err := session.SetRequestHeaders(map[string]string{"timezone": "Europe/Amsterdam"}); err != nil {
		t.Fatalf("SetRequestHeaders: %v", err)
	}

	req := userReq("hello")
	if _, err := bus.EmitVetoable("before:llm.request", req); err != nil {
		t.Fatalf("EmitVetoable: %v", err)
	}
	if !strings.Contains(req.Messages[0].Content, "Europe/Amsterdam") {
		t.Errorf("the allowlisted header did not reach the user message:\n%s", req.Messages[0].Content)
	}
}
