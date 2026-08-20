package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// spawnSpec describes a single instance spawn: which binary to exec, the temp
// config file the instance must read, and the lease/dial-back coordinates it
// needs to find its way back to this broker.
type spawnSpec struct {
	// binaryName is the registry entry the claim selected — the NAME, not the
	// path. It is carried alongside binaryPath rather than derived from it
	// because two entries may legitimately point at the same executable, so the
	// path cannot answer "which variant was this". Nothing on the spawn path
	// itself needs it; it exists so the lease can record what was claimed.
	binaryName string

	// binaryPath is the entry's RESOLVED absolute executable — BinaryEntry's
	// ResolvedPath, computed once at boot by LoadConfig. Spawn does no
	// filesystem work and no PATH lookup of its own.
	binaryPath string

	configPath string
	leaseID    string
	brokerAddr string // ws:// URL of the broker's instance dial-back endpoint

	// binaryArgs are the selected entry's static extra argv entries. They are
	// appended AFTER the broker's own arguments (see buildCommand), so a variant
	// can add flags but can never displace the -config/-recall contract the
	// instance protocol depends on.
	binaryArgs []string

	// binaryEnv are the selected entry's static extra environment variables.
	// They are layered UNDER the broker-owned NEXUS_BROKER_* variables, which
	// buildCommand appends last — an entry that could set the spawn secret would
	// defeat the dial-back second factor entirely.
	binaryEnv map[string]string

	// inheritEnv is the broker-level `inherit_env` list verbatim: the extra
	// variable NAMES (never values) a spawn may take from the broker's own
	// environment. buildCommand unions it with alwaysInheritedEnv and copies
	// across only what it names — everything else the broker process holds is
	// dropped, which is the point of the key.
	//
	// It is carried on the spec rather than read from the Config inside
	// buildCommand so that the whole environment a spawn will hold is decided by
	// its spec, and a test can pin it without a Config.
	inheritEnv []string

	// runAs is the EFFECTIVE OS credential this spawn runs under — the selected
	// entry's `run_as`, or the broker-level default folded into it at boot (see
	// foldRunAs). Nil means the credential the broker itself holds, which is what
	// every spawn used before the key existed.
	//
	// It carries the resolved HOME with it rather than having buildCommand look
	// one up, because the whole environment a spawn will hold is decided by its
	// spec — and because a passwd lookup per claim would let one entry's sessions
	// move under a running broker.
	runAs *RunAsSpec

	// spawnSecret is the per-lease second factor the instance must echo in its
	// register frame (see newSpawnSecret). The claim path mints it, records the
	// expected value on the lease, and passes it here; buildCommand injects it
	// into the child's environment. It is NEVER logged and never leaves the
	// broker↔child channel.
	spawnSecret string

	// recallSessionID, when non-empty, resumes a persisted session: the
	// instance is spawned with -recall <id> so the engine reloads that
	// session and replays its history instead of starting fresh.
	recallSessionID string
}

// processHandle is the broker's minimal view of a spawned instance process.
// It is tracked on the lease so later stories (release, crash, capacity) can
// manage the process lifecycle without the gateway knowing about exec.
type processHandle interface {
	// pid returns the OS process id, or 0 if the process has not started.
	pid() int
	// terminate asks the process — and everything it started — to stop cleanly:
	// SIGTERM to its process group, which the engine handles as a clean shutdown
	// that flushes and persists the session. It does NOT wait; the caller decides
	// how long to give it before escalating to kill.
	//
	// It exists because the shutdown frame is delivered over the dial-back socket,
	// and the case teardown most needs to handle is the one where that socket is
	// gone but the process is not. A signal needs nothing from the instance to
	// arrive.
	terminate() error
	// kill forcibly terminates the process and everything it started.
	kill() error
	// wait blocks until the process exits and returns its exit error.
	wait() error
}

// commandRunner builds and starts an instance process from a spawnSpec. The
// production implementation exec()s the nexus binary; unit tests substitute a
// fake that records the spec and returns a controllable handle without booting
// a real engine.
type commandRunner interface {
	start(ctx context.Context, spec spawnSpec) (processHandle, error)
}

// execRunner is the production commandRunner: it exec()s the configured nexus
// binary with the per-claim temp config and the broker dial-back env.
type execRunner struct{}

// start launches the instance. The process is intentionally NOT tied to the
// claim request's context — it must outlive the HTTP handler; the broker owns
// its lifecycle via the returned handle.
func (execRunner) start(_ context.Context, spec spawnSpec) (processHandle, error) {
	cmd := buildCommand(spec)
	if err := cmd.Start(); err != nil {
		// A credential the broker is not privileged to set fails HERE, at Start:
		// the child reports the setgroups/setgid/setuid errno back over its status
		// pipe between fork and exec, so os/exec surfaces it synchronously. That is
		// what makes a misconfigured run_as a claim that fails at once with a
		// reason, rather than an instance that never dials back and is only given
		// up on at the ready deadline half a minute later.
		if cred := spec.runAs; cred != nil && cred.UID != nil && cred.GID != nil {
			return nil, fmt.Errorf("starting nexus instance as run_as uid %d gid %d "+
				"(the broker process must be root, or hold CAP_SETUID and CAP_SETGID, to change a child's credentials): %w",
				*cred.UID, *cred.GID, err)
		}
		return nil, fmt.Errorf("starting nexus instance: %w", err)
	}
	return &execProcess{cmd: cmd}, nil
}

// buildCommand constructs the *exec.Cmd for an instance spawn. It is split out
// from start so a unit test can assert the args and env without launching a
// process. The instance is told to read the temp config via -config (matching
// cmd/nexus/main.go) and is handed its dial-back target through the shared
// brokerframe env constants — the single source of truth for these names.
//
// Both layering rules below are ORDER-DEPENDENT, and both point the same way:
// whatever a registry entry contributes goes on before what the broker owns, so
// a `binaries:` entry can extend a spawn but never redirect it.
func buildCommand(spec spawnSpec) *exec.Cmd {
	args := []string{"-config", spec.configPath}
	if spec.recallSessionID != "" {
		args = append(args, "-recall", spec.recallSessionID)
	}
	// Entry args come LAST in argv. Go's flag package stops at the first
	// non-flag argument, so prepending an entry's args could swallow -config
	// and leave the instance booting whatever default config it finds — the
	// broker's own arguments have to be the ones the parser sees first.
	args = append(args, spec.binaryArgs...)
	cmd := exec.Command(spec.binaryPath, args...)

	// The spawn secret travels in the ENVIRONMENT rather than in argv on
	// purpose: argv is world-readable through /proc (and `ps`) on the machines
	// this runs on, whereas a process's environment is readable only by its own
	// uid and root. It is injected unconditionally — even when the broker runs
	// without an `auth:` block and therefore does not enforce it — so that
	// turning authentication on is a pure broker-config change and never
	// requires a different spawn path.
	// NOT os.Environ(). A spawned instance is handed a config the CALLER wrote,
	// and every provider resolves its credential from an env var that config
	// NAMES (`api_key_env` and its equivalents) while the same config chooses
	// `base_url` — so anything left in the child's environment is both reachable
	// and exfiltratable by whoever posted the claim. Wholesale inheritance
	// therefore handed every claimant every secret the broker was started with,
	// and no allowlist of *known* provider keys could have fixed it, because the
	// caller picks the name. The only bound is the one applied here: a spawn
	// carries what the OPERATOR declared and nothing else.
	env := inheritedEnv(spec.inheritEnv)
	// HOME FOLLOWS run_as. It is appended after the inherited half, so it beats
	// the broker's own HOME that alwaysInheritedEnv just contributed.
	//
	// Without this the key is broken on arrival: HOME is what resolves ~/.nexus
	// (pkg/engine/paths.go), so an instance dropped to another uid while still
	// pointed at the BROKER'S home directory cannot create its session directory
	// and the claim fails at the first write. It is resolved at boot from the
	// run_as user's passwd entry (resolveRunAsHomes) and is empty when the entry's
	// `env` sets HOME itself — that map is applied next and would win regardless,
	// which is the operator-set data dir escape hatch.
	if spec.runAs != nil && spec.runAs.ResolvedHome != "" {
		env = append(env, envHomeKey+"="+spec.runAs.ResolvedHome)
	}
	// Entry env sits between what the broker's own environment contributed and
	// the broker-owned variables: a variant may override something inherited from
	// the broker's shell, which is the point of the key. Keys are applied in
	// sorted order so a spawn's environment is byte-identical across restarts
	// and a boot is reproducible; Go randomizes map iteration otherwise.
	for _, key := range sortedEnvKeys(spec.binaryEnv) {
		env = append(env, key+"="+spec.binaryEnv[key])
	}
	// The three brokerframe variables are appended LAST, and that is a security
	// boundary rather than a style choice: os/exec resolves a duplicated key to
	// its final occurrence, so this is what makes an entry unable to point an
	// instance at a different broker, hand it another lease's id, or — the one
	// that matters — supply its own NEXUS_BROKER_SPAWN_SECRET and thereby
	// authenticate a dial-back the broker never minted.
	env = append(env,
		brokerframe.EnvBrokerAddr+"="+spec.brokerAddr,
		brokerframe.EnvLeaseID+"="+spec.leaseID,
		brokerframe.EnvSpawnSecret+"="+spec.spawnSecret,
	)
	cmd.Env = env

	// Order between these two does not matter — both EXTEND SysProcAttr rather
	// than assigning it — but that is a property worth keeping true; see the
	// comment on applyRunAs.
	applyProcessGroup(cmd)
	applyRunAs(cmd, spec.runAs)

	// Surface the child's logs through the broker's stderr for observability.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd
}

// applyProcessGroup makes the instance the leader of a process group of its own,
// so teardown can signal the whole tree the instance created rather than only the
// instance itself.
//
// Without it, an instance's descendants — shell-tool commands, MCP stdio servers,
// code interpreters — are in the BROKER's process group and simply survive their
// instance's death, re-parented to init. A released lease then leaves running
// processes behind holding the session's files, ports and API budget, and nothing
// in the broker knows they exist. It is also what makes signalling a negative pid
// safe at all: groupSignalTarget only ever targets a group whose leader IS the
// instance, and that is only true because of this call.
//
// Like applyRunAs it EXTENDS SysProcAttr rather than assigning it — the two
// stories set different fields on the same struct, and either one replacing it
// would silently undo the other. Setpgid is set unconditionally, including when
// no run_as is configured, because an orphaned subprocess is not a
// credential-specific problem.
func applyProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// applyRunAs sets the OS credential the child is exec()d under, or does nothing
// at all when no run_as is configured — which is what keeps an unconfigured
// broker's spawn carrying the credential it always did (no Credential is
// attached, so os/exec performs no setgroups/setgid/setuid at all).
//
// SysProcAttr is EXTENDED here, never assigned wholesale: applyProcessGroup sets
// Setpgid on the same struct, and either one replacing it would silently drop
// whichever field the other set — turning a privilege drop into a no-op, or
// leaving every instance in the broker's process group. Any later story that
// needs a process attribute adds a field the same way.
//
// Groups is left nil with NoSetGroups false on purpose. That makes the child call
// setgroups(0, NULL), dropping every supplementary group the broker held — a
// credential change that kept the broker's group memberships would leave the
// instance holding whatever those groups can reach, which is most of what the key
// exists to take away.
//
// setgroups is itself privileged, so this is also why run_as needs root (or
// CAP_SETUID + CAP_SETGID) even when the uid it names is the broker's own: an
// unprivileged broker cannot make this call at all, and the spawn fails at Start.
func applyRunAs(cmd *exec.Cmd, spec *RunAsSpec) {
	if spec == nil || spec.UID == nil || spec.GID == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Range-checked at load (validateUnixID), so neither conversion can wrap.
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: uint32(*spec.UID),
		Gid: uint32(*spec.GID),
	}
}

// sortedEnvKeys returns an entry's env map keys in a stable order.
//
// It is deliberately not shared with sortedBinaryNames: that helper exists to
// make registry VALIDATION deterministic (which of two bad entries is reported),
// while this one exists to make a spawned process's environment reproducible.
// Same three lines, unrelated reasons to change.
func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// alwaysInheritedEnv names the variables every spawn takes from the broker's own
// environment REGARDLESS of what `inherit_env` says. They are not credentials
// and are not optional:
//
//   - HOME decides where ~/.nexus is (pkg/engine/paths.go, via os.UserHomeDir).
//     Without it an instance cannot create a session directory, so -recall has
//     nothing to resume and the claim fails for a reason that has nothing to do
//     with the operator's configuration.
//   - PATH is what makes exec() and the shell tool work at all; a child without
//     it can run nothing by name.
//   - TZ and LANG decide how the instance renders times and text. Omitting them
//     would silently move every instance to UTC and the C locale, which is a
//     behaviour change dressed up as a security fix.
//
// Held sorted, because inheritedEnvNames returns a sorted union and starting
// from a sorted base makes that obvious to a reader.
var alwaysInheritedEnv = []string{"HOME", "LANG", "PATH", "TZ"}

// brokerOwnedEnv names the three variables buildCommand injects itself, last, so
// nothing an entry or the broker's shell contributes can displace them. It
// exists so the boot log and config validation talk about the same set the
// spawn path does, rather than three literals repeated in four places.
var brokerOwnedEnv = []string{
	brokerframe.EnvBrokerAddr,
	brokerframe.EnvLeaseID,
	brokerframe.EnvSpawnSecret,
}

// inheritedEnvNames returns the sorted, de-duplicated set of variable names a
// spawn may take from the broker's environment: alwaysInheritedEnv plus whatever
// `inherit_env` declared. Declaring a name that is already in the always-pass set
// is redundant, not an error, so it collapses here rather than being rejected at
// load — an operator listing PATH explicitly is being clear, not wrong.
func inheritedEnvNames(configured []string) []string {
	set := make(map[string]struct{}, len(alwaysInheritedEnv)+len(configured))
	for _, name := range alwaysInheritedEnv {
		set[name] = struct{}{}
	}
	for _, name := range configured {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// inheritedEnv returns the `KEY=VALUE` entries a spawn inherits from the broker's
// own environment, in sorted key order for the same reproducibility reason
// sortedEnvKeys exists.
//
// A declared name the broker does not actually hold is SKIPPED rather than
// exported empty: an instance can tell "unset" from "set to the empty string",
// and a provider that finds its api_key_env set to "" fails differently (and less
// legibly) than one that finds it absent.
func inheritedEnv(configured []string) []string {
	names := inheritedEnvNames(configured)
	env := make([]string, 0, len(names))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// spawnEnvNames returns, sorted, the NAMES of every variable one registry entry's
// spawns will actually carry: what is inherited and present, what the entry
// declares, and the three broker-owned variables.
//
// It exists for the boot log, and it reports what will be CARRIED rather than
// what was declared on purpose. A name declared under `inherit_env` that the
// broker process does not hold is therefore absent from the line, which is the
// cheapest possible diagnostic for the failure this story introduces: an operator
// who declared ANTHROPIC_API_KEY but did not export it into the broker's shell
// sees that at boot instead of at the first turn.
//
// Values are never returned. The caller logs this, and half of these names are
// credentials.
func spawnEnvNames(configured []string, entryEnv map[string]string) []string {
	set := make(map[string]struct{})
	for _, name := range inheritedEnvNames(configured) {
		if _, ok := os.LookupEnv(name); ok {
			set[name] = struct{}{}
		}
	}
	for key := range entryEnv {
		set[key] = struct{}{}
	}
	for _, name := range brokerOwnedEnv {
		set[name] = struct{}{}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// missingInheritEnv returns the declared `inherit_env` names the broker process
// does not actually hold, sorted. Nothing fails over it — an operator may well
// run one broker.yaml across machines where only some of the names are set — but
// it is worth one WARN at boot, because the alternative discovery path is an
// instance failing to authenticate to a provider.
func missingInheritEnv(configured []string) []string {
	var missing []string
	for _, name := range configured {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := os.LookupEnv(name); !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// newSpawnSecret returns a 128-bit random hex secret for one instance spawn —
// the same width and generator as a lease id (see newLeaseID), because it is
// used for the same purpose: an unguessable bearer value.
//
// It lives beside the spawn plumbing rather than in the registry because it is a
// property of a SPAWN, not of a lease: it is minted once per exec, handed to
// exactly that child, and is meaningless to anything the broker did not start.
// The claim path mints it BEFORE calling the runner so the expected value is
// recorded on the lease no matter which commandRunner is wired — a fake runner
// in a test must not be able to bypass the check by never injecting the env.
func newSpawnSecret() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating spawn secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// spawnKeyFileName is the file inside state_dir holding this broker's
// spawn-secret derivation key. It sits beside `broker-id` and `leases.jsonl`
// because it is, like them, per-broker state that must survive a restart.
const spawnKeyFileName = "spawn-key"

// spawnKeyBytes is the derivation key's width. 32 bytes matches the HMAC-SHA256
// block output it feeds, which is the point at which a longer key buys nothing.
const spawnKeyBytes = 32

// spawnSecretDomain namespaces the HMAC input so this key can only ever produce
// spawn secrets. If a later story needs a second value derived from the same
// key it gets its own domain string, and no input can be made to collide with
// this one — the NUL terminator keeps a lease id from running into whatever
// follows it.
const spawnSecretDomain = "nexus-broker/spawn-secret/v1\x00"

// spawnKey is the broker-held master key that per-lease spawn secrets are
// DERIVED from, rather than randomly minted and forgotten.
//
// # Why derivation, and why this is not "persisting the secret"
//
// E5-S1 minted the spawn secret with crypto/rand and E5-S2 refused to write it to
// the journal, on the reasoning that a secret outliving its broker authenticates
// nothing because the process it belonged to died too. Restart recovery is
// precisely the case where that premise is false: the instance is still running,
// still holding the secret it was handed in its environment, and still dialing
// back. Something has to let the restarted broker recognise it.
//
// The two available answers were (a) derive the secret reproducibly from a
// broker-held key, or (b) declare restored leases unauthenticatable and reap them
// all — which would have made "a restarted broker reclaims its instances" mean
// "a restarted broker tidies up after itself". (a) is implemented here.
//
// What is on disk is a KEY, not a credential:
//
//   - It is not a bearer value. Presenting the contents of <state_dir>/spawn-key
//     to /instance authenticates nothing; only HMAC(key, lease id) does, and
//     lease ids are 128-bit random.
//   - It is not addressed to any lease, so it does not go stale, and reading the
//     journal (which holds no secret) tells an attacker nothing extra.
//   - The threat model is unchanged in the case that matters: anyone who can read
//     0600 files in the broker's state_dir is running as the broker's uid (or
//     root) and can already read the spawn secret straight out of the child's
//     environment, sign whatever it likes, or simply exec instances itself.
//
// The cost is real and deliberate: an attacker who can read this file AND knows a
// live lease id can impersonate that lease's instance, which was not true when
// every secret was random and memory-only. That is why the file is 0600 inside a
// 0700 directory, why a wider mode is warned about at boot, and why nothing
// derived from it is ever logged.
//
// LOSING OR ROTATING THE KEY IS SAFE, JUST LOSSY: derived secrets stop matching,
// every restored lease fails to reattach, and the reattach reaper kills its
// instance and frees its slot. The failure mode is the one this story was going
// to have anyway under answer (b) — never an unauthenticated attach.
//
// An EMPTY key means no state_dir is configured. secretFor then falls back to the
// original per-spawn random secret, so an unpersisted broker behaves exactly as it
// did before this existed.
type spawnKey []byte

// secretFor returns the spawn secret for a lease id: HMAC-SHA256 of the lease id
// under the broker's key, hex encoded to the same width as a random one so the
// wire format is indistinguishable.
//
// With no key it falls back to newSpawnSecret — a fresh random value that no
// restart can reproduce, which is the correct behaviour for a broker that keeps
// no state to recover from.
func (k spawnKey) secretFor(leaseID string) (string, error) {
	if len(k) == 0 {
		return newSpawnSecret()
	}
	mac := hmac.New(sha256.New, k)
	// Writes to an hmac.Hash never error.
	_, _ = mac.Write([]byte(spawnSecretDomain))
	_, _ = mac.Write([]byte(leaseID))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// loadSpawnKey reads (or creates) the broker's spawn-secret derivation key under
// stateDir. An empty stateDir returns a nil key, which disables derivation.
//
// Failure policy mirrors the lease store's: a state_dir that is set but unusable
// is a BOOT failure, because an operator who configured durability and silently
// did not get it only finds out from the restart that was supposed to work. The
// one exception is a key file that exists but is unusable as a key (empty,
// truncated, not hex) — that is treated as absent and rewritten, exactly as
// resolveBrokerID treats a zero-byte broker-id file, because refusing to boot
// over it would turn a recoverable degradation into an outage. The WARN says out
// loud what the operator loses.
func loadSpawnKey(logger *slog.Logger, stateDir string) (spawnKey, error) {
	if stateDir == "" {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	path := filepath.Join(stateDir, spawnKeyFileName)

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, decodeErr := hex.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr == nil && len(key) >= spawnKeyBytes {
			warnIfSpawnKeyModeTooWide(logger, path)
			return key, nil
		}
		logger.Warn("the broker spawn key is unreadable as a key and is being regenerated; "+
			"any instance this broker spawned before now can no longer prove its identity "+
			"after a restart and will be reaped instead of reattached",
			"path", path, "error", decodeErr)
	case errors.Is(err, fs.ErrNotExist):
		// First boot with this state_dir.
	default:
		return nil, fmt.Errorf("reading broker spawn key %q: %w", path, err)
	}

	key := make([]byte, spawnKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating broker spawn key: %w", err)
	}
	// 0600: unlike the journal, this file really is key material. It also lives in
	// a 0700 directory, so the mode is defence in depth rather than the only guard.
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("persisting broker spawn key %q: %w", path, err)
	}
	return key, nil
}

// warnIfSpawnKeyModeTooWide logs when the key file is readable by anyone but its
// owner. It WARNs rather than refusing to boot: the operator may have a
// deliberate reason (a shared-group deployment), and a broker that will not start
// because of a file mode is a worse outage than one that says so loudly. A stat
// failure is ignored — the file has just been read successfully, so there is
// nothing actionable to report.
func warnIfSpawnKeyModeTooWide(logger *slog.Logger, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		logger.Warn("the broker spawn key is readable beyond its owner; "+
			"anyone who can read it and knows a live lease id can impersonate that lease's instance",
			"path", path, "mode", mode.String())
	}
}

// errUnsafeSignalTarget is returned by groupSignalTarget for a pid that must
// never be turned into a group signal. It is a sentinel so a caller can log the
// refusal distinctly from an ordinary signal failure, and so a test can assert
// the refusal without matching on message text.
var errUnsafeSignalTarget = errors.New("unsafe process-group signal target")

// groupSignalTarget resolves the pid argument to pass to kill(2) in order to
// terminate an instance AND everything it started.
//
// The mechanism is a negative pid, which kill(2) reads as "every process in that
// group". That makes the arithmetic load-bearing, and its failure mode
// catastrophic rather than merely wrong:
//
//   - kill(-1, sig) signals EVERY process the caller may signal. On a broker
//     running as root that is the machine.
//   - kill(0, sig) signals the CALLER's own process group — the broker, and on a
//     typical systemd unit or shell session everything beside it.
//
// Both are reachable by accident from a pid the broker merely believes in: 0 is
// what processHandle.pid() returns before a process starts and what a truncated
// journal record decodes to, and negating 1 gets to -1. So pid <= 1 is REFUSED
// outright rather than clamped — there is no instance whose teardown legitimately
// wants that signal, and a refusal that returns an error is recoverable where a
// mis-aimed SIGKILL is not.
//
// The second guard is subtler and matters just as much. A negative pid is only
// the right target when the process actually LEADS its own group; the broker sets
// that up with Setpgid (applyProcessGroup), but a process adopted from an older
// broker that did not, or one whose setpgid the kernel refused, is still sitting
// in the BROKER's group — and negating that pid would signal a group the broker
// belongs to. So the leader check is not an optimisation: it is what stops a
// missing process group from becoming a broker suicide. A non-leader falls back
// to signalling the single process, which is exactly the behaviour that existed
// before process groups did.
func groupSignalTarget(pid int) (int, error) {
	if pid <= 1 {
		return 0, fmt.Errorf("refusing to signal pid %d: %w", pid, errUnsafeSignalTarget)
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		// The process is gone, or is not ours to ask about. Signal it directly and
		// let kill(2) report ESRCH/EPERM for itself; guessing a group here would be
		// guessing with a negative pid.
		return pid, nil
	}
	if pgid != pid {
		// Not a group leader — the group is somebody else's, quite possibly the
		// broker's own. Signal only the process.
		return pid, nil
	}
	return -pgid, nil
}

// signalProcessGroup delivers sig to the process group led by pid, falling back
// to the single process when pid does not lead one. See groupSignalTarget for why
// the target is computed rather than assumed.
//
// The underlying errno is wrapped, not swallowed, so callers can test for
// syscall.ESRCH ("already gone", which every caller here treats as success).
func signalProcessGroup(pid int, sig syscall.Signal) error {
	target, err := groupSignalTarget(pid)
	if err != nil {
		return err
	}
	if err := syscall.Kill(target, sig); err != nil {
		return fmt.Errorf("signalling %v to %d: %w", sig, target, err)
	}
	return nil
}

// signalDone reports whether err means the target had already exited. Every
// signal caller in the broker treats that as success: the point of the signal was
// to have the process gone, and it is.
func signalDone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

// execProcess adapts an *exec.Cmd to the processHandle interface.
type execProcess struct {
	cmd *exec.Cmd

	// mu guards reaped, which wait sets once cmd.Wait has returned.
	//
	// It exists because signalling goes through the raw pid (a group signal has
	// to — os.Process can only signal the one process it owns), and a raw pid is
	// exactly what the kernel is free to reuse the moment the child is reaped. The
	// flag is what keeps a late kill from landing on whatever inherited the number.
	// os.Process guards its own signals the same way.
	mu     sync.Mutex
	reaped bool
}

func (p *execProcess) pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// terminate sends SIGTERM to the instance's process group. The engine installs a
// SIGTERM handler and treats it as a clean shutdown — flushing and persisting the
// session — so this is a graceful request, not a kill.
func (p *execProcess) terminate() error {
	if err := p.signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("terminating instance process: %w", err)
	}
	return nil
}

// kill sends SIGKILL to the instance's process group. Unlike the adopted path it
// does NOT escalate from SIGTERM itself: releaseLease owns the escalation for a
// spawned instance and has already sent both the shutdown frame and SIGTERM by
// the time it calls here.
func (p *execProcess) kill() error {
	if err := p.signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("killing instance process: %w", err)
	}
	return nil
}

// signal delivers sig to the instance's process group, treating "not started"
// and "already reaped" as no-ops and "already gone" as success.
func (p *execProcess) signal(sig syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd.Process == nil || p.reaped {
		return nil
	}
	if err := signalProcessGroup(p.cmd.Process.Pid, sig); err != nil && !signalDone(err) {
		return err
	}
	return nil
}

func (p *execProcess) wait() error {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.reaped = true
	p.mu.Unlock()
	return err
}
