//go:build evalrecord

// Recorder for the multi-subagent-fanout seed case.
//
// Run from repo root:
//
//	go run -tags evalrecord ./tests/eval/cases/multi-subagent-fanout/_record/
//
// Single-turn dialogue. The case's interesting events (three tool.register
// emissions, one per subagent instance) fire live during boot — the
// recorded journal only needs the io.input + agent turn frame so the
// replay coordinator has something to drive.
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
	caseDir, err := filepath.Abs(filepath.Join("tests/eval/cases/multi-subagent-fanout"))
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
		SessionID:  "multi-subagent-fanout-golden",
	})
	if err != nil {
		return err
	}

	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	envelopes := []journal.Envelope{
		{Seq: 1, Ts: t0, Type: "io.session.start", Payload: map[string]any{"session_id": "multi-subagent-fanout-golden"}},
		{Seq: 2, Ts: t0.Add(10 * time.Millisecond), Type: "io.input", Payload: events.UserInput{Content: "Spawn the researcher subagent to look up Go modules best practices."}},
		{Seq: 3, Ts: t0.Add(20 * time.Millisecond), Type: "agent.turn.start"},
		{Seq: 4, Ts: t0.Add(30 * time.Millisecond), Type: "llm.response", Payload: events.LLMResponse{
			Model:        "mock",
			FinishReason: "end_turn",
			Usage:        events.Usage{PromptTokens: 130, CompletionTokens: 50, TotalTokens: 180},
			Content:      "I would delegate to the researcher subagent here, but this is a mock test turn.",
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
