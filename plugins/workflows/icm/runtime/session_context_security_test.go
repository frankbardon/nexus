package runtime

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/posture"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
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
//   - plugins/workflows/icm/runtime/session_context_security_test.go (this file: OperatorTemplateCtx + PayloadBuilder)
//   - plugins/agents/react/session_context_security_test.go
//   - plugins/agents/orchestrator/session_context_security_test.go (decompose + synthesis paths)
//   - plugins/agents/subagent/session_context_security_test.go
//
// A future change that lets a reserved key leak into ICM's operator template
// (OperatorTemplateCtx.Context, E3-S1) or its per-turn payload
// (PayloadBuilder's <session_context> block, E3-S5) breaks THIS test —
// unambiguously, by name, as a security regression rather than an incidental
// prompt-format change.
func TestSessionContext_ReservedKeyNeverLeaksIntoPrompt(t *testing.T) {
	sess, err := engine.NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	if err := sess.SetLabel("team", "acme"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if err := sess.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}

	t.Run("OperatorTemplateCtx", func(t *testing.T) {
		wf := newWorkflow(t, `{{ .Stage.ID }} team={{ .Context.team }}`, "doc\n")
		stage := newStage("01_draft")

		b := &PostureBuilder{
			Workflow:   wf,
			InstanceID: "nexus.workflows.icm",
			Registry:   posture.NewRegistry(),
			Session:    sess,
		}

		got, err := b.Build(stage)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !strings.Contains(got.SystemPrompt, "team=acme") {
			t.Errorf("SystemPrompt missing general label: %s", got.SystemPrompt)
		}
		if strings.Contains(got.SystemPrompt, "user-42") || strings.Contains(got.SystemPrompt, "_principal_id") {
			t.Errorf("SECURITY REGRESSION: OperatorTemplateCtx leaked reserved label into prompt: %s", got.SystemPrompt)
		}
	})

	t.Run("PayloadBuilder", func(t *testing.T) {
		f := setup(t)
		b := &PayloadBuilder{Workflow: f.workflow, Session: f.session, EngineSession: sess}
		stage := &workspace.Stage{ID: "01_draft", Folder: f.stageDir, Role: "drafter"}

		out, err := b.Build(PayloadInputs{Stage: stage, Turn: 1})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !strings.Contains(out, "team: acme") {
			t.Errorf("payload missing general label: %s", out)
		}
		if strings.Contains(out, "user-42") || strings.Contains(out, "_principal_id") {
			t.Errorf("SECURITY REGRESSION: PayloadBuilder leaked reserved label into payload: %s", out)
		}
	})
}
