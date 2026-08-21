package main

import (
	"os"
	"os/signal"
	"syscall"
)

// hupGuard keeps a SIGHUP handler registered for the entire lifetime of this
// package's test binary, in every build configuration, and is never stopped.
//
// This is not belt-and-braces. SIGHUP's default disposition is to TERMINATE the
// process, and Go restores that default the moment the last channel notified for
// a signal is unregistered. So a SIGHUP delivered while no handler is registered
// does not fail a test — it kills the test binary, taking the whole package down
// with a bare `signal: hangup` and no test-level FAIL. That is precisely the
// intermittent `make test-race` / `make test-broker-integration` death this
// guard exists to make impossible rather than merely unlikely.
//
// The reachable window is narrow but real. TestReload_SIGHUPTriggersReload
// raises real SIGHUPs at this process — it has to, because the operator-facing
// contract it pins is the TRIGGER, not reload() itself — and it re-raises on
// every poll iteration, so the last signal may still be in flight when the
// condition reads true. A guard registered by that test cannot close the hole,
// because a per-test guard is unregistered when the test returns and the
// in-flight signal arrives after that. Only a handler installed before the first
// test runs and never stopped can.
//
// Registration happens in init() rather than in a TestMain deliberately: the
// package already has a TestMain (claim_integration_test.go, //go:build
// integration) which links the stub instance binaries, and a second one — even
// in an untagged file — would be a duplicate declaration under
// `-tags integration`. init() in an untagged file runs in BOTH build
// configurations, before TestMain and before any test, which is exactly the
// coverage needed: `make test` / `make test-race` run the untagged set,
// `make test-broker-integration` runs the tagged one, and the signalling test is
// untagged so it is present in both.
//
// The channel is buffered and never drained on purpose. os/signal drops a
// delivery when the buffer is full instead of blocking, so an undrained buffered
// channel is a correct discard sink — the guard's only job is to keep SIGHUP
// "wanted" so the process-killing default stays unreachable.
//
// Nothing in this package relies on SIGHUP reaching its default disposition. The
// other signalling tests (process_test.go, release_test.go) send SIGTERM and
// SIGKILL to child processes, never SIGHUP to this one, and a signal disposition
// set here is not inherited across exec anyway.
//
// Remove this and the failure mode that returns is a process kill, not a test
// failure — which is why it stayed invisible until a job started running -race.
var hupGuard = make(chan os.Signal, 1)

func init() {
	// Never paired with signal.Stop(hupGuard): stopping it is the thing that
	// restores the disposition that terminates the process.
	signal.Notify(hupGuard, syscall.SIGHUP)
}
