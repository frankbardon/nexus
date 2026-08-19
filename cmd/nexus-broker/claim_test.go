package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// fakeProcess is a controllable processHandle that never boots a real engine.
type fakeProcess struct {
	pidVal   int
	killOnce sync.Once
	killed   chan struct{} // closed by kill()
	exited   chan struct{} // close to make an unkilled process exit on its own
}

func newFakeProcess(pid int) *fakeProcess {
	return &fakeProcess{
		pidVal: pid,
		killed: make(chan struct{}),
		exited: make(chan struct{}),
	}
}

func (p *fakeProcess) pid() int { return p.pidVal }

func (p *fakeProcess) kill() error {
	p.killOnce.Do(func() { close(p.killed) })
	return nil
}

func (p *fakeProcess) wait() error {
	select {
	case <-p.killed:
		return errors.New("signal: killed")
	case <-p.exited:
		return nil
	}
}

// fakeRunner records the spawn spec and hands back a preset handle or error.
type fakeRunner struct {
	started chan spawnSpec
	handle  processHandle
	err     error
}

func (f *fakeRunner) start(_ context.Context, spec spawnSpec) (processHandle, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.started <- spec
	return f.handle, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testBinaryRegistry builds the single-entry, already-RESOLVED spawn registry a
// claim test needs.
//
// ResolvedPath is set explicitly because the resolution step lives in
// LoadConfig, not in LoadConfigFromBytes — a Config built from bytes (or from a
// struct literal) legitimately carries an empty ResolvedPath, and
// TestLoadConfigFromBytesLeavesRegistryUnresolved pins that. Tests that want a
// spawn target therefore state one rather than expecting the parser to have
// stat()ed a path that need not exist.
func testBinaryRegistry(path string) map[string]BinaryEntry {
	return map[string]BinaryEntry{
		reservedBinaryName: {Path: path, ResolvedPath: path},
	}
}

func newClaimTestServer(t *testing.T, runner commandRunner, cfg Config) (*httptest.Server, *Registry, *ClaimServer) {
	t.Helper()
	reg := NewRegistry(testLogger(), 0)
	// No ticket store: this helper serves the pre-auth topology, where a claim
	// returns no ticket at all.
	cs := NewClaimServer(testLogger(), reg, cfg, runner, nil)
	mux := http.NewServeMux()
	cs.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg, cs
}

func postClaim(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/claim", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /claim: %v", err)
	}
	return resp
}

func TestBuildCommand_ArgsAndEnv(t *testing.T) {
	spec := spawnSpec{
		binaryPath: "/opt/nexus/bin/nexus",
		configPath: "/tmp/claim-123.yaml",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
	}
	cmd := buildCommand(spec)

	wantArgs := []string{"/opt/nexus/bin/nexus", "-config", "/tmp/claim-123.yaml"}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("cmd.Args = %v, want %v", cmd.Args, wantArgs)
	}
	if !envHas(cmd.Env, brokerframe.EnvBrokerAddr+"=ws://127.0.0.1:8080/instance") {
		t.Errorf("env missing %s; got %v", brokerframe.EnvBrokerAddr, cmd.Env)
	}
	if !envHas(cmd.Env, brokerframe.EnvLeaseID+"=lease-abc") {
		t.Errorf("env missing %s; got %v", brokerframe.EnvLeaseID, cmd.Env)
	}
}

// TestBuildCommand_InjectsSpawnSecret pins how the second factor reaches the
// child: through the ENVIRONMENT, and never through argv.
//
// The argv assertion is the load-bearing half. argv is world-readable via `ps`
// and /proc on the machines this runs on, so a secret that leaked into the
// command line would be readable by every local user — which would defeat the
// point of having a second factor at all.
func TestBuildCommand_InjectsSpawnSecret(t *testing.T) {
	const secret = "7f3b91c2ad4e5806b1c2d3e4f5061728"
	cmd := buildCommand(spawnSpec{
		binaryPath:  "/opt/nexus/bin/nexus",
		configPath:  "/tmp/claim-123.yaml",
		leaseID:     "lease-abc",
		brokerAddr:  "ws://127.0.0.1:8080/instance",
		spawnSecret: secret,
	})

	if !envHas(cmd.Env, brokerframe.EnvSpawnSecret+"="+secret) {
		t.Errorf("env missing %s=<secret>; got %v", brokerframe.EnvSpawnSecret, cmd.Env)
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, secret) {
			t.Fatalf("the spawn secret appears in argv (%q): argv is world-readable", arg)
		}
	}
}

// TestNewSpawnSecret_UnguessableAndUnique checks the two properties the value's
// entire security rests on: it is 128 bits of crypto/rand hex (the same width as
// a lease id), and consecutive calls differ.
//
// Uniqueness cannot prove randomness, but a counter, a constant or a per-process
// value would all fail it — and those are the realistic ways this gets broken
// later.
func TestNewSpawnSecret_UnguessableAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		s, err := newSpawnSecret()
		if err != nil {
			t.Fatalf("newSpawnSecret: %v", err)
		}
		if len(s) != 32 {
			t.Fatalf("secret %q has length %d, want 32 hex chars (128 bits)", s, len(s))
		}
		if _, err := hex.DecodeString(s); err != nil {
			t.Fatalf("secret %q is not hex: %v", s, err)
		}
		if seen[s] {
			t.Fatalf("newSpawnSecret repeated a value after %d calls: %q", i, s)
		}
		seen[s] = true
	}
}

// TestClaim_RecordsSpawnSecretOnLease is the join between the two halves of the
// mechanism: the value handed to the runner must be the value the registry will
// accept from a dial-back.
//
// It is asserted through AttachInstance rather than through a getter because
// there deliberately is no getter — the expected secret is write-mostly so it
// cannot be projected onto GET /leases or into a log by accident. The wrong-value
// case runs FIRST, so the accept afterwards proves the check is real and not
// simply admitting everything.
func TestClaim_RecordsSpawnSecretOnLease(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(8801)}
	cfg := Config{ListenAddr: "127.0.0.1:8080", Binaries: testBinaryRegistry("/bin/nexus")}
	ts, reg, cs := newClaimTestServer(t, runner, cfg)
	cs.sessionReportGrace = 50 * time.Millisecond

	respCh := make(chan *http.Response, 1)
	go func() { respCh <- postClaim(t, ts.URL, `{"config":"engine:\n  name: test\n"}`) }()

	var spec spawnSpec
	select {
	case spec = <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner.start was never called")
	}

	if spec.spawnSecret == "" {
		t.Fatal("the claim spawned an instance with no spawn secret")
	}
	if spec.spawnSecret == spec.leaseID {
		t.Fatal("the spawn secret is the lease id; it must be an independent value")
	}

	// A value that is not the minted one is refused...
	if err := reg.AttachInstance(spec.leaseID, newWSConn(nil), "not-the-minted-secret"); err == nil {
		t.Fatal("the registry accepted a secret the claim never minted")
	}
	// ...and the one handed to the runner is accepted, so the registry's stored
	// expectation and the child's environment agree.
	if err := reg.AttachInstance(spec.leaseID, newWSConn(nil), spec.spawnSecret); err != nil {
		t.Fatalf("the registry refused the secret it handed the runner: %v", err)
	}

	// Let the parked claim finish rather than leaving it to time out, so the
	// server teardown does not block on the ready deadline.
	reg.MarkReady(spec.leaseID)
	select {
	case resp := <-respCh:
		resp.Body.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("claim never completed after the instance was marked ready")
	}
}

func TestBuildCommand_RecallSession(t *testing.T) {
	spec := spawnSpec{
		binaryPath:      "/opt/nexus/bin/nexus",
		configPath:      "/tmp/claim-123.yaml",
		leaseID:         "lease-abc",
		brokerAddr:      "ws://127.0.0.1:8080/instance",
		recallSessionID: "sess-resume-9",
	}
	cmd := buildCommand(spec)

	wantArgs := []string{"/opt/nexus/bin/nexus", "-config", "/tmp/claim-123.yaml", "-recall", "sess-resume-9"}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("cmd.Args = %v, want %v", cmd.Args, wantArgs)
	}
}

func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestClaim_NewSession_ReadyRoundTrip(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(4321)}
	cfg := Config{ListenAddr: "127.0.0.1:8080", Binaries: testBinaryRegistry("/bin/nexus")}
	ts, reg, _ := newClaimTestServer(t, runner, cfg)

	const wantConfig = "engine:\n  name: test\n"
	respCh := make(chan *http.Response, 1)
	go func() {
		respCh <- postClaim(t, ts.URL, `{"config":`+jsonString(wantConfig)+`}`)
	}()

	// The handler spawns synchronously; capture and assert the spec.
	var spec spawnSpec
	select {
	case spec = <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner.start was never called")
	}

	if spec.binaryPath != "/bin/nexus" {
		t.Errorf("binaryPath = %q", spec.binaryPath)
	}
	if spec.leaseID == "" {
		t.Error("leaseID not minted")
	}
	if spec.brokerAddr != "ws://127.0.0.1:8080/instance" {
		t.Errorf("brokerAddr = %q", spec.brokerAddr)
	}
	// Temp config exists and holds the supplied bytes while the instance boots.
	data, err := os.ReadFile(spec.configPath)
	if err != nil {
		t.Fatalf("temp config not written: %v", err)
	}
	if string(data) != wantConfig {
		t.Errorf("temp config = %q, want %q", string(data), wantConfig)
	}

	// New session: no recall arg should be requested.
	if spec.recallSessionID != "" {
		t.Errorf("recallSessionID = %q, want empty for a new session", spec.recallSessionID)
	}

	// Simulate the instance dialing back, signalling ready, then reporting
	// the engine-generated session id.
	reg.MarkReady(spec.leaseID)
	reg.MarkSessionID(spec.leaseID, "engine-sess-7")

	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var cr claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if cr.LeaseID != spec.leaseID {
		t.Errorf("lease_id = %q, want %q", cr.LeaseID, spec.leaseID)
	}
	if cr.SessionID != "engine-sess-7" {
		t.Errorf("session_id = %q, want %q (engine-generated id reported back)", cr.SessionID, "engine-sess-7")
	}
	if want := ClientWSPath(spec.leaseID); !strings.HasSuffix(cr.WSURL, want) {
		t.Errorf("ws_url = %q, want suffix %q", cr.WSURL, want)
	}
	if !strings.HasPrefix(cr.WSURL, "ws://127.0.0.1:8080") {
		t.Errorf("ws_url = %q, want ws://127.0.0.1:8080 prefix", cr.WSURL)
	}

	// Process is tracked on the lease.
	if pid := reg.PID(spec.leaseID); pid != 4321 {
		t.Errorf("tracked pid = %d, want 4321", pid)
	}

	// Temp config is cleaned up once the handler returns.
	waitFor(t, func() bool {
		_, err := os.Stat(spec.configPath)
		return os.IsNotExist(err)
	})
}

func TestClaim_ReadyTimeout_KillsProcessAndCleansUp(t *testing.T) {
	proc := newFakeProcess(999)
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: proc}
	cfg := Config{ListenAddr: "127.0.0.1:8080", Binaries: testBinaryRegistry("/bin/nexus")}
	ts, reg, cs := newClaimTestServer(t, runner, cfg)
	cs.readyTimeout = 50 * time.Millisecond // never marked ready

	respCh := make(chan *http.Response, 1)
	go func() { respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n"}`) }()

	spec := <-runner.started

	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}

	// The process was killed (no leak).
	select {
	case <-proc.killed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed-out instance was not killed")
	}

	// Lease dropped and temp config removed.
	if reg.Has(spec.leaseID) {
		t.Error("lease not removed after timeout")
	}
	waitFor(t, func() bool {
		_, err := os.Stat(spec.configPath)
		return os.IsNotExist(err)
	})
}

func TestClaim_InstanceExitsBeforeReady(t *testing.T) {
	proc := newFakeProcess(1000)
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: proc}
	cfg := Config{ListenAddr: "127.0.0.1:8080", Binaries: testBinaryRegistry("/bin/nexus")}
	ts, reg, _ := newClaimTestServer(t, runner, cfg)

	respCh := make(chan *http.Response, 1)
	go func() { respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n"}`) }()

	spec := <-runner.started
	close(proc.exited) // process dies before ready

	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if reg.Has(spec.leaseID) {
		t.Error("lease not removed after early exit")
	}
}

func TestClaim_SpawnError(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), err: errors.New("exec failed")}
	cfg := Config{ListenAddr: "127.0.0.1:8080", Binaries: testBinaryRegistry("/bin/nexus")}
	ts, _, _ := newClaimTestServer(t, runner, cfg)

	resp := postClaim(t, ts.URL, `{"config":"engine: {}\n"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestClaim_Resume_PassesRecallAndEchoesSessionID(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(5555)}
	cfg := Config{ListenAddr: "127.0.0.1:8080", Binaries: testBinaryRegistry("/bin/nexus")}
	ts, reg, _ := newClaimTestServer(t, runner, cfg)

	respCh := make(chan *http.Response, 1)
	go func() {
		respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n","session_id":"prior-sess-3"}`)
	}()

	var spec spawnSpec
	select {
	case spec = <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner.start was never called")
	}

	// Resume must hand the engine -recall <id> via the spawn spec.
	if spec.recallSessionID != "prior-sess-3" {
		t.Errorf("recallSessionID = %q, want %q", spec.recallSessionID, "prior-sess-3")
	}

	// Instance boots, signals ready, and reports the recalled session id.
	reg.MarkReady(spec.leaseID)
	reg.MarkSessionID(spec.leaseID, "prior-sess-3")

	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var cr claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// For a resume the returned id matches the requested one.
	if cr.SessionID != "prior-sess-3" {
		t.Errorf("session_id = %q, want %q", cr.SessionID, "prior-sess-3")
	}
}

// TestClaim_RejectsEmptyConfig keeps `config` required. The registry is
// deliberately VALID here so the 400 can only have come from the config check —
// with an empty registry the binary resolution below it would return a 400 of
// its own and the test would pass for the wrong reason.
func TestClaim_RejectsEmptyConfig(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1)}
	cfg := Config{ListenAddr: ":8080", Binaries: testBinaryRegistry("/bin/nexus")}
	ts, _, _ := newClaimTestServer(t, runner, cfg)

	resp := postClaim(t, ts.URL, `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// variantRegistryConfig is a two-entry registry: the reserved `nexus` and a
// `vision` variant carrying static args and env. Both entries are pre-resolved
// to paths that need not exist, because nothing in these tests exec()s anything
// — the fake runner records the spec instead.
func variantRegistryConfig() Config {
	return Config{
		ListenAddr: "127.0.0.1:8080",
		Binaries: map[string]BinaryEntry{
			reservedBinaryName: {Path: "nexus", ResolvedPath: "/usr/local/bin/nexus"},
			"vision": {
				Path:         "~/builds/nexus-vision",
				ResolvedPath: "/opt/builds/nexus-vision",
				Args:         []string{"-profile", "vision"},
				Env:          map[string]string{"NEXUS_VISION": "1"},
			},
		},
	}
}

// awaitSpawn blocks until the fake runner records a spawn, failing the test if
// the claim never got that far.
func awaitSpawn(t *testing.T, runner *fakeRunner) spawnSpec {
	t.Helper()
	select {
	case spec := <-runner.started:
		return spec
	case <-time.After(2 * time.Second):
		t.Fatal("runner.start was never called")
		return spawnSpec{}
	}
}

// TestClaim_NamedBinarySpawnsThatEntry is the story's headline: the name in the
// claim body decides which registry entry is spawned, and the entry's whole
// contribution — resolved path, static args, static env — reaches the runner.
//
// The name is asserted alongside the path because two entries may legitimately
// point at the same executable, so the path alone cannot tell a later consumer
// which variant was claimed.
func TestClaim_NamedBinarySpawnsThatEntry(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(9101)}
	ts, reg, cs := newClaimTestServer(t, runner, variantRegistryConfig())
	cs.sessionReportGrace = 50 * time.Millisecond

	respCh := make(chan *http.Response, 1)
	go func() { respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n","binary":"vision"}`) }()

	spec := awaitSpawn(t, runner)
	if spec.binaryName != "vision" {
		t.Errorf("binaryName = %q, want vision", spec.binaryName)
	}
	if spec.binaryPath != "/opt/builds/nexus-vision" {
		t.Errorf("binaryPath = %q, want the entry's RESOLVED path", spec.binaryPath)
	}
	if !reflect.DeepEqual(spec.binaryArgs, []string{"-profile", "vision"}) {
		t.Errorf("binaryArgs = %v, want the entry's args", spec.binaryArgs)
	}
	if !reflect.DeepEqual(spec.binaryEnv, map[string]string{"NEXUS_VISION": "1"}) {
		t.Errorf("binaryEnv = %v, want the entry's env", spec.binaryEnv)
	}

	reg.MarkReady(spec.leaseID)
	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestClaim_OmittedBinaryResolvesToReservedNexus is the backward-compatibility
// half: a client written before the registry existed sends no `binary` and must
// still get the base build. It asserts the RESOLVED path of the reserved entry
// rather than merely a non-empty one, so a regression that fell through to some
// other entry would be caught.
func TestClaim_OmittedBinaryResolvesToReservedNexus(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(9102)}
	ts, reg, cs := newClaimTestServer(t, runner, variantRegistryConfig())
	cs.sessionReportGrace = 50 * time.Millisecond

	respCh := make(chan *http.Response, 1)
	go func() { respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n"}`) }()

	spec := awaitSpawn(t, runner)
	if spec.binaryName != reservedBinaryName {
		t.Errorf("binaryName = %q, want %q for an omitted binary", spec.binaryName, reservedBinaryName)
	}
	if spec.binaryPath != "/usr/local/bin/nexus" {
		t.Errorf("binaryPath = %q, want the reserved entry's resolved path", spec.binaryPath)
	}
	if spec.binaryArgs != nil || spec.binaryEnv != nil {
		t.Errorf("reserved entry contributed args=%v env=%v, want neither", spec.binaryArgs, spec.binaryEnv)
	}

	reg.MarkReady(spec.leaseID)
	resp := <-respCh
	resp.Body.Close()
}

// TestClaim_StampsBinaryOnLeaseForSessionLookup is the join the resume check is
// built on: the entry a claim resolved is stamped on its lease, so once the
// instance reports a session id the broker can answer "which build is this
// session running".
//
// It asserts through BinaryForSession rather than by reaching into the lease
// struct, because the lookup is the field's only consumer and one that could not
// be read back would be bookkeeping with no user.
//
// The ORDER matters and is what the test really pins: the binary is known at
// claim time, the session id only arrives later, and the mapping must exist once
// the second of the two lands. This server is also wired with NO lease store —
// the pre-durability topology, and the same shape as an unset state_dir — so the
// mapping is shown to hold in memory whether or not anything is journaled.
func TestClaim_StampsBinaryOnLeaseForSessionLookup(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(9311)}
	ts, reg, cs := newClaimTestServer(t, runner, variantRegistryConfig())
	cs.sessionReportGrace = 2 * time.Second

	respCh := make(chan *http.Response, 1)
	go func() { respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n","binary":"vision"}`) }()

	spec := awaitSpawn(t, runner)
	// Nothing is recorded against a session id yet — there is no session id.
	if got, ok := reg.BinaryForSession(""); ok {
		t.Errorf("BinaryForSession(\"\") = (%q, true) mid-claim, want unknown", got)
	}

	reg.MarkReady(spec.leaseID)
	reg.MarkSessionID(spec.leaseID, "engine-sess-vision")

	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got, ok := reg.BinaryForSession("engine-sess-vision")
	if !ok {
		t.Fatal("BinaryForSession reported the session this claim just created unknown")
	}
	if got != "vision" {
		t.Errorf("BinaryForSession = %q, want vision (the entry the claim named)", got)
	}
}

// TestClaim_UnknownBinaryAllocatesNothing is the ordering criterion, and it is
// the reason resolution sits where it does in handleClaim.
//
// Everything below that point takes something the broker then has to give back:
// a capacity slot, a lease id, a temp config file, a child process. A caller's
// typo must consume NONE of it — a broker whose slots could be exhausted by
// misspelled binary names would be trivially deniable-of-service. Each of the
// four is asserted separately because they are freed by different code and a
// regression could leak any one of them alone.
func TestClaim_UnknownBinaryAllocatesNothing(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(9103)}
	ts, reg, _ := newClaimTestServer(t, runner, variantRegistryConfig())

	tempsBefore := claimTempConfigCount(t)

	resp := postClaim(t, ts.URL, `{"config":"engine: {}\n","binary":"vison"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// The message echoes the rejected name: without it an operator reading a
	// client's logs cannot tell a typo from a broker that was never configured
	// with the variant at all.
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(body["error"], `"vison"`) {
		t.Errorf("error %q does not echo the requested name", body["error"])
	}

	if snap := reg.Snapshot(); len(snap.Leases) != 0 {
		t.Errorf("registry holds %d leases after a rejected claim, want 0", len(snap.Leases))
	}
	if used := reg.SlotsInUse(); used != 0 {
		t.Errorf("SlotsInUse = %d after a rejected claim, want 0", used)
	}
	select {
	case spec := <-runner.started:
		t.Fatalf("a rejected claim spawned %q", spec.binaryPath)
	default:
	}
	if after := claimTempConfigCount(t); after != tempsBefore {
		t.Errorf("temp claim configs went from %d to %d; a rejected claim wrote one", tempsBefore, after)
	}
}

// claimTempConfigCount counts the temp files writeTempConfig would have created.
// A before/after comparison (rather than an absolute zero) keeps it honest on a
// machine whose temp dir already holds unrelated leftovers.
func claimTempConfigCount(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "nexus-broker-claim-*.yaml"))
	if err != nil {
		t.Fatalf("glob temp configs: %v", err)
	}
	return len(matches)
}

// seedSessionBinding puts a claim test's registry in the state a REAL resume
// meets: the lease that created the session is long gone, and the only thing
// that remembers which variant it ran is the durable index.
//
// It seeds through RecordSessionBinary rather than by minting a lease on purpose
// — a live lease would let the live-lease half of BinaryForSession answer, and
// the resume path that matters is precisely the one where it cannot.
func seedSessionBinding(t *testing.T, reg *Registry, sessionID, binary string) {
	t.Helper()
	idx, err := openSessionBinaryIndex(testLogger(), Config{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("openSessionBinaryIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	reg.useSessionBinaryIndex(idx)
	reg.RecordSessionBinary(sessionID, binary)
}

// TestClaim_ResumeWithOmittedBinaryInheritsRecordedEntry is the headline: a
// client that captured a session id from a variant claim and reconnects without
// restating the variant gets its own session back, not a base build replaying it.
//
// The reserved entry's resolved path is asserted to be absent rather than merely
// asserting the vision one is present, because the regression this guards
// against is exactly the fallthrough to `nexus` that an omitted `binary` means
// on every non-resume claim.
func TestClaim_ResumeWithOmittedBinaryInheritsRecordedEntry(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(9401)}
	ts, reg, cs := newClaimTestServer(t, runner, variantRegistryConfig())
	cs.sessionReportGrace = 50 * time.Millisecond
	seedSessionBinding(t, reg, "sess-vision-1", "vision")

	respCh := make(chan *http.Response, 1)
	go func() {
		respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n","session_id":"sess-vision-1"}`)
	}()

	spec := awaitSpawn(t, runner)
	if spec.binaryName != "vision" {
		t.Errorf("binaryName = %q, want vision (the recorded entry) for a resume with no binary", spec.binaryName)
	}
	if spec.binaryPath != "/opt/builds/nexus-vision" {
		t.Errorf("binaryPath = %q, want the recorded entry's resolved path", spec.binaryPath)
	}
	// The recorded entry's whole contribution is inherited, not just its path:
	// a variant whose args were dropped would boot the wrong profile.
	if !reflect.DeepEqual(spec.binaryArgs, []string{"-profile", "vision"}) {
		t.Errorf("binaryArgs = %v, want the recorded entry's args", spec.binaryArgs)
	}
	if spec.recallSessionID != "sess-vision-1" {
		t.Errorf("recallSessionID = %q, want the requested session", spec.recallSessionID)
	}

	reg.MarkReady(spec.leaseID)
	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestClaim_ResumeWithMatchingBinaryProceeds pins the common case, which must
// stay entirely uneventful: a client that DOES restate the variant it recorded
// is not made to pay for the check.
func TestClaim_ResumeWithMatchingBinaryProceeds(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(9402)}
	ts, reg, cs := newClaimTestServer(t, runner, variantRegistryConfig())
	cs.sessionReportGrace = 50 * time.Millisecond
	seedSessionBinding(t, reg, "sess-vision-2", "vision")

	respCh := make(chan *http.Response, 1)
	go func() {
		respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n","session_id":"sess-vision-2","binary":"vision"}`)
	}()

	spec := awaitSpawn(t, runner)
	if spec.binaryName != "vision" {
		t.Errorf("binaryName = %q, want vision", spec.binaryName)
	}

	reg.MarkReady(spec.leaseID)
	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestClaim_ResumeWithDifferentBinaryConflicts is the refusal, and it uses the
// reserved `nexus` as the requested name on purpose: `{"binary":"nexus"}` is the
// one value that proves the check reads what the CALLER SENT rather than what an
// omitted field resolves to. If the two were conflated this claim would be
// indistinguishable from the inherit case above and would silently succeed.
//
// It also re-asserts the allocation criterion at the 409, for the same reason
// TestClaim_UnknownBinaryAllocatesNothing does it at the 400: the four resources
// are freed by four different code paths, so a rejection that ran past the
// resolution block could leak any one of them alone.
func TestClaim_ResumeWithDifferentBinaryConflicts(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(9403)}
	ts, reg, _ := newClaimTestServer(t, runner, variantRegistryConfig())
	seedSessionBinding(t, reg, "sess-vision-3", "vision")

	tempsBefore := claimTempConfigCount(t)

	resp := postClaim(t, ts.URL, `{"config":"engine: {}\n","session_id":"sess-vision-3","binary":"nexus"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	// BOTH names, or the message cannot be acted on: the recorded one is the
	// value a correct retry has to send, and the requested one is what the caller
	// has to recognise as its own mistake.
	for _, want := range []string{`"vision"`, `"nexus"`} {
		if !strings.Contains(body["error"], want) {
			t.Errorf("error %q does not name %s", body["error"], want)
		}
	}

	if snap := reg.Snapshot(); len(snap.Leases) != 0 {
		t.Errorf("registry holds %d leases after a refused resume, want 0", len(snap.Leases))
	}
	if used := reg.SlotsInUse(); used != 0 {
		t.Errorf("SlotsInUse = %d after a refused resume, want 0", used)
	}
	select {
	case spec := <-runner.started:
		t.Fatalf("a refused resume spawned %q", spec.binaryPath)
	default:
	}
	if after := claimTempConfigCount(t); after != tempsBefore {
		t.Errorf("temp claim configs went from %d to %d; a refused resume wrote one", tempsBefore, after)
	}
}

// TestClaim_ResumeWithUnknownBindingSpawnsRequestedBinary is the fallthrough, and
// it is the branch most likely to be broken by an over-eager implementation of
// the check above.
//
// This server has NO session binary index at all — the shape a broker with no
// state_dir has, and the same answer a pruned binding or a session predating the
// index gives. Every one of those is a legitimate resume, so an unknown binding
// must resolve exactly as a fresh claim does and never as a mismatch.
func TestClaim_ResumeWithUnknownBindingSpawnsRequestedBinary(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(9404)}
	ts, reg, cs := newClaimTestServer(t, runner, variantRegistryConfig())
	cs.sessionReportGrace = 50 * time.Millisecond

	respCh := make(chan *http.Response, 1)
	go func() {
		respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n","session_id":"sess-unrecorded","binary":"vision"}`)
	}()

	spec := awaitSpawn(t, runner)
	if spec.binaryName != "vision" {
		t.Errorf("binaryName = %q, want vision (the requested entry) for an unrecorded session", spec.binaryName)
	}

	reg.MarkReady(spec.leaseID)
	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; an unknown binding must never be reported as a conflict", resp.StatusCode)
	}
}

// TestClaim_ResumeWithUnknownBindingAndNoBinaryFallsBackToNexus is the other
// half of the fallthrough: with nothing recorded AND nothing requested there is
// no opinion to inherit, so the reserved entry applies exactly as it does to a
// fresh claim. This is what every pre-registry client's resume looks like.
func TestClaim_ResumeWithUnknownBindingAndNoBinaryFallsBackToNexus(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(9405)}
	ts, reg, cs := newClaimTestServer(t, runner, variantRegistryConfig())
	cs.sessionReportGrace = 50 * time.Millisecond

	respCh := make(chan *http.Response, 1)
	go func() {
		respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n","session_id":"sess-unrecorded-2"}`)
	}()

	spec := awaitSpawn(t, runner)
	if spec.binaryName != reservedBinaryName {
		t.Errorf("binaryName = %q, want %q for an unrecorded session with no binary", spec.binaryName, reservedBinaryName)
	}
	if spec.binaryPath != "/usr/local/bin/nexus" {
		t.Errorf("binaryPath = %q, want the reserved entry's resolved path", spec.binaryPath)
	}

	reg.MarkReady(spec.leaseID)
	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestClaim_ResumeWhoseRecordedBinaryIsGoneConflicts covers the operator-caused
// half: the binding is intact but the entry it names has been removed from
// `binaries:` since the session was created.
//
// The refusal matters more here than it looks. There is no requested name to
// compare against, so the tempting behaviour is to shrug and spawn the reserved
// entry — which is the same foreign-build replay a mismatch is refused for,
// reached through an operator's config change instead of a caller's parameter.
// It must name the missing entry, or the operator cannot tell which one to
// restore.
func TestClaim_ResumeWhoseRecordedBinaryIsGoneConflicts(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(9406)}
	ts, reg, _ := newClaimTestServer(t, runner, variantRegistryConfig())
	seedSessionBinding(t, reg, "sess-nocturne", "nocturne") // never in variantRegistryConfig

	resp := postClaim(t, ts.URL, `{"config":"engine: {}\n","session_id":"sess-nocturne"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(body["error"], `"nocturne"`) {
		t.Errorf("error %q does not name the recorded entry that is gone", body["error"])
	}

	if snap := reg.Snapshot(); len(snap.Leases) != 0 {
		t.Errorf("registry holds %d leases after a refused resume, want 0", len(snap.Leases))
	}
	select {
	case spec := <-runner.started:
		t.Fatalf("a refused resume spawned %q", spec.binaryPath)
	default:
	}
}

// TestResolveSpawnBinary_TypoBeatsMismatch pins the precedence between the two
// rejections, which is the one ordering a reader cannot infer from the criteria.
//
// A name this broker has never had is a 400 listing the names it does have, even
// on a session bound to something else. Reporting it as a 409 would tell the
// caller its session is bound elsewhere — true, but not the problem — and would
// withhold the list that is the only way to discover the correct spelling.
func TestResolveSpawnBinary_TypoBeatsMismatch(t *testing.T) {
	binaries := variantRegistryConfig().Binaries

	_, _, status, err := resolveSpawnBinary(binaries, "sess-x", "vison", "vision", true)
	if err == nil {
		t.Fatal("resolveSpawnBinary accepted a name that is in no registry entry")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown name even on a bound session", status)
	}
	if !strings.Contains(err.Error(), "this broker spawns:") {
		t.Errorf("error %q does not list the registry's entries", err)
	}
}

// TestResolveSpawnBinary_WhitespaceOnlyBinaryIsOmitted keeps the resume path in
// step with resolveClaimBinary, which reads whitespace as "omitted" rather than
// as a name made of spaces. If the two diverged, `{"binary":"  "}` would be
// compared against the record as a literal and 409 every bound session.
func TestResolveSpawnBinary_WhitespaceOnlyBinaryIsOmitted(t *testing.T) {
	binaries := variantRegistryConfig().Binaries

	name, entry, status, err := resolveSpawnBinary(binaries, "sess-x", "  \t", "vision", true)
	if err != nil {
		t.Fatalf("resolveSpawnBinary: %v (status %d)", err, status)
	}
	if name != "vision" {
		t.Errorf("name = %q, want the recorded vision entry inherited", name)
	}
	if entry.ResolvedPath != "/opt/builds/nexus-vision" {
		t.Errorf("entry = %+v, want the vision entry", entry)
	}
}

// TestResolveClaimBinary_TrimsRequestedName mirrors foldBinaryRegistry, which
// trims entry names as it loads them. The two have to agree: a registry that
// stores `vision` while a claim looks up `vision ` verbatim would make an entry
// selectable by no name a human would write.
func TestResolveClaimBinary_TrimsRequestedName(t *testing.T) {
	binaries := variantRegistryConfig().Binaries

	name, entry, err := resolveClaimBinary(binaries, "  vision\t")
	if err != nil {
		t.Fatalf("resolveClaimBinary: %v", err)
	}
	if name != "vision" {
		t.Errorf("name = %q, want vision", name)
	}
	if entry.ResolvedPath != "/opt/builds/nexus-vision" {
		t.Errorf("entry = %+v, want the vision entry", entry)
	}

	// Whitespace-only is "omitted", not "a name made of spaces".
	if name, _, err := resolveClaimBinary(binaries, "   "); err != nil || name != reservedBinaryName {
		t.Errorf("resolveClaimBinary(%q) = %q, %v; want the reserved entry", "   ", name, err)
	}
}

// TestBuildCommand_AppendsEntryArgsAfterBrokerArgs pins argv ORDER. Go's flag
// package stops parsing at the first non-flag argument, so entry args placed
// ahead of -config would leave the instance booting some other config entirely.
func TestBuildCommand_AppendsEntryArgsAfterBrokerArgs(t *testing.T) {
	cmd := buildCommand(spawnSpec{
		binaryPath:      "/opt/builds/nexus-vision",
		configPath:      "/tmp/claim-123.yaml",
		leaseID:         "lease-abc",
		brokerAddr:      "ws://127.0.0.1:8080/instance",
		recallSessionID: "sess-resume-9",
		binaryArgs:      []string{"-profile", "vision"},
	})

	wantArgs := []string{
		"/opt/builds/nexus-vision",
		"-config", "/tmp/claim-123.yaml",
		"-recall", "sess-resume-9",
		"-profile", "vision",
	}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("cmd.Args = %v, want %v", cmd.Args, wantArgs)
	}
}

// TestBuildCommand_MergesEntryEnv checks the additive half: an entry's env
// reaches the child alongside the always-pass set, without displacing it.
func TestBuildCommand_MergesEntryEnv(t *testing.T) {
	t.Setenv("HOME", "/home/broker")

	cmd := buildCommand(spawnSpec{
		binaryPath: "/opt/builds/nexus-vision",
		configPath: "/tmp/claim-123.yaml",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
		binaryEnv:  map[string]string{"NEXUS_VISION": "1", "NEXUS_VARIANT": "vision"},
	})

	for _, want := range []string{"NEXUS_VISION=1", "NEXUS_VARIANT=vision", "HOME=/home/broker"} {
		if !envHas(cmd.Env, want) {
			t.Errorf("env missing %q; got %v", want, cmd.Env)
		}
	}
}

// TestBuildCommand_DropsUndeclaredBrokerEnv is THE test for this boundary: a
// variable the broker process holds but no operator declared must not reach the
// child at all.
//
// It matters because a claim supplies the whole engine config, and a provider
// resolves its credential from an env var that config NAMES (`api_key_env` and
// its equivalents) while the same config chooses `base_url`. Anything left in the
// child's environment is therefore readable AND postable by whoever claimed the
// lease — including credentials that have nothing to do with Nexus. Asserting
// absence rather than "the declared ones are present" is the point: presence
// tests pass just as happily under wholesale inheritance.
func TestBuildCommand_DropsUndeclaredBrokerEnv(t *testing.T) {
	// One a caller could plausibly name in api_key_env, and one that is not a
	// credential at all — the rule is not "scrub things that look secret".
	t.Setenv("AWS_SECRET_ACCESS_KEY", "broker-only-secret")
	t.Setenv("NEXUS_BROKER_TEST_UNDECLARED", "from-broker")

	cmd := buildCommand(spawnSpec{
		binaryPath: "/opt/builds/nexus",
		configPath: "/tmp/claim-123.yaml",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
		// Declares something ELSE, so the spec is not trivially empty.
		inheritEnv: []string{"NEXUS_BROKER_TEST_DECLARED"},
	})

	for _, name := range []string{"AWS_SECRET_ACCESS_KEY", "NEXUS_BROKER_TEST_UNDECLARED"} {
		if value, ok := lastEnvValue(cmd.Env, name); ok {
			t.Errorf("child env carries undeclared %s=%q; the broker's environment leaked into a claimed instance", name, value)
		}
	}
}

// TestBuildCommand_CarriesDeclaredAndAlwaysPassEnv pins the other side of the
// same boundary: the always-pass set and the declared names DO get through, and
// the three broker-owned variables are always present.
//
// HOME and PATH are here for a reason unrelated to credentials — without them an
// instance cannot resolve ~/.nexus (pkg/engine/paths.go) or exec anything by
// name, so a scrub that dropped them would break sessions and the shell tool and
// look like a Nexus bug rather than a policy decision.
func TestBuildCommand_CarriesDeclaredAndAlwaysPassEnv(t *testing.T) {
	t.Setenv("HOME", "/home/broker")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("TZ", "Europe/Amsterdam")
	t.Setenv("LANG", "en_GB.UTF-8")
	t.Setenv("ANTHROPIC_API_KEY", "declared-key")

	cmd := buildCommand(spawnSpec{
		binaryPath:  "/opt/builds/nexus",
		configPath:  "/tmp/claim-123.yaml",
		leaseID:     "lease-abc",
		brokerAddr:  "ws://127.0.0.1:8080/instance",
		spawnSecret: "7f3b91c2ad4e5806b1c2d3e4f5061728",
		inheritEnv:  []string{"ANTHROPIC_API_KEY"},
	})

	want := map[string]string{
		"HOME":                     "/home/broker",
		"PATH":                     "/usr/bin:/bin",
		"TZ":                       "Europe/Amsterdam",
		"LANG":                     "en_GB.UTF-8",
		"ANTHROPIC_API_KEY":        "declared-key",
		brokerframe.EnvBrokerAddr:  "ws://127.0.0.1:8080/instance",
		brokerframe.EnvLeaseID:     "lease-abc",
		brokerframe.EnvSpawnSecret: "7f3b91c2ad4e5806b1c2d3e4f5061728",
	}
	for name, value := range want {
		got, ok := lastEnvValue(cmd.Env, name)
		if !ok {
			t.Errorf("child env has no %s at all; got %v", name, cmd.Env)
			continue
		}
		if got != value {
			t.Errorf("child env %s = %q, want %q", name, got, value)
		}
	}
}

// TestBuildCommand_SkipsDeclaredButUnsetEnv checks that a declared name the
// broker does not hold is OMITTED rather than exported empty. An instance can
// tell "unset" from "set to empty", and a provider handed api_key_env="" fails
// less legibly than one that finds the variable absent.
func TestBuildCommand_SkipsDeclaredButUnsetEnv(t *testing.T) {
	cmd := buildCommand(spawnSpec{
		binaryPath: "/opt/builds/nexus",
		configPath: "/tmp/claim-123.yaml",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
		inheritEnv: []string{"NEXUS_BROKER_TEST_NEVER_SET"},
	})

	if _, ok := lastEnvValue(cmd.Env, "NEXUS_BROKER_TEST_NEVER_SET"); ok {
		t.Errorf("a declared-but-unset variable was exported anyway; got %v", cmd.Env)
	}
}

// TestBuildCommand_InheritedEnvIsSorted pins the reproducibility property the
// entry-env layering already had: the environment a spawn is handed must be
// byte-identical across restarts, so the inherited half is emitted in sorted key
// order rather than in os.Environ()'s order.
func TestBuildCommand_InheritedEnvIsSorted(t *testing.T) {
	t.Setenv("ZZ_LAST_DECLARED", "z")
	t.Setenv("AA_FIRST_DECLARED", "a")

	cmd := buildCommand(spawnSpec{
		binaryPath: "/opt/builds/nexus",
		configPath: "/tmp/claim-123.yaml",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
		// Declared out of order on purpose.
		inheritEnv: []string{"ZZ_LAST_DECLARED", "AA_FIRST_DECLARED"},
	})

	var names []string
	for _, kv := range cmd.Env {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	// Everything up to the entry/broker-owned tail is the inherited half.
	inherited := names[:len(names)-len(brokerOwnedEnv)]
	if !sort.StringsAreSorted(inherited) {
		t.Errorf("inherited env is not in sorted key order: %v", inherited)
	}
}

// TestSpawnEnvNames_NamesWhatASpawnWillCarry pins the boot log's contract: one
// line per registry entry naming EXACTLY the variables that entry's spawns will
// hold — and names only, because several of them are credentials by
// construction.
//
// A declared name the broker does not actually hold is absent from the list
// rather than listed, which is the cheapest diagnostic for the break this story
// introduces: "I declared it and it is not in the boot log" means "it was never
// in the broker's environment".
func TestSpawnEnvNames_NamesWhatASpawnWillCarry(t *testing.T) {
	t.Setenv("HOME", "/home/broker")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("TZ", "UTC")
	t.Setenv("LANG", "C")
	t.Setenv("ANTHROPIC_API_KEY", "declared-key")

	got := spawnEnvNames(
		[]string{"ANTHROPIC_API_KEY", "NEXUS_BROKER_TEST_NEVER_SET"},
		map[string]string{"NEXUS_VISION": "1"},
	)

	want := []string{
		"ANTHROPIC_API_KEY",
		"HOME",
		"LANG",
		brokerframe.EnvBrokerAddr,
		brokerframe.EnvLeaseID,
		brokerframe.EnvSpawnSecret,
		"NEXUS_VISION",
		"PATH",
		"TZ",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spawnEnvNames = %v, want %v", got, want)
	}

	// Names only. A value appearing here would be logged verbatim at boot.
	for _, name := range got {
		if strings.Contains(name, "=") || name == "declared-key" {
			t.Errorf("spawnEnvNames returned something that is not a bare name: %q", name)
		}
	}
}

// TestMissingInheritEnv_ReportsOnlyWhatIsNotHeld backs the boot WARN.
func TestMissingInheritEnv_ReportsOnlyWhatIsNotHeld(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "declared-key")

	got := missingInheritEnv([]string{"ANTHROPIC_API_KEY", "NEXUS_BROKER_TEST_NEVER_SET"})
	if !reflect.DeepEqual(got, []string{"NEXUS_BROKER_TEST_NEVER_SET"}) {
		t.Errorf("missingInheritEnv = %v, want only the unset name", got)
	}
}

// TestBuildCommand_BrokerEnvWinsOverEntryEnv is a SECURITY test, not a style
// one. os/exec resolves a duplicated environment key to its final occurrence, so
// the broker's three variables have to be appended last:
//
//   - NEXUS_BROKER_SPAWN_SECRET is the dial-back second factor. An entry that
//     could set it would let anyone with write access to broker.yaml choose the
//     value an instance presents, and the registry would then accept a dial-back
//     the broker never minted.
//   - NEXUS_BROKER_ADDR would let an entry point its instances at a different
//     broker entirely.
//   - NEXUS_BROKER_LEASE_ID would let an entry's instance attach to another
//     lease.
//
// The assertion is on the LAST occurrence of each key, because that is the value
// the child actually sees; asserting mere presence would pass even with the
// layering inverted.
func TestBuildCommand_BrokerEnvWinsOverEntryEnv(t *testing.T) {
	const secret = "7f3b91c2ad4e5806b1c2d3e4f5061728"
	cmd := buildCommand(spawnSpec{
		binaryPath:  "/opt/builds/nexus-vision",
		configPath:  "/tmp/claim-123.yaml",
		leaseID:     "lease-abc",
		brokerAddr:  "ws://127.0.0.1:8080/instance",
		spawnSecret: secret,
		binaryEnv: map[string]string{
			brokerframe.EnvSpawnSecret: "attacker-chosen-secret",
			brokerframe.EnvBrokerAddr:  "ws://attacker.example.com/instance",
			brokerframe.EnvLeaseID:     "someone-elses-lease",
			"NEXUS_VISION":             "1",
		},
	})

	wants := map[string]string{
		brokerframe.EnvSpawnSecret: secret,
		brokerframe.EnvBrokerAddr:  "ws://127.0.0.1:8080/instance",
		brokerframe.EnvLeaseID:     "lease-abc",
	}
	for key, want := range wants {
		got, ok := lastEnvValue(cmd.Env, key)
		if !ok {
			t.Errorf("env has no %s at all; got %v", key, cmd.Env)
			continue
		}
		if got != want {
			t.Errorf("effective %s = %q, want the broker's %q: an entry overrode a broker-owned variable", key, got, want)
		}
	}
	// The entry's own, non-colliding variable still gets through.
	if !envHas(cmd.Env, "NEXUS_VISION=1") {
		t.Errorf("entry env was dropped along with the collisions; got %v", cmd.Env)
	}
}

// lastEnvValue returns the value os/exec would resolve key to: the final
// occurrence wins when a key appears more than once.
func lastEnvValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if rest, ok := strings.CutPrefix(kv, key+"="); ok {
			value, found = rest, true
		}
	}
	return value, found
}

// newGuardedClaimTestServer is newClaimTestServer with /claim registered THROUGH
// the auth guard, which is the topology run() uses. It exists because the
// ownership stamp can only be observed on the guarded path: an unguarded handler
// never sees a Principal.
// It also wires the ticket store exactly as run() does — inert when the config
// enables no auth — so the `ticket` field in the claim response is observable on
// both sides of that switch.
func newGuardedClaimTestServer(t *testing.T, runner commandRunner, cfg Config) (*httptest.Server, *Registry) {
	t.Helper()
	ts, reg, _ := newGuardedClaimTestServerWithTickets(t, runner, cfg)
	return ts, reg
}

// newGuardedClaimTestServerWithTickets is newGuardedClaimTestServer plus the
// ticket store, for tests that assert on issuance directly rather than through
// the response body.
func newGuardedClaimTestServerWithTickets(t *testing.T, runner commandRunner, cfg Config) (*httptest.Server, *Registry, *ticketStore) {
	t.Helper()
	logger := testLogger()
	reg := NewRegistry(logger, 0)
	guard := newAuthGuard(logger, cfg.AuthChain)
	tickets := newTicketStore(logger, guard.enabled())
	reg.useTicketStore(tickets)
	cs := NewClaimServer(logger, reg, cfg, runner, tickets)
	mux := http.NewServeMux()
	cs.Register(guard.Guard(mux))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg, tickets
}

// runClaimToReady drives a claim to a 200: it posts, waits for the spawn, then
// plays the instance's ready + session-id report. It returns the spawn spec (for
// the minted lease id) and the response.
func runClaimToReady(t *testing.T, ts *httptest.Server, reg *Registry, runner *fakeRunner, token string) (spawnSpec, *http.Response) {
	t.Helper()
	respCh := make(chan *http.Response, 1)
	go func() {
		respCh <- doAuthed(t, http.MethodPost, ts.URL+"/claim", token, `{"config":"engine: {}\n"}`)
	}()

	var spec spawnSpec
	select {
	case spec = <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner.start was never called")
	}
	reg.MarkReady(spec.leaseID)
	reg.MarkSessionID(spec.leaseID, "engine-sess-owned")

	resp := <-respCh
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return spec, resp
}

// TestClaim_StampsAuthenticatedOwner is the story's headline: POST /claim records
// the identity the guard authenticated onto the lease it mints. No enforcement is
// asserted here — the record is the deliverable.
func TestClaim_StampsAuthenticatedOwner(t *testing.T) {
	cfg := mustLoadConfig(t, staticAuthYAML)
	cfg.ListenAddr = "127.0.0.1:8080"
	cfg.Binaries = testBinaryRegistry("/bin/nexus")
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(7001)}
	ts, reg := newGuardedClaimTestServer(t, runner, cfg)

	spec, _ := runClaimToReady(t, ts, reg, runner, "good-token")

	owner, ok := reg.LeaseOwner(spec.leaseID)
	if !ok {
		t.Fatal("claimed lease has no owner record")
	}
	if owner.ID != "ci-runner" {
		t.Errorf("lease owner ID = %q, want ci-runner (the authenticated principal)", owner.ID)
	}
	if owner.Tenant != "acme" {
		t.Errorf("lease owner Tenant = %q, want acme", owner.Tenant)
	}
}

// TestClaim_AnonymousOwnerWhenAuthDisabled is the backward-compatibility half:
// with no `auth:` block /claim is registered on the raw mux, no Principal reaches
// the handler, and the lease records the anonymous owner. The claim itself must
// succeed exactly as before.
func TestClaim_AnonymousOwnerWhenAuthDisabled(t *testing.T) {
	cfg := mustLoadConfig(t, "")
	cfg.ListenAddr = "127.0.0.1:8080"
	cfg.Binaries = testBinaryRegistry("/bin/nexus")
	if cfg.AuthChain.Enabled() {
		t.Fatal("precondition: auth should be disabled for this test")
	}
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(7002)}
	// Guard() on a disabled chain returns the mux unchanged, so this is the same
	// route topology an unguarded registration produces.
	ts, reg := newGuardedClaimTestServer(t, runner, cfg)

	spec, _ := runClaimToReady(t, ts, reg, runner, "")

	owner, ok := reg.LeaseOwner(spec.leaseID)
	if !ok {
		t.Fatal("claimed lease has no owner record")
	}
	if owner.ID != anonymousOwner().ID {
		t.Errorf("lease owner ID = %q, want the anonymous owner's empty id", owner.ID)
	}
	if owner.Scopes != nil || owner.Claims != nil {
		t.Errorf("lease owner = %+v, want the zero Principal", owner)
	}
}

// TestClaim_ReturnsRedeemableTicket proves the claim response's `ticket` is a real
// capability and not just a populated field: it redeems for the lease that was
// claimed and resolves to the principal that claimed it.
//
// The redemption is the assertion. A handler that returned any random string would
// satisfy a presence check while handing the client a credential the WebSocket will
// refuse, and the failure would only surface in E3-S3.
func TestClaim_ReturnsRedeemableTicket(t *testing.T) {
	cfg := mustLoadConfig(t, staticAuthYAML)
	cfg.ListenAddr = "127.0.0.1:8080"
	cfg.Binaries = testBinaryRegistry("/bin/nexus")
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(7003)}
	ts, reg, tickets := newGuardedClaimTestServerWithTickets(t, runner, cfg)

	spec, resp := runClaimToReady(t, ts, reg, runner, "good-token")

	var cr claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	if cr.Ticket == "" {
		t.Fatal("authenticated claim returned no ticket")
	}

	// Bound to THIS lease: another lease id is refused.
	if _, ok := tickets.redeem(cr.Ticket, "some-other-lease"); ok {
		t.Error("the claim's ticket was accepted for a different lease")
	}
	// ...and redeems once, for the claimant.
	got, ok := tickets.redeem(cr.Ticket, spec.leaseID)
	if !ok {
		t.Fatal("the claim's ticket is not redeemable for its own lease")
	}
	if got != "ci-runner" {
		t.Errorf("ticket principal = %q, want ci-runner (the authenticated claimant)", got)
	}
	if _, ok := tickets.redeem(cr.Ticket, spec.leaseID); ok {
		t.Error("the claim's ticket was redeemable twice")
	}
}

// TestClaim_OmitsTicketWhenAuthDisabled is the backward-compatibility half: with no
// `auth:` block the claim body carries no `ticket` KEY at all, so an existing
// client sees byte-for-byte the response it saw before tickets existed.
func TestClaim_OmitsTicketWhenAuthDisabled(t *testing.T) {
	cfg := mustLoadConfig(t, "")
	cfg.ListenAddr = "127.0.0.1:8080"
	cfg.Binaries = testBinaryRegistry("/bin/nexus")
	if cfg.AuthChain.Enabled() {
		t.Fatal("precondition: auth should be disabled for this test")
	}
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(7004)}
	ts, reg, tickets := newGuardedClaimTestServerWithTickets(t, runner, cfg)

	_, resp := runClaimToReady(t, ts, reg, runner, "")

	// Read the RAW body: decoding into claimResponse turns an omitted `ticket` into
	// an indistinguishable empty string, and absence is the property under test.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read claim body: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode claim body %q: %v", raw, err)
	}
	if _, present := fields["ticket"]; present {
		t.Errorf("claim body carries a ticket with auth disabled: %s", raw)
	}
	if got := tickets.outstanding(); got != 0 {
		t.Errorf("outstanding tickets = %d, want 0 with auth disabled", got)
	}
}

// TestClientWSHost_Precedence walks the resolution order one branch at a time:
// advertise_addr → explicit host in listen_addr → request Host → loopback. Each
// case removes exactly one input so the branch under test is the one that fires.
func TestClientWSHost_Precedence(t *testing.T) {
	cases := []struct {
		name          string
		advertiseHost string
		listenAddr    string
		requestHost   string
		want          string
	}{
		{
			name:          "advertise_addr wins over an explicit listen host",
			advertiseHost: "broker-1.example.com:8443",
			listenAddr:    "10.0.0.7:8080",
			requestHost:   "lb.example.com",
			want:          "broker-1.example.com:8443",
		},
		{
			name:          "advertise_addr wins over the request Host on a wildcard bind",
			advertiseHost: "broker-1.example.com:8443",
			listenAddr:    ":8080",
			requestHost:   "lb.example.com",
			want:          "broker-1.example.com:8443",
		},
		{
			name:        "explicit listen host wins over the request Host",
			listenAddr:  "10.0.0.7:8080",
			requestHost: "lb.example.com",
			want:        "10.0.0.7:8080",
		},
		{
			name:        "request Host is used for a wildcard bind",
			listenAddr:  ":8080",
			requestHost: "lb.example.com",
			want:        "lb.example.com",
		},
		{
			name:        "request Host is used for an explicit 0.0.0.0 bind",
			listenAddr:  "0.0.0.0:8080",
			requestHost: "lb.example.com",
			want:        "lb.example.com",
		},
		{
			name:        "request Host is used for a :: bind",
			listenAddr:  "[::]:8080",
			requestHost: "lb.example.com",
			want:        "lb.example.com",
		},
		{
			name:       "loopback is the last resort when there is no request Host",
			listenAddr: ":9090",
			want:       "127.0.0.1:9090",
		},
		{
			name:       "loopback with the hardcoded port for an unparseable listen addr",
			listenAddr: "nonsense",
			want:       "127.0.0.1:8080",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientWSHost(tc.advertiseHost, tc.listenAddr, tc.requestHost); got != tc.want {
				t.Errorf("clientWSHost(%q, %q, %q) = %q, want %q",
					tc.advertiseHost, tc.listenAddr, tc.requestHost, got, tc.want)
			}
		})
	}
}

// TestClientWSBaseURL_Scheme pins the scheme half: ws:// unless a
// scheme-qualified advertise_addr says otherwise, so nothing changes for a
// deployment that has not set the key.
func TestClientWSBaseURL_Scheme(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "no advertise_addr keeps ws:// and the listen host",
			cfg:  Config{ListenAddr: "10.0.0.7:8080"},
			want: "ws://10.0.0.7:8080",
		},
		{
			name: "bare host:port advertise_addr keeps ws://",
			cfg:  Config{ListenAddr: ":8080", AdvertiseHost: "broker-1.example.com:8443"},
			want: "ws://broker-1.example.com:8443",
		},
		{
			name: "wss advertise_addr changes the scheme",
			cfg:  Config{ListenAddr: ":8080", AdvertiseScheme: "wss", AdvertiseHost: "gw.example.com"},
			want: "wss://gw.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientWSBaseURL(tc.cfg, "lb.example.com"); got != tc.want {
				t.Errorf("clientWSBaseURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInstanceDialHost_CollapsesWildcardToLoopback guards the deliberate
// asymmetry with clientWSHost: instances are same-host by design and dial back
// over loopback, so advertise_addr must never reach this path.
func TestInstanceDialHost_CollapsesWildcardToLoopback(t *testing.T) {
	cases := []struct{ listenAddr, want string }{
		{":8080", "127.0.0.1:8080"},
		{"0.0.0.0:9000", "127.0.0.1:9000"},
		{"[::]:9000", "127.0.0.1:9000"},
		{"10.0.0.7:8080", "10.0.0.7:8080"}, // explicit host is preserved
		{"nonsense", "127.0.0.1:8080"},
	}
	for _, tc := range cases {
		if got := instanceDialHost(tc.listenAddr); got != tc.want {
			t.Errorf("instanceDialHost(%q) = %q, want %q", tc.listenAddr, got, tc.want)
		}
	}
}

// TestWarnIfAdvertiseAddrMissing asserts the boot warning fires on exactly the
// configuration shape that produces a guessed ws_url, and stays quiet otherwise —
// a warning that fires on a correct config gets tuned out.
func TestWarnIfAdvertiseAddrMissing(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		wantWarn bool
	}{
		{"wildcard bind with no advertise_addr warns", Config{ListenAddr: ":8080"}, true},
		{"0.0.0.0 bind with no advertise_addr warns", Config{ListenAddr: "0.0.0.0:8080"}, true},
		{"[::] bind with no advertise_addr warns", Config{ListenAddr: "[::]:8080"}, true},
		{"unparseable listen addr warns", Config{ListenAddr: "nonsense"}, true},
		{"explicit listen host is quiet", Config{ListenAddr: "10.0.0.7:8080"}, false},
		{"advertise_addr silences a wildcard bind", Config{ListenAddr: ":8080", AdvertiseHost: "gw.example.com:443"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			warnIfAdvertiseAddrMissing(logger, tc.cfg)

			out := buf.String()
			if tc.wantWarn {
				if !strings.Contains(out, "level=WARN") {
					t.Fatalf("expected a WARN record, got %q", out)
				}
				// The consequence, not just the symptom: a vague warning is ignored.
				for _, want := range []string{"advertise_addr", "Host header", "proxy"} {
					if !strings.Contains(out, want) {
						t.Errorf("warning %q does not mention %q", out, want)
					}
				}
			} else if out != "" {
				t.Errorf("expected no warning, got %q", out)
			}
		})
	}
}

// TestClaim_AdvertiseAddrDrivesReturnedWSURL is the end-to-end proof: the ws_url
// in a real claim response names the advertised address, not the request Host the
// claim arrived on (which for an httptest server is 127.0.0.1:<random>).
func TestClaim_AdvertiseAddrDrivesReturnedWSURL(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte("listen_addr: \":8080\"\nadvertise_addr: \"wss://broker-1.example.com\"\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	cfg.Binaries = testBinaryRegistry("/bin/nexus")
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(7100)}
	ts, reg, _ := newClaimTestServer(t, runner, cfg)

	respCh := make(chan *http.Response, 1)
	go func() { respCh <- postClaim(t, ts.URL, `{"config":"engine: {}\n"}`) }()

	var spec spawnSpec
	select {
	case spec = <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner.start was never called")
	}
	// The instance still dials back over loopback: advertise_addr is client-facing
	// only and must not leak into the spawn env.
	if spec.brokerAddr != "ws://127.0.0.1:8080/instance" {
		t.Errorf("brokerAddr = %q, want the loopback dial-back address", spec.brokerAddr)
	}

	reg.MarkReady(spec.leaseID)
	reg.MarkSessionID(spec.leaseID, "engine-sess-adv")

	resp := <-respCh
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var cr claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := "wss://broker-1.example.com" + ClientWSPath(spec.leaseID)
	if cr.WSURL != want {
		t.Errorf("ws_url = %q, want %q", cr.WSURL, want)
	}
}

// jsonString quotes s as a JSON string literal for embedding in a request body.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestLogBinaryRegistry_NamesTheSpawnEnvironment pins the boot log's half of this
// contract: one line per registry entry naming EXACTLY the variables that entry's
// spawns will carry, and never a value.
//
// The names-only assertion is the load-bearing one. Half of what this line lists
// is credentials by construction — an operator declares ANTHROPIC_API_KEY here
// precisely because it is a secret — so a boot log that printed values would
// leak them into whatever collects the broker's stderr.
func TestLogBinaryRegistry_NamesTheSpawnEnvironment(t *testing.T) {
	t.Setenv("HOME", "/home/broker")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("TZ", "UTC")
	t.Setenv("LANG", "C")
	t.Setenv("ANTHROPIC_API_KEY", "super-secret-value")

	cfg := DefaultConfig()
	cfg.InheritEnv = []string{"ANTHROPIC_API_KEY", "NEXUS_BROKER_TEST_NEVER_SET"}
	cfg.Binaries = map[string]BinaryEntry{
		reservedBinaryName: {Path: "nexus", ResolvedPath: "/usr/local/bin/nexus"},
		"vision": {
			Path:         "/opt/builds/nexus-vision",
			ResolvedPath: "/opt/builds/nexus-vision",
			Env:          map[string]string{"NEXUS_VISION": "1"},
		},
	}

	var buf bytes.Buffer
	logBinaryRegistry(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})), cfg)
	records := decodeLogRecords(t, buf.Bytes())

	// Two entries plus the one WARN for the declared name this broker does not hold.
	if len(records) != 3 {
		t.Fatalf("got %d records, want 2 entry lines + 1 missing-name warning:\n%s", len(records), buf.String())
	}

	base := "ANTHROPIC_API_KEY,HOME,LANG," +
		brokerframe.EnvBrokerAddr + "," + brokerframe.EnvLeaseID + "," + brokerframe.EnvSpawnSecret
	want := map[string]string{
		// Sorted, so the entry's own NEXUS_VISION lands between the broker-owned
		// names and PATH.
		reservedBinaryName: base + ",PATH,TZ",
		"vision":           base + ",NEXUS_VISION,PATH,TZ",
	}
	for _, record := range records[:2] {
		name, _ := record["name"].(string)
		if got := record["spawn_env"]; got != want[name] {
			t.Errorf("entry %q spawn_env = %v, want %q", name, got, want[name])
		}
	}
	if got := records[2]["missing"]; got != "NEXUS_BROKER_TEST_NEVER_SET" {
		t.Errorf("warning missing = %v, want the declared name this broker does not hold", got)
	}

	// Never a value. The whole line is searched, not just the spawn_env field, so
	// a future attribute cannot reintroduce the leak.
	if strings.Contains(buf.String(), "super-secret-value") {
		t.Errorf("the boot log printed an environment VALUE, not just a name:\n%s", buf.String())
	}
}

// TestBuildCommand_NoRunAsLeavesTheSpawnUnchanged is the compatibility half of
// run_as: a broker that does not configure it must build the same *exec.Cmd it
// built before the key existed.
//
// SysProcAttr is asserted to be nil rather than "has no Credential" on purpose.
// A non-nil SysProcAttr changes which path os/exec takes for every spawn, so
// leaving one behind would be a behaviour change even with every field zero.
func TestBuildCommand_NoRunAsLeavesTheSpawnUnchanged(t *testing.T) {
	cmd := buildCommand(spawnSpec{
		binaryPath: "/opt/nexus/bin/nexus",
		configPath: "/tmp/claim-123.yaml",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
	})

	if cmd.SysProcAttr != nil {
		t.Fatalf("SysProcAttr = %+v, want nil when no run_as is configured", cmd.SysProcAttr)
	}
}

// TestBuildCommand_RunAsSetsCredentialAndHome pins both halves of the credential
// drop, because either alone is broken.
//
// The Credential is what makes the process boundary a privilege boundary. HOME is
// what makes the instance able to work at all once it is over that boundary: it
// resolves ~/.nexus (pkg/engine/paths.go), so an instance running as another uid
// while still pointed at the BROKER'S home cannot create its session directory
// and every claim fails at the first write.
func TestBuildCommand_RunAsSetsCredentialAndHome(t *testing.T) {
	t.Setenv("HOME", "/home/broker")

	uid, gid := 1002, 1003
	cmd := buildCommand(spawnSpec{
		binaryPath: "/opt/builds/nexus-vision",
		configPath: "/tmp/claim-123.yaml",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
		runAs:      &RunAsSpec{UID: &uid, GID: &gid, ResolvedHome: "/home/nexus-vision"},
	})

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatalf("SysProcAttr = %+v, want a Credential", cmd.SysProcAttr)
	}
	if got := cmd.SysProcAttr.Credential.Uid; got != uint32(uid) {
		t.Errorf("Credential.Uid = %d, want %d", got, uid)
	}
	if got := cmd.SysProcAttr.Credential.Gid; got != uint32(gid) {
		t.Errorf("Credential.Gid = %d, want %d", got, gid)
	}
	// Left nil with NoSetGroups false, so the child calls setgroups(0, NULL) and
	// drops the broker's supplementary groups. Keeping them would leave the
	// instance holding most of what the key exists to take away.
	if groups := cmd.SysProcAttr.Credential.Groups; len(groups) != 0 {
		t.Errorf("Credential.Groups = %v, want none (supplementary groups are dropped)", groups)
	}
	if cmd.SysProcAttr.Credential.NoSetGroups {
		t.Error("Credential.NoSetGroups is set, so the broker's supplementary groups survive into the instance")
	}

	if got, _ := lastEnvValue(cmd.Env, "HOME"); got != "/home/nexus-vision" {
		t.Errorf("child HOME = %q, want the run_as user's home; the broker's own HOME must not survive the credential drop", got)
	}
}

// TestBuildCommand_EntryEnvHomeBeatsRunAsHome pins the escape hatch: an operator
// who keeps instance state somewhere other than a home directory sets HOME under
// the entry's `env`, and that wins.
//
// It is the same layering `env` already had, and it is why a uid with no passwd
// entry is a boot failure that names this key rather than a dead end.
func TestBuildCommand_EntryEnvHomeBeatsRunAsHome(t *testing.T) {
	t.Setenv("HOME", "/home/broker")

	uid, gid := 1002, 1003
	cmd := buildCommand(spawnSpec{
		binaryPath: "/opt/builds/nexus-vision",
		configPath: "/tmp/claim-123.yaml",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
		binaryEnv:  map[string]string{"HOME": "/var/lib/nexus/vision"},
		runAs:      &RunAsSpec{UID: &uid, GID: &gid, ResolvedHome: "/home/nexus-vision"},
	})

	if got, _ := lastEnvValue(cmd.Env, "HOME"); got != "/var/lib/nexus/vision" {
		t.Errorf("child HOME = %q, want the entry's declared data dir", got)
	}
}

// TestApplyRunAs_ExtendsSysProcAttr guards the seam E4-S2 lands on: that story
// sets Setpgid on the SAME struct, so a credential drop that assigned
// SysProcAttr wholesale would silently undo it (and vice versa).
func TestApplyRunAs_ExtendsSysProcAttr(t *testing.T) {
	cmd := exec.Command("/bin/true")
	// Stand-in for whatever a later story sets on this struct.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	uid, gid := 1002, 1003
	applyRunAs(cmd, &RunAsSpec{UID: &uid, GID: &gid})

	if !cmd.SysProcAttr.Setpgid {
		t.Error("applyRunAs replaced SysProcAttr instead of extending it; a field another story set was dropped")
	}
	if cmd.SysProcAttr.Credential == nil {
		t.Fatal("applyRunAs set no Credential")
	}
}

// TestExecRunner_RunAsWithoutPrivilegeFailsTheSpawn covers the failure an
// operator is most likely to hit: run_as configured on a broker that is not
// privileged to set it.
//
// The point is WHERE it fails. os/exec reports a refused setgroups/setgid/setuid
// synchronously from Start, so the claim gets an error immediately instead of an
// instance that never dials back and is given up on at the ready deadline. The
// message has to name run_as, because "fork/exec: operation not permitted" on its
// own points at the binary rather than at the credential.
//
// Actually DROPPING privilege cannot be tested without root; this asserts the
// refusal path only.
func TestExecRunner_RunAsWithoutPrivilegeFailsTheSpawn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: setting an arbitrary credential would succeed, so there is no refusal to observe")
	}
	binary, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("no `true` binary on PATH to spawn: %v", err)
	}

	// uid 1 (daemon) is not this process, so the drop requires privilege the test
	// does not have.
	uid, gid := 1, 1
	handle, err := execRunner{}.start(context.Background(), spawnSpec{
		binaryPath: binary,
		configPath: "/dev/null",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
		runAs:      &RunAsSpec{UID: &uid, GID: &gid},
	})
	if err == nil {
		_ = handle.kill()
		_ = handle.wait()
		t.Skip("the credential drop unexpectedly succeeded; this host grants it without root")
	}
	if !strings.Contains(err.Error(), "run_as") {
		t.Errorf("error = %v, want it to name run_as so the operator knows which key refused the spawn", err)
	}
	if !strings.Contains(err.Error(), "uid 1 gid 1") {
		t.Errorf("error = %v, want it to name the credential that was refused", err)
	}
}

// TestClaim_HandsTheRunnerTheEntrysRunAs is the wiring: the credential the load
// folded onto an entry is the one the spawn is actually given.
func TestClaim_HandsTheRunnerTheEntrysRunAs(t *testing.T) {
	uid, gid := 1002, 1003
	cfg := variantRegistryConfig()
	vision := cfg.Binaries["vision"]
	vision.RunAs = &RunAsSpec{UID: &uid, GID: &gid, ResolvedHome: "/home/nexus-vision"}
	cfg.Binaries["vision"] = vision

	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(4242)}
	ts, reg, cs := newClaimTestServer(t, runner, cfg)
	cs.sessionReportGrace = 50 * time.Millisecond

	respCh := make(chan *http.Response, 1)
	go func() {
		respCh <- postClaim(t, ts.URL, `{"binary":"vision","config":"engine:\n  name: test\n"}`)
	}()

	spec := awaitSpawn(t, runner)
	if spec.runAs == nil || spec.runAs.UID == nil || *spec.runAs.UID != uid {
		t.Fatalf("spec.runAs = %+v, want the vision entry's credential", spec.runAs)
	}
	if spec.runAs.ResolvedHome != "/home/nexus-vision" {
		t.Errorf("spec.runAs.ResolvedHome = %q, want the home resolved at boot", spec.runAs.ResolvedHome)
	}

	// Let the parked claim finish so teardown does not block on the ready deadline.
	reg.MarkReady(spec.leaseID)
	select {
	case resp := <-respCh:
		resp.Body.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("claim never completed after the instance was marked ready")
	}
}

// TestLogBinaryRegistry_NamesTheRunAsCredential pins the operator's view of who
// an entry's instances will run as, alongside what environment they carry.
func TestLogBinaryRegistry_NamesTheRunAsCredential(t *testing.T) {
	uid, gid := 1002, 1003
	cfg := DefaultConfig()
	cfg.Binaries = map[string]BinaryEntry{
		reservedBinaryName: {
			Path:         "nexus",
			ResolvedPath: "/usr/local/bin/nexus",
			RunAs:        &RunAsSpec{UID: &uid, GID: &gid, ResolvedHome: "/home/nexus"},
		},
	}

	var buf bytes.Buffer
	logBinaryRegistry(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})), cfg)
	records := decodeLogRecords(t, buf.Bytes())
	if len(records) == 0 {
		t.Fatalf("no log records:\n%s", buf.String())
	}
	if got := records[0]["run_as"]; got != "1002:1003" {
		t.Errorf("run_as = %v, want \"1002:1003\"", got)
	}
	if got := records[0]["run_as_home"]; got != "/home/nexus" {
		t.Errorf("run_as_home = %v, want the resolved home", got)
	}
}
