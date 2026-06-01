package icm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/frankbardon/nexus/pkg/delegate"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// judgeResponseSchemaName is the registered name for the fixed
// {verdict, feedback, score?} schema the LLM judge must conform to.
const judgeResponseSchemaName = "icm.judge.response"

// judgeResponseSchemaJSON is the canonical JSON Schema (draft-2020-12)
// every `type: llm` predicate's judge response must satisfy. Registered
// at Ready() via ctx.Schemas so judge dispatchers can validate model
// output before returning a verdict.
const judgeResponseSchemaJSON = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["verdict", "feedback"],
  "additionalProperties": false,
  "properties": {
    "verdict":  { "type": "string", "enum": ["pass", "fail"] },
    "feedback": { "type": "string" },
    "score":    { "type": "number", "minimum": 0, "maximum": 1 }
  }
}`

// judgeResponse is the typed parse of a judge sub-agent's response.
type judgeResponse struct {
	Verdict  string   `json:"verdict"`
	Feedback string   `json:"feedback"`
	Score    *float64 `json:"score,omitempty"`
}

// installPredicateDispatchers wires the evaluator's Human + Judge
// dispatchers onto plugin methods. Called from Init after the evaluator
// is constructed and the delegate runtime is built.
func (p *Plugin) installPredicateDispatchers() {
	p.evaluator.Human = p.dispatchHumanPredicate
	p.evaluator.Judge = p.dispatchJudgePredicate
}

// registerJudgeResponseSchema registers the fixed icm.judge.response
// schema with the engine SchemaRegistry once at Ready(). Idempotent —
// re-registration overwrites the prior entry.
func (p *Plugin) registerJudgeResponseSchema() error {
	if p.schemas == nil {
		return fmt.Errorf("icm: schema registry unavailable")
	}
	var schemaMap map[string]any
	if err := json.Unmarshal([]byte(judgeResponseSchemaJSON), &schemaMap); err != nil {
		return fmt.Errorf("icm: judge response schema: %w", err)
	}
	p.schemas.Register(judgeResponseSchemaName, schemaMap, p.instanceID)
	p.registeredSchemas = append(p.registeredSchemas, judgeResponseSchemaName)
	return nil
}

// dispatchHumanPredicate is the predicates.HumanDispatch implementation.
// It builds a hitl.requested event, waits for hitl.responded keyed by
// request ID, and translates the operator's choice into (verdict,
// feedback) the evaluator can interpret.
func (p *Plugin) dispatchHumanPredicate(ctx context.Context, pr *workspace.Predicate, artifact []byte, sc predicates.StageEvalContext) (bool, string, error) {
	reqID := newHITLID("predicate", sc.RunID, sc.StageID, pr.Name)

	requireFeedback := true
	if pr.RequireFeedbackOnContinue != nil {
		requireFeedback = *pr.RequireFeedbackOnContinue
	}

	actionRef := map[string]any{
		"run_id":         sc.RunID,
		"stage_id":       sc.StageID,
		"item_id":        sc.ItemID,
		"iteration":      sc.Iteration,
		"container":      sc.Container,
		"predicate":      pr.Name,
		"predicate_type": string(pr.Type),
		"artifact_bytes": len(artifact),
	}

	req := events.HITLRequest{
		SchemaVersion:   events.HITLRequestVersion,
		ID:              reqID,
		TurnID:          sc.ParentTurnID,
		RequesterPlugin: p.instanceID,
		ActionKind:      "icm.predicate",
		ActionRef:       actionRef,
		Mode:            events.HITLModeBoth,
		Choices: []events.HITLChoice{
			{ID: "pass", Label: "Pass", Kind: events.ChoiceCustom},
			{ID: "fail", Label: "Fail", Kind: events.ChoiceCustom},
		},
		Prompt: pr.Prompt,
		Metadata: map[string]any{
			"icm.kind":     "predicate",
			"icm.run_id":   sc.RunID,
			"icm.stage":    sc.StageID,
			"icm.required": requireFeedback,
		},
	}

	resp, err := p.emitHITLAndWait(ctx, req)
	if err != nil {
		return false, fmt.Sprintf("hitl wait: %v", err), err
	}
	if resp.Cancelled {
		return false, fmt.Sprintf("cancelled: %s", resp.CancelReason), nil
	}
	verdict := resp.ChoiceID == "pass"
	feedback := resp.FreeText
	if !verdict && feedback == "" {
		feedback = "rejected by operator"
	}
	return verdict, feedback, nil
}

// dispatchJudgePredicate is the predicates.JudgeDispatch implementation.
// It loads the rubric file, builds a judge-task XML payload, dispatches
// via the private delegate runtime against the selected judge posture,
// parses the response into a judgeResponse value, and returns the
// (verdict, feedback, score) triple.
func (p *Plugin) dispatchJudgePredicate(ctx context.Context, pr *workspace.Predicate, artifact []byte, sc predicates.StageEvalContext) (bool, string, *float64, error) {
	posturName := pr.Model
	if posturName == "" {
		posturName = p.cfg.DefaultJudgePosture
	}
	if posturName == "" {
		return false, "default_judge_posture is not configured", nil, nil
	}

	rubricPath := pr.Rubric
	if rubricPath == "" {
		return false, "llm predicate missing rubric path", nil, nil
	}
	// Loader has already verified the path is non-empty + exists at
	// workspace root resolution time. Re-read on every eval so authors
	// can edit rubrics between runs without restart.
	abs := resolveWorkspacePath(sc.WorkspaceRoot, rubricPath)
	rubricBytes, err := readFile(abs)
	if err != nil {
		return false, fmt.Sprintf("read rubric %q: %v", pr.Rubric, err), nil, err
	}

	userMsg := buildJudgeUserMessage(string(rubricBytes), artifact)

	out, err := p.runtime.Run(ctx, delegate.Input{
		Posture:     posturName,
		Task:        userMsg,
		Context:     nil,
		ParentTurn:  sc.ParentTurnID,
		ParentDepth: sc.ParentDepth + 1,
	})
	if err != nil {
		return false, fmt.Sprintf("judge dispatch: %v", err), nil, err
	}
	if out.Status == delegate.StatusError || out.Status == delegate.StatusTimeout {
		return false, fmt.Sprintf("judge status %s: %s", out.Status, out.Error), nil, nil
	}

	// LLM judges (especially small/cheap models) habitually wrap the
	// verdict JSON in a Markdown code fence even when the rubric asks
	// for raw JSON. Strip that defensively so a stylistic quirk does
	// not cause an unrecoverable loop failure. Falls back to extracting
	// the first `{...}` substring when the response still contains
	// commentary around the JSON document.
	raw := unwrapJudgeJSON(out.Result)
	var parsed judgeResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return false, fmt.Sprintf("judge output malformed (not JSON): %v", err), nil, nil
	}
	if parsed.Verdict != "pass" && parsed.Verdict != "fail" {
		return false, fmt.Sprintf("judge output malformed: verdict %q", parsed.Verdict), nil, nil
	}
	return parsed.Verdict == "pass", parsed.Feedback, parsed.Score, nil
}

// emitHITLAndWait registers a per-request channel, emits the
// hitl.requested event, and blocks until either the response arrives
// or the context cancels. On ctx cancel it emits hitl.cancel so the
// HITL plugin tears down the persisted request file.
func (p *Plugin) emitHITLAndWait(ctx context.Context, req events.HITLRequest) (events.HITLResponse, error) {
	ch := make(chan events.HITLResponse, 1)
	p.hitlMu.Lock()
	p.hitlWait[req.ID] = ch
	p.hitlMu.Unlock()
	defer func() {
		p.hitlMu.Lock()
		delete(p.hitlWait, req.ID)
		p.hitlMu.Unlock()
	}()

	if veto, err := p.bus.EmitVetoable("before:hitl.requested", &req); err == nil && veto.Vetoed {
		return events.HITLResponse{}, fmt.Errorf("hitl request vetoed: %s", veto.Reason)
	}
	if err := p.bus.Emit("hitl.requested", req); err != nil {
		return events.HITLResponse{}, fmt.Errorf("emit hitl.requested: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		_ = p.bus.Emit("hitl.cancel", events.HITLCancel{
			SchemaVersion: events.HITLCancelVersion,
			RequestID:     req.ID,
			Reason:        ctx.Err().Error(),
		})
		return events.HITLResponse{}, ctx.Err()
	}
}

// newHITLID returns a unique HITL request ID prefixed `icm-` so the
// plugin's hitl.responded handler can fast-filter foreign responses.
// Format: icm-<kind>-<runID>-<stageID>-<extra>-<8 random hex>
func newHITLID(kind, runID, stageID, extra string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("icm-%s-%s-%s-%s-%s", kind, runID, stageID, extra, hex.EncodeToString(b[:]))
}

// buildJudgeUserMessage produces the XML user message the judge
// sub-agent receives. Keep it shaped like a stage payload (predictable
// to LLMs already trained on ICM payloads) but compact — the judge has
// no grounding or layer data beyond the rubric + artifact.
func buildJudgeUserMessage(rubric string, artifact []byte) string {
	var b []byte
	b = append(b, []byte(`<judge_task>`+"\n")...)
	b = append(b, []byte(`  <rubric>`+"\n")...)
	b = append(b, []byte(engine.XMLCDATA(rubric))...)
	b = append(b, []byte("\n  </rubric>\n")...)
	b = append(b, []byte(`  <artifact>`+"\n")...)
	b = append(b, []byte(engine.XMLCDATA(string(artifact)))...)
	b = append(b, []byte("\n  </artifact>\n")...)
	b = append(b, []byte(`  <response_schema name="`+judgeResponseSchemaName+`"/>`+"\n")...)
	b = append(b, []byte(`</judge_task>`+"\n")...)
	return string(b)
}
