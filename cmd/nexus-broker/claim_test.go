package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
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

func newClaimTestServer(t *testing.T, runner commandRunner, cfg Config) (*httptest.Server, *Registry, *ClaimServer) {
	t.Helper()
	reg := NewRegistry(testLogger(), 0)
	cs := NewClaimServer(testLogger(), reg, cfg, runner)
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
	cfg := Config{ListenAddr: "127.0.0.1:8080", NexusBinaryPath: "/bin/nexus"}
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
	cfg := Config{ListenAddr: "127.0.0.1:8080", NexusBinaryPath: "/bin/nexus"}
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
	cfg := Config{ListenAddr: "127.0.0.1:8080", NexusBinaryPath: "/bin/nexus"}
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
	cfg := Config{ListenAddr: "127.0.0.1:8080", NexusBinaryPath: "/bin/nexus"}
	ts, _, _ := newClaimTestServer(t, runner, cfg)

	resp := postClaim(t, ts.URL, `{"config":"engine: {}\n"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestClaim_Resume_PassesRecallAndEchoesSessionID(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(5555)}
	cfg := Config{ListenAddr: "127.0.0.1:8080", NexusBinaryPath: "/bin/nexus"}
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

func TestClaim_RejectsEmptyConfig(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1)}
	ts, _, _ := newClaimTestServer(t, runner, Config{ListenAddr: ":8080"})

	resp := postClaim(t, ts.URL, `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// newGuardedClaimTestServer is newClaimTestServer with /claim registered THROUGH
// the auth guard, which is the topology run() uses. It exists because the
// ownership stamp can only be observed on the guarded path: an unguarded handler
// never sees a Principal.
func newGuardedClaimTestServer(t *testing.T, runner commandRunner, cfg Config) (*httptest.Server, *Registry) {
	t.Helper()
	reg := NewRegistry(testLogger(), 0)
	cs := NewClaimServer(testLogger(), reg, cfg, runner)
	mux := http.NewServeMux()
	cs.Register(newAuthGuard(testLogger(), cfg.AuthChain).Guard(mux))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg
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
	cfg.NexusBinaryPath = "/bin/nexus"
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
	cfg.NexusBinaryPath = "/bin/nexus"
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
	cfg.NexusBinaryPath = "/bin/nexus"
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
