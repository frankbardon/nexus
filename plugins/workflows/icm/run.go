package icm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/session"
)

// runWorkflow is the io.input entry point. It generates a runID,
// creates a session under the plugin's per-instance dataDir, copies
// any initial inputs into 00_input/, emits plan.created, builds an
// orchestrator from the workflow + plugin state, runs the workflow,
// and emits a final io.output with either the final stage's artifact
// content or an error notice.
func (p *Plugin) runWorkflow(in events.UserInput) {
	runID := engine.GenerateID()
	turnID := engine.GenerateID()

	sess, err := session.NewSession(p.dataDir, runID, p.logger)
	if err != nil {
		p.logger.Error("icm: session create failed", "err", err)
		p.emitOutput(turnID, fmt.Sprintf("icm: session create failed: %v", err))
		return
	}

	// Write the immutable run.json so the run is inspectable from disk
	// even before any stage has executed.
	wsName := workspaceName(p.workflow.Root)
	startedAt := time.Now().UTC()
	_ = sess.WriteRunMeta(session.RunMeta{
		RunID:         runID,
		InstanceID:    p.instanceID,
		WorkspaceRoot: p.workflow.Root,
		WorkspaceName: wsName,
		StartedAt:     startedAt,
		ConfigSnapshot: map[string]any{
			"default_judge_posture":    p.cfg.DefaultJudgePosture,
			"default_workflow_posture": p.cfg.DefaultWorkflowPosture,
			"loop_max_restarts":        p.cfg.LoopMaxRestarts,
		},
	})

	if err := p.copyInitialInputs(sess, in); err != nil {
		p.logger.Error("icm: initial input copy failed", "err", err)
		p.emitOutput(turnID, fmt.Sprintf("icm: initial input copy failed: %v", err))
		return
	}

	p.emitPlanCreated(runID, turnID, wsName)

	orch := p.buildOrchestrator(runID, sess)
	orch.ParentTurnID = turnID

	p.orchMu.Lock()
	p.orchestrators[runID] = orch
	p.orchMu.Unlock()

	defer func() {
		p.orchMu.Lock()
		delete(p.orchestrators, runID)
		p.orchMu.Unlock()
	}()

	if err := orch.Run(context.Background()); err != nil {
		p.logger.Warn("icm: run halted", "run_id", runID, "err", err)
		p.emitOutput(turnID, fmt.Sprintf("Workflow halted: %v", err))
		return
	}

	// On clean completion, surface the last stage's artifact path as the
	// io.output. Production embedders often want the actual file content,
	// but emitting the path keeps the bus payload small; consumers can
	// read the file via standard tools.
	lastPath := p.finalArtifactPath(sess)
	p.emitOutput(turnID, fmt.Sprintf("Workflow completed. Final artifact: %s", lastPath))
}

// copyInitialInputs honors the configured input handling: an optional
// pre-populated workspace_inputs_dir first, then either the
// io.input.Content as a literal file path (stat-based heuristic) or as
// the input_filename content. File attachments on io.input always
// land in 00_input/ verbatim.
func (p *Plugin) copyInitialInputs(sess *session.Session, in events.UserInput) error {
	if p.cfg.WorkspaceInputsDir != "" {
		if err := sess.CopyInitialInputsFromDir(p.cfg.WorkspaceInputsDir); err != nil {
			return fmt.Errorf("workspace_inputs_dir: %w", err)
		}
	}

	for _, att := range in.Files {
		if att.Name == "" {
			continue
		}
		dst := filepath.Join(sess.RootDir, "00_input", filepath.Base(att.Name))
		if err := sess.WriteArtifact(dst, att.Data); err != nil {
			return fmt.Errorf("attach %s: %w", att.Name, err)
		}
	}

	content := strings.TrimSpace(in.Content)
	if content == "" {
		return nil
	}

	if p.cfg.TreatInputAsPathIfExists {
		if info, err := os.Stat(content); err == nil && !info.IsDir() {
			if err := sess.CopyInitialInputs([]string{content}); err != nil {
				return fmt.Errorf("copy input path %s: %w", content, err)
			}
			return nil
		}
	}

	dst := filepath.Join(sess.RootDir, "00_input", p.cfg.InputFilename)
	return sess.WriteArtifact(dst, []byte(in.Content))
}

// emitPlanCreated emits the deterministic plan as a PlanResult so any
// UI that already renders plan.created shows the stage list before any
// stage dispatches. Stages map 1:1 to PlanResultStep entries.
func (p *Plugin) emitPlanCreated(runID, turnID, wsName string) {
	stages := p.workflow.Stages
	steps := make([]events.PlanResultStep, 0, len(stages))
	for i := range stages {
		s := &stages[i]
		steps = append(steps, events.PlanResultStep{
			ID:           s.ID,
			Description:  s.Display,
			Instructions: s.Role,
			Order:        i,
			Status:       "pending",
		})
	}
	_ = p.bus.Emit("plan.created", events.PlanResult{
		SchemaVersion: events.PlanResultVersion,
		TurnID:        turnID,
		PlanID:        runID,
		Steps:         steps,
		Summary:       firstLine(p.workflow.WorkspaceDoc),
		Approved:      true,
		Source:        "icm/" + wsName,
	})
	_ = p.bus.Emit("icm.run.started", icmtypes.ICMRunStarted{
		SchemaVersion: icmtypes.ICMRunStartedVersion,
		RunID:         runID,
		InstanceID:    p.instanceID,
		WorkspaceRoot: p.workflow.Root,
		WorkspaceName: wsName,
		Stages:        len(stages),
	})
}

// emitOutput emits the agent's final io.output. Vetoable so plugins
// can intercept the rendered string before it reaches IO transports.
func (p *Plugin) emitOutput(turnID, content string) {
	out := events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion,
		Content:       content,
		Role:          "assistant",
		TurnID:        turnID,
	}
	if veto, err := p.bus.EmitVetoable("before:io.output", &out); err == nil && veto.Vetoed {
		return
	}
	_ = p.bus.Emit("io.output", out)
}

// finalArtifactPath returns the artifact path produced by the last
// stage in execution order. Used for io.output narration. Falls back
// to the session root when stage 0 hasn't written yet (shouldn't
// happen on a clean completion).
func (p *Plugin) finalArtifactPath(sess *session.Session) string {
	if len(p.workflow.Stages) == 0 {
		return sess.RootDir
	}
	last := p.workflow.Stages[len(p.workflow.Stages)-1]
	return sess.ArtifactPath(last.ID, last.Output.Filename)
}

// workspaceName returns the workspace folder's basename, used as the
// canonical "name" surfaced in plan.created and icm.run.started.
func workspaceName(root string) string {
	return filepath.Base(strings.TrimRight(root, "/\\"))
}

// firstLine returns the trimmed first non-empty line of s, or the
// trimmed entire string when no newline is present.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}
