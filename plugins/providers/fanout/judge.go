package fanout

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// judgeResponse is the expected JSON structure from the judge LLM call.
type judgeResponse struct {
	ChosenIndex int    `json:"chosen_index"`
	Reason      string `json:"reason"`
}

// judgeResponseSchema is the JSON schema for structured output from the judge.
var judgeResponseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"chosen_index": map[string]any{
			"type":        "integer",
			"description": "Zero-based index of the best response",
		},
		"reason": map[string]any{
			"type":        "string",
			"description": "Brief explanation of why this response was chosen",
		},
	},
	"required":             []any{"chosen_index", "reason"},
	"additionalProperties": false,
}

// buildJudgePrompt constructs the prompt that asks the judge LLM to pick the
// best response from the fanout candidates.
func buildJudgePrompt(responses []events.LLMResponse) string {
	var b strings.Builder
	b.WriteString("You are a response quality judge. Below are multiple LLM responses to the same prompt. ")
	b.WriteString("Evaluate each response for accuracy, completeness, clarity, and helpfulness. ")
	b.WriteString("Choose the single best response.\n\n")

	for i, r := range responses {
		provider, _ := r.Metadata["_fanout_provider"].(string)
		fmt.Fprintf(&b, "--- Response %d (provider: %s, model: %s) ---\n", i, provider, r.Model)
		b.WriteString(r.Content)
		b.WriteString("\n\n")
	}

	b.WriteString("Return your choice as JSON with chosen_index (zero-based) and reason.")
	return b.String()
}

// parseJudgeResponse parses the judge LLM's JSON output into a judgeResponse.
func parseJudgeResponse(content string, numResponses int) (judgeResponse, error) {
	// Trim whitespace and try to extract JSON if wrapped in markdown fences.
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		// Strip first and last lines (fences).
		if len(lines) >= 3 {
			content = strings.Join(lines[1:len(lines)-1], "\n")
			content = strings.TrimSpace(content)
		}
	}

	var jr judgeResponse
	if err := json.Unmarshal([]byte(content), &jr); err != nil {
		return judgeResponse{}, fmt.Errorf("parse judge JSON: %w", err)
	}

	if jr.ChosenIndex < 0 || jr.ChosenIndex >= numResponses {
		return judgeResponse{}, fmt.Errorf("chosen_index %d out of range [0, %d)", jr.ChosenIndex, numResponses)
	}

	return jr, nil
}

// selectByJudge makes an LLM call to pick the best response from the fanout
// candidates, then emits the final combined response with the chosen response
// as primary. Falls back to "all" strategy (first response = primary) on
// any failure.
func (p *Plugin) selectByJudge(fanoutID string, state *fanoutState, responses []events.LLMResponse, origRequest events.LLMRequest) {
	prompt := buildJudgePrompt(responses)

	// Compute judge deadline: min(deadline/2, 10s).
	judgeDeadline := p.cfg.deadline / 2
	if judgeDeadline > 10*time.Second {
		judgeDeadline = 10 * time.Second
	}
	if judgeDeadline < 2*time.Second {
		judgeDeadline = 2 * time.Second
	}

	// Channel to receive the judge response.
	judgeCh := make(chan events.LLMResponse, 1)

	// One-shot subscription for the judge response. Filter by _fanout_judge
	// metadata. The handleResponse method already skips these, so this sub
	// is the only consumer.
	unsub := p.bus.Subscribe("llm.response", func(event engine.Event[any]) {
		resp, ok := event.Payload.(events.LLMResponse)
		if !ok {
			return
		}
		if _, isJudge := resp.Metadata["_fanout_judge"]; !isJudge {
			return
		}
		select {
		case judgeCh <- resp:
		default:
		}
	}, engine.WithPriority(0), engine.WithSource(pluginID))
	defer unsub()

	// Build and emit the judge LLM request.
	judgeMeta := map[string]any{
		"_source":       pluginID,
		"_fanout_judge": true,
		"_fanout_id":    fanoutID,
	}

	judgeReq := events.LLMRequest{
		Role: p.cfg.JudgeRole,
		Messages: []events.Message{
			{Role: "user", Content: prompt},
		},
		Stream:   false,
		Metadata: judgeMeta,
		ResponseFormat: &events.ResponseFormat{
			Type:   "json_schema",
			Name:   "fanout_judge_selection",
			Schema: judgeResponseSchema,
			Strict: true,
		},
	}

	p.logger.Info("judge selection started",
		"fanout_id", fanoutID,
		"judge_role", p.cfg.JudgeRole,
		"candidates", len(responses),
		"deadline_ms", judgeDeadline.Milliseconds(),
	)

	p.bus.EmitAsync("llm.request", judgeReq)

	// Wait for judge response or timeout.
	timer := time.NewTimer(judgeDeadline)
	defer timer.Stop()

	select {
	case judgeResp := <-judgeCh:
		jr, err := parseJudgeResponse(judgeResp.Content, len(responses))
		if err != nil {
			p.logger.Warn("judge response parse failed, falling back to first response",
				"fanout_id", fanoutID,
				"error", err,
			)
			p.emitFinalResponse(fanoutID, state, responses)
			return
		}

		p.logger.Info("judge selected response",
			"fanout_id", fanoutID,
			"chosen_index", jr.ChosenIndex,
			"reason", jr.Reason,
		)

		// Reorder: chosen response first, then the rest in original order.
		reordered := make([]events.LLMResponse, 0, len(responses))
		reordered = append(reordered, responses[jr.ChosenIndex])
		for i, r := range responses {
			if i != jr.ChosenIndex {
				reordered = append(reordered, r)
			}
		}
		p.emitFinalResponse(fanoutID, state, reordered)

	case <-timer.C:
		p.logger.Warn("judge deadline reached, falling back to first response",
			"fanout_id", fanoutID,
		)
		p.emitFinalResponse(fanoutID, state, responses)
	}
}
