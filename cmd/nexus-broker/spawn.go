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
	// kill forcibly terminates the process.
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
	env := os.Environ()
	// Entry env sits between the broker process's own environment and the
	// broker-owned variables: a variant may override something inherited from
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

	// Surface the child's logs through the broker's stderr for observability.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd
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

// execProcess adapts an *exec.Cmd to the processHandle interface.
type execProcess struct {
	cmd *exec.Cmd
}

func (p *execProcess) pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *execProcess) kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("killing instance process: %w", err)
	}
	return nil
}

func (p *execProcess) wait() error {
	return p.cmd.Wait()
}
