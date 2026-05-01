package engine

import (
	"log/slog"
	"sync"
)

// NondeterministicWarn logs a one-shot warning that a plugin read non-
// deterministic state (wall clock, randomness, env, network) during
// dispatch. During deterministic replay these reads diverge from the
// journaled run.
//
// The helper deduplicates by reason so a hot-path call site does not flood
// the log. Plugins should pass a stable reason string ("dynvars: time.Now",
// "tool.web: http.Get") so the dedup map keeps a small, comprehensible
// shape.
func NondeterministicWarn(logger *slog.Logger, reason string) {
	if logger == nil {
		logger = slog.Default()
	}
	if seenNondet(reason) {
		return
	}
	logger.Warn("nondeterministic read during dispatch — replay may diverge", "reason", reason)
}

var nondetMu sync.Mutex
var nondetSeen = map[string]struct{}{}

func seenNondet(reason string) bool {
	nondetMu.Lock()
	defer nondetMu.Unlock()
	if _, ok := nondetSeen[reason]; ok {
		return true
	}
	nondetSeen[reason] = struct{}{}
	return false
}
