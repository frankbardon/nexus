package runner

import (
	"context"
	"os"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
)

// writeStubJournal creates a minimal valid journal at dir suitable for the
// runner: io.session.start + io.input + agent.turn.start + llm.response +
// agent.turn.end. Used by multi-runner tests so each test owns its own
// disposable case bundle.
func writeStubJournal(dir, sessionID string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	w, err := journal.NewWriter(dir, journal.WriterOptions{
		FsyncMode:  journal.FsyncNone,
		BufferSize: 16,
		SessionID:  sessionID,
	})
	if err != nil {
		return err
	}
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	envs := []journal.Envelope{
		{Seq: 1, Ts: t0, Type: "io.session.start", Payload: map[string]any{"session_id": sessionID}},
		{Seq: 2, Ts: t0.Add(10 * time.Millisecond), Type: "io.input", Payload: events.UserInput{Content: "hello"}},
		{Seq: 3, Ts: t0.Add(20 * time.Millisecond), Type: "agent.turn.start"},
		{Seq: 4, Ts: t0.Add(30 * time.Millisecond), Type: "llm.response", Payload: events.LLMResponse{
			Model:        "mock",
			FinishReason: "end_turn",
			Usage:        events.Usage{PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60},
			Content:      "ok",
		}},
		{Seq: 5, Ts: t0.Add(40 * time.Millisecond), Type: "agent.turn.end"},
	}
	for i := range envs {
		w.Append(&envs[i])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return w.Close(ctx)
}
