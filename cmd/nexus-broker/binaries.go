package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// BinaryInfo is the CLIENT-facing projection of one binary registry entry: the
// name a claim selects it by, plus the two presentational strings an operator
// wrote for exactly this purpose.
//
// The fields it does NOT have are the point. `path`/`ResolvedPath`, `args` and
// `env` are broker-host detail: they name filesystem layout, build locations and
// deployment flags that a claiming client has no use for and every reason not to
// learn. Projecting into a separate struct — rather than encoding BinaryEntry
// with `json:"-"` on the private fields — means a field added to BinaryEntry
// later cannot leak by default: it has to be copied here deliberately.
//
// Label and Description are OMITTED when unset (the broker's usual omitempty
// convention, as on claimResponse.SessionID and LeaseSnapshot.Reason) rather
// than emitted empty. Either would work for the documented "fall back to name"
// rule, and omitted is chosen so a client can tell "the operator wrote nothing"
// from "the operator wrote an empty string" — and so the common zero-config
// entry encodes as just {"name":"nexus"}.
type BinaryInfo struct {
	// Name is the registry key — the exact string a claim puts in its `binary`
	// field. It is always present; a client with no label falls back to it.
	Name string `json:"name"`

	// Label is the entry's short human-readable name, for a picker's primary
	// text. Omitted when unset.
	Label string `json:"label,omitempty"`

	// Description is the entry's one-line explanation, for a picker's secondary
	// text. Omitted when unset.
	Description string `json:"description,omitempty"`
}

// binariesBody is the ENVELOPE GET /binaries returns: the listing under a single
// `binaries` key rather than as a bare top-level array.
//
// The envelope buys additive evolution. A top-level array has nowhere to put a
// broker-wide fact — a default-binary hint, a schema version, a "this registry
// was truncated" marker — so adding one later would mean changing the top-level
// JSON type, which breaks every client at once. With the envelope a new sibling
// key is invisible to a client that ignores unknown keys, the same additive
// contract the rest of the broker's JSON follows (see claimResponse.Ticket).
//
// Decided while there were no external clients, which is the only moment the
// choice is free: the cost of the wrapper is one key, and the cost of switching
// later is a coordinated client migration.
type binariesBody struct {
	// Binaries is the listing. Always present and never null — an empty registry
	// encodes as `[]` — so a client can iterate it unconditionally.
	Binaries []BinaryInfo `json:"binaries"`
}

// BinariesServer handles GET /binaries: a read-only listing of the binaries this
// broker can spawn, so a client can render a picker from live broker truth
// instead of hardcoding names that may not exist on the broker it is talking to.
//
// The listing is UNFILTERED — every caller sees every entry. Per-principal
// visibility is deliberately out of scope: an entry name is not a secret (a
// wrong one is already a 400 from POST /claim, which names the alternatives), and
// a filter would need a per-entry authorization model the config has no key for.
// If one is ever wanted it belongs next to that model, not bolted on here.
type BinariesServer struct {
	logger *slog.Logger

	// live is the atomic configuration snapshot this handler reads. It carries
	// the whole response envelope already projected and sorted — see
	// projectBinaries — so the listing is still computed ONCE per configuration
	// rather than per request.
	//
	// It is a holder rather than a stored envelope because SIGHUP can replace the
	// registry: caching the finished JSON at construction, which is what this
	// server used to do, would have kept GET /binaries advertising the
	// pre-reload registry for the life of the process. Reading through the holder
	// keeps the precomputation and loses the staleness.
	//
	// Precomputing is not only about the saved allocations. The served value
	// simply does not contain a path, an arg or an env var, so no future edit to
	// the handler can accidentally encode one. The envelope (not just its slice)
	// is held, so a broker-wide field added to binariesBody later is likewise
	// computed once, in projectBinaries, rather than assembled per request.
	live *configHolder
}

// NewBinariesServer builds the listing over a private snapshot of one binary
// registry. binaries is the fully folded map from Config.Binaries.
//
// run() calls useConfigHolder afterwards to point it at the process-wide
// snapshot instead, which is what makes the listing follow a reload.
func NewBinariesServer(logger *slog.Logger, binaries map[string]BinaryEntry) *BinariesServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &BinariesServer{logger: logger, live: newLocalConfigHolder(Config{Binaries: binaries})}
}

// useConfigHolder points the listing at the process-wide configuration snapshot,
// so a SIGHUP that changes `binaries:` changes what GET /binaries advertises.
//
// It is a setter rather than a constructor parameter for the same reason
// ClaimServer.useSpawnKey is: every existing caller hands this server a bare
// registry map, and threading the shared holder through the constructor would
// churn them all for a dependency only run() has. A nil holder is ignored, so a
// caller that never shares one keeps the private snapshot the constructor built.
func (s *BinariesServer) useConfigHolder(h *configHolder) {
	if h == nil {
		return
	}
	s.live = h
}

// projectBinaries renders the client-facing listing envelope from a folded
// binary registry. binaries is the fully folded map from Config.Binaries, so the
// reserved `nexus` entry is included like any other — it is spawnable from every
// broker and a picker that omitted it would be lying.
//
// It lives beside BinaryInfo rather than in reload.go because it IS the
// projection this file exists to define: the one place a registry entry becomes
// something a client may see. liveConfig calls it, once per configuration.
func projectBinaries(binaries map[string]BinaryEntry) binariesBody {
	// sortedBinaryNames, not a map walk: Go randomizes map iteration, and a
	// listing whose order changed per request would reshuffle every client's
	// picker and make any assertion on the response order flaky.
	names := sortedBinaryNames(binaries)
	// Non-nil even when empty so `binaries` encodes as `[]` rather than `null`. A
	// load always yields at least the reserved entry, so this is defensive against
	// a hand-built Config in a test rather than a reachable production state.
	entries := make([]BinaryInfo, 0, len(names))
	for _, name := range names {
		entry := binaries[name]
		entries = append(entries, BinaryInfo{
			Name:        name,
			Label:       entry.Label,
			Description: entry.Description,
		})
	}
	return binariesBody{Binaries: entries}
}

// Register wires the binaries endpoint onto a mux. It takes a routeMux so main
// can register it behind the auth guard, exactly as POST /claim is: a client
// that may not claim has no business enumerating what it cannot spawn.
func (s *BinariesServer) Register(mux routeMux) {
	mux.HandleFunc("GET /binaries", s.handleBinaries)
}

// handleBinaries writes the precomputed listing envelope as JSON.
//
// It reads no request state at all — not even the Principal. That is not an
// oversight: the listing is unfiltered, and an auth-disabled broker (where
// PrincipalFrom reports absent, the supported state POST /claim handles by
// falling back to anonymousOwner) must answer this route exactly as an
// authenticated one does. Touching the principal here would be the first step
// toward a scoping rule this story explicitly does not have.
func (s *BinariesServer) handleBinaries(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(s.live.get().binaries); err != nil {
		// The status and part of the body are already on the wire, so there is no
		// error response left to send. Log it and move on — the client sees a
		// truncated body and its own decode failure.
		s.logger.Warn("encoding binaries listing failed", "error", err)
	}
}
