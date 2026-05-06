//go:build evalrecord

// Recorder for the react-planner-handoff seed case.
//
// Run from repo root:
//
//	go run -tags evalrecord ./tests/eval/cases/react-planner-handoff/_record/
//
// One-turn ReAct dialogue with static planner enabled. The planner is
// deterministic and approval: never, so plan.created/plan.result fire on
// every replay. Only the agent's llm.response is stashed for replay
// short-circuiting; every other event is live-emitted.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "record: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("recorded journal at ../journal/")
}

func run() error {
	caseDir, err := filepath.Abs(filepath.Join("tests/eval/cases/react-planner-handoff"))
	if err != nil {
		return err
	}
	if _, err := os.Stat(caseDir); err != nil {
		return fmt.Errorf("not at repo root? %w", err)
	}
	journalDir := filepath.Join(caseDir, "journal")
	_ = os.RemoveAll(journalDir)
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		return err
	}

	w, err := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode:  journal.FsyncEveryEvent,
		BufferSize: 32,
		SessionID:  "react-planner-handoff-golden",
	})
	if err != nil {
		return err
	}

	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	steps := []events.PlanResultStep{
		{ID: "step_1", Description: "Read the codebase", Instructions: "List files and read key sources.", Status: "pending", Order: 1},
		{ID: "step_2", Description: "Identify issues", Instructions: "Flag bugs and quality problems.", Status: "pending", Order: 2},
		{ID: "step_3", Description: "Propose fixes", Instructions: "Suggest concrete code changes.", Status: "pending", Order: 3},
	}
	planResult := events.PlanResult{SchemaVersion: events.PlanResultVersion, TurnID: "turn-1",
		PlanID:   "plan-1",
		Steps:    steps,
		Summary:  "Three-step review workflow",
		Approved: true,
		Source:   "static",
	}
	envelopes := []journal.Envelope{
		{Seq: 1, Ts: t0, Type: "io.session.start", Payload: map[string]any{"session_id": "react-planner-handoff-golden"}},
		{Seq: 2, Ts: t0.Add(10 * time.Millisecond), Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "Please review the codebase."}},
		{Seq: 3, Ts: t0.Add(20 * time.Millisecond), Type: "agent.turn.start"},
		{Seq: 4, Ts: t0.Add(30 * time.Millisecond), Type: "plan.request", Payload: events.PlanRequest{SchemaVersion: events.PlanRequestVersion, TurnID: "turn-1", Input: "Please review the codebase."}},
		{Seq: 5, Ts: t0.Add(40 * time.Millisecond), Type: "plan.created", Payload: planResult},
		{Seq: 6, Ts: t0.Add(50 * time.Millisecond), Type: "plan.result", Payload: planResult},
		{Seq: 7, Ts: t0.Add(60 * time.Millisecond), Type: "llm.response", Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Model: "mock",
			FinishReason: "end_turn",
			Usage:        events.Usage{PromptTokens: 220, CompletionTokens: 80, TotalTokens: 300},
			Content:      "Plan complete: codebase reviewed, no critical issues found.",
		}},
		{Seq: 8, Ts: t0.Add(70 * time.Millisecond), Type: "agent.turn.end"},
	}
	for i := range envelopes {
		w.Append(&envelopes[i])
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return w.Close(closeCtx)
}
