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

	base := ln.Addr().String()

	// Claim a new session; this blocks until the stub signals ready.
	body := `{"config":"engine:\n  name: stub\n"}`
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
	if cr.LeaseID == "" || cr.WSURL == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
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
