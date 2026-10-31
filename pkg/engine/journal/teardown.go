package journal

// TeardownFilter classifies journal envelopes, in seq order, as belonging to
// a run's teardown or not.
//
// The engine journals its own teardown: core.shutdown, whatever the bus
// drain dispatches, and everything plugins emit from Shutdown. Consumers that
// model a run as "what the session did" — replay's response stash, eval
// promote's generated assertions — must not mistake those trailing envelopes
// for session work.
//
// An envelope is teardown when it falls between a core.shutdown (inclusive)
// and the next core.boot (exclusive) — a recalled run appends to the same
// journal and starts with core.boot — UNLESS it belongs to a turn that was
// still open when core.shutdown fired. A turn that finishes during the drain
// is real work: its llm.response / tool.result / hitl.responded answer a
// request the replayed turn will make again, and its closing agent.turn.end
// is what marks it complete. Once every such turn has ended, the rest of the
// window is teardown. A turn that starts inside the window is teardown too.
//
// Open turns are counted by agent.turn.start / agent.turn.end depth, reset
// at core.boot: a new process has no turns open, whatever an earlier crashed
// run left unmatched.
//
// The zero value is ready to use. Not safe for concurrent use.
type TeardownFilter struct {
	inTeardown bool
	openTurns  int
}

// Observe advances the filter past e and reports whether e is teardown.
// Envelopes must be observed in journal (seq) order.
func (f *TeardownFilter) Observe(e Envelope) bool {
	switch e.Type {
	case "core.boot":
		f.inTeardown = false
		f.openTurns = 0
		return false
	case "core.shutdown":
		f.inTeardown = true
		return true
	case "agent.turn.start":
		if f.inTeardown && f.openTurns == 0 {
			return true
		}
		f.openTurns++
		return false
	case "agent.turn.end":
		if f.openTurns > 0 {
			f.openTurns--
			return false
		}
		return f.inTeardown
	}
	return f.inTeardown && f.openTurns == 0
}
