package main

import (
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
	reg := NewRegistry(testLogger())
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

	// Simulate the instance dialing back and signalling ready.
	reg.MarkReady(spec.leaseID)

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

func TestClaim_RejectsResume(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1)}
	ts, _, _ := newClaimTestServer(t, runner, Config{ListenAddr: ":8080"})

	resp := postClaim(t, ts.URL, `{"config":"engine: {}\n","session_id":"abc"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
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

// jsonString quotes s as a JSON string literal for embedding in a request body.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
