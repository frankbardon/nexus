package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
)

// This file owns the broker's live configuration: the single atomic snapshot
// every request path reads, and the SIGHUP handler that replaces it.
//
// WHY A SNAPSHOT RATHER THAN A LOCKED STRUCT. Before this existed, ClaimServer,
// BinariesServer and A2AServer each held a by-value copy of Config taken at
// construction, and BinariesServer went further and cached the finished JSON of
// GET /binaries. Nothing could be changed without restarting the process — and
// a restart is the one event that costs every lease whose instance fails to
// reattach within reattach_window. The fix is not a mutex around Config: a
// reader that took a lock per field could still observe a half-applied
// configuration, and the whole point of this design is that no request can ever
// see one. Instead every reader dereferences ONE atomic pointer to an
// IMMUTABLE liveConfig and reads everything it needs from that one value. A
// reload builds a complete replacement off to the side, validates it, and
// publishes it with a single store.
//
// THE IMMUTABILITY RULE. A liveConfig that has been published must never be
// mutated — not the Config inside it, not the maps that Config holds. Readers
// hold pointers into it for the life of a request. Every change goes through a
// freshly built value and a swap.

// liveConfig is one immutable, atomically published snapshot of the broker's
// configuration together with everything derived from it that a request path
// reads.
//
// The derived halves are computed HERE, once per configuration, rather than per
// request, for two different reasons:
//
//   - binaries is the GET /binaries response envelope. Projecting it in one
//     place is what keeps a filesystem path, an argv entry or an environment
//     variable from ever reaching a client: the served value simply does not
//     contain one, so no future edit to the handler can leak it by accident.
//   - cards are the rendered Agent Cards. They are rendered with the rest of
//     the snapshot so a request can never see one profile's identity under
//     another's name — the whole card map is published in the same store as the
//     configuration it was rendered from.
type liveConfig struct {
	// cfg is the settled configuration. Treat it as read-only.
	cfg Config

	// binaries is the GET /binaries envelope for cfg.Binaries.
	binaries binariesBody

	// cards is the rendered Agent Card per profile name. It doubles as the A2A
	// routing table: a path whose {profile} is not a key here names no agent.
	// Nil when no profile is configured, which is a valid empty snapshot.
	cards map[string]*servedAgentCard

	// names is the profile list in a stable order, for the boot and reload logs.
	names []string
}

// newLiveConfig settles a Config and renders everything derived from it.
//
// It fails when a profile's Agent Card cannot be rendered, which is what makes
// an unservable card a BOOT failure rather than a surprise for the first client
// that fetches it — and, on a reload, a REJECTED reload rather than a broker
// serving a card it could not finish building.
func newLiveConfig(cfg Config) (*liveConfig, error) {
	settled := settleConfig(cfg)
	lc := &liveConfig{
		cfg:      settled,
		binaries: projectBinaries(settled.Binaries),
	}
	if len(settled.Agents) == 0 {
		return lc, nil
	}
	// Sorted rather than a map walk so a config with two unservable cards fails
	// on the same one every time.
	lc.names = sortedAgentNames(settled.Agents)
	lc.cards = make(map[string]*servedAgentCard, len(settled.Agents))
	for _, name := range lc.names {
		card, err := buildAgentCard(name, settled.Agents[name], settled.A2ABaseURL, settled.AuthValidators)
		if err != nil {
			return nil, err
		}
		lc.cards[name] = card
	}
	return lc, nil
}

// newLocalLiveConfig settles a Config WITHOUT rendering Agent Cards.
//
// It exists for the component-local holders the per-concern constructors build
// when they are handed a bare Config — the shape every test and any in-process
// embedder uses. Those components (the claim handler, the binaries listing, the
// release handler, the idle sweeper) never read a card, so rendering one would
// buy nothing and would give their constructors an error to return for a
// failure they cannot encounter. run() builds the one card-rendering holder and
// shares it with all of them through useConfigHolder.
func newLocalLiveConfig(cfg Config) *liveConfig {
	settled := settleConfig(cfg)
	return &liveConfig{cfg: settled, binaries: projectBinaries(settled.Binaries)}
}

// settleConfig folds the bounds whose zero value has no sane reading onto their
// defaults, so every reader of a published snapshot sees one settled number.
//
// It is NOT a second validation path. LoadConfigFromBytes already refuses a
// non-positive ready_timeout, session_report_grace or max_claim_body naming the
// key, so a LOADED config never reaches a fallback here. The fallbacks exist for
// a Config built as a struct literal rather than loaded, where "unset" must keep
// meaning "the default" rather than "reject every claim instantly" — which is
// exactly the reading NewClaimServer and NewReleaseServer applied before this
// file existed.
//
// The values it deliberately leaves alone are the ones whose non-positive
// reading is meaningful: idle_timeout and max_turn_duration disable a bound,
// queue_wait_timeout disables waiting, and max_concurrent means unlimited.
func settleConfig(cfg Config) Config {
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = defaultReadyTimeout
	}
	if cfg.SessionReportGrace <= 0 {
		cfg.SessionReportGrace = defaultSessionReportGrace
	}
	if cfg.MaxClaimBody <= 0 {
		cfg.MaxClaimBody = defaultMaxClaimBody
	}
	if cfg.ReleaseGrace <= 0 {
		cfg.ReleaseGrace = defaultReleaseGrace
	}
	return cfg
}

// configHolder is the atomic cell every live-config reader dereferences. It is
// safe for concurrent use by construction: readers only ever load the pointer,
// and a writer only ever stores a fully built replacement.
type configHolder struct {
	v atomic.Pointer[liveConfig]
}

// newConfigHolder builds the process-wide holder, rendering every configured
// profile's Agent Card. An unrenderable card is returned as an error, so it
// fails the boot.
func newConfigHolder(cfg Config) (*configHolder, error) {
	lc, err := newLiveConfig(cfg)
	if err != nil {
		return nil, err
	}
	h := &configHolder{}
	h.v.Store(lc)
	return h, nil
}

// newLocalConfigHolder builds a component-local holder from a bare Config. See
// newLocalLiveConfig for why it renders no cards and cannot fail.
func newLocalConfigHolder(cfg Config) *configHolder {
	h := &configHolder{}
	h.v.Store(newLocalLiveConfig(cfg))
	return h
}

// get returns the current snapshot. It is never nil: every constructor stores
// one before the holder escapes.
func (h *configHolder) get() *liveConfig { return h.v.Load() }

// config returns the current configuration. The returned pointer aims INTO an
// immutable snapshot — read it, never write through it.
func (h *configHolder) config() *Config { return &h.v.Load().cfg }

// store publishes a replacement snapshot. This is the whole reload primitive:
// one store, so no reader can observe a half-applied configuration.
func (h *configHolder) store(lc *liveConfig) { h.v.Store(lc) }

// reloadableKeys names the config keys SIGHUP applies. It is documentation that
// the code reads: reloadDiff walks it, so the log line an operator sees and the
// set mergeReloadable copies cannot drift apart.
//
// The list is the answer to "what can be changed without costing every live
// lease": what a claim spawns (the binary registry and the environment it
// carries), which agents this broker fronts, and the behavioural bounds. Every
// one of them is read at USE time from the snapshot, which is why swapping the
// snapshot is enough to change it.
var reloadableKeys = []struct {
	name string
	same func(prev, next Config) bool
}{
	// binaries: folds in the deprecated nexus_binary_path alias and the
	// broker-level run_as default, so all three are one reloadable unit — after
	// the fold there is no way to tell which of them an operator edited.
	{"binaries", func(p, n Config) bool { return reflect.DeepEqual(p.Binaries, n.Binaries) }},
	{"inherit_env", func(p, n Config) bool { return reflect.DeepEqual(p.InheritEnv, n.InheritEnv) }},
	{"agents", func(p, n Config) bool { return reflect.DeepEqual(p.Agents, n.Agents) }},
	{"max_concurrent", func(p, n Config) bool { return p.MaxConcurrent == n.MaxConcurrent }},
	{"idle_timeout", func(p, n Config) bool { return p.IdleTimeout == n.IdleTimeout }},
	{"max_turn_duration", func(p, n Config) bool { return p.MaxTurnDuration == n.MaxTurnDuration }},
	{"queue_wait_timeout", func(p, n Config) bool { return p.QueueWaitTimeout == n.QueueWaitTimeout }},
	{"release_grace", func(p, n Config) bool { return p.ReleaseGrace == n.ReleaseGrace }},
	{"ready_timeout", func(p, n Config) bool { return p.ReadyTimeout == n.ReadyTimeout }},
	{"session_report_grace", func(p, n Config) bool { return p.SessionReportGrace == n.SessionReportGrace }},
	{"max_claim_body", func(p, n Config) bool { return p.MaxClaimBody == n.MaxClaimBody }},
}

// bootOnlyKeys names the config keys a reload REPORTS AND IGNORES. Each one is
// boot-only for a stated reason, and the reasons are not interchangeable.
//
//   - auth: is the load-bearing one. The jwks validator holds a live kid cache
//     with rate-limited fetches, and two documented guarantees rest on it
//     surviving: key rotation needs no restart, and an unreachable issuer never
//     turns into an allow. Rebuilding the chain discards that cache, so a reload
//     performed during an IdP outage would turn a working broker into one that
//     denies every JWT — an outage of the broker's own making, caused by the
//     very mechanism meant to avoid one. admin_scope rides with it because it is
//     lifted out of the same block.
//   - listen_addr, state_dir and broker_id are structurally boot-only: a new
//     listener is a restart, the lease journal / spawn key / session index are
//     already open against the old directory and recovery has already run, and
//     the broker id is stamped on every record this broker has already written.
//   - advertise_addr is stamped into each lease record at registration, so
//     changing it live would make this process's records disagree with each
//     other about the same broker.
//   - reattach_window is consumed once, at boot, by the restored-lease reaper.
//   - client_replay_buffer_bytes is stamped on a lease's stream when the lease
//     is created, so it has no per-request read to swap.
//   - max_queue_depth and the two per-principal caps are admission state held by
//     the registry rather than read from the config per request.
//   - a2a.tasks: sizes a durable store that is already open, on the same footing
//     as state_dir.
var bootOnlyKeys = []struct {
	name string
	same func(prev, next Config) bool
}{
	{"listen_addr", func(p, n Config) bool { return p.ListenAddr == n.ListenAddr }},
	{"advertise_addr", func(p, n Config) bool { return p.AdvertiseAddr == n.AdvertiseAddr }},
	{"state_dir", func(p, n Config) bool { return p.StateDir == n.StateDir }},
	{"broker_id", func(p, n Config) bool { return p.BrokerID == n.BrokerID }},
	{"auth", func(p, n Config) bool { return reflect.DeepEqual(p.Auth, n.Auth) }},
	{"auth.admin_scope", func(p, n Config) bool { return p.AdminScope == n.AdminScope }},
	{"reattach_window", func(p, n Config) bool { return p.ReattachWindow == n.ReattachWindow }},
	{"client_replay_buffer_bytes", func(p, n Config) bool {
		return p.ClientReplayBufferBytes == n.ClientReplayBufferBytes
	}},
	{"max_queue_depth", func(p, n Config) bool { return p.MaxQueueDepth == n.MaxQueueDepth }},
	{"max_leases_per_principal", func(p, n Config) bool {
		return p.MaxLeasesPerPrincipal == n.MaxLeasesPerPrincipal
	}},
	{"max_queued_per_principal", func(p, n Config) bool {
		return p.MaxQueuedPerPrincipal == n.MaxQueuedPerPrincipal
	}},
	{"a2a.tasks", func(p, n Config) bool {
		return p.A2ATaskRetention == n.A2ATaskRetention && p.A2AInputTimeout == n.A2AInputTimeout
	}},
}

// mergeReloadable builds the configuration a reload will publish: the one
// currently IN FORCE, with only the reloadable keys taken from the newly read
// file.
//
// Building it this way round — start from prev, overwrite a named list — is what
// makes "a boot-only key that changed is ignored" true BY CONSTRUCTION rather
// than by a reviewer remembering to check. A key nobody added to the list below
// cannot be applied by accident, whatever the file says.
//
// It also carries the derived boot-only state that has no key of its own:
// AuthChain (the live validator chain, cache and all), AuthValidators (which the
// cards' securitySchemes are derived from), A2ABaseURL and the parsed
// advertise scheme/host. All of them come along for free because they are simply
// not on the overwrite list.
func mergeReloadable(prev, next Config) (Config, []string) {
	out := prev
	var ignored []string

	out.Binaries = next.Binaries
	out.InheritEnv = next.InheritEnv
	out.RunAs = next.RunAs
	// Inert after foldBinaryRegistry, but carried so the merged Config is a
	// faithful record of the file it came from rather than a mix of two.
	out.NexusBinaryPath = next.NexusBinaryPath

	out.MaxConcurrent = next.MaxConcurrent
	out.IdleTimeout = next.IdleTimeout
	out.MaxTurnDuration = next.MaxTurnDuration
	out.QueueWaitTimeout = next.QueueWaitTimeout
	out.ReleaseGrace = next.ReleaseGrace
	out.ReadyTimeout = next.ReadyTimeout
	out.SessionReportGrace = next.SessionReportGrace
	out.MaxClaimBody = next.MaxClaimBody

	// The warnings belong to the document that was just read, so a reload's
	// deprecation notices are the reloaded file's and not the boot file's.
	out.Warnings = next.Warnings

	// Profiles, with ONE exception. A broker that booted with no `agents:` block
	// registered no A2A routes at all — that absence is the documented mechanism
	// behind "a broker with no agents: behaves exactly as it did before the
	// ingress existed" — and it also opened neither the context index nor the
	// durable task store, because a broker that fronts no agents must not gain
	// files it will never write to. Publishing cards a reload could not route to,
	// backed by state that was never opened, would be a silently degraded
	// ingress. Turning the ingress ON is therefore a restart; changing, adding or
	// removing profiles on a broker that already serves at least one is not.
	if len(prev.Agents) == 0 && len(next.Agents) > 0 {
		ignored = append(ignored, "agents (enabling the a2a ingress on a broker that booted without one)")
	} else {
		out.Agents = next.Agents
	}

	for _, k := range bootOnlyKeys {
		if !k.same(prev, next) {
			ignored = append(ignored, k.name)
		}
	}
	return out, ignored
}

// reloadDiff names the reloadable keys whose value actually changed, for the
// applied-reload log line. An operator who sent SIGHUP needs to see what the
// broker believes it just changed, not merely that it reloaded.
func reloadDiff(prev, next Config) []string {
	var changed []string
	for _, k := range reloadableKeys {
		if !k.same(prev, next) {
			changed = append(changed, k.name)
		}
	}
	return changed
}

// reloader re-reads the broker config file on demand and applies the reloadable
// subset of it.
//
// It is VALIDATE-THEN-SWAP and it is atomic. The file is parsed and resolved by
// exactly the boot path (LoadConfig), merged onto the configuration in force,
// and rendered into a complete liveConfig — all off to the side. Only when every
// one of those steps has succeeded is anything published. A config that fails at
// any step leaves the previous one ENTIRELY in force and logs why; there is no
// state in which half of it applied.
type reloader struct {
	logger   *slog.Logger
	path     string
	live     *configHolder
	registry *Registry
}

// newReloader wires the SIGHUP handler to the holder every request path reads
// and to the registry, which owns the one reloadable value that is not read
// from the snapshot (the capacity ceiling lives behind the registry's lock,
// because acquiring a slot and consulting the ceiling have to be one step).
func newReloader(logger *slog.Logger, path string, live *configHolder, registry *Registry) *reloader {
	if logger == nil {
		logger = slog.Default()
	}
	return &reloader{logger: logger, path: path, live: live, registry: registry}
}

// reload re-reads the config file and publishes it. It returns the reason a
// reload was refused, or nil when one was applied.
func (rl *reloader) reload() error {
	next, err := LoadConfig(rl.path)
	if err != nil {
		rl.logger.Error("config reload rejected; the configuration already in force is unchanged",
			"path", rl.path, "error", err)
		rl.registry.Metrics().reloadOutcome(reloadOutcomeRejected)
		return err
	}

	prev := rl.live.get()
	merged, ignored := mergeReloadable(prev.cfg, next)

	// Rendered BEFORE anything is published, so a profile whose card cannot be
	// built refuses the whole reload rather than leaving the broker with a
	// half-swapped card map.
	lc, err := newLiveConfig(merged)
	if err != nil {
		rl.logger.Error("config reload rejected; the configuration already in force is unchanged",
			"path", rl.path, "error", err)
		rl.registry.Metrics().reloadOutcome(reloadOutcomeRejected)
		return err
	}

	changed := reloadDiff(prev.cfg, lc.cfg)

	// Drained here rather than at the parse site for the same reason boot drains
	// them: the parser has no logger, and a deprecation an operator reintroduced
	// in the file they just reloaded should be said out loud again.
	for _, warning := range lc.cfg.Warnings {
		rl.logger.Warn(warning)
	}

	// THE SWAP. One store publishes the configuration, the GET /binaries
	// projection and every Agent Card together, so no request can see a mixture.
	rl.live.store(lc)

	// Counted immediately after the swap, which is the moment the reload became
	// real. A rejected reload is counted at each refusal site above, so the two
	// series together are an exhaustive account of every SIGHUP this process
	// handled — an operator can alert on the rejected rate without having to
	// scrape logs to know a reload happened at all.
	rl.registry.Metrics().reloadOutcome(reloadOutcomeApplied)

	// The capacity ceiling is the one value that cannot ride along in the
	// snapshot: it is consulted under the registry's lock in the same step that
	// takes a slot, so it lives there. Applied immediately after the store, and
	// a raised ceiling admits whatever the capacity queue is already holding.
	if rl.registry != nil {
		rl.registry.setMaxConcurrent(lc.cfg.MaxConcurrent)
	}

	if len(ignored) > 0 {
		rl.logger.Warn("config reload: these keys changed in the file but are only read at boot, "+
			"so the values in force are unchanged; restart the broker to apply them",
			"path", rl.path, "keys", strings.Join(ignored, ","))
	}

	if len(changed) == 0 {
		rl.logger.Info("config reload applied; no reloadable key changed", "path", rl.path)
		return nil
	}
	rl.logger.Info("config reload applied",
		"path", rl.path,
		"changed", strings.Join(changed, ","),
		"binaries", len(lc.cfg.Binaries),
		"agents", len(lc.cards),
		"max_concurrent", lc.cfg.MaxConcurrent,
		"idle_timeout", lc.cfg.IdleTimeout,
		"queue_wait_timeout", lc.cfg.QueueWaitTimeout,
		"release_grace", lc.cfg.ReleaseGrace,
		"ready_timeout", lc.cfg.ReadyTimeout)
	return nil
}

// watchReloadSignals runs the SIGHUP loop until ctx is cancelled.
//
// It takes its OWN signal channel rather than riding on run()'s
// signal.NotifyContext, and it has to: that context is cancelled by the first
// signal it sees, which is exactly the wrong response to a reload request.
// SIGHUP's default disposition is to terminate the process, so a broker whose
// operator sends one before this handler is installed dies instead of
// reloading — which is why it is installed in run() rather than lazily.
//
// Reloads are serialized by construction: one loop, one goroutine, one reload at
// a time. A second SIGHUP that arrives mid-reload is coalesced into the buffered
// channel and handled next, so a rapid burst cannot interleave two swaps.
func watchReloadSignals(ctx context.Context, rl *reloader) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			rl.logger.Info("SIGHUP received, reloading config", "path", rl.path)
			// The error is already logged at the failure site, with the reason.
			// Nothing here can act on it: a refused reload is a no-op by design.
			_ = rl.reload()
		}
	}
}
