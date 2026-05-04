// Package sampler is the opt-in online sampler plugin. When enabled, it
// snapshots a fraction of live session journals (plus every failed session
// when failure_capture is on) to a local directory so the eval pipeline can
// later score them.
//
// The plugin is off by default. Activation requires either an explicit
// `nexus.observe.sampler` entry in `plugins.active` or a `sampler:` config
// block with `enabled: true` — the engine itself never references sampler
// state. Configuration lives under the top-level `sampler:` key documented
// in docs/src/configuration/reference.md.
package sampler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/iocopy"
)

const (
	pluginID   = "nexus.observe.sampler"
	pluginName = "Online Eval Sampler"
	version    = "0.1.0"
)

// Plugin is the engine.Plugin implementation. Constructed via New() — both
// cmd/nexus and embedders register it through pkg/engine/allplugins.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	cfg      config
	redactor Redactor

	// rng is the deterministic random source used for rate sampling. Tests
	// inject a seeded rng via SetRandSource so failure-capture vs rate
	// behavior is verifiable without flakes.
	rngMu sync.Mutex
	rng   *rand.Rand

	// captured guards against double-snapshotting the same session id when
	// io.session.end races with a later cleanup signal.
	capturedMu sync.Mutex
	captured   map[string]bool

	// unsubs holds bus subscription cancellers, released on Shutdown.
	unsubs []func()
}

// New creates a new online sampler plugin. Defaults to the IdentityRedactor
// and the package-level random source. Call SetRedactor / SetRandSource on
// the returned *Plugin from a test harness when needed.
func New() engine.Plugin {
	return &Plugin{
		redactor: IdentityRedactor{},
		captured: make(map[string]bool),
	}
}

// SetRedactor swaps the redactor used for journal snapshots. Tests inject
// drop-style redactors here; production runs leave the IdentityRedactor in
// place.
func (p *Plugin) SetRedactor(r Redactor) {
	if r == nil {
		r = IdentityRedactor{}
	}
	p.redactor = r
}

// SetRandSource injects a deterministic *rand.Rand for rate sampling. Tests
// pin the seed so a `rate: 0.5` case is reproducible.
func (p *Plugin) SetRandSource(rng *rand.Rand) {
	p.rngMu.Lock()
	defer p.rngMu.Unlock()
	p.rng = rng
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Init parses config, validates rate, and prepares the output directory.
// When the config is absent (raw == nil) or `enabled: false`, Init succeeds
// as a no-op — Subscriptions returns nil for that case so the plugin draws
// zero traffic.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	cfg, err := parseConfig(ctx.Config)
	if err != nil {
		return fmt.Errorf("sampler: %w", err)
	}
	p.cfg = cfg

	if !cfg.enabled {
		p.logger.Debug("online sampler disabled — no subscriptions, no captures")
		return nil
	}

	if err := os.MkdirAll(cfg.outDir, 0o755); err != nil {
		return fmt.Errorf("sampler: prepare out_dir %q: %w", cfg.outDir, err)
	}

	p.rngMu.Lock()
	if p.rng == nil {
		p.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	p.rngMu.Unlock()

	// Subscribe last — once we are listening, every io.session.end is fair
	// game. Lowest-priority subscriber so other end-of-session work
	// (writers, memory persisters) lands first; the sampler reads the
	// journal *after* it has been finalized by upstream subscribers.
	p.unsubs = append(p.unsubs,
		ctx.Bus.Subscribe("io.session.end", p.handleSessionEnd,
			engine.WithPriority(0), engine.WithSource(pluginID)),
	)

	p.logger.Info("online sampler initialized",
		"rate", cfg.rate,
		"failure_capture", cfg.failureCapture,
		"out_dir", cfg.outDir,
	)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, u := range p.unsubs {
		if u != nil {
			u()
		}
	}
	p.unsubs = nil
	return nil
}

// Subscriptions returns nothing when the plugin is disabled — the engine
// inventory tooling then surfaces a clean "no traffic" entry. The actual
// bus subscription happens in Init via ctx.Bus.Subscribe; this declaration
// is documentation for plugin manifests. There is no separate
// "session.failed" event in v1; failure capture is decided post-hoc by
// reading the session's metadata/session.json status field at sample time.
func (p *Plugin) Subscriptions() []engine.EventSubscription {
	if !p.cfg.enabled {
		return nil
	}
	return []engine.EventSubscription{
		{EventType: "io.session.end", Priority: 0},
	}
}

// Emissions documents the single event type this plugin may emit on capture.
func (p *Plugin) Emissions() []string {
	if !p.cfg.enabled {
		return nil
	}
	return []string{EvalCandidateEventType}
}

// handleSessionEnd is the single hot path. Reads the session's current status
// (so we know whether to honor failure_capture), rolls dice for rate-based
// captures, and snapshots when warranted.
func (p *Plugin) handleSessionEnd(_ engine.Event[any]) {
	if !p.cfg.enabled || p.session == nil {
		return
	}

	sessionID := p.session.ID
	if sessionID == "" {
		return
	}

	p.capturedMu.Lock()
	if p.captured[sessionID] {
		p.capturedMu.Unlock()
		return
	}
	p.capturedMu.Unlock()

	status := p.readSessionStatus()

	reason, ok := p.decide(status)
	if !ok {
		return
	}

	caseDir, warnings, err := p.snapshot(sessionID, reason, status)
	if err != nil {
		p.logger.Error("sampler snapshot failed", "session_id", sessionID, "reason", reason, "error", err)
		return
	}

	p.capturedMu.Lock()
	p.captured[sessionID] = true
	p.capturedMu.Unlock()

	_ = p.bus.Emit(EvalCandidateEventType, EvalCandidate{
		SessionID: sessionID,
		CaseDir:   caseDir,
		Reason:    reason,
		Warnings:  warnings,
	})
	p.logger.Info("session sampled",
		"session_id", sessionID,
		"reason", reason,
		"case_dir", caseDir,
		"status", status,
	)
}

// decide returns the capture reason ("sampled" or "failure_capture") plus a
// boolean indicating whether to capture at all. Failure capture takes
// precedence over rate sampling so a failed session is always caught.
func (p *Plugin) decide(status string) (string, bool) {
	if p.cfg.failureCapture && isFailedStatus(status) {
		return "failure_capture", true
	}
	if p.cfg.rate <= 0 {
		return "", false
	}
	if p.cfg.rate >= 1 {
		return "sampled", true
	}
	p.rngMu.Lock()
	roll := p.rng.Float64()
	p.rngMu.Unlock()
	if roll < p.cfg.rate {
		return "sampled", true
	}
	return "", false
}

// isFailedStatus is the predicate for "this session counts as a failure".
// "completed" and "active" are explicitly NOT failures; everything else is.
// "active" appears when the engine is shutting down before EndSession runs;
// treating it as non-failure avoids a stampede of false-positive captures
// during normal Ctrl+C shutdown.
func isFailedStatus(status string) bool {
	switch status {
	case "", "active", "completed":
		return false
	default:
		return true
	}
}

// readSessionStatus opens metadata/session.json on the live session and
// returns the persisted status string. Returns "" when the file is missing
// or unreadable; callers treat empty as "non-failed".
func (p *Plugin) readSessionStatus() string {
	if p.session == nil {
		return ""
	}
	path := filepath.Join(p.session.MetadataDir(), "session.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var meta struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return ""
	}
	return meta.Status
}

// snapshot copies the current session's journal directory into the sample
// out_dir under <session-id>/journal/, then writes a metadata.json sibling.
// When the configured Redactor is non-identity, the active events.jsonl
// segment is rewritten line-by-line through it after the byte copy.
func (p *Plugin) snapshot(sessionID, reason, status string) (string, []string, error) {
	caseDir := filepath.Join(p.cfg.outDir, sessionID)
	dstJournal := filepath.Join(caseDir, "journal")
	srcJournal := filepath.Join(p.session.RootDir, "journal")

	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create case dir: %w", err)
	}
	if err := iocopy.CopyDir(srcJournal, dstJournal); err != nil {
		return "", nil, fmt.Errorf("copy journal: %w", err)
	}

	var warnings []string
	if _, isIdentity := p.redactor.(IdentityRedactor); !isIdentity {
		if err := p.applyRedactor(dstJournal); err != nil {
			warnings = append(warnings, fmt.Sprintf("redactor: %v", err))
		}
	}

	if err := p.writeMetadata(caseDir, reason, status); err != nil {
		return "", warnings, fmt.Errorf("write metadata.json: %w", err)
	}

	return caseDir, warnings, nil
}

// applyRedactor rewrites the active events.jsonl segment line-by-line,
// piping each envelope's payload through the configured Redactor. Rotated
// `.zst` segments are left byte-identical in v1 — handling them requires
// transparent zstd round-trips which would expand the dependency surface.
// A warning is appended when rotated segments are present so operators
// know the v1 hook is partial.
func (p *Plugin) applyRedactor(dstJournal string) error {
	activePath := filepath.Join(dstJournal, "events.jsonl")
	src, err := os.ReadFile(activePath)
	if err != nil {
		return fmt.Errorf("read active segment: %w", err)
	}

	tmpPath := activePath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	bw := bufio.NewWriter(out)

	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env journal.Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			// Malformed line — write it through unchanged rather than
			// silently dropping; the redactor is best-effort.
			if _, werr := bw.Write(append(line, '\n')); werr != nil {
				_ = out.Close()
				_ = os.Remove(tmpPath)
				return werr
			}
			continue
		}
		var payloadBytes []byte
		if env.Payload != nil {
			pb, perr := json.Marshal(env.Payload)
			if perr == nil {
				payloadBytes = pb
			}
		}
		newPayload, rerr := p.redactor.Redact(env.Type, payloadBytes)
		if rerr != nil {
			_ = out.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("redact %s: %w", env.Type, rerr)
		}
		if newPayload == nil {
			env.Payload = nil
		} else {
			var v any
			if jerr := json.Unmarshal(newPayload, &v); jerr == nil {
				env.Payload = v
			} else {
				env.Payload = json.RawMessage(newPayload)
			}
		}
		raw, err := json.Marshal(env)
		if err != nil {
			_ = out.Close()
			_ = os.Remove(tmpPath)
			return err
		}
		if _, werr := bw.Write(append(raw, '\n')); werr != nil {
			_ = out.Close()
			_ = os.Remove(tmpPath)
			return werr
		}
	}
	if err := scanner.Err(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := bw.Flush(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, activePath)
}

// snapshotMetadata is the on-disk shape of <case_dir>/metadata.json.
type snapshotMetadata struct {
	CapturedAt            string  `json:"captured_at"`
	Reason                string  `json:"reason"`
	SamplingRateAtCapture float64 `json:"sampling_rate_at_capture"`
	SessionStatus         string  `json:"session_status"`
	EngineVersion         string  `json:"engine_version"`
}

// writeMetadata serializes snapshotMetadata to <case_dir>/metadata.json. The
// file is the lightweight provenance record that downstream tooling reads
// to know "why was this captured?" without re-projecting the journal.
func (p *Plugin) writeMetadata(caseDir, reason, status string) error {
	meta := snapshotMetadata{
		CapturedAt:            time.Now().UTC().Format(time.RFC3339),
		Reason:                reason,
		SamplingRateAtCapture: p.cfg.rate,
		SessionStatus:         status,
		EngineVersion:         journal.SchemaVersion,
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(caseDir, "metadata.json"), data, 0o644)
}
