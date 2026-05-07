//go:build evalrecord

// Recorder for the skills-discovery seed case.
//
// Run from the repo root:
//
//	go run -tags evalrecord ./tests/eval/cases/skills-discovery/_record/
//
// The runtime journal exercises a single ReAct turn — the user asks for the
// skill catalog, the assistant replies with a placeholder. The skills
// plugin's skill.discover event is NOT in this journal: it fires LIVE
// during boot of every replay (the skills plugin re-scans on Ready), so
// assertions read it off the runner's observed-journal projection.
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
	caseDir, err := filepath.Abs(filepath.Join("tests/eval/cases/skills-discovery"))
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
		BufferSize: 16,
		SessionID:  "skills-discovery-golden",
	})
	if err != nil {
		return err
	}

	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	envelopes := []journal.Envelope{
		{Seq: 1, Ts: t0, Type: "io.session.start", Payload: map[string]any{"session_id": "skills-discovery-golden"}},
		{Seq: 2, Ts: t0.Add(10 * time.Millisecond), Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "What skills are available?"}},
		{Seq: 3, Ts: t0.Add(20 * time.Millisecond), Type: "agent.turn.start"},
		{Seq: 4, Ts: t0.Add(30 * time.Millisecond), Type: "llm.response", Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Model: "mock",
			FinishReason: "end_turn",
			Usage:        events.Usage{PromptTokens: 90, CompletionTokens: 30, TotalTokens: 120},
			Content:      "I see code-review, doc-analysis, and git-workflow available.",
		}},
		{Seq: 5, Ts: t0.Add(40 * time.Millisecond), Type: "agent.turn.end"},
	}
	for i := range envelopes {
		w.Append(&envelopes[i])
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return w.Close(closeCtx)
}
