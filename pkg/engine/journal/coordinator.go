package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// EventEmitter is the subset of the bus that the coordinator needs. The
// journal package cannot import engine without creating a cycle, so this
// minimal interface is enough.
type EventEmitter interface {
	Emit(eventType string, payload any) error
}

// ReplayController is the subset of engine.ReplayState the coordinator
// drives. Same anti-cycle reason as EventEmitter.
type ReplayController interface {
	SetActive(bool)
	Push(eventType string, payload any)
	Reset()
}

// PayloadConverter optionally re-types a journal-deserialized payload
// (typically map[string]any after JSON round-trip) into whatever struct
// type the live subscribers expect. The journal package does not import
// any plugin-specific types, so callers wire the per-event-type
// conversions in. A nil converter passes the payload through unchanged.
type PayloadConverter func(eventType string, payload any) (any, error)

// CoordinatorOptions tune replay behavior.
type CoordinatorOptions struct {
	// TurnTimeout caps how long the coordinator waits between re-emitting
	// an io.input and observing the next agent.turn.end. Replay aborts
	// with a clear error when exceeded — typically signals a provider
	// short-circuit miss or a stalled handler.
	TurnTimeout time.Duration
	// Logger is optional; nil silently uses slog.Default().
	Logger *slog.Logger
	// PayloadConverter, when set, is applied to every payload before the
	// coordinator emits it on the bus or pushes it onto a stash queue.
	// Engines wire this to a per-event-type type-restoration helper.
	PayloadConverter PayloadConverter
}

// Coordinator drives deterministic replay of a session journal.
//
// At Run(), it scans the journal once to seed the replay state's per-event
// queues with journaled responses (llm.response, tool.result,
// io.ask.response), then re-emits io.input events from the journal in seq
// order. The live agent reacts to each io.input as if it were a fresh
// turn; the side-effecting plugins (providers, tools) check the engine's
// replay flag and pop the next stashed response instead of calling out.
//
// Synchronization between turns is via a transient subscription to
// agent.turn.end. The coordinator does not own the engine's lifecycle —
// the caller must boot the engine before calling Run.
type Coordinator struct {
	journalDir string
	bus        EventEmitter
	state      ReplayController
	logger     *slog.Logger
	turnWait   time.Duration
	convert    PayloadConverter

	turnEndSub func()
	turnEndCh  chan uint64

	// Journaled io.input events to replay, in seq order.
	inputs []Envelope
	// Last journaled agent.turn.end seq, captured at scan time. Used by
	// IsPartialTurn for the Phase 3 crash-resume API.
	lastTurnEnd       uint64
	lastTurnEndOk     bool
	lastSeq           uint64
	hasUnfinishedTurn bool
}

// SubscribingBus is the bus capability the coordinator needs to install
// the agent.turn.end synchronization handler. The engine's bus implements
// it; tests can supply a stub.
type SubscribingBus interface {
	EventEmitter
	Subscribe(eventType string, handler func(eventType string, payload any), priority int) (unsubscribe func())
}

// NewCoordinator builds a coordinator for a session's journal directory.
// Returns ErrEmptyJournal if the journal contains no replayable inputs.
func NewCoordinator(journalDir string, bus EventEmitter, state ReplayController, opts CoordinatorOptions) (*Coordinator, error) {
	if journalDir == "" {
		return nil, fmt.Errorf("coordinator: empty journal dir")
	}
	if bus == nil {
		return nil, fmt.Errorf("coordinator: nil bus")
	}
	if state == nil {
		return nil, fmt.Errorf("coordinator: nil replay state")
	}
	if opts.TurnTimeout <= 0 {
		opts.TurnTimeout = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	c := &Coordinator{
		journalDir: journalDir,
		bus:        bus,
		state:      state,
		logger:     opts.Logger,
		turnWait:   opts.TurnTimeout,
		convert:    opts.PayloadConverter,
		turnEndCh:  make(chan uint64, 16),
	}
	if err := c.scan(); err != nil {
		return nil, err
	}
	return c, nil
}

// scan walks the journal once to seed queues and collect io.input events.
// Also records the partial-turn state so IsPartialTurn can answer without
// a second walk.
func (c *Coordinator) scan() error {
	r, err := Open(c.journalDir)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}

	var lastTurnStart uint64
	var lastTurnStartOk bool

	err = r.Iter(func(e Envelope) bool {
		c.lastSeq = e.Seq
		switch e.Type {
		case "llm.response", "tool.result", "io.ask.response":
			payload := e.Payload
			if c.convert != nil {
				if conv, cerr := c.convert(e.Type, payload); cerr == nil {
					payload = conv
				}
			}
			c.state.Push(e.Type, payload)
		case "io.input":
			// Skip vetoed io.input — those did not actually drive a turn.
			if !e.Vetoed {
				c.inputs = append(c.inputs, e)
			}
		case "agent.turn.start":
			lastTurnStart = e.Seq
			lastTurnStartOk = true
		case "agent.turn.end":
			c.lastTurnEnd = e.Seq
			c.lastTurnEndOk = true
		}
		return true
	})
	if err != nil {
		return err
	}

	// Partial turn: a turn started but never ended.
	if lastTurnStartOk && lastTurnStart > c.lastTurnEnd {
		c.hasUnfinishedTurn = true
	}
	return nil
}

// Inputs returns the io.input envelopes the coordinator will replay.
// Visible for tests.
func (c *Coordinator) Inputs() []Envelope { return c.inputs }

// LastTurnBoundary returns the seq of the most recent agent.turn.end in the
// scanned journal, or (0, false) if none.
func (c *Coordinator) LastTurnBoundary() (uint64, bool) {
	return c.lastTurnEnd, c.lastTurnEndOk
}

// IsPartialTurn reports whether the journal ends mid-turn — an
// agent.turn.start without a matching agent.turn.end. Phase 3 crash-resume
// uses this to decide whether to replay-then-continue or just replay.
func (c *Coordinator) IsPartialTurn() bool { return c.hasUnfinishedTurn }

// LastSeq returns the highest seq seen in the source journal.
func (c *Coordinator) LastSeq() uint64 { return c.lastSeq }

// AttachTurnSync subscribes to agent.turn.end on the supplied bus so Run
// can synchronize re-emitted io.inputs against turn completion. Caller
// supplies a subscribe closure to avoid the journal package importing
// engine. The returned unsubscribe func runs on Run completion.
//
// If unset, Run emits inputs back-to-back without per-turn synchronization
// — useful for tests where the system processes inputs synchronously.
func (c *Coordinator) AttachTurnSync(subscribe func(handler func(seq uint64)) func()) {
	if subscribe == nil {
		return
	}
	c.turnEndSub = subscribe(func(seq uint64) {
		select {
		case c.turnEndCh <- seq:
		default:
		}
	})
}

// Run executes the replay. The engine must already be booted; the caller
// is responsible for tearing it down. Returns when all journaled io.inputs
// have been replayed and their turns have settled, or on first error.
func (c *Coordinator) Run(ctx context.Context) error {
	c.state.SetActive(true)
	defer c.state.SetActive(false)

	if c.turnEndSub != nil {
		defer c.turnEndSub()
	}

	c.logger.Info("journal replay starting",
		"inputs", len(c.inputs),
		"last_seq", c.lastSeq,
		"partial_turn", c.hasUnfinishedTurn)

	for i, in := range c.inputs {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Drain stale turn-end signals from a prior iteration.
		for {
			select {
			case <-c.turnEndCh:
			default:
				goto emit
			}
		}
	emit:
		c.logger.Debug("replay re-emitting io.input",
			"index", i,
			"original_seq", in.Seq)

		payload := in.Payload
		if c.convert != nil {
			if conv, cerr := c.convert("io.input", payload); cerr == nil {
				payload = conv
			} else {
				c.logger.Warn("io.input payload conversion failed; emitting raw",
					"original_seq", in.Seq, "error", cerr)
			}
		}

		if err := c.bus.Emit("io.input", payload); err != nil {
			return fmt.Errorf("re-emitting io.input seq=%d: %w", in.Seq, err)
		}

		if c.turnEndSub == nil {
			continue
		}

		select {
		case <-c.turnEndCh:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.turnWait):
			return fmt.Errorf("replay turn timeout (>%s) at original_seq=%d index=%d",
				c.turnWait, in.Seq, i)
		}
	}

	c.logger.Info("journal replay complete", "inputs_replayed", len(c.inputs))
	return nil
}

// PayloadAs round-trips a journal-decoded payload (map[string]any after
// JSON unmarshal) back into a typed value via JSON. Side-effecting plugins
// use this to re-materialize their stashed responses.
//
// Generic helper rather than per-event-type: the journal package has no
// dependency on plugins/events, so providers call it with their own type.
func PayloadAs[T any](payload any) (T, error) {
	var zero T
	if payload == nil {
		return zero, fmt.Errorf("payload is nil")
	}
	// Fast path: already the right type (writer-side push without round-trip).
	if v, ok := payload.(T); ok {
		return v, nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return zero, fmt.Errorf("marshal payload: %w", err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return zero, fmt.Errorf("unmarshal as %T: %w", out, err)
	}
	return out, nil
}
