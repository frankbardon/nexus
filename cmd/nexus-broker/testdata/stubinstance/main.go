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

	// The spawn secret is echoed in the register frame, exactly as the real
	// nexus.io.broker plugin does. It is read WITHOUT a presence check on
	// purpose: the broker injects it unconditionally but only enforces it when
	// authenticated, and the stub must reproduce the unauthenticated case too.
	spawnSecret := os.Getenv(brokerframe.EnvSpawnSecret)

	sessionID := *recall
	if sessionID == "" {
		sessionID = newSessionID
	}

	// When STUB_IGNORE_SHUTDOWN=1 the stub deliberately ignores shutdown
	// frames so the broker's force-kill grace path can be exercised end to
	// end. The default stub exits cleanly on a shutdown frame (graceful path).
	ignoreShutdown := os.Getenv("STUB_IGNORE_SHUTDOWN") == "1"

	// When STUB_CRASH_AFTER_READY=1 the stub simulates an unexpected crash: on
	// the first inbound IO frame it exits abnormally (non-zero, no graceful
	// shutdown handshake), letting the broker's crash-detection path be
	// exercised on demand. Triggering on an IO frame keeps it deterministic —
	// the crash happens only once a client deliberately pokes this instance, so
	// sibling instances stay untouched.
	crashAfterReady := os.Getenv("STUB_CRASH_AFTER_READY") == "1"

	// When STUB_RECONNECT=1 the stub mirrors the real nexus.io.broker plugin's
	// reconnect loop (plugins/io/broker/server.go): a dropped connection is
	// retried with backoff until a shutdown frame arrives. It is what makes the
	// broker-restart reattach test possible — the default stub exits on the first
	// read error, which would end the process the moment the old broker went away.
	reconnect := os.Getenv("STUB_RECONNECT") == "1"

	sess := session{
		addr:            addr,
		leaseID:         leaseID,
		spawnSecret:     spawnSecret,
		sessionID:       sessionID,
		ignoreShutdown:  ignoreShutdown,
		crashAfterReady: crashAfterReady,
	}

	ctx := context.Background()
	if !reconnect {
		// Exit code semantics predate the reconnect mode and are preserved: a
		// failure BEFORE the handshake completes is exit 1 (the broker's claim path
		// asserts on a pre-ready exit), while a connection that simply ends is exit
		// 0. Several tests tear the broker down under a live stub, and a non-zero
		// exit there would be read as a crash.
		if registered, _ := sess.run(ctx); !registered {
			os.Exit(1)
		}
		return
	}

	// The backoff is fixed and short rather than exponential: the plugin's real
	// ceiling is 5s, and a test that had to wait that long between attempts would
	// be slow for no extra coverage — the property under test is that the instance
	// keeps dialing, not the shape of the curve.
	const (
		retryEvery = 100 * time.Millisecond
		giveUpTime = 60 * time.Second
	)
	deadline := time.Now().Add(giveUpTime)
	for time.Now().Before(deadline) {
		if _, err := sess.run(ctx); err == nil {
			// A clean end means the broker asked for shutdown; do not undo it by
			// reconnecting, exactly as the plugin's shutdown latch does not.
			return
		}
		time.Sleep(retryEvery)
	}
	os.Exit(1)
}

// session is one dial-back attempt's worth of state.
type session struct {
	addr            string
	leaseID         string
	spawnSecret     string
	sessionID       string
	ignoreShutdown  bool
	crashAfterReady bool
}

// run dials the broker, performs the register/ready/session-id handshake, and
// echoes IO frames until the connection drops or a shutdown frame arrives.
//
// registered reports whether the handshake completed, which is what separates
// "this instance never got off the ground" from "it ran and the socket ended".
// err is nil ONLY for a clean, broker-requested shutdown, so a reconnecting stub
// can tell "stop" from "try again".
func (s session) run(ctx context.Context) (registered bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, s.addr, nil)
	cancel()
	if err != nil {
		return false, err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	if err := write(ctx, conn, brokerframe.Frame{
		LeaseID: s.leaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  s.spawnSecret,
	}); err != nil {
		return false, err
	}
	if err := write(ctx, conn, brokerframe.Frame{LeaseID: s.leaseID, Signal: brokerframe.SignalReady}); err != nil {
		return false, err
	}
	if err := write(ctx, conn, brokerframe.Frame{
		LeaseID:   s.leaseID,
		Signal:    brokerframe.SignalSessionIDReport,
		SessionID: s.sessionID,
	}); err != nil {
		return false, err
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return true, err
		}
		frame, err := brokerframe.Decode(data)
		if err != nil {
			continue
		}
		switch frame.Signal {
		case brokerframe.SignalIO:
			if s.crashAfterReady {
				// Simulate an unexpected crash mid-session: exit abnormally
				// without the graceful shutdown handshake.
				os.Exit(7)
			}
			_ = write(ctx, conn, brokerframe.Frame{
				LeaseID: s.leaseID,
				Signal:  brokerframe.SignalIO,
				Payload: frame.Payload,
			})
		case brokerframe.SignalShutdown:
			if s.ignoreShutdown {
				// Simulate a stuck instance: keep the connection open and do
				// not exit, forcing the broker to fall back to a force-kill.
				continue
			}
			return true, nil
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
