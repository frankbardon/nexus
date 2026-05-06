package engine

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/events"
)

// pluginPathFragment is the substring that marks a stack frame as originating
// in plugin code. The recovery wrapper only swallows panics when the deepest
// non-runtime frame contains this fragment — engine-package panics re-raise
// because they indicate engine bugs, which must crash loudly rather than be
// masked by the plugin recovery shield.
const pluginPathFragment = "/plugins/"

// enginePathFragment marks frames originating in the engine itself. Used to
// skip past recovery-wrapper frames when locating the panic's true origin.
const enginePathFragment = "/pkg/engine/"

// dispatchInError is a per-goroutine flag set while a core.error emit is in
// flight. The recovery wrapper consults it: when a handler subscribed to
// core.error itself panics, the panic is logged and dropped — never re-emitted
// as another core.error — so a buggy observer cannot trigger an infinite
// fault loop. sync.Map keyed by goroutine ID lets the flag survive the
// nested invokeHandler calls without leaking goroutine state across runs.
var dispatchInError sync.Map // map[int64]struct{}

// originClassifier inspects a captured stack and reports whether the panic
// originated in plugin code (recover) or engine code (re-panic). Stored as
// a package variable so tests can stub out the production check —
// pkg/engine tests cannot naturally produce stacks containing "/plugins/"
// since their handlers live in this package, so a per-test override lets
// the recovery rules be exercised end-to-end without spinning up a real
// plugin tree.
var originClassifier = panicFromPlugin

// busLogger returns the bus's logger if available, else a discard logger.
// Buses constructed via NewEventBus have no logger by default; the engine
// installs one via SetLogger after construction.
func (b *eventBus) busLogger() *slog.Logger {
	if b.logger != nil {
		return b.logger
	}
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// invokeHandler runs a single handler with panic recovery applied per the
// stack-frame origin rule documented at pluginPathFragment. The boolean
// return reports whether the handler completed without a recovered panic;
// for vetoable events the caller uses this to translate panics into vetoes.
//
// failFast bypasses recovery entirely so test harnesses can surface panics
// with their original stack trace.
func (b *eventBus) invokeHandler(sub *subscription, event Event[any], vetoable bool) (ok bool) {
	if b.failFast {
		sub.handler(event)
		return true
	}

	defer func() {
		r := recover()
		if r == nil {
			ok = true
			return
		}

		stack := debug.Stack()

		// Stack-frame origin rule: a panic from pkg/engine/... is an engine
		// bug and must crash. Only panics that bubble up from plugins/... are
		// caught and emitted as core.error. Walking runtime.Callers from the
		// deferred function gives us the panic's path; we look for the first
		// frame outside this recovery wrapper to classify it.
		if !originClassifier(stack) {
			panic(r)
		}

		ok = false
		pluginID := sub.source
		if pluginID == "" {
			pluginID = "unknown"
		}
		eventType := event.Type
		reason := fmt.Sprint(r)

		b.busLogger().Error("plugin handler panic recovered",
			"plugin", pluginID,
			"event", eventType,
			"panic", reason,
			"stack", string(stack))

		// Vetoable events: translate the panic into a fail-closed veto so
		// the action is blocked rather than silently allowed through.
		if vetoable {
			if vp, vok := event.Payload.(*VetoablePayload); vok {
				vp.Veto = VetoResult{
					Vetoed: true,
					Reason: fmt.Sprintf("plugin panic: %s", reason),
				}
			}
		}

		// Recursion guard: a handler subscribed to core.error that itself
		// panics must not re-trigger another core.error emit. The flag is
		// set for the duration of the core.error emit below; checked first
		// here so a panic from inside core.error dispatch is swallowed
		// without a follow-up emit.
		gid := goroutineID()
		if _, alreadyInError := dispatchInError.Load(gid); alreadyInError {
			b.busLogger().Error("panic during core.error dispatch — dropping to break recursion",
				"plugin", pluginID,
				"event", eventType,
				"panic", reason)
			return
		}

		errPayload := events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Source: pluginID,
			Err:       fmt.Errorf("plugin panic: %s", reason),
			EventType: eventType,
			Stack:     string(stack),
		}
		dispatchInError.Store(gid, struct{}{})
		defer dispatchInError.Delete(gid)
		// Use the standard EmitEvent path; the recursion flag above keeps
		// any panic in a core.error handler from looping back here.
		_ = b.Emit("core.error", errPayload)
	}()

	sub.handler(event)
	return true
}

// panicFromPlugin reports whether a captured stack trace originated in
// plugin code rather than engine code. The deepest non-runtime frame
// determines origin: a panic from plugins/foo/bar.go is recoverable; a
// panic from pkg/engine/somewhere.go re-panics so engine bugs crash
// loudly rather than hiding behind the plugin shield.
//
// Walks debug.Stack output (which is human-formatted but stable enough)
// and inspects each "\tpath:line" frame line. The first frame whose path
// contains "/plugins/" wins; if none does, the origin is treated as
// engine code.
func panicFromPlugin(stack []byte) bool {
	// debug.Stack alternates "<func>(...)" lines with "\t<file>:<line> <off>"
	// lines. We only care about the file lines and only about the topmost
	// non-runtime frame — that is the panic source after the runtime
	// machinery is unwound.
	lines := strings.Split(string(stack), "\n")
	for _, line := range lines {
		// File lines start with a tab; runtime frames live in GOROOT/src/runtime
		// and are filtered so the first user/library frame governs the rule.
		if !strings.HasPrefix(line, "\t") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "runtime/") || strings.Contains(trimmed, "/src/runtime/") {
			continue
		}
		// Skip the recovery wrapper itself — the very first non-runtime
		// frame on the stack is bus_recovery.go's defer, which is not the
		// origin of the panic.
		if strings.Contains(trimmed, "/pkg/engine/bus_recovery.go") {
			continue
		}
		if strings.Contains(trimmed, pluginPathFragment) {
			return true
		}
		if strings.Contains(trimmed, enginePathFragment) {
			return false
		}
		// First non-runtime, non-plugin, non-engine frame: likely test
		// scaffolding or third-party. Treat as engine-adjacent (re-panic)
		// so an in-tree non-plugin bug surfaces.
		return false
	}
	return false
}
