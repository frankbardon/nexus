//go:build evalrecord

// Recorder for the build-error-fix seed case. Run with the `evalrecord`
// build tag to regenerate the journal:
//
//	go run -tags evalrecord ./tests/eval/cases/build-error-fix/_record/
//
// Hand-crafted journal: a 2-turn ReAct + tool-use dialogue, ~2.5KB on disk.
// Committed alongside the case so the runner is reproducible without
// per-machine LLM/tool plumbing. Run with `go run ./tests/eval/cases/build-error-fix/_record/`
// from the repo root to regenerate the journal in ../journal/.
//
// Hand-crafts a 2-turn journal that exercises ReAct + tool use + final
// answer. The journal is the "golden trace" the runner replays.
package main

import (
	"context"
	"encoding/json"
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
	caseDir, err := filepath.Abs(filepath.Join("tests/eval/cases/build-error-fix"))
	if err != nil {
		return err
	}
	if _, err := os.Stat(caseDir); err != nil {
		return fmt.Errorf("not at repo root? %w", err)
	}
	journalDir := filepath.Join(caseDir, "journal")
	// Wipe and rebuild.
	_ = os.RemoveAll(journalDir)
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		return err
	}

	w, err := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode:  journal.FsyncEveryEvent,
		BufferSize: 32,
		SessionID:  "build-error-fix-golden",
	})
	if err != nil {
		return err
	}

	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	tcArgs, _ := json.Marshal(map[string]any{"path": "main.go"})
	envelopes := []journal.Envelope{
		{Seq: 1, Ts: t0, Type: "io.session.start", Payload: map[string]any{"session_id": "build-error-fix-golden"}},

		// Turn 1: user asks about build error; assistant uses read_file then explains.
		{Seq: 2, Ts: t0.Add(10 * time.Millisecond), Type: "io.input", Payload: events.UserInput{Content: "Why won't main.go build?"}},
		{Seq: 3, Ts: t0.Add(20 * time.Millisecond), Type: "agent.turn.start"},
		{Seq: 4, Ts: t0.Add(30 * time.Millisecond), Type: "llm.response", Payload: events.LLMResponse{
			Model:        "mock",
			FinishReason: "tool_use",
			Usage:        events.Usage{PromptTokens: 80, CompletionTokens: 40, TotalTokens: 120},
			ToolCalls: []events.ToolCallRequest{
				{ID: "tc_1", Name: "read_file", Arguments: string(tcArgs)},
			},
		}},
		{Seq: 5, Ts: t0.Add(40 * time.Millisecond), Type: "tool.invoke", Payload: events.ToolCall{
			ID:        "tc_1",
			Name:      "read_file",
			Arguments: map[string]any{"path": "main.go"},
		}},
		{Seq: 6, Ts: t0.Add(50 * time.Millisecond), Type: "tool.result", Payload: events.ToolResult{
			ID:     "tc_1",
			Name:   "read_file",
			Output: "package main\n\nfunc main() {\n\treturn 0\n}\n",
		}},
		{Seq: 7, Ts: t0.Add(60 * time.Millisecond), Type: "llm.response", Payload: events.LLMResponse{
			Model:        "mock",
			FinishReason: "end_turn",
			Usage:        events.Usage{PromptTokens: 140, CompletionTokens: 70, TotalTokens: 210},
			Content:      "main() returns 0 but its declared type is void; remove the return value or change the signature.",
		}},
		{Seq: 8, Ts: t0.Add(70 * time.Millisecond), Type: "agent.turn.end"},

		// Turn 2: follow-up; pure conversational, no tool calls.
		{Seq: 9, Ts: t0.Add(80 * time.Millisecond), Type: "io.input", Payload: events.UserInput{Content: "Show me the fixed version."}},
		{Seq: 10, Ts: t0.Add(90 * time.Millisecond), Type: "agent.turn.start"},
		{Seq: 11, Ts: t0.Add(100 * time.Millisecond), Type: "llm.response", Payload: events.LLMResponse{
			Model:        "mock",
			FinishReason: "end_turn",
			Usage:        events.Usage{PromptTokens: 160, CompletionTokens: 50, TotalTokens: 210},
			Content:      "package main\n\nfunc main() {}\n",
		}},
		{Seq: 12, Ts: t0.Add(110 * time.Millisecond), Type: "agent.turn.end"},
	}
	for i := range envelopes {
		w.Append(&envelopes[i])
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return w.Close(closeCtx)
}
