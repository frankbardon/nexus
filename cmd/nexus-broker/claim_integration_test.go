//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// TestClaimSpawnProxyRoundTrip proves the full E1-S4 spine deterministically:
// POST /claim spawns a stub instance, the instance dials back + registers +
// signals ready, the claim returns {lease_id, ws_url}, and a client connecting
// to ws_url exchanges an IO frame with the instance through the gateway. It
// uses a stub instance (testdata/stubinstance) instead of the real nexus
// binary, so it needs no LLM API key and makes no network calls.
func TestClaimSpawnProxyRoundTrip(t *testing.T) {
	stubBin := buildStubInstance(t)
	base := startStubBroker(t, stubBin)

	// Claim a new session; this blocks until the stub signals ready.
	cr := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if cr.LeaseID == "" || cr.WSURL == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	// A new session reports the engine-generated id back to the caller.
	if cr.SessionID != "stub-new-session" {
		t.Fatalf("new-session claim session_id = %q, want %q", cr.SessionID, "stub-new-session")
	}

	// Connect a client to the broker's per-lease endpoint and round-trip a
	// frame through the spawned instance (which echoes it back).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, cr.WSURL, nil)
	if err != nil {
		t.Fatalf("dial ws_url %s: %v", cr.WSURL, err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	out, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: cr.LeaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"hello":"world"}`),
	})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := client.Write(ctx, websocket.MessageText, out); err != nil {
		t.Fatalf("client write: %v", err)
	}

	_, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	echo, err := brokerframe.Decode(data)
	if err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if echo.Signal != brokerframe.SignalIO || string(echo.Payload) != `{"hello":"world"}` {
		t.Fatalf("unexpected echo frame: %+v", echo)
	}
}

// TestClaimResumePassesRecall proves the resume path deterministically: a claim
// carrying session_id spawns the stub with -recall <id>. The stub reports the
// recalled id back as its session id, so the claim response echoing that id is
// proof the broker passed -recall (no LLM, no real engine).
func TestClaimResumePassesRecall(t *testing.T) {
	stubBin := buildStubInstance(t)
	base := startStubBroker(t, stubBin)

	const priorSession = "prior-session-xyz"
	cr := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n","session_id":"`+priorSession+`"}`)
	if cr.LeaseID == "" || cr.WSURL == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	// The stub reports back exactly the id it was told to -recall; an echo of
	// the requested id proves the broker spawned it with -recall <id>.
	if cr.SessionID != priorSession {
		t.Fatalf("resume claim session_id = %q, want %q (proves -recall was passed)", cr.SessionID, priorSession)
	}
}

// startStubBroker binds a real listener, wires the gateway + claim handler over
// it pointing nexus_binary_path at the stub, serves it, and returns the broker's
// base host:port. The server is torn down via t.Cleanup.
func startStubBroker(t *testing.T, stubBin string) string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(logger)
	gateway := NewGateway(logger, registry)

	// Bind a real listener first so we know the broker's address before wiring
	// the claim handler (it needs it to build the instance dial-back URL).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := Config{ListenAddr: ln.Addr().String(), NexusBinaryPath: stubBin}
	claims := NewClaimServer(logger, registry, cfg, execRunner{})
	claims.readyTimeout = 15 * time.Second

	mux := http.NewServeMux()
	gateway.Register(mux)
	claims.Register(mux)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		gateway.Shutdown()
	})
	return ln.Addr().String()
}

// postClaimJSON posts a claim body to the broker and returns the decoded
// success response, failing the test on any non-200 status.
func postClaimJSON(t *testing.T, base, body string) claimResponse {
	t.Helper()
	resp, err := http.Post("http://"+base+"/claim", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /claim: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d, want 200", resp.StatusCode)
	}
	var cr claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	return cr
}

// buildStubInstance compiles the testdata stub instance to a temp binary and
// returns its path.
func buildStubInstance(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stubinstance")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/stubinstance")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub instance: %v\n%s", err, out)
	}
	return bin
}
