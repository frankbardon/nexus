package main

import (
	"context"
	_ "embed"
	"fmt"
)

//go:embed recipe-browser-ui.yaml
var browserUIRecipeConfig []byte

// runBrowserUIRecipe boots the engine with the io/browser HTTP/WS
// transport. The browser plugin starts an HTTP server with an embedded
// chat UI and a WebSocket bridge to the bus; the operator interacts via
// any web browser pointed at the bound port.
//
// What this recipe demonstrates:
//
//   - The io/browser transport as an alternative to wails. Browser is
//     useful when:
//   - you don't want to ship a desktop app (just run the binary on
//     a server, browse to it from any machine)
//   - you need multi-user access (each browser tab is its own
//     session; the engine handles multi-session inside one process)
//   - you're embedding Nexus inside an existing web property
//   - That transport selection is a config-only decision. Researcher's
//     RAG stack works identically under wails / tui / browser / oneshot.
//
// The recipe blocks until the operator closes the browser AND sends
// SIGINT, since the HTTP server has no natural stop signal otherwise.
//
// Default bind: 127.0.0.1:8889. Override via env (NEXUS_BROWSER_HOST,
// NEXUS_BROWSER_PORT) — the YAML reads ${env:...} at boot.
func runBrowserUIRecipe(ctx context.Context, args []string) error {
	eng, err := bootRecipeEngine(ctx, browserUIRecipeConfig)
	if err != nil {
		return err
	}
	defer func() {
		_ = eng.Stop(context.Background())
	}()

	if err := eng.StartSession(); err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	fmt.Println("Recipe: browser-ui")
	fmt.Println("  HTTP/WS server bound (see logs above for host:port).")
	fmt.Println("  Open the printed URL in any browser to start chatting.")
	fmt.Println("  Press Ctrl-C in this terminal to stop.")

	select {
	case <-eng.SessionEnded():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
