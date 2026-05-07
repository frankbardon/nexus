//go:build evalrecord

// Recorder for the provider-fallback seed case.
//
// Run from repo root:
//
//	go run -tags evalrecord ./tests/eval/cases/provider-fallback/_record/
//
// The recorded journal represents the happy-path replay: the primary
// provider (mock-primary) returns the stashed llm.response without
// erroring, so the fallback plugin's chain-advance path never triggers.
// This is the deterministic smoke test — the live error->fallback path is
// covered by tests/integration/fallback_test.go, which needs an API key.
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
	caseDir, err := filepath.Abs(filepath.Join("tests/eval/cases/provider-fallback"))
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
		SessionID:  "provider-fallback-golden",
	})
	if err != nil {
		return err
	}

	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	envelopes := []journal.Envelope{
		{Seq: 1, Ts: t0, Type: "io.session.start", Payload: map[string]any{"session_id": "provider-fallback-golden"}},
		{Seq: 2, Ts: t0.Add(10 * time.Millisecond), Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "Hello, respond with exactly: fallback chain ready"}},
		{Seq: 3, Ts: t0.Add(20 * time.Millisecond), Type: "agent.turn.start"},
		{Seq: 4, Ts: t0.Add(30 * time.Millisecond), Type: "llm.response", Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Model: "mock-primary",
			FinishReason: "end_turn",
			Usage:        events.Usage{PromptTokens: 110, CompletionTokens: 20, TotalTokens: 130},
			Content:      "fallback chain ready",
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
