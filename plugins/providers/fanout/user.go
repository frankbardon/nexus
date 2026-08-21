package fanout

import (
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

// timerHandle is the subset of *time.Timer the user-choice deadline needs. It
// is an interface rather than a concrete *time.Timer so tests can inject a
// timer whose firing they control and whose Stop they can observe.
type timerHandle interface {
	// C returns the channel the deadline fires on.
	C() <-chan time.Time
	// Stop releases the timer's runtime resources.
	Stop() bool
}

// realTimer adapts *time.Timer to timerHandle.
type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }

// newRealTimer is the production timer factory. Tests replace it per-Plugin via
// the newTimer field — there is deliberately no package-level seam, so two
// tests can never race on the same address.
func newRealTimer(d time.Duration) timerHandle { return realTimer{t: time.NewTimer(d)} }

// presentToUser emits a provider.fanout.choose event with all responses and
// waits for the user to pick one via provider.fanout.chosen. On timeout or
// user declining (index -1), falls back to the "all" strategy behavior.
func (p *Plugin) presentToUser(fanoutID string, state *fanoutState, responses []events.LLMResponse) {
	// Build options from responses.
	options := make([]events.ProviderFanoutOption, len(responses))
	for i, r := range responses {
		provider, _ := r.Metadata["_fanout_provider"].(string)
		options[i] = events.ProviderFanoutOption{
			Index:    i,
			Provider: provider,
			Model:    r.Model,
			Content:  r.Content,
			CostUSD:  r.CostUSD,
		}
	}

	// Create a buffered channel for the user's choice.
	choiceCh := make(chan int, 1)

	p.mu.Lock()
	p.pendingChoices[fanoutID] = choiceCh
	p.mu.Unlock()

	// Emit the choice event for IO plugins to render.
	_ = p.bus.Emit("provider.fanout.choose", events.ProviderFanoutChoose{SchemaVersion: events.ProviderFanoutChooseVersion, FanoutID: fanoutID,
		Role:      state.role,
		Responses: options,
	})

	// Wait for user choice or deadline.
	go p.awaitUserChoice(fanoutID, state, responses, choiceCh)
}

// awaitUserChoice waits for the user to pick a response or for the deadline
// to expire, then emits the final response.
func (p *Plugin) awaitUserChoice(fanoutID string, state *fanoutState, responses []events.LLMResponse, choiceCh chan int) {
	var chosenIndex int
	fallback := false

	// cancelDeadline releases the deadline goroutine and its timer as soon as
	// the outcome is settled — a prompt user choice must not leave either alive
	// for the remainder of the deadline.
	cancelDeadline := make(chan struct{})
	deadline := p.deadlineTimer(cancelDeadline)

	select {
	case idx := <-choiceCh:
		if idx < 0 || idx >= len(responses) {
			// User declined or invalid index — fall back to "all" behavior.
			p.logger.Warn("user declined fanout choice, falling back to all strategy",
				"fanout_id", fanoutID,
				"chosen_index", idx,
			)
			fallback = true
		} else {
			chosenIndex = idx
		}
	case <-deadline:
		p.logger.Warn("fanout user choice timed out, falling back to all strategy",
			"fanout_id", fanoutID,
		)
		fallback = true
	}
	close(cancelDeadline)

	// Clean up pending choice.
	p.mu.Lock()
	delete(p.pendingChoices, fanoutID)
	p.mu.Unlock()

	if fallback {
		// Fall back to "all" strategy: first response as primary.
		p.emitFinalResponse(fanoutID, state, responses)
		return
	}

	// Reorder responses so the chosen one is first.
	reordered := make([]events.LLMResponse, 0, len(responses))
	reordered = append(reordered, responses[chosenIndex])
	for i, r := range responses {
		if i != chosenIndex {
			reordered = append(reordered, r)
		}
	}

	p.logger.Info("user selected fanout response",
		"fanout_id", fanoutID,
		"chosen_index", chosenIndex,
		"chosen_model", responses[chosenIndex].Model,
	)

	p.emitFinalResponse(fanoutID, state, reordered)
}

// deadlineTimer returns a channel that is closed once the configured deadline
// elapses. Closing cancel stops the timer and retires the watching goroutine
// without firing, so a settled choice leaks neither.
func (p *Plugin) deadlineTimer(cancel <-chan struct{}) <-chan struct{} {
	newTimer := p.newTimer
	if newTimer == nil {
		newTimer = newRealTimer
	}

	ch := make(chan struct{})
	timer := newTimer(p.cfg.deadline)
	go func() {
		defer timer.Stop()
		select {
		case <-timer.C():
			close(ch)
		case <-cancel:
		}
	}()
	return ch
}
