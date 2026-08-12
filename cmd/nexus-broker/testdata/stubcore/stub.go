// Package stubcore holds the entire behaviour of the broker integration
// suite's fake nexus instance: the dial-back handshake, the echo loop, the
// failure-injection switches, and the self-report a test uses to find out what
// the broker actually exec()d.
//
// It exists so that the suite can point the broker's `binaries:` registry at
// TWO separately linked executables — testdata/stubinstance and
// testdata/stubvariant — without forking the protocol logic. Duplicating the
// stub's source was the obvious alternative and was rejected: the property the
// two-binary test proves is "the broker exec()d the entry it was asked for",
// and a second copy of the handshake would quietly turn every future protocol
// fix into a two-place edit whose halves can drift.
//
// Each main package supplies its own compile-time Variant, so the identity a
// running stub reports is decided at LINK time. That is what makes the
// assertion meaningful: nothing in a broker config, a claim body, or a registry
// entry's args/env can talk one binary into impersonating the other, so a test
// that sees VariantAlt come back has proof that the second executable is the
// one that ran.
//
// It lives under testdata/ so `go build ./...`, `go vet ./...` and staticcheck
// all skip it; the integration test builds the two mains on demand.
package stubcore

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// Variant identifiers, one per stub main package. They are declared here rather
// than in the mains so the test asserts against the same constants the binaries
// were linked with — a renamed variant then breaks the build instead of turning
// an assertion into a comparison of two stale string literals.
const (
	// VariantBase is testdata/stubinstance, the stub every pre-existing test
	// spawns. Its value is load-bearing: NewSessionID(VariantBase) has to keep
	// producing "stub-new-session", the id those tests already assert on.
	VariantBase = "stub"

	// VariantAlt is testdata/stubvariant, the second executable. It exists only
	// to be told apart from VariantBase at runtime.
	VariantAlt = "stubvariant"
)

// NewSessionID is the deterministic id a stub reports for a FRESH session (no
// -recall). It is variant-scoped so the claim response alone identifies which
// executable answered, which keeps the cheapest possible assertion — comparing
// claimResponse.SessionID — sufficient for the "which binary ran" tests. The
// resume path deliberately does not carry the variant: there the id under test
// is the one the broker passed in.
func NewSessionID(variant string) string { return variant + "-new-session" }

// EnvReportPrefix selects which environment variables a stub will disclose in
// its report. Reporting the whole environment would be simpler and is not done
// on purpose: the child's environment carries the broker's spawn secret, and a
// fixture that ships secrets over a socket is a bad pattern to leave lying
// around in a repo even when the only listener is the test that spawned it.
const EnvReportPrefix = "STUB_ENV_"

// ReportRequest is the IO payload that asks a stub to describe itself instead
// of echoing. The default IO behaviour is a verbatim echo, so the marker only
// has to be something no other test would send.
//
// It is a quoted JSON STRING, not a bare token, because brokerframe carries the
// payload as json.RawMessage and refuses to encode anything that is not valid
// JSON — a plain `__stub_report__` fails at the sender, before the stub is ever
// asked.
const ReportRequest = `"__stub_report__"`

// Report is what a stub answers ReportRequest with: everything the test needs
// to prove which executable the broker chose and what it handed that
// executable. It travels as the JSON payload of an ordinary IO frame, so it
// rides the same claim → ws_url → gateway → instance path a real client uses
// and proves that path works for the selected binary too.
type Report struct {
	// Variant is the linked-in identity — the answer to "which binary ran".
	Variant string `json:"variant"`

	// Args is os.Args[1:] verbatim, BEFORE flag parsing, so the test can assert
	// on argv ORDER (broker flags first, registry-entry args last) and not only
	// on presence.
	Args []string `json:"args"`

	// Env holds every EnvReportPrefix-prefixed variable the process was started
	// with, keyed by full name.
	Env map[string]string `json:"env"`
}

// Run is the stub's whole main(). variant is the caller's compile-time
// identity; it never comes from argv or the environment, because a
// runtime-supplied identity could be forged by the very registry entry the test
// is trying to verify.
//
// Run terminates the process on the failure paths (rather than returning an
// error) because it IS main: the exit codes below are part of the contract the
// broker's claim path asserts on.
func Run(variant string) {
	// argv is captured before flag.Parse consumes it. The report has to show
	// what the broker actually built, not what the flag package left over.
	rawArgs := append([]string(nil), os.Args[1:]...)

	recall := flag.String("recall", "", "session id to resume")
	// The stub also receives -config <path>; accept and ignore it so flag
	// parsing does not fail on the real spawn args.
	_ = flag.String("config", "", "config path (ignored by the stub)")
	// -stub-tag is accepted and ignored purely so a registry entry under test
	// can carry a FLAG-shaped extra argument. Without it the flag package would
	// exit(2) on an unrecognised flag, and the args test would be forced to use
	// positional arguments only — a weaker fixture than the `-profile vision`
	// shape an operator would really configure.
	_ = flag.String("stub-tag", "", "opaque tag (ignored by the stub)")
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
		sessionID = NewSessionID(variant)
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
		report: Report{
			Variant: variant,
			Args:    rawArgs,
			Env:     reportedEnv(),
		},
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

// reportedEnv collects the EnvReportPrefix-prefixed environment the process was
// started with. SplitN(_, 2) rather than Split: a value may legitimately
// contain '=' and truncating it would make an env assertion pass or fail for
// reasons unrelated to the broker.
func reportedEnv() map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 || !strings.HasPrefix(parts[0], EnvReportPrefix) {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}

// session is one dial-back attempt's worth of state.
type session struct {
	addr            string
	leaseID         string
	spawnSecret     string
	sessionID       string
	ignoreShutdown  bool
	crashAfterReady bool
	report          Report
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
			payload := frame.Payload
			if string(payload) == ReportRequest {
				// A malformed report is answered with an empty payload rather
				// than dropped: a client blocked on a read it will never get
				// would surface as a test timeout, which says nothing about
				// what went wrong. json.Marshal of a Report cannot realistically
				// fail, so this is belt-and-braces.
				if encoded, mErr := json.Marshal(s.report); mErr == nil {
					payload = encoded
				} else {
					payload = nil
				}
			}
			_ = write(ctx, conn, brokerframe.Frame{
				LeaseID: s.leaseID,
				Signal:  brokerframe.SignalIO,
				Payload: payload,
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
