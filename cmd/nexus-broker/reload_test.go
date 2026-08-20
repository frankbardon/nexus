package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// setClaimBounds rewrites the claim server's OWN configuration snapshot.
//
// It exists because the four claim-path bounds are no longer fields: they are
// read from the configuration snapshot at use time, which is what makes them
// reloadable. A test that wants a 50ms ready timeout therefore has to say so the
// way a config does. It replaces the private snapshot the constructor built, so
// it must not be called on a server already wired to the process-wide holder —
// no test does, and run() is the only caller that shares one.
func setClaimBounds(cs *ClaimServer, mutate func(*Config)) {
	cfg := *cs.config()
	mutate(&cfg)
	cs.live.store(newLocalLiveConfig(cfg))
}

// reloadFixture is a broker assembled the way run() assembles one — a single
// configuration snapshot shared by the claim handler, the binaries listing, the
// A2A ingress and the registry — over a real config file on disk that a test can
// rewrite and reload.
//
// It drives reload() directly rather than raising a real SIGHUP. The signal loop
// is three lines over this same call (watchReloadSignals), and delivering a
// process-wide signal from a test would race every other test in the package.
type reloadFixture struct {
	t        *testing.T
	dir      string
	path     string
	logs     *syncBuffer
	live     *configHolder
	registry *Registry
	claims   *ClaimServer
	runner   *fakeRunner
	agents   *A2AServer
	rl       *reloader
	ts       *httptest.Server
}

func newReloadFixture(t *testing.T, yaml string) *reloadFixture {
	t.Helper()
	dir := t.TempDir()
	f := &reloadFixture{
		t:    t,
		dir:  dir,
		path: filepath.Join(dir, "broker.yaml"),
		logs: &syncBuffer{},
	}
	f.write(yaml)

	cfg, err := LoadConfig(f.path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	f.live, err = newConfigHolder(cfg)
	if err != nil {
		t.Fatalf("newConfigHolder: %v", err)
	}
	f.registry = NewRegistry(logger, cfg.MaxConcurrent)
	f.runner = &fakeRunner{started: make(chan spawnSpec, 8), handle: newFakeProcess(4242)}

	f.claims = NewClaimServer(logger, f.registry, cfg, f.runner, nil)
	f.claims.useConfigHolder(f.live)
	binaries := NewBinariesServer(logger, cfg.Binaries)
	binaries.useConfigHolder(f.live)
	f.agents = newA2AServer(logger, f.live)

	mux := http.NewServeMux()
	f.claims.Register(mux)
	binaries.Register(mux)
	f.agents.Register(mux)
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)

	f.rl = newReloader(logger, f.path, f.live, f.registry)
	return f
}

// write replaces the config file on disk without reloading it.
func (f *reloadFixture) write(yaml string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(yaml), 0o600); err != nil {
		f.t.Fatalf("write broker config: %v", err)
	}
}

// rewrite replaces the config file and reloads it, failing the test if the
// reload was refused.
func (f *reloadFixture) rewrite(yaml string) {
	f.t.Helper()
	f.write(yaml)
	if err := f.rl.reload(); err != nil {
		f.t.Fatalf("reload: %v", err)
	}
}

// binariesListing fetches GET /binaries and returns the entry names.
func (f *reloadFixture) binariesListing() []string {
	f.t.Helper()
	resp, err := http.Get(f.ts.URL + "/binaries")
	if err != nil {
		f.t.Fatalf("GET /binaries: %v", err)
	}
	defer resp.Body.Close()
	var body binariesBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		f.t.Fatalf("decode /binaries: %v", err)
	}
	names := make([]string, 0, len(body.Binaries))
	for _, b := range body.Binaries {
		names = append(names, b.Name)
	}
	return names
}

// fetchCard fetches a profile's Agent Card over HTTP and returns the decoded
// document. Reading it off the wire rather than out of the server's map is the
// point: the claim is that a REQUEST sees the reloaded card.
func (f *reloadFixture) fetchCard(profile string) map[string]any {
	f.t.Helper()
	resp, err := http.Get(f.ts.URL + agentCardPath(profile))
	if err != nil {
		f.t.Fatalf("GET agent card: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("GET agent card status = %d, want 200", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		f.t.Fatalf("decode agent card: %v", err)
	}
	return doc
}

// claimBinary posts a claim naming an entry and returns the response status. A
// claim that gets as far as spawning also parks a spec on runner.started.
func (f *reloadFixture) claimBinary(binary string) int {
	f.t.Helper()
	resp := postClaim(f.t, f.ts.URL, `{"config":"engine: {}\n","binary":"`+binary+`"}`)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// reloadYAML renders a broker config for the fixture. Every argument is a whole
// YAML fragment so a test can state exactly the document it means.
func reloadYAML(fragments ...string) string {
	return strings.Join(fragments, "\n") + "\n"
}

// writeNexusFixture writes an executable stand-in for a nexus build, which is
// what LoadConfig's registry resolution insists on: a path that exists and is
// executable, or the boot fails.
func writeNexusFixture(t *testing.T, dir, name string) string {
	t.Helper()
	return writeBinaryFixture(t, dir, name, 0o755)
}

// TestReload_ValidReloadChangesWhatAClaimSpawns is the story's headline: an
// operator adds a variant and the very next claim can spawn it, with no restart
// and therefore without costing a single lease.
func TestReload_ValidReloadChangesWhatAClaimSpawns(t *testing.T) {
	dir := t.TempDir()
	base := writeNexusFixture(t, dir, "nexus")
	variant := writeNexusFixture(t, dir, "nexus-vision")

	f := newReloadFixture(t, reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`ready_timeout: 50ms`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
	))

	// Before: the variant does not exist, so the claim is refused outright and
	// nothing is spawned.
	if got := f.claimBinary("vision"); got != http.StatusBadRequest {
		t.Fatalf("claim for an unconfigured variant = %d, want 400", got)
	}
	select {
	case spec := <-f.runner.started:
		t.Fatalf("a refused claim spawned %s", spec.binaryPath)
	default:
	}

	f.rewrite(reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`ready_timeout: 50ms`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		`  vision:`,
		`    path: "`+variant+`"`,
		`    label: "Nexus (vision)"`,
	))

	// After: the same claim spawns, and it spawns the binary the operator just
	// added rather than the base one.
	done := make(chan int, 1)
	go func() { done <- f.claimBinary("vision") }()

	select {
	case spec := <-f.runner.started:
		if spec.binaryName != "vision" {
			t.Errorf("spawned entry = %q, want vision", spec.binaryName)
		}
		if spec.binaryPath != variant {
			t.Errorf("spawned path = %q, want %q", spec.binaryPath, variant)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the claim never spawned after the reload added the entry")
	}
	<-done
}

// TestReload_InvalidReloadChangesNothing is the atomicity guarantee: a config
// that fails to load leaves the previous one ENTIRELY in force. Not the parts
// that happened to parse — all of it.
func TestReload_InvalidReloadChangesNothing(t *testing.T) {
	dir := t.TempDir()
	base := writeNexusFixture(t, dir, "nexus")
	variant := writeNexusFixture(t, dir, "nexus-vision")

	f := newReloadFixture(t, reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`max_concurrent: 4`,
		`idle_timeout: 5m`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		`  vision:`,
		`    path: "`+variant+`"`,
	))

	// A document that changes several reloadable keys AND names a binary that
	// does not exist. Everything before the bad entry is perfectly valid, which is
	// exactly the shape a half-applying reload would leak.
	f.write(reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`max_concurrent: 99`,
		`idle_timeout: 1s`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		`  ghost:`,
		`    path: "`+filepath.Join(dir, "does-not-exist")+`"`,
	))
	if err := f.rl.reload(); err == nil {
		t.Fatal("a config naming a missing binary was accepted")
	}

	cfg := f.live.config()
	if cfg.MaxConcurrent != 4 {
		t.Errorf("max_concurrent = %d after a refused reload, want 4", cfg.MaxConcurrent)
	}
	if cfg.IdleTimeout != 5*time.Minute {
		t.Errorf("idle_timeout = %v after a refused reload, want 5m", cfg.IdleTimeout)
	}
	if _, ok := cfg.Binaries["vision"]; !ok {
		t.Error("the vision entry disappeared on a refused reload")
	}
	if _, ok := cfg.Binaries["ghost"]; ok {
		t.Error("an entry from a refused config reached the live registry")
	}
	f.registry.mu.Lock()
	live := f.registry.maxConcurrent
	f.registry.mu.Unlock()
	if live != 4 {
		t.Errorf("registry max_concurrent = %d after a refused reload, want 4", live)
	}
	if got := f.binariesListing(); !contains(got, "vision") || contains(got, "ghost") {
		t.Errorf("GET /binaries = %v after a refused reload, want the pre-reload registry", got)
	}
	if !strings.Contains(f.logs.String(), "config reload rejected") {
		t.Error("a refused reload did not log why")
	}
}

// TestReload_PartialValidityIsStillARefusal is the atomicity guarantee across
// two independent sections of one document: the `binaries:` block is perfectly
// valid and would apply on its own, but the `agents:` block below it names an
// entry the same edit removed. Nothing applies.
func TestReload_PartialValidityIsStillARefusal(t *testing.T) {
	dir := t.TempDir()
	base := writeNexusFixture(t, dir, "nexus")
	variant := writeNexusFixture(t, dir, "nexus-vision")
	agentCfg := writeAgentConfig(t)

	profile := func(binary string) string {
		return reloadYAML(
			`agents:`,
			`  support:`,
			`    binary: `+binary,
			`    config: "`+agentCfg+`"`,
			`    card:`,
			`      name: "Support Agent"`,
			`      description: "Answers customer questions."`,
			`      version: "1.2.0"`,
			`      skills:`,
			`        - id: "answer"`,
			`          name: "Answer questions"`,
			`          description: "Answers a customer question."`,
		)
	}

	f := newReloadFixture(t, reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`max_concurrent: 4`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		`  vision:`,
		`    path: "`+variant+`"`,
	)+profile("vision"))

	// The registry edit alone is valid. The profile beneath it is not, because it
	// still names the entry this edit removes.
	f.write(reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`max_concurrent: 9`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
	) + profile("vision"))

	if err := f.rl.reload(); err == nil {
		t.Fatal("a config whose profile names a removed binary was accepted")
	}

	cfg := f.live.config()
	if _, ok := cfg.Binaries["vision"]; !ok {
		t.Error("the binary edit was applied even though the reload was refused")
	}
	if cfg.MaxConcurrent != 4 {
		t.Errorf("max_concurrent = %d after a refused reload, want 4", cfg.MaxConcurrent)
	}
	if got := f.fetchCard("support")["name"]; got != "Support Agent" {
		t.Errorf("card name = %v after a refused reload, want the pre-reload card", got)
	}
}

// TestReload_RemovedEntryLeavesLiveLeasesAlone pins the rule that makes this
// safe to run against a busy broker: a lease is a RUNNING PROCESS, and removing
// the registry entry it was spawned from is a statement about future claims, not
// a reason to tear down work already in flight.
func TestReload_RemovedEntryLeavesLiveLeasesAlone(t *testing.T) {
	dir := t.TempDir()
	base := writeNexusFixture(t, dir, "nexus")
	variant := writeNexusFixture(t, dir, "nexus-vision")

	f := newReloadFixture(t, reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		`  vision:`,
		`    path: "`+variant+`"`,
	))

	proc := newFakeProcess(777)
	leaseID, _, _ := seedLiveLease(t, f.registry, proc)

	f.rewrite(reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
	))

	if !f.registry.Has(leaseID) {
		t.Fatal("a live lease was dropped by a reload that removed its binary entry")
	}
	select {
	case <-proc.killed:
		t.Fatal("a reload killed a running instance")
	default:
	}

	// The removal did take effect for everything that looks FORWARD.
	if got := f.binariesListing(); contains(got, "vision") {
		t.Errorf("GET /binaries = %v, want the removed entry gone", got)
	}
	if got := f.claimBinary("vision"); got != http.StatusBadRequest {
		t.Errorf("claim for a removed entry = %d, want 400", got)
	}
}

// TestReload_BootOnlyKeysAreReportedAndIgnored covers the keys a reload must
// refuse to touch. auth: is the one that matters most: rebuilding the validator
// chain would discard the JWKS kid cache, so a reload during an IdP outage would
// turn a working broker into one that denies every JWT.
func TestReload_BootOnlyKeysAreReportedAndIgnored(t *testing.T) {
	dir := t.TempDir()
	base := writeNexusFixture(t, dir, "nexus")
	stateA := filepath.Join(dir, "state-a")
	stateB := filepath.Join(dir, "state-b")

	f := newReloadFixture(t, reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`advertise_addr: "ws://broker.example:8080"`,
		`state_dir: "`+stateA+`"`,
		`broker_id: "broker-a"`,
		`reattach_window: 1m`,
		`max_concurrent: 4`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		staticAuthYAML,
	))
	chainBefore := f.live.config().AuthChain

	f.rewrite(reloadYAML(
		`listen_addr: "127.0.0.1:9999"`,
		`advertise_addr: "wss://elsewhere.example"`,
		`state_dir: "`+stateB+`"`,
		`broker_id: "broker-b"`,
		`reattach_window: 9m`,
		`max_concurrent: 6`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		`auth:`,
		`  admin_scope: "somebody.else"`,
		`  validators:`,
		`    - type: static`,
		`      tokens:`,
		`        - token: "rotated-token"`,
		`          principal: "somebody-else"`,
	))

	cfg := f.live.config()
	for _, tc := range []struct {
		key       string
		got, want any
	}{
		{"listen_addr", cfg.ListenAddr, "127.0.0.1:8080"},
		{"advertise_addr", cfg.AdvertiseAddr, "ws://broker.example:8080"},
		{"state_dir", cfg.StateDir, stateA},
		{"broker_id", cfg.BrokerID, "broker-a"},
		{"reattach_window", cfg.ReattachWindow, time.Minute},
		{"auth.admin_scope", cfg.AdminScope, defaultAdminScope},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v after reload, want %v (boot-only)", tc.key, tc.got, tc.want)
		}
	}
	// The validator chain is not merely equal, it is the SAME object: nothing
	// rebuilt it, so whatever live cache it holds survived the reload.
	if f.live.config().AuthChain != chainBefore {
		t.Error("the auth chain was rebuilt by a reload; the JWKS kid cache would have been lost")
	}

	// The reloadable key in the same document still applied — a boot-only change
	// is ignored, not a reason to refuse everything around it.
	if cfg.MaxConcurrent != 6 {
		t.Errorf("max_concurrent = %d, want 6 (reloadable)", cfg.MaxConcurrent)
	}

	logs := f.logs.String()
	if !strings.Contains(logs, "only read at boot") {
		t.Fatalf("no boot-only report in the log:\n%s", logs)
	}
	for _, key := range []string{"listen_addr", "advertise_addr", "state_dir", "broker_id",
		"reattach_window", "auth", "auth.admin_scope"} {
		if !strings.Contains(logs, key) {
			t.Errorf("the boot-only report does not name %s", key)
		}
	}
}

// TestReload_BinariesListingAndAgentCardFollowTheReload proves the two
// projections that used to be frozen at construction now track the config: the
// GET /binaries envelope and every profile's rendered Agent Card.
func TestReload_BinariesListingAndAgentCardFollowTheReload(t *testing.T) {
	dir := t.TempDir()
	base := writeNexusFixture(t, dir, "nexus")
	variant := writeNexusFixture(t, dir, "nexus-vision")
	agentCfg := writeAgentConfig(t)

	profile := func(description string) string {
		return reloadYAML(
			`agents:`,
			`  support:`,
			`    config: "`+agentCfg+`"`,
			`    card:`,
			`      name: "Support Agent"`,
			`      description: "`+description+`"`,
			`      version: "1.2.0"`,
			`      skills:`,
			`        - id: "answer"`,
			`          name: "Answer questions"`,
			`          description: "Answers a customer question."`,
		)
	}

	f := newReloadFixture(t, reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		profile("Answers customer questions."),
	))

	if got := f.binariesListing(); contains(got, "vision") {
		t.Fatalf("GET /binaries = %v before the reload, want no vision entry", got)
	}
	before := f.fetchCard("support")
	if before["description"] != "Answers customer questions." {
		t.Fatalf("card description = %v before reload", before["description"])
	}

	f.rewrite(reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		`  vision:`,
		`    path: "`+variant+`"`,
		profile("Answers customer questions, now with pictures."),
	))

	if got := f.binariesListing(); !contains(got, "vision") {
		t.Errorf("GET /binaries = %v after the reload, want the new vision entry", got)
	}
	after := f.fetchCard("support")
	if after["description"] != "Answers customer questions, now with pictures." {
		t.Errorf("card description = %v after reload, want the reloaded text", after["description"])
	}
	// The ETag is a content hash, so a changed card must produce a new validator
	// or a cached client stays pinned to the old document.
	if f.agents.card("support").etag == "" {
		t.Error("the reloaded card has no ETag")
	}
}

// TestReload_AddedAndRemovedProfilesSwapAsAUnit pins the property a
// profile-by-profile swap would break: at no point may a request see one
// profile's identity under another's name. The reload replaces support AND adds
// research in one store, so a card fetched at any moment is internally
// consistent.
func TestReload_AddedAndRemovedProfilesSwapAsAUnit(t *testing.T) {
	dir := t.TempDir()
	base := writeNexusFixture(t, dir, "nexus")
	agentCfg := writeAgentConfig(t)

	card := func(profile, name string) string {
		return reloadYAML(
			`  `+profile+`:`,
			`    config: "`+agentCfg+`"`,
			`    card:`,
			`      name: "`+name+`"`,
			`      description: "An agent called `+name+`."`,
			`      version: "1.0.0"`,
			`      skills:`,
			`        - id: "answer"`,
			`          name: "Answer"`,
			`          description: "Answers a question."`,
		)
	}
	head := reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		`agents:`,
	)

	f := newReloadFixture(t, head+card("support", "Support Agent"))
	if got := f.fetchCard("support")["name"]; got != "Support Agent" {
		t.Fatalf("support card name = %v", got)
	}

	f.rewrite(head + card("research", "Research Agent"))

	// support is gone: its route still exists (the mux is fixed at boot) and
	// answers the honest refusal rather than somebody else's card.
	resp, err := http.Get(f.ts.URL + agentCardPath("support"))
	if err != nil {
		t.Fatalf("GET removed card: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("removed profile card status = %d, want 404", resp.StatusCode)
	}
	if got := f.fetchCard("research")["name"]; got != "Research Agent" {
		t.Errorf("research card name = %v, want Research Agent", got)
	}
	if card := f.agents.card("research"); card == nil || card.spec.ResolvedConfig == "" {
		t.Error("the reloaded card does not carry its resolved profile")
	}
}

// TestReload_CannotEnableTheA2AIngress pins the one profile change a reload
// refuses. A broker that booted with no `agents:` block registered no A2A routes
// and opened neither the context index nor the durable task store, so publishing
// cards would produce an ingress that is silently degraded rather than one that
// works.
func TestReload_CannotEnableTheA2AIngress(t *testing.T) {
	dir := t.TempDir()
	base := writeNexusFixture(t, dir, "nexus")
	agentCfg := writeAgentConfig(t)

	f := newReloadFixture(t, reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
	))
	if f.agents.enabled() {
		t.Fatal("the fixture booted with an enabled ingress")
	}

	f.rewrite(reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`max_concurrent: 3`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
		`agents:`,
		`  support:`,
		`    config: "`+agentCfg+`"`,
		`    card:`,
		`      name: "Support Agent"`,
		`      description: "Answers customer questions."`,
		`      version: "1.2.0"`,
		`      skills:`,
		`        - id: "answer"`,
		`          name: "Answer questions"`,
		`          description: "Answers a customer question."`,
	))

	if len(f.live.config().Agents) != 0 {
		t.Error("a reload enabled the a2a ingress on a broker that booted without one")
	}
	if f.agents.enabled() {
		t.Error("the ingress reports enabled after a reload that could not register its routes")
	}
	if !strings.Contains(f.logs.String(), "enabling the a2a ingress") {
		t.Error("the refusal to enable the ingress was not reported")
	}
	// Everything else in the same document still applied.
	if f.live.config().MaxConcurrent != 3 {
		t.Errorf("max_concurrent = %d, want 3", f.live.config().MaxConcurrent)
	}
}

// TestReload_RaisingMaxConcurrentAdmitsQueuedClaims pins the one reloadable
// value that does not live in the snapshot. Without the handoff, raising the cap
// would change a number nothing acted on until the next release — which looks
// exactly like the reload not having worked.
func TestReload_RaisingMaxConcurrentAdmitsQueuedClaims(t *testing.T) {
	reg := NewRegistry(testLogger(), 1)
	if _, err := reg.NewLease(anonymousOwner()); err != nil {
		t.Fatalf("NewLease: %v", err)
	}

	queued := make(chan error, 1)
	go func() {
		_, err := reg.NewLeaseQueued(context.Background(), 5*time.Second, anonymousOwner())
		queued <- err
	}()

	waitFor(t, func() bool {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		return reg.waiters.Len() == 1
	})

	reg.setMaxConcurrent(2)

	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("the queued claim was not admitted after the cap was raised: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("raising max_concurrent did not admit the parked claim")
	}
}

// TestReload_LoweringMaxConcurrentNeverEvicts is the other half: a configuration
// edit must not destroy live work. The broker goes over its cap and drains.
func TestReload_LoweringMaxConcurrentNeverEvicts(t *testing.T) {
	reg := NewRegistry(testLogger(), 4)
	var ids []string
	for range 3 {
		id, err := reg.NewLease(anonymousOwner())
		if err != nil {
			t.Fatalf("NewLease: %v", err)
		}
		ids = append(ids, id)
	}

	reg.setMaxConcurrent(1)

	for _, id := range ids {
		if !reg.Has(id) {
			t.Fatalf("lease %s was evicted by lowering max_concurrent", id)
		}
	}
	// And the broker admits nothing new until it drains back under.
	if _, err := reg.NewLease(anonymousOwner()); err == nil {
		t.Error("an over-cap broker admitted a fresh lease")
	}
}

// TestReload_ConcurrentWithServing is the -race gate. A reload IS concurrent
// with serving by definition, and a reload racing a claim is the whole risk this
// design exists to remove.
func TestReload_ConcurrentWithServing(t *testing.T) {
	dir := t.TempDir()
	base := writeNexusFixture(t, dir, "nexus")
	variant := writeNexusFixture(t, dir, "nexus-vision")
	agentCfg := writeAgentConfig(t)

	doc := func(description string, withVariant bool) string {
		out := reloadYAML(
			`listen_addr: "127.0.0.1:8080"`,
			`ready_timeout: 20ms`,
			`binaries:`,
			`  nexus:`,
			`    path: "`+base+`"`,
		)
		if withVariant {
			out += reloadYAML(
				`  vision:`,
				`    path: "`+variant+`"`,
			)
		}
		return out + reloadYAML(
			`agents:`,
			`  support:`,
			`    config: "`+agentCfg+`"`,
			`    card:`,
			`      name: "Support Agent"`,
			`      description: "`+description+`"`,
			`      version: "1.2.0"`,
			`      skills:`,
			`        - id: "answer"`,
			`          name: "Answer questions"`,
			`          description: "Answers a customer question."`,
		)
	}

	f := newReloadFixture(t, doc("first", false))

	// Drain spawn specs so a claim that gets through never blocks on the runner's
	// channel.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range f.runner.started {
		}
	}()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	reader := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					fn()
				}
			}
		}()
	}
	// Readers of all three surfaces the snapshot feeds.
	reader(func() { f.binariesListing() })
	reader(func() { f.fetchCard("support") })
	reader(func() { _ = f.live.config().Binaries })
	reader(func() { f.agents.card("support") })

	for i := range 40 {
		f.write(doc("round", i%2 == 0))
		if err := f.rl.reload(); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	close(f.runner.started)
	<-drained
}

// TestReload_SIGHUPTriggersReload closes the loop between the signal and the
// swap. Every other test in this file drives reload() directly, which proves the
// mechanism but not the trigger — and the trigger is the entire operator-facing
// contract of this story.
//
// The test installs its OWN SIGHUP handler before signalling. That is not
// belt-and-braces: SIGHUP's default disposition is to terminate the process, so
// a signal that arrived before watchReloadSignals had called signal.Notify would
// kill the test binary rather than fail a test. Registering first makes the
// default unreachable for the whole test, and the retry loop then covers the
// remaining race — the reload goroutine registering after the first signal.
func TestReload_SIGHUPTriggersReload(t *testing.T) {
	dir := t.TempDir()
	base := writeNexusFixture(t, dir, "nexus")

	f := newReloadFixture(t, reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`max_concurrent: 4`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
	))

	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGHUP)
	defer signal.Stop(guard)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchReloadSignals(ctx, f.rl)
	}()

	f.write(reloadYAML(
		`listen_addr: "127.0.0.1:8080"`,
		`max_concurrent: 6`,
		`binaries:`,
		`  nexus:`,
		`    path: "`+base+`"`,
	))

	waitFor(t, func() bool {
		_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
		return f.live.config().MaxConcurrent == 6
	})

	// Stop signalling before the handler goes away, so no SIGHUP can outlive it
	// and hit the default disposition.
	cancel()
	<-done
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
