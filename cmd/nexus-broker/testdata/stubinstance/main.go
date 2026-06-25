// Command stubinstance is a tiny stand-in for the real nexus binary used by the
// broker's integration test. It imports pkg/brokerframe, reads the broker
// dial-back env the broker injects at spawn, connects to the broker's instance
// endpoint, registers its lease, signals ready, reports a session id, and then
// echoes any inbound IO frame straight back. This proves the broker's
// claim/spawn/proxy mechanics end to end without booting a real engine or
// requiring an LLM API key.
//
// It mirrors the real engine's resume contract just enough for the test: when
// spawned with -recall <id> it reports that id back as its session id (proving
// the broker passed the recall arg); otherwise it synthesizes a deterministic
// new-session id the broker returns to the caller.
//
// It lives under testdata/ so the normal `go build ./...` ignores it; the
// integration test builds it on demand and points nexus_binary_path at it.
package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// newSessionID is the deterministic id the stub reports for a fresh session
// (no -recall). The integration test asserts the claim response echoes it.
const newSessionID = "stub-new-session"

func main() {
	recall := flag.String("recall", "", "session id to resume")
	// The stub also receives -config <path>; accept and ignore it so flag
	// parsing does not fail on the real spawn args.
	_ = flag.String("config", "", "config path (ignored by the stub)")
	flag.Parse()

	addr := os.Getenv(brokerframe.EnvBrokerAddr)
	leaseID := os.Getenv(brokerframe.EnvLeaseID)
	if addr == "" || leaseID == "" {
		os.Exit(2)
	}

	sessionID := *recall
	if sessionID == "" {
		sessionID = newSessionID
	}

	// When STUB_IGNORE_SHUTDOWN=1 the stub deliberately ignores shutdown
	// frames so the broker's force-kill grace path can be exercised end to
	// end. The default stub exits cleanly on a shutdown frame (graceful path).
	ignoreShutdown := os.Getenv("STUB_IGNORE_SHUTDOWN") == "1"

	ctx := context.Background()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, addr, nil)
	cancel()
	if err != nil {
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	if err := write(ctx, conn, brokerframe.Frame{LeaseID: leaseID, Signal: brokerframe.SignalRegister}); err != nil {
		os.Exit(1)
	}
	if err := write(ctx, conn, brokerframe.Frame{LeaseID: leaseID, Signal: brokerframe.SignalReady}); err != nil {
		os.Exit(1)
	}
	if err := write(ctx, conn, brokerframe.Frame{
		LeaseID:   leaseID,
		Signal:    brokerframe.SignalSessionIDReport,
		SessionID: sessionID,
	}); err != nil {
		os.Exit(1)
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		frame, err := brokerframe.Decode(data)
		if err != nil {
			continue
		}
		switch frame.Signal {
		case brokerframe.SignalIO:
			_ = write(ctx, conn, brokerframe.Frame{
				LeaseID: leaseID,
				Signal:  brokerframe.SignalIO,
				Payload: frame.Payload,
			})
		case brokerframe.SignalShutdown:
			if ignoreShutdown {
				// Simulate a stuck instance: keep the connection open and do
				// not exit, forcing the broker to fall back to a force-kill.
				continue
			}
			return
		}
	}
}

func write(ctx context.Context, conn *websocket.Conn, f brokerframe.Frame) error {
	data, err := brokerframe.Encode(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, data)
}
