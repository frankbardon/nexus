package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

//go:embed recipe-fanout-vote.yaml
var fanoutVoteRecipeConfig []byte

// runFanoutVoteRecipe demonstrates providers/fanout in pure-CLI form:
// the same prompt is dispatched to anthropic + openai + gemini in
// parallel, the deadline expires (or all responses arrive), and an
// llm_judge synthesis pass picks the winner. The recipe prints each
// individual response plus the judge's pick.
//
// Why this exists as a recipe even though the Wails Orchestrator agent
// also exposes the `vote` role:
//
//   - Reproducibility: a single CLI invocation captures the full
//     fanout-vote behavior in stdout for screenshots / writeups.
//   - No GUI: anyone evaluating the pattern can run it from CI or a
//     terminal without booting the desktop app.
//
// Required env: ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY.
//
// Usage:
//
//	cmd/demo recipe fanout-vote [--prompt "..."]
func runFanoutVoteRecipe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fanout-vote", flag.ContinueOnError)
	prompt := fs.String("prompt",
		"In one sentence, what is the canonical use case for retrieval-augmented generation?",
		"Prompt to fan out across the three providers.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	eng, err := bootRecipeEngine(ctx, fanoutVoteRecipeConfig)
	if err != nil {
		return err
	}
	defer func() {
		_ = eng.Stop(context.Background())
	}()

	if err := eng.StartSession(); err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	// Subscribe to llm.response so we capture each provider's leg as
	// well as the final winning response. The fanout plugin tags the
	// per-provider responses with _fanout_id metadata; the winner
	// arrives without it.
	type capture struct {
		provider string
		text     string
		isWinner bool
	}
	captureCh := make(chan capture, 8)
	unsub := eng.Bus.Subscribe("llm.response", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.LLMResponse); ok {
			provider := "(winner)"
			isWinner := true
			if r.Metadata != nil {
				if id, ok := r.Metadata["_fanout_id"]; ok {
					if s, ok := id.(string); ok && s != "" {
						provider = s
						isWinner = false
					}
				}
			}
			text := r.Content
			select {
			case captureCh <- capture{provider, text, isWinner}:
			default:
			}
		}
	})
	defer unsub()

	// Submit the request on the `vote` role. The fanout plugin
	// intercepts before:llm.request when role.IsFanout==true and
	// dispatches in parallel to all three configured providers.
	req := events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "vote",
		MaxTokens:     512,
		Messages: []events.Message{{
			Role:    "user",
			Content: *prompt,
		}},
	}
	if err := eng.Bus.Emit("llm.request", req); err != nil {
		return fmt.Errorf("emit llm.request: %w", err)
	}

	fmt.Println("Recipe: fanout-vote")
	fmt.Printf("  Prompt: %s\n", *prompt)
	fmt.Println()
	fmt.Println("  Dispatching to anthropic + openai + gemini in parallel...")
	fmt.Println()

	// Wait up to 60s for the fanout to settle. The plugin's deadline
	// (configured in YAML to 30s) will force a selection earlier if
	// any leg is dragging.
	deadline := time.After(60 * time.Second)
	got := 0
	winnerSeen := false
	for {
		select {
		case c := <-captureCh:
			got++
			marker := "  leg"
			if c.isWinner {
				marker = "WINNER"
				winnerSeen = true
			}
			label := c.provider
			if len(label) > 40 {
				label = label[:40]
			}
			snippet := strings.ReplaceAll(strings.TrimSpace(c.text), "\n", " ")
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			fmt.Printf("  [%s] %-40s %s\n", marker, label, snippet)

			if winnerSeen {
				return nil
			}
		case <-deadline:
			return fmt.Errorf("timeout waiting for fanout result (got %d responses, no winner)", got)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
