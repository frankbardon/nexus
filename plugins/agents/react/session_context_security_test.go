package react

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// TestSessionContext_ReservedKeyNeverLeaksIntoPrompt is one of four sibling
// tests forming a single, deliberate cross-cutting regression suite (story
// E3-S4) proving one security-relevant invariant across every prompt-
// consuming surface this effort (arc-per-turn-identity) added session
// context to: a reserved ("_"-prefixed) session label — in particular
// _principal_id, the identity value AG-UI binds via
// SessionWorkspace.SetReservedLabel — must NEVER reach LLM-visible prompt
// text, even though it lives in the exact same SessionMeta.Labels map as a
// general, intentionally-exposed label (here "team"). This is the concrete
// enforcement of the "must never be conflatable" requirement between
// identity (Ask 1) and context (Ask 2) in
// .planning/arc-per-turn-identity/interview.md.
//
// The four sibling files, one per prompt-consuming surface, are kept
// physically separate rather than merged into one because three of the four
// (react/orchestrator/subagent) drive unexported plugin state — p.session,
// orchestrator's p.originalInput + sendSynthesizeRequest — that is only
// reachable from inside each plugin's own package; see this story's
// FOLLOWUPS for the concrete reason a single file isn't possible. All four
// are deliberately named identically (Go test names are scoped per package)
// and use the identical label pair ("team"/"acme" general,
// "_principal_id"/"user-42" reserved) so the four runs read as one table:
//
//   - plugins/agents/react/session_context_security_test.go (this file)
//   - plugins/agents/orchestrator/session_context_security_test.go (decompose + synthesis paths)
//   - plugins/agents/subagent/session_context_security_test.go
//   - plugins/workflows/icm/runtime/session_context_security_test.go (OperatorTemplateCtx + PayloadBuilder)
//
// A future change that lets a reserved key leak into react's <session_context>
// system-prompt section (E3-S2) breaks THIS test — unambiguously, by name,
// as a security regression rather than an incidental prompt-format change.
func TestSessionContext_ReservedKeyNeverLeaksIntoPrompt(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(), contract.WithPluginConfig(map[string]any{
		"system_prompt": "Base system prompt.",
	}))
	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	if err := p.session.SetLabel("team", "acme"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if err := p.session.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}

	h.Inject("io.input", events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "hello",
		SessionID:     "sess-1",
	})

	req := findLLMRequest(t, h)
	sys := systemMessage(t, req)

	if !strings.Contains(sys, "team: acme") {
		t.Errorf("system prompt missing general label: %s", sys)
	}
	if strings.Contains(sys, "user-42") || strings.Contains(sys, "_principal_id") {
		t.Errorf("SECURITY REGRESSION: react system prompt leaked reserved label: %s", sys)
	}
}
