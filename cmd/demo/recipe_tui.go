package main

import (
	"context"
	_ "embed"
	"fmt"
)

//go:embed recipe-tui.yaml
var tuiRecipeConfig []byte

// runTUIRecipe boots the engine with the io/tui transport plugin
// against a researcher-flavoured config. The TUI plugin owns stdin/
// stdout and runs an interactive prompt loop until the user exits.
//
// What this recipe demonstrates:
//
//   - The io/* transport abstraction. Every io plugin (wails, browser,
//     tui, oneshot) implements the same input/output event contract
//     (io.input / io.output). Swapping wails for tui in the YAML is the
//     only difference between the desktop app and this terminal flow.
//   - Researcher-style RAG retrieval (chromem + sqlite_fts + hybrid +
//     reranker) running outside any GUI.
//   - That the engine is fully usable from a plain SSH session — useful
//     for remote debugging and for users who prefer the terminal.
//
// API keys: read from env (ANTHROPIC_API_KEY, OPENAI_API_KEY,
// BRAVE_API_KEY). The recipe's YAML uses ${env:NAME} substitution so
// the operator doesn't need to edit the embedded config.
//
// This recipe BLOCKS — control returns to the OS shell only when the
// user terminates the TUI session.
func runTUIRecipe(ctx context.Context, args []string) error {
	eng, err := bootRecipeEngine(ctx, tuiRecipeConfig)
	if err != nil {
		return err
	}
	defer func() {
		_ = eng.Stop(context.Background())
	}()

	if err := eng.StartSession(); err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	// The TUI plugin owns the foreground loop. We block until either
	// the engine ends the session (user typed /exit etc) or the
	// recipe's context is cancelled (SIGINT).
	select {
	case <-eng.SessionEnded():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
