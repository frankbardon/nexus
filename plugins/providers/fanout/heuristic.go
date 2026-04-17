package fanout

import (
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

// selectByHeuristic scores responses according to the configured heuristic
// preference and returns the index of the winning response.
func (p *Plugin) selectByHeuristic(responses []events.LLMResponse, receivedAt []time.Time) int {
	candidates := makeIndices(len(responses))

	// Filter by finish reason if configured.
	if p.cfg.HeuristicRequireFinish {
		filtered := filterByFinishReason(responses, candidates, "end_turn")
		if len(filtered) > 0 {
			candidates = filtered
		}
		// If all filtered out, fall back to unfiltered set.
	}

	if len(candidates) == 1 {
		return candidates[0]
	}

	switch p.cfg.HeuristicPrefer {
	case "shortest":
		return selectShortest(responses, candidates)
	case "fastest":
		return selectFastest(receivedAt, candidates)
	case "cheapest":
		return selectCheapest(responses, candidates)
	default: // "longest" is the default
		return selectLongest(responses, candidates)
	}
}

// makeIndices returns a slice [0, 1, 2, ..., n-1].
func makeIndices(n int) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	return idx
}

// filterByFinishReason returns indices of responses whose FinishReason matches.
func filterByFinishReason(responses []events.LLMResponse, candidates []int, reason string) []int {
	var out []int
	for _, i := range candidates {
		if responses[i].FinishReason == reason {
			out = append(out, i)
		}
	}
	return out
}

// selectLongest returns the index of the response with the longest Content.
func selectLongest(responses []events.LLMResponse, candidates []int) int {
	best := candidates[0]
	bestLen := len(responses[best].Content)
	for _, i := range candidates[1:] {
		if l := len(responses[i].Content); l > bestLen {
			best = i
			bestLen = l
		}
	}
	return best
}

// selectShortest returns the index of the response with the shortest Content.
func selectShortest(responses []events.LLMResponse, candidates []int) int {
	best := candidates[0]
	bestLen := len(responses[best].Content)
	for _, i := range candidates[1:] {
		if l := len(responses[i].Content); l < bestLen {
			best = i
			bestLen = l
		}
	}
	return best
}

// selectFastest returns the index of the response that arrived first.
func selectFastest(receivedAt []time.Time, candidates []int) int {
	best := candidates[0]
	bestTime := receivedAt[best]
	for _, i := range candidates[1:] {
		if receivedAt[i].Before(bestTime) {
			best = i
			bestTime = receivedAt[i]
		}
	}
	return best
}

// selectCheapest returns the index of the response with the lowest CostUSD.
func selectCheapest(responses []events.LLMResponse, candidates []int) int {
	best := candidates[0]
	bestCost := responses[best].CostUSD
	for _, i := range candidates[1:] {
		if responses[i].CostUSD < bestCost {
			best = i
			bestCost = responses[i].CostUSD
		}
	}
	return best
}
