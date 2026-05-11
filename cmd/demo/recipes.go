// Recipe runner — the non-Wails side of cmd/demo.
//
// A "recipe" is a small, self-contained scenario that boots the Nexus
// engine with a tightly-scoped config and runs it to completion. Recipes
// exist to demo plugins that don't fit a chat UI:
//
//   - batch-briefs   → llm/batch + io/oneshot, queues 50 brief jobs and
//     polls the provider batch endpoint until done.
//   - tui            → io/tui transport against the Researcher config so
//     operators can run the same agent in a terminal.
//   - browser-ui     → io/browser HTTP/WS transport, also against the
//     Researcher config; useful as a no-Wails fallback
//     and to demo the alt transport.
//   - embeddings-mock → embeddings/mock (deterministic, hash-based) +
//     chromem, ingests + queries a doc with no API
//     keys. CI-safe; useful for `go test` parity.
//
// Recipe scaffolding: every recipe uses the same shape — embed a YAML,
// build the engine via engine.NewFromBytes, register the relevant
// factories from commonFactories(), Boot, Run-until-done, Stop. The
// recipeContext helper centralises signal handling and shared logging.
//
// Recipes intentionally do NOT use the desktop framework's Settings UI —
// they read API keys from environment variables (ANTHROPIC_API_KEY,
// OPENAI_API_KEY, etc.) and config paths from flags. Recipe mode is
// scriptable; the desktop mode is interactive.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/frankbardon/nexus/pkg/engine"
)

// recipeFn is the signature every recipe implements. Each recipe is
// responsible for parsing its own argv (flags, positional args). The
// runner just dispatches by name; it doesn't care about per-recipe
// argument shapes.
type recipeFn func(ctx context.Context, args []string) error

// recipes is the lookup table the dispatcher consults. To add a new
// recipe: implement a `runX(ctx, args)` function below, then register
// it here. Keep names short and lowercase-with-dashes so they look
// good in CLI invocations.
var recipes = map[string]recipeFn{
	"batch-briefs":    runBatchBriefsRecipe,
	"tui":             runTUIRecipe,
	"browser-ui":      runBrowserUIRecipe,
	"embeddings-mock": runEmbeddingsMockRecipe,
	"eval":            runEvalRecipe,       // Phase 8
	"otel-trace":      runOTelTraceRecipe,  // Phase 8
	"voice":           runVoiceRecipe,      // Phase 9
	"fanout-vote":     runFanoutVoteRecipe, // Phase 9
}

// runRecipe is the entry point called from main() when argv[1]=="recipe".
// It sets up signal handling, looks up the recipe, and runs it. Errors
// are returned for main() to print + exit on.
func runRecipe(name string, args []string) error {
	fn, ok := recipes[name]
	if !ok {
		return fmt.Errorf("unknown recipe %q\n\n%s", name, recipeHelp())
	}

	// SIGINT/SIGTERM cancels the recipe's context so engines can shut
	// down cleanly. Recipes that want different signal handling can
	// re-derive a child context.
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return fn(ctx, args)
}

// printRecipeHelp / recipeHelp emit a one-line summary of every
// registered recipe. Kept simple — the recipe itself is responsible for
// printing its own --help if invoked with a help flag.
func printRecipeHelp() {
	fmt.Fprintln(os.Stderr, recipeHelp())
}

func recipeHelp() string {
	var b strings.Builder
	b.WriteString("Usage: cmd/demo recipe <name> [args...]\n\n")
	b.WriteString("Recipes:\n")
	for _, name := range recipeNames() {
		b.WriteString("  ")
		b.WriteString(name)
		b.WriteString("\n")
	}
	return b.String()
}

func recipeNames() []string {
	names := make([]string, 0, len(recipes))
	for k := range recipes {
		names = append(names, k)
	}
	// Sort isn't strictly required but keeps the help output stable.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}

// bootRecipeEngine is the shared boot path for recipes. It:
//  1. parses the embedded YAML via engine.NewFromBytes
//  2. registers every factory in commonFactories() so the engine can
//     instantiate any plugin the YAML references
//  3. Boots the engine (returns error if Init fails)
//
// Caller is responsible for Run / Stop. This factoring exists so each
// recipe's body can focus on its specific orchestration logic without
// duplicating the boilerplate plumbing.
func bootRecipeEngine(ctx context.Context, configBytes []byte) (*engine.Engine, error) {
	eng, err := engine.NewFromBytes(configBytes)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	for id, factory := range commonFactories() {
		eng.Registry.Register(id, factory)
	}
	if err := eng.Boot(ctx); err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}
	return eng, nil
}
