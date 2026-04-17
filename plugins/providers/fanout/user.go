package fanout

import (
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

// newTimer is a package-level variable so tests can replace it with a
// fast-firing timer to avoid real delays.
var newTimer = time.NewTimer

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
	_ = p.bus.Emit("provider.fanout.choose", events.ProviderFanoutChoose{
		FanoutID:  fanoutID,
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
	case <-p.deadlineTimer(state):
		p.logger.Warn("fanout user choice timed out, falling back to all strategy",
			"fanout_id", fanoutID,
		)
		fallback = true
	}

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

// deadlineTimer returns a channel that fires after the configured deadline.
func (p *Plugin) deadlineTimer(state *fanoutState) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		timer := newTimer(p.cfg.deadline)
		<-timer.C
		timer.Stop()
		close(ch)
	}()
	return ch
}
