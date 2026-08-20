package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/nexusauth"
	"gopkg.in/yaml.v3"
)

// Config holds the nexus-broker service configuration. It is loaded from a
// YAML file at startup, mirroring the engine's file-based config style.
//
// All keys are live: listen_addr (gateway bind), binaries (the named registry
// of nexus variants this broker may spawn, superseding the deprecated
// nexus_binary_path), advertise_addr (client-reachable address),
// max_concurrent (capacity cap),
// client_replay_buffer_bytes (per-lease client-bound replay retention),
// idle_timeout (idle reaping), max_turn_duration (in-flight turn bound),
// release_grace (graceful-shutdown grace),
// queue_wait_timeout (FIFO capacity wait), max_queue_depth (queue ceiling),
// max_leases_per_principal / max_queued_per_principal (per-tenant admission
// caps), auth (client authentication), and
// agents (the named A2A profiles this broker publishes).
type Config struct {
	// ListenAddr is the host:port the broker's HTTP/WS gateway binds to.
	ListenAddr string `yaml:"listen_addr"`

	// AdvertiseAddr is the address CLIENTS use to reach THIS broker, and it is
	// the highest-precedence input to the ws_url returned by POST /claim.
	//
	// It exists because listen_addr cannot answer the question. A wildcard bind
	// (":8080") names no host, so without this key the ws_url falls back to the
	// claim request's Host header — which behind a reverse proxy or load
	// balancer names the PROXY. A client then reconnects through the LB and may
	// land on a different broker, one that does not hold its lease. Auto-detection
	// is deliberately not attempted: a heuristic would fail silently in exactly
	// the deployment shape that matters, so this is explicit config or nothing.
	//
	// Accepted forms are `host:port` (implying the ws:// scheme, so existing
	// deployments see no change) or a scheme-qualified `ws://`, `wss://`,
	// `http://` or `https://` host, with the port optional in that form. The
	// scheme-qualified form exists for the TLS-terminating-proxy deployment,
	// where the broker itself speaks plain HTTP but clients must dial wss://.
	//
	// Empty (the default) preserves the pre-existing resolution order exactly.
	AdvertiseAddr string `yaml:"advertise_addr"`

	// AdvertiseScheme and AdvertiseHost are the parsed, validated form of
	// AdvertiseAddr. They are not YAML keys: LoadConfigFromBytes derives them so
	// a malformed advertise_addr fails the BOOT rather than quietly minting
	// broken URLs at claim time, and so the parse happens once per process
	// instead of once per claim.
	//
	// Both are empty when advertise_addr is unset. AdvertiseScheme is empty for
	// the bare `host:port` form too — the ws:// default is applied at use
	// (clientWSScheme), which keeps "unset" a single state rather than two.
	AdvertiseScheme string `yaml:"-"`
	AdvertiseHost   string `yaml:"-"`

	// NexusBinaryPath is the DEPRECATED single spawn target: the path to the
	// nexus binary the broker exec()s to spawn OS-isolated instances. Expanded
	// through engine.ExpandPath.
	//
	// It has been superseded by Binaries. The field survives so that existing
	// broker.yaml files keep parsing and so that readers not yet migrated keep
	// compiling; LoadConfigFromBytes folds its value into the `nexus` registry
	// entry, which is what spawn actually resolves against. Nothing new should
	// read it.
	NexusBinaryPath string `yaml:"nexus_binary_path"`

	// Binaries is the registry of named nexus variants this broker may spawn,
	// keyed by the name a claim selects. It replaces the single
	// nexus_binary_path target: one broker can front a base nexus, a
	// vision-enabled build, a pinned older release, and so on.
	//
	// After a successful load this map is ALWAYS non-empty and ALWAYS contains
	// the reserved `nexus` entry — see foldBinaryRegistry. That guarantee is
	// what makes "the base Nexus binary is spawnable from every broker" true
	// regardless of what an operator wrote, and it is why there is deliberately
	// no `default: true` field on an entry: a claim that omits `binary` always
	// means `nexus`, and an operator must not be able to silently change what an
	// existing client ends up spawning.
	Binaries map[string]BinaryEntry `yaml:"binaries"`

	// InheritEnv names the variables a spawned instance inherits from the
	// BROKER'S OWN environment, on top of the always-pass set (HOME, LANG, PATH,
	// TZ — see alwaysInheritedEnv) and the three broker-owned NEXUS_BROKER_*
	// variables. Names only: a value is whatever the broker process holds, so
	// this key declares what may cross the boundary, never what it is.
	//
	// Empty (the default) means an instance carries the always-pass set, its
	// entry's `env:`, and nothing else. That is a DELIBERATE BREAK with the
	// behaviour that preceded it, where a spawn simply took os.Environ(): a claim
	// supplies the whole engine config, every provider resolves its credential
	// from an env var that config NAMES (`api_key_env` and equivalents) and the
	// same config sets `base_url` — so wholesale inheritance let any caller read
	// any variable the broker held and post it wherever it liked. An allowlist of
	// known provider key names could not have closed that, because the caller
	// chooses the name; only reducing the environment to what the operator
	// declared bounds it.
	//
	// It is broker-level rather than per-entry because it answers a question
	// about the BROKER'S environment ("which of the things I was started with may
	// leave this process"), which does not vary by which variant is spawned. A
	// value that IS per-variant belongs in that entry's `env:`, which sets it
	// outright instead of forwarding it.
	InheritEnv []string `yaml:"inherit_env"`

	// RunAs is the DEFAULT OS credential spawned instances run under, for every
	// registry entry that does not declare its own. Absent (the default) means
	// instances run as the broker's own uid and gid, exactly as they always have.
	//
	// It is the fallback rather than the whole answer because the interesting
	// separation is between VARIANTS: a broker fronting a vision build and a
	// support agent wants those two apart from each other, not merely apart from
	// the host. An entry that declares `run_as` replaces this outright; see
	// BinaryEntry.RunAs and foldRunAs.
	RunAs *RunAsSpec `yaml:"run_as"`

	// Warnings are non-fatal configuration complaints collected during the load
	// — deprecated keys, folded aliases — for the caller to log.
	//
	// They are returned rather than logged in place because LoadConfigFromBytes
	// has no logger: config is parsed before anything else exists, and threading
	// a logger into it purely to say "this key is deprecated" would put a
	// dependency on the parser that nothing else needs. run() drains this at
	// boot so the complaint lands in the same output as every other startup
	// line, and tests can assert on it without capturing a log sink.
	Warnings []string `yaml:"-"`

	// StateDir is the per-broker directory holding this broker's lease journal
	// (`leases.jsonl`) and, when broker_id is unset, its generated identity
	// (`broker-id`). Expanded through engine.ExpandPath.
	//
	// EMPTY (the default) DISABLES PERSISTENCE ENTIRELY: nothing is written, no
	// directory is created, and the broker behaves exactly as it did before lease
	// durability existed. That is the default because durability writes an
	// operator did not ask for — into a path Nexus picked — is a worse surprise
	// than a restart that loses in-memory bookkeeping, which is the behaviour
	// every existing deployment already has.
	//
	// It must NOT be shared between brokers. Two brokers pointed at one directory
	// would append to the same journal and compact each other's live leases away.
	// Each broker gets its own.
	StateDir string `yaml:"state_dir"`

	// BrokerID is this broker's identity, stamped on every persisted lease record
	// alongside advertise_addr so a future shared store can tell whose lease is
	// whose. It must be STABLE ACROSS RESTARTS of the same broker.
	//
	// Empty (the default) means the broker generates one on first boot and
	// persists it at <state_dir>/broker-id, reusing it thereafter — stable and
	// unique with no operator effort. Set it explicitly to give a broker a name
	// that means something in a cluster ("broker-eu-1"). Irrelevant while
	// state_dir is unset, since nothing is then recorded.
	BrokerID string `yaml:"broker_id"`

	// MaxConcurrent caps the number of live instances. Placeholder for the
	// capacity story.
	MaxConcurrent int `yaml:"max_concurrent"`

	// ClientReplayBuffer is the raw `client_replay_buffer_bytes` key: how many
	// bytes of already-sent, client-bound frames each lease retains so a client
	// that missed them can be replayed.
	//
	// It is a POINTER so "not written" and "written as 0" stay distinguishable.
	// Absent takes defaultClientReplayBufferBytes; an explicit 0 DISABLES
	// retention while leaving sequencing intact, which is the supported way to
	// say "I want loss to be detectable but I will not pay memory for it"; a
	// negative value is a boot failure rather than a silent clamp, because it has
	// no reading a broker could act on.
	//
	// The bound is in BYTES rather than frames deliberately: client-bound
	// payloads run from a few-byte token delta to a hundred-kilobyte tool result,
	// so a frame count says nothing about how much memory a lease can pin. Use
	// ClientReplayBufferBytes, not this field — this one is the unresolved input.
	ClientReplayBuffer *int `yaml:"client_replay_buffer_bytes"`

	// ClientReplayBufferBytes is the RESOLVED per-lease replay bound. It is not a
	// YAML key: LoadConfigFromBytes folds ClientReplayBuffer onto the default so
	// every reader sees one settled number.
	//
	// Worst-case broker-wide retention is this value times MaxConcurrent.
	ClientReplayBufferBytes int `yaml:"-"`

	// IdleTimeout is how long a lease with NO turn in flight survives with no
	// client activity before the idle sweeper tears it down. A non-positive value
	// disables reaping entirely.
	//
	// It measures the HUMAN PAUSE, not the turn: a lease whose instance has
	// reported that it is working is exempt until its turn settles (see
	// MaxTurnDuration), so this can be set to the longest pause a session should
	// survive rather than to the longest turn an agent might take.
	IdleTimeout time.Duration `yaml:"idle_timeout"`

	// MaxTurnDuration bounds how long a single in-flight turn may exempt its
	// lease from IdleTimeout before the sweeper tears the lease down anyway, with
	// reasonTurnTimeout rather than reasonIdle.
	//
	// It exists because the idle exemption is otherwise unbounded: an instance
	// that wedges mid-turn, or whose tool never returns, never reports the idle
	// state that settles the turn, and would hold its lease and its capacity slot
	// for the lifetime of the broker.
	//
	// A non-positive value DISABLES the bound, restoring that unbounded
	// exemption; the default is defaultMaxTurnDuration. It is enforced by the
	// idle sweeper, so it is inert when IdleTimeout <= 0 has switched reaping off
	// altogether.
	MaxTurnDuration time.Duration `yaml:"max_turn_duration"`

	// QueueWaitTimeout is how long an over-capacity claim parks in the FIFO
	// capacity wait queue before returning a timeout error. A non-positive value
	// disables waiting: an at-capacity claim is rejected immediately.
	QueueWaitTimeout time.Duration `yaml:"queue_wait_timeout"`

	// MaxQueueDepth caps how many over-capacity claims may be parked in the FIFO
	// wait queue at once. A claim arriving past it is refused IMMEDIATELY, with a
	// message distinct from both other capacity refusals.
	//
	// It exists because MaxConcurrent bounds live instances and nothing bounded
	// the waiters behind them: every parked claim costs a goroutine, a timer and
	// an open connection for up to QueueWaitTimeout, so an over-capacity broker
	// otherwise accumulates all three without limit.
	//
	// A non-positive value means UNLIMITED, the same reading MaxConcurrent gives
	// the same shape.
	MaxQueueDepth int `yaml:"max_queue_depth"`

	// MaxLeasesPerPrincipal caps how many live leases ONE authenticated principal
	// may hold at once. Non-positive (the default) means no per-principal cap.
	//
	// It is enforced ONLY when authentication is configured, and never for the
	// anonymous principal. With no `auth:` block every lease is owned by the same
	// anonymous identity, so applying it there would count the whole broker
	// against one principal and silently become a second, lower MaxConcurrent —
	// see Registry.principalCaps.
	MaxLeasesPerPrincipal int `yaml:"max_leases_per_principal"`

	// MaxQueuedPerPrincipal caps how many claims ONE authenticated principal may
	// have parked in the FIFO capacity queue at once. Non-positive (the default)
	// means no per-principal cap.
	//
	// It is what stops a single caller looping on POST /claim from occupying the
	// whole queue and timing every other tenant's claim out behind it. Queue
	// ordering itself stays strictly FIFO — this bounds how much of the queue one
	// caller may hold, it does not reorder it.
	//
	// Gated on authentication exactly as MaxLeasesPerPrincipal is.
	MaxQueuedPerPrincipal int `yaml:"max_queued_per_principal"`

	// ReleaseGrace bounds how long a release (manual, idle, or crash teardown)
	// waits for an instance to shut its engine down cleanly before the broker
	// force-kills it. The session is always persisted by the graceful path; the
	// kill is the orphan-prevention backstop.
	ReleaseGrace time.Duration `yaml:"release_grace"`

	// ReattachWindow bounds how long a lease restored from the journal at boot may
	// sit with no instance reattached before the broker reaps it: kills the
	// process, frees the slot, and closes the record out.
	//
	// It exists because restart recovery adopts leases on the strength of a live
	// pid, and a pid can be alive without anything ever coming back for it — the
	// instance may have been killed and its pid reused, or it may be wedged. The
	// window is the point at which the broker stops holding a capacity slot open
	// for an instance that is not going to reconnect.
	//
	// A non-positive value falls back to defaultReattachWindow. It is deliberately
	// NOT disableable: "wait forever" is the orphaned-lease behaviour this key
	// exists to bound. Irrelevant while state_dir is unset, since nothing is then
	// restored.
	ReattachWindow time.Duration `yaml:"reattach_window"`

	// Auth is the raw `auth:` block, handed to pkg/nexusauth verbatim. It is
	// kept as a map rather than a typed struct because nexusauth owns the
	// validator vocabulary and parses it identically for the broker and for
	// plugin config; duplicating that shape here would let the two drift.
	//
	// Absent (or empty) means authentication is not configured, which the broker
	// treats as auth-off plus a loud startup warning — see run() in main.go.
	Auth authBlock `yaml:"auth"`

	// AdminScope is the scope a validated credential must carry to be treated as
	// a broker operator. It widens GET /leases from "the caller's own leases" to
	// the whole registry plus the capacity aggregates, and nothing else — the
	// mutating lease routes remain strict principal-id ownership.
	//
	// It is configured as `auth.admin_scope` but is NOT part of the map handed to
	// nexusauth: that package owns the `auth:` vocabulary and rejects every key it
	// does not know, so LoadConfigFromBytes lifts this one out of the block before
	// building the chain (see liftAdminScope). One key, one owner.
	//
	// Empty means NO caller is an operator: with auth on, GET /leases is then
	// caller-scoped for everybody. That is the safe reading of "no admin scope was
	// configured", and setting `admin_scope: ""` is the supported way to switch the
	// operator view off entirely.
	AdminScope string `yaml:"-"`

	// AuthChain is the validator chain built from Auth. It is not a YAML key:
	// LoadConfigFromBytes populates it so a malformed `auth:` block fails at
	// load — before anything is served — and so the chain (which a future
	// network-backed validator may hold live state for) is constructed exactly
	// once per process. It is never nil after a successful load, and a chain with
	// no validators is disabled, not permissive.
	AuthChain *nexusauth.Chain `yaml:"-"`

	// AuthValidators describes the parsed `auth:` block one entry per validator,
	// in chain order. It is not a YAML key: LoadConfigFromBytes keeps what
	// building the chain already had to parse.
	//
	// It exists because a Chain is deliberately opaque — it exposes only its
	// validator NAMES, since nothing on the request path may care what a
	// validator is made of. The Agent Card's securitySchemes do care: a card
	// must tell a client WHAT to present (a shared bearer token, an OIDC JWT
	// from a named issuer), and deriving that from the config is what keeps the
	// published card from claiming a credential the broker does not actually
	// accept. Nil when no `auth:` block is configured.
	AuthValidators []nexusauth.ValidatorConfig `yaml:"-"`

	// Agents is the `agents:` block: the named A2A profiles this broker
	// publishes, keyed by the name their routes are namespaced under.
	//
	// NIL (the default) means this broker has no A2A ingress at all — no card,
	// no JSON-RPC endpoint, no REST endpoint, and nothing new in the boot log.
	// That is what keeps a broker.yaml written before profiles existed behaving
	// exactly as it did.
	Agents map[string]AgentProfile `yaml:"agents"`

	// A2ABaseURL is the absolute origin the Agent Cards advertise their
	// endpoints under, derived from advertise_addr (or, failing that, from a
	// listen_addr that names a dialable host). Not a YAML key, and empty
	// whenever Agents is empty — see resolveA2ABaseURL for why it is resolved at
	// load rather than per request.
	A2ABaseURL string `yaml:"-"`

	// A2A is the `a2a:` block: the settings every `agents:` profile shares.
	//
	// It is separate from `agents:` because nothing in it is per profile. The
	// task store is one store for the whole broker — one file in state_dir, one
	// retention policy, one set of caps — so hanging its knobs off each profile
	// would invite an operator to configure four different retentions for one
	// file and be silently given whichever was resolved last.
	A2A A2ASettings `yaml:"a2a"`

	// A2ATaskRetention and A2AInputTimeout are the resolved, validated form of
	// the `a2a.tasks:` block. They are not YAML keys: LoadConfigFromBytes derives
	// them so a negative duration fails the BOOT rather than producing a store
	// that silently keeps nothing, and so "absent" and "explicitly zero" stay
	// distinguishable — zero DISABLES both knobs, which an absent key must not.
	A2ATaskRetention a2aTaskRetention `yaml:"-"`
	A2AInputTimeout  time.Duration    `yaml:"-"`
}

// A2ASettings is the `a2a:` block.
type A2ASettings struct {
	// Tasks bounds the durable A2A task store.
	Tasks A2ATaskSettings `yaml:"tasks"`
}

// A2ATaskSettings is the `a2a.tasks:` block.
//
// Every field is a POINTER so that "the key was not written" is distinguishable
// from "the key was written as zero". The distinction is load-bearing here: zero
// means DISABLED for all three knobs — keep every task for ever, never cap a
// context, let a question wait until the process exits — and an absent key must
// get the default instead, not the disabled behaviour.
//
// The key names and their meanings are nexus.io.a2a's, deliberately: an operator
// who has configured the standalone A2A listener already knows what `ttl`,
// `max_per_context` and `input_timeout` do, and two spellings of one policy
// would be a needless second thing to learn.
type A2ATaskSettings struct {
	// TTL is how long a terminal task is kept after its last transition.
	// "0s" keeps every task until a cap evicts it.
	TTL *time.Duration `yaml:"ttl"`

	// MaxPerContext is how many tasks are kept per (caller, contextId).
	// 0 disables the cap.
	MaxPerContext *int `yaml:"max_per_context"`

	// InputTimeout is how long a task may sit at TASK_STATE_INPUT_REQUIRED
	// before the broker abandons it. "0s" disables the deadline.
	InputTimeout *time.Duration `yaml:"input_timeout"`
}

// A2A task defaults.
//
// Retention is load-bearing rather than housekeeping. A broker persists a record
// for every task every client ever runs, and the store folds into memory, so an
// unbounded policy would grow with TRAFFIC rather than with any conversation.
//
// defaultA2ATaskTTL is 24 hours, the same number nexus.io.a2a uses and for the
// same reason: a task is only useful to a client that still holds its id, and an
// A2A client that has been away for a day has restarted, retried or given up. A
// day is also comfortably longer than any plausible reconnect window, so the TTL
// never expires a task somebody is still following. Matching the standalone
// listener matters on its own — the same client talking to the same agent must
// not find its task history disappearing on a different schedule depending on
// whether a broker is in front of it.
//
// defaultA2ATasksPerContext is 50, and it is deliberately LOWER than
// nexus.io.a2a's 200. A standalone listener serves exactly one context, so its
// per-context cap is also its total. A broker serves an unbounded number of them
// at once, so the per-context number multiplies by however many conversations
// are in flight; 50 turns of readable history per conversation is far more than
// a client polls back over, and it keeps the global ceiling
// (maxA2ATaskRecords) a backstop rather than the working limit.
//
// defaultA2AInputTimeout is 15 minutes, matching nexus.io.a2a. It is the
// deadlock policy for serial queueing as much as a resource bound: a task parked
// at INPUT_REQUIRED holds its conversation's queue AND its leased instance,
// because the agent loop that asked the question is blocked inside ask_user. A
// question nobody answers would therefore pin the conversation for ever, so
// there is always a deadline and it is stated rather than implied. Fifteen
// minutes is chosen against a human, not a machine: a question routed to a
// person has to survive being paged, read, thought about and answered. On expiry
// the task is driven to FAILED — a real terminal transition that closes every
// attached stream, frees the queue and tells the instance to cancel the turn.
// "0s" disables the deadline for an operator who genuinely wants a task parked
// until its instance is reaped; the cost of that choice is documented, not
// hidden.
const (
	defaultA2ATaskTTL         = 24 * time.Hour
	defaultA2ATasksPerContext = 50
	defaultA2AInputTimeout    = 15 * time.Minute
)

// BinaryEntry is one named nexus variant in the broker's spawn registry. The
// map key — not a field — is the name a claim selects it by, so an entry cannot
// disagree with its own name.
type BinaryEntry struct {
	// Path is the executable the broker exec()s for this entry. It is required:
	// an entry with no path names nothing spawnable, and defaulting it to the
	// entry name would turn a typo into a silent PATH lookup for a binary the
	// operator never meant to run. Expanded through engine.ExpandPath, so `~`
	// works here exactly as it does in every other Nexus path key.
	Path string `yaml:"path"`

	// Label is a short human-readable name for operator and client surfaces
	// ("Nexus (vision)"). Purely presentational — nothing routes on it. Empty is
	// fine; a consumer falls back to the entry name.
	Label string `yaml:"label"`

	// Description is a one-line explanation of what this variant is for, for the
	// same surfaces as Label. Purely presentational.
	Description string `yaml:"description"`

	// Args are extra argv entries for this variant, appended AFTER the broker's
	// own spawn arguments so they can add to the command line but never displace
	// the `-config`/`-recall` contract the instance protocol depends on.
	Args []string `yaml:"args"`

	// Env are extra environment variables for this variant, layered UNDER the
	// broker-owned NEXUS_BROKER_* variables (see pkg/brokerframe). Those name the
	// dial-back address, the lease and the spawn secret; letting an entry
	// override them would let a config entry point an instance at another broker
	// or hand it the wrong lease, so the broker's values always win.
	Env map[string]string `yaml:"env"`

	// RunAs is the OS credential this entry's instances are exec()d under. Absent
	// (the default) means the broker's own uid and gid, which is what every spawn
	// has always used.
	//
	// It is per ENTRY, with the broker-level `run_as` as the fallback, for the
	// same reason `env` is per entry: an operator who fronts a vision build and a
	// support agent from one broker wants them separated from EACH OTHER, not
	// merely from the host. An entry that declares the key REPLACES the broker
	// default outright rather than merging field by field — a uid taken from one
	// place and a gid from another is a credential nobody wrote down.
	//
	// After LoadConfigFromBytes this holds the EFFECTIVE credential (the entry's
	// own, or the folded broker default), so nothing downstream has to know which
	// of the two it came from — see foldRunAs.
	RunAs *RunAsSpec `yaml:"run_as"`

	// ResolvedPath is the ABSOLUTE, verified executable this entry spawns: Path
	// after tilde expansion, after a PATH lookup if Path was a bare name, and
	// after a stat that confirmed a regular file with an execute bit.
	//
	// It is not a YAML key. LoadConfig derives it, following the
	// AdvertiseScheme/AdvertiseHost precedent, for two reasons. First, a typo or a
	// missing build then fails the BOOT — naming the entry — instead of surfacing
	// as one user's failed claim hours later. Second, spawn does no filesystem
	// work per claim: the answer is computed once per process, so a PATH lookup
	// cannot resolve to one binary at boot and a different one mid-flight.
	//
	// Empty after LoadConfigFromBytes alone, which parses but deliberately does
	// not touch the filesystem — see LoadConfig.
	ResolvedPath string `yaml:"-"`
}

// RunAsSpec is the OS credential a spawned instance runs under: the uid and gid
// the broker sets on the child before exec.
//
// # Why the broker offers this at all
//
// Without it every instance runs as the BROKER'S uid with the broker's HOME, so
// the process boundary between two claims is not a privilege boundary: one
// tenant's instance can read every other tenant's session directory under
// ~/.nexus/sessions, and it can read <state_dir>/spawn-key — which is enough to
// derive any live lease's dial-back secret and impersonate its instance. Dropping
// to a different uid is what turns "a separate process" into "a separate
// principal".
//
// Both fields are pointers so "not written" is distinguishable from "written as
// 0": uid 0 is root, and defaulting an unwritten uid to it would be the worst
// possible reading of a half-filled block.
type RunAsSpec struct {
	// UID is the numeric user id the instance runs as. Numeric rather than a
	// name: resolving a name needs the passwd database, which is exactly the
	// thing a hardened deployment may not have inside the container the broker
	// runs in, and a name that resolves differently on two hosts is a silent
	// privilege change.
	UID *int `yaml:"uid"`

	// GID is the numeric group id the instance runs as. Required whenever UID is
	// set — see validateRunAs.
	GID *int `yaml:"gid"`

	// ResolvedHome is the HOME a spawn under this credential is given, and it is
	// not a YAML key: LoadConfig derives it from the run_as user's passwd entry,
	// following the ResolvedPath precedent.
	//
	// It exists because run_as without a HOME story is broken on arrival. HOME is
	// what resolves ~/.nexus (pkg/engine/paths.go), so an instance dropped to
	// another uid while still pointed at the BROKER'S home directory cannot write
	// its own session directory, and every claim fails at the first write. HOME
	// therefore follows the credential.
	//
	// Empty means the entry's `env` already sets HOME — the operator-set data dir
	// — in which case nothing is looked up and that value stands. It is also empty
	// after LoadConfigFromBytes alone, which touches no filesystem and reads no
	// passwd database.
	ResolvedHome string `yaml:"-"`
}

// keyRunAs is the config key, named in every error validateRunAs produces so an
// operator can grep for it.
const keyRunAs = "run_as"

// maxUnixID is the largest value a uid or gid can carry over to
// syscall.Credential, whose fields are uint32. Rejecting out-of-range values at
// load rather than truncating them in the spawn path is the difference between a
// boot failure naming the entry and an instance quietly running as a uid the
// operator never wrote.
// Typed as int64 so the bound is the same on a 32-bit build, where a plain int
// cannot hold it.
const maxUnixID int64 = 1<<32 - 1

// validateRunAs checks one declared `run_as` block. scope is the key path the
// errors are reported under ("run_as", or "binaries: vision: run_as"), so a
// broker-level mistake and a per-entry one each name the place they were written.
//
// A nil spec is the key being absent, which is always valid: run_as is opt-in and
// its absence must leave a spawn byte-identical to what it was before the key
// existed.
//
// Requiring BOTH fields whenever either is written is deliberate. A uid without a
// gid leaves the instance in the broker's primary group, so its files stay
// group-accessible to the broker's other instances — a boundary that looks
// complete in the config and is not. There is no reading of a half-written block
// that is safer than refusing it.
func validateRunAs(scope string, spec *RunAsSpec) error {
	if spec == nil {
		return nil
	}
	switch {
	case spec.UID == nil && spec.GID == nil:
		return fmt.Errorf("%s: declares neither uid nor gid; remove the block to run instances as the broker's own user, or set both", scope)
	case spec.UID == nil:
		return fmt.Errorf("%s: gid is set but uid is not; set both, because a group change alone leaves instances running as the broker's user", scope)
	case spec.GID == nil:
		return fmt.Errorf("%s: uid is set but gid is not; set both, because a uid change alone leaves instances in the broker's primary group and their session files reachable from it", scope)
	}
	if err := validateUnixID(scope, "uid", *spec.UID); err != nil {
		return err
	}
	return validateUnixID(scope, "gid", *spec.GID)
}

// validateUnixID rejects an id the kernel cannot represent. Negative is the
// realistic mistake (a YAML `-1`, or a placeholder nobody filled in); the upper
// bound exists because the value is carried in a uint32 and a silent wrap would
// spawn instances as an arbitrary user.
func validateUnixID(scope, field string, id int) error {
	if id < 0 || int64(id) > maxUnixID {
		return fmt.Errorf("%s.%s: %d is not a valid unix id; it must be between 0 and %d", scope, field, id, maxUnixID)
	}
	return nil
}

// foldRunAs validates the broker-level `run_as` default and every entry's own,
// then folds the default into the entries that declared none — so after this
// every entry carries the EFFECTIVE credential and the spawn path never has to
// consult two places.
//
// The default is folded rather than consulted at spawn time for the reason
// foldBinaryRegistry folds the deprecated alias: one resolved answer per entry,
// decided once at boot, is what makes the boot log able to state what each entry
// will actually do.
//
// Each entry gets a COPY of the default, not a pointer to it, because
// resolveRunAsHomes writes a per-entry answer onto it — an entry whose `env`
// already sets HOME resolves nothing, and sharing one struct would let that
// entry's outcome depend on which other entry was walked first.
func foldRunAs(binaries map[string]BinaryEntry, brokerDefault *RunAsSpec) error {
	if err := validateRunAs(keyRunAs, brokerDefault); err != nil {
		return err
	}
	for _, name := range sortedBinaryNames(binaries) {
		entry := binaries[name]
		if err := validateRunAs(fmt.Sprintf("binaries: %s: %s", name, keyRunAs), entry.RunAs); err != nil {
			return err
		}
		effective := entry.RunAs
		if effective == nil {
			effective = brokerDefault
		}
		if effective == nil {
			continue
		}
		spec := *effective
		entry.RunAs = &spec
		binaries[name] = entry
	}
	return nil
}

// envHomeKey is the environment variable HOME, spelled once so the passwd lookup
// and the spawn path agree on what an entry's `env` has to name to opt out of it.
const envHomeKey = "HOME"

// resolveRunAsHomes fills in ResolvedHome for every entry that runs under a
// run_as credential, from the passwd database, and refuses the boot when it
// cannot.
//
// This is the environmental half of the key and therefore lives in LoadConfig
// beside resolveBinaryRegistry, not in the pure parser: the same bytes
// legitimately resolve differently on two hosts.
//
// # Why HOME follows the credential
//
// HOME is what resolves ~/.nexus (pkg/engine/paths.go). An instance dropped to
// another uid while still holding the BROKER'S HOME cannot create
// ~/.nexus/sessions/<id>, so the claim fails at the first write — and if it
// could write there, the isolation the key was configured for would be undone
// by every instance sharing one session tree anyway.
//
// Sessions are therefore consistent PER REGISTRY ENTRY, which is the granularity
// the rest of the broker already binds them at: a session records the entry that
// created it and a resume under a different entry is refused with a 409 (see
// resolveSpawnBinary), so a session can never be replayed under an entry whose
// HOME would not contain it.
//
// An entry whose `env` already sets HOME is left alone. That is the escape hatch
// for a deployment that keeps instance state somewhere other than a home
// directory — a data dir under /var/lib, say — and it is why a uid with no passwd
// entry is a boot failure that names the alternative rather than a fatal dead end.
func resolveRunAsHomes(binaries map[string]BinaryEntry) error {
	for _, name := range sortedBinaryNames(binaries) {
		entry := binaries[name]
		if entry.RunAs == nil || entry.RunAs.UID == nil {
			continue
		}
		if _, ok := entry.Env[envHomeKey]; ok {
			continue
		}
		uid := *entry.RunAs.UID
		home, err := runAsHomeDir(uid)
		if err != nil {
			return fmt.Errorf("binaries: %s: %s.uid %d: %w; set binaries.%s.env.%s to the directory that user's instances should keep their sessions in",
				name, keyRunAs, uid, err, name, envHomeKey)
		}
		entry.RunAs.ResolvedHome = home
		binaries[name] = entry
	}
	return nil
}

// runAsHomeDir returns the home directory recorded for a uid.
//
// A relative home is refused rather than used: HOME is resolved by the INSTANCE,
// whose working directory the broker does not control, so a relative value would
// place one entry's sessions in different directories depending on how the broker
// happened to be started.
func runAsHomeDir(uid int) (string, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", fmt.Errorf("no home directory could be resolved for that user: %w", err)
	}
	home := strings.TrimSpace(u.HomeDir)
	if home == "" {
		return "", fmt.Errorf("that user has no home directory recorded")
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("that user's home directory %q is not absolute", home)
	}
	return home, nil
}

// registryUsesRunAs reports whether any entry spawns under a credential other
// than the broker's own. It exists for the boot WARN — see logBinaryRegistry.
func registryUsesRunAs(binaries map[string]BinaryEntry) bool {
	for _, entry := range binaries {
		if entry.RunAs != nil {
			return true
		}
	}
	return false
}

// reservedBinaryName is the registry entry that must exist after every load. It
// is what a claim gets when it names no binary, which is why it is synthesized
// rather than merely defaulted: an operator's `binaries:` map may add entries,
// but it may not take this one away.
const reservedBinaryName = "nexus"

// defaultNexusBinaryPath is the path the reserved entry takes when neither
// `binaries.nexus` nor the deprecated alias says otherwise. A bare name, so it
// resolves through PATH — exactly the zero-config behaviour the broker has had
// since it existed.
const defaultNexusBinaryPath = "nexus"

// keyNexusBinaryPath is the deprecated alias key, named in the errors and
// warnings the folding rules produce so an operator can grep for it.
const keyNexusBinaryPath = "nexus_binary_path"

// binaryAliasProbe answers the one question the merged Config cannot: did the
// OPERATOR write nexus_binary_path, or is Config.NexusBinaryPath just carrying
// the value DefaultConfig put there? The merged struct cannot tell the two
// apart, and the difference decides whether the deprecation warning fires and
// whether the alias/entry conflict is a conflict at all. A pointer decoded from
// the same bytes distinguishes absent from set, including set-to-empty.
type binaryAliasProbe struct {
	NexusBinaryPath *string `yaml:"nexus_binary_path"`
}

// foldBinaryRegistry validates the declared `binaries:` map and folds the
// deprecated nexus_binary_path alias into it, returning the resolved registry
// plus any non-fatal warnings.
//
// alias is nil when the key is absent from the YAML. The four cases:
//
//   - alias set, `binaries.nexus` absent → the alias is synthesized into the
//     reserved entry and a deprecation warning names the replacement key. This
//     is what makes every pre-registry deployment boot unchanged.
//   - alias set AND `binaries.nexus` set → ERROR. Picking a winner would be
//     worse than refusing: whichever way the tie broke, half of the operators
//     hitting it would silently spawn the binary they did not mean, and the
//     mistake would only surface as instances behaving oddly. Two keys naming
//     one thing is a config bug, and the cheapest place to fix it is a boot
//     failure that names both.
//   - neither set → the reserved entry is synthesized with the historical
//     default path, so zero-config is byte-for-byte what it always was.
//   - `binaries.nexus` set alone → taken as written; nothing to fold.
func foldBinaryRegistry(declared map[string]BinaryEntry, alias *string) (map[string]BinaryEntry, []string, error) {
	out := make(map[string]BinaryEntry, len(declared)+1)

	for _, raw := range sortedBinaryNames(declared) {
		entry := declared[raw]
		// Trimmed so `"nexus ":` cannot smuggle in a second entry that looks
		// identical to the reserved one in every log line and UI.
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, nil, fmt.Errorf("binaries: an entry has an empty name; every entry must be keyed by the name a claim selects it with")
		}
		if _, dup := out[name]; dup {
			return nil, nil, fmt.Errorf("binaries: %q is declared more than once (names are compared with surrounding whitespace trimmed)", name)
		}
		path := strings.TrimSpace(entry.Path)
		if path == "" {
			return nil, nil, fmt.Errorf("binaries: %s: path is required and must name the executable to spawn", name)
		}
		entry.Path = engine.ExpandPath(path)
		out[name] = entry
	}

	_, reservedDeclared := out[reservedBinaryName]
	var warnings []string
	switch {
	case alias != nil && reservedDeclared:
		return nil, nil, fmt.Errorf("%s and binaries.%s both name the base nexus binary; remove %s and keep the registry entry",
			keyNexusBinaryPath, reservedBinaryName, keyNexusBinaryPath)
	case alias != nil:
		path := strings.TrimSpace(*alias)
		if path == "" {
			return nil, nil, fmt.Errorf("%s is set but empty; remove the key to take the default, or declare binaries.%s.path",
				keyNexusBinaryPath, reservedBinaryName)
		}
		out[reservedBinaryName] = BinaryEntry{Path: engine.ExpandPath(path)}
		warnings = append(warnings, fmt.Sprintf(
			"%s is deprecated: declare it as binaries.%s.path instead. Its value was folded into the %q registry entry for this boot.",
			keyNexusBinaryPath, reservedBinaryName, reservedBinaryName))
	case !reservedDeclared:
		out[reservedBinaryName] = BinaryEntry{Path: defaultNexusBinaryPath}
	}

	return out, warnings, nil
}

// keyInheritEnv is the broker-level pass-through key, named in the errors
// resolveInheritEnv produces so an operator can grep for it.
const keyInheritEnv = "inherit_env"

// resolveInheritEnv validates the declared `inherit_env` list and returns it
// trimmed, de-duplicated and sorted.
//
// Sorted rather than as-written because the list decides a spawned process's
// environment, and that environment is already built in sorted order so a boot is
// reproducible (see sortedEnvKeys); an order-sensitive list would be the only
// thing in the spawn path that was not.
//
// The three rejections are all "this entry can never do what it looks like it
// does", which is exactly what a boot failure is for:
//
//   - An empty entry names no variable. It is almost always a stray `-` or a
//     trailing comma in the YAML, and silently dropping it would hide the typo
//     next to it.
//   - A name containing `=` is not a variable name; an operator who wrote
//     `FOO=bar` here meant an entry's `env:` map, which SETS a value, and saying
//     so is more useful than exporting a variable literally called "FOO=bar".
//   - A NEXUS_BROKER_* name is meaningless: buildCommand injects those three
//     last, so declaring one changes nothing at all. Accepting it would leave an
//     operator believing they had configured the dial-back.
func resolveInheritEnv(declared []string) ([]string, error) {
	seen := make(map[string]struct{}, len(declared))
	// nil, not an empty slice: an undeclared list must stay indistinguishable from
	// DefaultConfig's zero value, which the defaults test compares structurally.
	var out []string
	for _, raw := range declared {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("%s: an entry is empty; every entry must name a variable to inherit from the broker's own environment", keyInheritEnv)
		}
		if strings.Contains(name, "=") {
			return nil, fmt.Errorf("%s: %q is a name=value pair, not a variable name; %s forwards a variable the broker already holds, and binaries.<name>.env is where a value is set",
				keyInheritEnv, name, keyInheritEnv)
		}
		if slices.Contains(brokerOwnedEnv, name) {
			return nil, fmt.Errorf("%s: %s is set by the broker on every spawn and cannot be inherited; remove it",
				keyInheritEnv, name)
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// sortedBinaryNames returns a registry map's keys in a stable order.
//
// Every walk of the registry goes through this, because Go randomizes map
// iteration: without it, a config with two bad entries would fail on a
// different one each boot (miserable to debug from a crash-looping container)
// and the boot log would list the entries in a different order every restart.
// Generic over the value type so it serves both the raw declared map and the
// resolved one.
func sortedBinaryNames[T any](m map[string]T) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolveBinaryRegistry resolves and verifies every entry's spawn target,
// recording the answer on each entry's ResolvedPath. It mutates the map in
// place; on the first failure it returns and the caller refuses the boot.
//
// The accepted tradeoff: a broker restarted midway through a variant rollout,
// while one of its binaries is momentarily absent, will not come up. That is
// deliberate — the alternative is a broker that boots fine and then rejects
// every claim for that variant, which the operator only discovers from a user.
// The error message is what makes it tolerable, so each one names the entry,
// the path that was resolved, and the specific reason.
func resolveBinaryRegistry(binaries map[string]BinaryEntry) error {
	for _, name := range sortedBinaryNames(binaries) {
		entry := binaries[name]
		resolved, err := resolveBinaryPath(name, entry.Path)
		if err != nil {
			return err
		}
		entry.ResolvedPath = resolved
		binaries[name] = entry
	}
	return nil
}

// resolveBinaryPath turns one entry's configured path into an absolute path
// that is known to be an executable file, or explains why it is not.
//
// The order is expand → PATH lookup → absolute → stat, and each step exists:
//
//   - ExpandPath so `~/builds/nexus-vision` works here exactly as it does in
//     every other Nexus path key. (foldBinaryRegistry already expanded, and the
//     helper is idempotent; repeating it keeps this function correct on its own.)
//   - exec.LookPath for a BARE name, so `nexus` means "whatever the broker's own
//     PATH resolves" — the zero-config default, and allowed for every entry, not
//     just the reserved one, because an operator who installs variants into
//     /usr/local/bin should not have to spell out directories.
//   - filepath.Abs so what is stored can be exec()d regardless of what the
//     process working directory later becomes, and so the boot log names a path
//     an operator can paste into a shell.
func resolveBinaryPath(name, path string) (string, error) {
	candidate := engine.ExpandPath(strings.TrimSpace(path))

	if isBareBinaryName(candidate) {
		found, err := exec.LookPath(candidate)
		if err != nil {
			return "", fmt.Errorf("binaries: %s: %q has no path separator, so it was looked up on the broker's PATH, and not found there: %w",
				name, candidate, err)
		}
		candidate = found
	}

	resolved, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("binaries: %s: cannot resolve %q to an absolute path: %w", name, candidate, err)
	}

	// Stat rather than exec: the broker must not run an operator's binary just to
	// find out whether it is runnable. Stat follows symlinks, so a symlinked
	// variant is judged on its target, which is what exec() would do too.
	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("binaries: %s: no such file %s%s", name, resolved, configuredAs(resolved, path))
		}
		return "", fmt.Errorf("binaries: %s: cannot stat %s%s: %w", name, resolved, configuredAs(resolved, path), err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("binaries: %s: %s is a directory, not an executable%s", name, resolved, configuredAs(resolved, path))
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("binaries: %s: %s is not a regular file (mode %s), so it cannot be exec()d%s",
			name, resolved, info.Mode(), configuredAs(resolved, path))
	}
	// Any of the three execute bits counts. This is deliberately NOT an attempt to
	// emulate the kernel's euid/egid permission check: the common failure is a
	// build artifact that was never chmod +x'd, and catching that is worth far
	// more than the false negative where a file is executable only by some other
	// user (which still fails, at exec time, with a clear EACCES).
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("binaries: %s: %s is not executable (mode %s); chmod +x it or point the entry at a different build%s",
			name, resolved, info.Mode().Perm(), configuredAs(resolved, path))
	}
	return resolved, nil
}

// isBareBinaryName reports whether path names something to look up on PATH
// rather than a location on disk. `nexus` is bare; `./nexus`, `bin/nexus` and
// `/usr/local/bin/nexus` are not.
//
// Both '/' and the host separator are checked because Go accepts '/' as a
// separator on every platform it targets, so a config written on one OS still
// reads as a path on another.
func isBareBinaryName(path string) bool {
	return !strings.ContainsRune(path, '/') && !strings.ContainsRune(path, filepath.Separator)
}

// configuredAs renders the trailing " (configured as ...)" clause that ties a
// resolved path back to what the operator actually wrote — but only when the two
// differ, so an entry that already names an absolute path does not get an error
// message that says the same thing twice.
func configuredAs(resolved, path string) string {
	if resolved == path {
		return ""
	}
	return fmt.Sprintf(" (configured as %q)", path)
}

// authBlock is the raw `auth:` mapping. It exists only to give a non-mapping
// `auth:` value an error that names the offending key: a silent fallback to
// "auth disabled" on a malformed block would be a security bug, so every
// malformed shape has to be a boot failure the operator can act on.
type authBlock map[string]any

// UnmarshalYAML implements yaml.Unmarshaler.
func (a *authBlock) UnmarshalYAML(node *yaml.Node) error {
	// `auth:` with nothing under it decodes as a null scalar. Treat it as absent
	// rather than malformed — it is a commented-out block mid-edit, not a typo.
	if node.Tag == "!!null" {
		*a = nil
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("auth: want a mapping of auth settings, got %s", node.Tag)
	}
	m := map[string]any{}
	if err := node.Decode(&m); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	*a = m
	return nil
}

// keyAdminScope is the broker-only key inside the `auth:` block that names the
// operator scope. It lives in the auth block because it is an authorization
// setting, but it is the broker's key, not nexusauth's — see liftAdminScope.
const keyAdminScope = "admin_scope"

// defaultAdminScope is the scope GET /leases treats as "operator" when the
// config does not say otherwise. It is namespaced like the rest of the project's
// dotted identifiers so it cannot collide with a scope an operator's IdP already
// issues for something else; a deployment whose IdP uses a different vocabulary
// overrides it with `auth.admin_scope`.
const defaultAdminScope = "nexus.broker.admin"

// defaultMaxQueueDepth is the ceiling on parked capacity waiters a broker takes
// when `max_queue_depth` is not written. It is 8x the default max_concurrent:
// generous enough that a burst of legitimate claims still queues rather than
// being refused, small enough that an over-capacity broker cannot accumulate
// goroutines, timers and held-open connections without limit.
//
// The per-principal caps have no default — they are off unless configured — for
// a different reason: a queue bound is a resource fact every broker wants, while
// a per-tenant quota is a policy only the operator can size.
const defaultMaxQueueDepth = 64

// DefaultConfig returns a Config populated with sane defaults. LoadConfig and
// LoadConfigFromBytes merge YAML on top of these.
//
// Auth defaults to absent, and therefore to a disabled AuthChain: a broker that
// has never been told about authentication must keep serving exactly as it did
// before authentication existed. AdminScope carries a default even so, because it
// only ever takes effect once auth IS configured.
func DefaultConfig() Config {
	return Config{
		ListenAddr:              ":8080",
		NexusBinaryPath:         defaultNexusBinaryPath,
		MaxConcurrent:           8,
		ClientReplayBufferBytes: defaultClientReplayBufferBytes,
		IdleTimeout:             5 * time.Minute,
		MaxTurnDuration:         defaultMaxTurnDuration,
		QueueWaitTimeout:        30 * time.Second,
		MaxQueueDepth:           defaultMaxQueueDepth,
		ReleaseGrace:            defaultReleaseGrace,
		ReattachWindow:          defaultReattachWindow,
		AdminScope:              defaultAdminScope,
		AuthChain:               nexusauth.NewChain(),
		A2ATaskRetention: a2aTaskRetention{
			ttl:           defaultA2ATaskTTL,
			maxPerContext: defaultA2ATasksPerContext,
		},
		A2AInputTimeout: defaultA2AInputTimeout,
	}
}

// LoadConfig reads a YAML broker config file from disk, parses it, and resolves
// it against the machine the broker is about to run on.
//
// It is the BOOT path, and the only caller that performs the second half. The
// split from LoadConfigFromBytes is deliberate: parsing is a pure function of
// the bytes and must stay that way, because the whole broker test suite loads
// config from string literals and would otherwise need a real nexus build on
// disk to assert anything about an unrelated key. Resolving the spawn registry,
// by contrast, is inherently environmental — the same bytes legitimately resolve
// differently on two machines — and it belongs with the step that was already
// touching the filesystem.
//
// The consequence for operators is intentional: a broker whose registry names a
// binary that is missing, is a directory, or has no execute bit does not start.
// That includes a zero-config broker whose PATH has no `nexus` on it, which
// previously got as far as accepting claims and only failed when one was made.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading broker config file: %w", err)
	}
	cfg, err := LoadConfigFromBytes(data)
	if err != nil {
		return Config{}, err
	}
	if err := resolveBinaryRegistry(cfg.Binaries); err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	// Same split, one step later: which directory a uid calls home is a property
	// of the machine, not of the document, so it is answered here rather than in
	// the pure parser. It runs for run_as entries only, so a broker that does not
	// use the key reads no passwd database at all.
	if err := resolveRunAsHomes(cfg.Binaries); err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	// Same split, same reasoning, one step later: a profile's config path is
	// environmental, so it is verified here rather than in the pure parser. A
	// broker with no `agents:` block does no work at all.
	if err := resolveAgentProfiles(cfg.Agents); err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	return cfg, nil
}

// LoadConfigFromBytes parses a YAML broker config from bytes, merged on top of
// DefaultConfig. Every filesystem path is funneled through engine.ExpandPath.
//
// It validates the DOCUMENT — shapes, required fields, mutually exclusive keys —
// and touches no filesystem. Binary registry entries come back with ResolvedPath
// empty; LoadConfig fills those in.
func LoadConfigFromBytes(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing broker config: %w", err)
	}
	cfg.NexusBinaryPath = engine.ExpandPath(cfg.NexusBinaryPath)

	// Resolve the spawn registry at load, from the same bytes, so a config that
	// names two spawn targets for the base binary — or an entry with no path —
	// fails the BOOT rather than the first claim that happens to select it. The
	// probe is decoded separately because only the raw document can say whether
	// the deprecated alias was written by the operator or defaulted by us.
	var probe binaryAliasProbe
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return Config{}, fmt.Errorf("parsing broker config: %w", err)
	}
	binaries, warnings, err := foldBinaryRegistry(cfg.Binaries, probe.NexusBinaryPath)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	cfg.Binaries = binaries
	cfg.Warnings = append(cfg.Warnings, warnings...)

	// Validated here, with the registry, because the two together decide the
	// whole environment a spawn will hold — and a mistake in either is a mistake
	// about what leaves this process, which must fail the boot rather than one
	// claim.
	inheritEnv, err := resolveInheritEnv(cfg.InheritEnv)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	cfg.InheritEnv = inheritEnv

	// Folded after the registry exists, because the broker-level default only
	// means anything relative to the entries it applies to — and validated in the
	// same breath, so a uid nobody can represent fails the boot naming the entry
	// rather than truncating into some other user at the first claim.
	if err := foldRunAs(cfg.Binaries, cfg.RunAs); err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}

	// Folded onto the default here so every reader of the config sees one settled
	// number, and so a value with no sane reading fails the BOOT rather than
	// quietly sizing every lease's buffer wrong for the life of the process.
	if cfg.ClientReplayBuffer != nil {
		if *cfg.ClientReplayBuffer < 0 {
			return Config{}, fmt.Errorf("broker config: client_replay_buffer_bytes: must not be negative; use 0 to disable replay retention")
		}
		cfg.ClientReplayBufferBytes = *cfg.ClientReplayBuffer
	}

	// Trimmed before expansion so a whitespace-only value reads as "unset"
	// (persistence disabled) rather than as a directory literally named " ".
	cfg.StateDir = engine.ExpandPath(strings.TrimSpace(cfg.StateDir))

	// Parse advertise_addr here so a malformed value is a boot failure. Deferring
	// it to claim time would surface the mistake as clients failing to connect to
	// a URL nobody looked at, which is the failure mode this key exists to remove.
	scheme, host, err := parseAdvertiseAddr(cfg.AdvertiseAddr)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	cfg.AdvertiseScheme = scheme
	cfg.AdvertiseHost = host

	// Take the broker's own key out of the auth block BEFORE the chain is built,
	// so what reaches nexusauth is exactly the block it owns.
	adminScope, err := liftAdminScope(cfg.Auth, cfg.AdminScope)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	cfg.AdminScope = adminScope

	// Build the validator chain here, at load, so an operator learns about a
	// misconfigured `auth:` block from a failed boot rather than from requests
	// being refused (or worse, waved through) in production. nexusauth's errors
	// name the offending key and path.
	//
	// Parsed and built in two steps rather than through the one-call
	// ChainFromMap, because the intermediate Config is worth keeping: the Agent
	// Card's securitySchemes are derived from it (see Config.AuthValidators).
	// The behaviour is identical — ChainFromMap is exactly these two calls.
	authCfg, err := nexusauth.ParseConfig(cfg.Auth)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	chain, err := nexusauth.BuildChain(authCfg)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	cfg.AuthChain = chain
	cfg.AuthValidators = authCfg.Validators

	// Profiles last: validating one needs the folded binary registry (to reject
	// a binary that does not exist) and the resolved advertise/listen addresses
	// (to derive the origin its card advertises), so both have to be settled
	// first.
	agents, err := foldAgentProfiles(cfg.Agents, cfg.Binaries)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	cfg.Agents = agents
	if len(cfg.Agents) > 0 {
		baseURL, err := resolveA2ABaseURL(cfg.AdvertiseScheme, cfg.AdvertiseHost, cfg.ListenAddr)
		if err != nil {
			return Config{}, fmt.Errorf("broker config: %w", err)
		}
		cfg.A2ABaseURL = baseURL
	}

	// The `a2a.tasks:` block is resolved unconditionally, not only when profiles
	// exist: a document that misspells a retention value is wrong whether or not
	// the ingress it configures is switched on, and failing only for brokers that
	// happen to front an agent would let the mistake sit unnoticed until the day
	// somebody adds one.
	retention, inputTimeout, err := resolveA2ATaskSettings(cfg.A2A.Tasks,
		cfg.A2ATaskRetention, cfg.A2AInputTimeout)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	cfg.A2ATaskRetention = retention
	cfg.A2AInputTimeout = inputTimeout

	return cfg, nil
}

// resolveA2ATaskSettings folds the `a2a.tasks:` block onto the defaults and
// validates it.
//
// A key that was not written keeps its default; a key written as zero DISABLES
// that knob, which is why the block is parsed through pointers. A NEGATIVE value
// is refused rather than clamped: "-1h" has no reading a broker could act on,
// and silently treating it as "disabled" would hide a typo behind a policy
// change nobody asked for.
func resolveA2ATaskSettings(block A2ATaskSettings, defaults a2aTaskRetention, defaultInput time.Duration) (a2aTaskRetention, time.Duration, error) {
	out := defaults
	input := defaultInput

	if block.TTL != nil {
		if *block.TTL < 0 {
			return out, input, fmt.Errorf("a2a.tasks.ttl: must not be negative; use \"0s\" to keep every task until a cap evicts it")
		}
		out.ttl = *block.TTL
	}
	if block.MaxPerContext != nil {
		if *block.MaxPerContext < 0 {
			return out, input, fmt.Errorf("a2a.tasks.max_per_context: must not be negative; use 0 to keep every task")
		}
		out.maxPerContext = *block.MaxPerContext
	}
	if block.InputTimeout != nil {
		if *block.InputTimeout < 0 {
			return out, input, fmt.Errorf("a2a.tasks.input_timeout: must not be negative; use \"0s\" to let a task stay parked until its instance is reaped")
		}
		input = *block.InputTimeout
	}
	return out, input, nil
}

// parseAdvertiseAddr splits a raw advertise_addr into the WebSocket scheme a
// client should use and the host:port it should dial. An empty input returns two
// empty strings and no error: advertise_addr is optional, and unset must keep the
// pre-existing ws_url resolution intact.
//
// Validation is deliberately strict, because every value that gets through here
// is handed to clients as an absolute URL:
//
//   - The bare form must carry a port. `example.com` alone would silently mean
//     port 80 via ws://, which is almost never where a broker listens.
//   - A wildcard host is rejected in either form. `0.0.0.0` is a bind address,
//     not a dialable one, and accepting it would reintroduce exactly the broken
//     ws_url this key exists to prevent.
//   - Path, query, fragment and userinfo are rejected in the URL form. The lease
//     path is appended by the caller, so anything here would corrupt the result.
func parseAdvertiseAddr(raw string) (scheme, host string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", nil
	}

	// "://" is the discriminator rather than ":" because an IPv6 literal
	// ("[::1]:8080") is full of colons but is not a URL.
	if strings.Contains(raw, "://") {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", "", fmt.Errorf("advertise_addr: %q is not a valid URL: %w", raw, perr)
		}
		switch u.Scheme {
		case "ws", "http":
			// http is accepted and normalized because operators reach for the
			// scheme they type into a browser; refusing it would be pedantry.
			scheme = "ws"
		case "wss", "https":
			scheme = "wss"
		default:
			return "", "", fmt.Errorf("advertise_addr: %q has unsupported scheme %q, want ws, wss, http or https", raw, u.Scheme)
		}
		if u.User != nil {
			return "", "", fmt.Errorf("advertise_addr: %q must not carry userinfo", raw)
		}
		if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", fmt.Errorf("advertise_addr: %q must name only a host[:port]; the lease path is appended by the broker", raw)
		}
		host = u.Host
	} else {
		h, port, serr := net.SplitHostPort(raw)
		if serr != nil {
			return "", "", fmt.Errorf("advertise_addr: %q is not a host:port (add a port, or use the ws://host / wss://host form): %w", raw, serr)
		}
		if port == "" {
			return "", "", fmt.Errorf("advertise_addr: %q has no port", raw)
		}
		host = net.JoinHostPort(h, port)
	}

	if err := checkAdvertiseHost(raw, host); err != nil {
		return "", "", err
	}
	return scheme, host, nil
}

// checkAdvertiseHost rejects a host that no client could dial. hostPort may or
// may not carry a port (the URL form allows omitting it), so the port is stripped
// only when it is actually there.
func checkAdvertiseHost(raw, hostPort string) error {
	bare := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		bare = h
	}
	switch bare {
	case "", "0.0.0.0", "::":
		return fmt.Errorf("advertise_addr: %q names no dialable host; it must name the address clients use to reach this broker", raw)
	}
	return nil
}

// liftAdminScope removes the broker-only `admin_scope` key from the raw auth
// block and returns the value it should take, falling back to current (the
// default) when the key is absent.
//
// It REMOVES rather than reads in place because everything left in the block is
// handed verbatim to nexusauth, which rejects any key it does not recognize: a
// key left behind would turn a perfectly valid config into a boot failure. The
// map is mutated in place so cfg.Auth keeps exactly one meaning — "the block
// nexusauth owns" — and nothing downstream has to remember to skip a key.
//
// `admin_scope:` present but empty (or explicitly null) is honoured as "no caller
// is an operator", so the operator view can be switched off; only an ABSENT key
// inherits the default.
func liftAdminScope(block authBlock, current string) (string, error) {
	raw, present := block[keyAdminScope]
	if !present {
		return current, nil
	}
	delete(block, keyAdminScope)
	if raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("auth: %s: want a string, got %T", keyAdminScope, raw)
	}
	return strings.TrimSpace(s), nil
}
