package main

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

//go:embed recipe-batch-briefs.yaml
var batchBriefsRecipeConfig []byte

// runBatchBriefsRecipe demonstrates the llm/batch coordinator by
// submitting one batch of brief-generation requests against
// Anthropic's Messages Batches API. Real batch jobs take hours to
// complete — the recipe submits the batch, prints the ID, and exits.
// State is persisted to disk; a later invocation can poll for results
// (or the engine in the Wails app can pick up the same persisted
// batches on next boot).
//
// What this recipe demonstrates:
//
//   - The llm/batch plugin's submit → status → results event contract.
//   - Persistence: batch state survives restart. Pollers reconnect on
//     boot. (Demoed by the fact that you can submit here, kill the
//     process, then resume polling later.)
//   - Cost optimization: batches run at provider's discounted rate
//     (Anthropic 50% off as of writing) compared to interactive calls.
//
// Required env: ANTHROPIC_API_KEY.
//
// Usage:
//
//	cmd/demo recipe batch-briefs ACME Vortex Loom Pulp
//
// Each positional arg becomes a brief request. If no args, a default
// 4-competitor list is used.
func runBatchBriefsRecipe(ctx context.Context, args []string) error {
	competitors := args
	if len(competitors) == 0 {
		competitors = []string{"ACME Corp", "Vortex AI", "Loom Systems", "Pulp Inc"}
	}

	eng, err := bootRecipeEngine(ctx, batchBriefsRecipeConfig)
	if err != nil {
		return err
	}
	defer func() {
		_ = eng.Stop(context.Background())
	}()

	if err := eng.StartSession(); err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	// Build one BatchRequest per competitor. CustomID lets the recipe
	// correlate per-result outputs back to the input list.
	reqs := make([]events.BatchRequest, 0, len(competitors))
	for _, name := range competitors {
		reqs = append(reqs, events.BatchRequest{
			CustomID: "brief-" + sanitizeID(name),
			// LLMRequest fields: Role/Model select the provider; Provider
			// is implicit via the model registry. For batch the actual
			// vendor dispatch is determined by BatchSubmit.Provider; this
			// inner Model just selects the wire-format model name.
			Request: events.LLMRequest{
				SchemaVersion: events.LLMRequestVersion,
				Model:         "claude-sonnet-4-6",
				MaxTokens:     2048,
				Messages: []events.Message{{
					Role:    "user",
					Content: fmt.Sprintf(briefPromptTemplate, name),
				}},
			},
		})
	}

	submit := events.BatchSubmit{
		SchemaVersion: events.BatchSubmitVersion,
		Provider:      "anthropic",
		Requests:      reqs,
		Metadata: map[string]any{
			"recipe": "batch-briefs",
			"count":  len(reqs),
		},
	}

	// Subscribe to status and results events so the operator sees
	// confirmation of submission. We unsubscribe on exit.
	statusCh := make(chan events.BatchStatus, 4)
	unsub := eng.Bus.Subscribe("llm.batch.status", func(ev engine.Event[any]) {
		if s, ok := ev.Payload.(events.BatchStatus); ok {
			select {
			case statusCh <- s:
			default:
			}
		}
	})
	defer unsub()

	if err := eng.Bus.Emit("llm.batch.submit", submit); err != nil {
		return fmt.Errorf("submit batch: %w", err)
	}

	// Wait briefly for the initial status event so the operator sees
	// the batch ID (the coordinator emits "submitted" right after
	// submission).
	select {
	case s := <-statusCh:
		fmt.Println("Recipe: batch-briefs")
		fmt.Printf("  Submitted: %d requests to provider %q\n", len(reqs), s.Provider)
		fmt.Printf("  Batch ID:  %s\n", s.BatchID)
		fmt.Printf("  Status:    %s\n", s.Status)
		fmt.Println()
		fmt.Println("Anthropic Messages Batches typically take 1+ hour to complete.")
		fmt.Println("State is persisted to ~/.nexus/batches; a later run will")
		fmt.Println("resume polling and emit llm.batch.results when the batch")
		fmt.Println("finishes. Hook a handler in your own driver to consume them.")
	case <-time.After(15 * time.Second):
		return fmt.Errorf("submit timed out — check ANTHROPIC_API_KEY and network")
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

// sanitizeID converts a competitor name into a CustomID-safe slug.
// Anthropic + OpenAI both accept conservative ASCII-only IDs.
func sanitizeID(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, byte(r))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

const briefPromptTemplate = `Write a one-paragraph competitor brief on %s. Cover positioning, target buyer, and primary wedge. Keep it under 200 words. Don't invent facts; if you don't have grounded information, say so.`
