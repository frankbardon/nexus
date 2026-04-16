package matcher

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/frankbardon/nexus/pkg/events"
)

// Pricing constants for the model configured in config.yaml. Rates
// are USD per million tokens, which is how Anthropic publishes
// their pricing. These MUST be updated when:
//   - the config.yaml model ID changes,
//   - Anthropic publishes new rates for the same model,
//   - a new pricing tier (caching, batch, etc.) is introduced.
//
// Kept as plain constants rather than config-driven because this
// is a PoC with one model and two numbers; YAML-driven pricing
// would be more machinery than value. Pricing is not a moving
// target the customer can edit, so baking it into the binary on
// every build is the right tradeoff.
//
// Current: claude-sonnet-4-6 pricing as of the session this was
// written. The official source is https://www.anthropic.com/pricing
// — check there before release and before any config.yaml model
// change.
const (
	pricePerMillionInputTokens  = 3.0
	pricePerMillionOutputTokens = 15.0
)

// calculateCost converts an Anthropic Usage record into a Cost
// struct for display. Latency is passed in separately because the
// Usage struct only knows about tokens, not wall-clock time —
// latency is measured at the bus round-trip boundary in runMatch.
func calculateCost(usage events.Usage, latencyMs int64) Cost {
	inputCost := float64(usage.PromptTokens) / 1_000_000 * pricePerMillionInputTokens
	outputCost := float64(usage.CompletionTokens) / 1_000_000 * pricePerMillionOutputTokens
	return Cost{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		USD:              inputCost + outputCost,
		LatencyMs:        latencyMs,
	}
}

// This file owns the ranking prompt and the LLM-response decoding.
// It is isolated from plugin.go on purpose: the plugin handler is
// concerned with bus orchestration (subscribe/emit/correlate),
// whereas the ranker is concerned with prompt authoring and JSON
// shape validation. The split makes both halves easier to test in
// isolation once tests exist.

// rankSystemPrompt is the Nexus-agnostic instruction the model
// receives. It is kept separate from the per-request user message so
// the model's role-based caching can reuse the system prompt across
// requests (Anthropic caches system prompts more aggressively than
// user prompts). The phrasing is deliberately strict on output
// format because loose instructions produce loose JSON and the
// ranker has to deterministically map scores back onto candidate
// IDs.
const rankSystemPrompt = `You are a technical recruiter ranking candidates against a job description.

You will be given a job description and a list of candidates, each with an ID, name, title, years of experience, skills, summary, location, and availability. Your task is to rank the candidates and produce a score plus short reasoning for each one.

Output format: respond with ONLY a JSON object, no preamble, no markdown, no code fences. The JSON must match this schema exactly:

{
  "rankings": [
    {
      "candidate_id": "<id from the input>",
      "score": <float between 0.0 and 1.0>,
      "reasoning": "<two or three sentences explaining the score>"
    }
  ]
}

Scoring rubric:
- 0.90-1.00: strong fit on required skills AND experience level AND location/availability
- 0.70-0.89: strong fit on most dimensions with one meaningful gap
- 0.50-0.69: partial match, one or two significant gaps, worth a conversation
- 0.30-0.49: weak match, probably not worth pursuing unless the top picks are unavailable
- 0.00-0.29: poor match, hard blocker on requirements

Rank from highest score to lowest. Include every candidate in the input, even the ones with low scores — the hiring manager wants to see the full list.

Reasoning should be concrete: name the specific skills, location constraints, or experience facts driving the score. Do not be vague. Do not invent facts about the candidate that are not in the input.`

// rankUserMessage builds the per-request user message containing
// the job description and the serialized candidate pool. This is
// the part that changes per call; rankSystemPrompt is fixed.
func rankUserMessage(jobText string, pool []candidateRecord) string {
	var sb strings.Builder

	sb.WriteString("## Job description\n\n")
	sb.WriteString(strings.TrimSpace(jobText))
	sb.WriteString("\n\n## Candidates\n\n")

	for _, c := range pool {
		fmt.Fprintf(&sb, "### %s (id: %s)\n", c.Name, c.ID)
		fmt.Fprintf(&sb, "- Title: %s\n", c.Title)
		fmt.Fprintf(&sb, "- Years of experience: %d\n", c.YearsExperience)
		fmt.Fprintf(&sb, "- Skills: %s\n", strings.Join(c.Skills, ", "))
		fmt.Fprintf(&sb, "- Location: %s\n", c.Location)
		fmt.Fprintf(&sb, "- Availability: %s\n", c.Availability)
		fmt.Fprintf(&sb, "- Summary: %s\n\n", c.Summary)
	}

	sb.WriteString("Rank every candidate above against the job description using the rubric and output format from your instructions.")
	return sb.String()
}

// rankingEntry is the per-candidate object the LLM produces. It is
// intentionally different from Candidate: the LLM only knows about
// IDs, scores, and reasoning — it does not have to (and must not)
// restate the input fields. We merge its output with the canonical
// candidate record on the Go side.
type rankingEntry struct {
	CandidateID string  `json:"candidate_id"`
	Score       float64 `json:"score"`
	Reasoning   string  `json:"reasoning"`
}

// rankingResponse is the top-level JSON schema we told the model to
// produce. Kept as its own type for clarity and because a future
// version may carry additional metadata (model confidence, flags,
// etc.) without touching the ranker handler shape.
type rankingResponse struct {
	Rankings []rankingEntry `json:"rankings"`
}

// parseRanking decodes the LLM's raw content string into ordered
// Candidate results. It is strict about the schema — anything other
// than the exact JSON shape documented in the system prompt produces
// an error — because silently accepting malformed LLM output is the
// fastest way to ship bugs nobody can reproduce.
//
// TopK, if non-zero, truncates the sorted result to that many
// candidates. Ranking is always returned in descending-score order
// regardless of the order the model happened to emit.
func parseRanking(content string, pool []candidateRecord, topK int) ([]Candidate, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("empty ranking response")
	}

	// Tolerate a surrounding markdown code fence in case the model
	// ignores the "no code fences" instruction. This is a single
	// targeted tolerance, not a general markdown parser — if the
	// model gets creative beyond this we want to see the error.
	if strings.HasPrefix(content, "```") {
		content = strings.TrimPrefix(content, "```json")
		content = strings.TrimPrefix(content, "```")
		content = strings.TrimSuffix(content, "```")
		content = strings.TrimSpace(content)
	}

	var resp rankingResponse
	if err := json.Unmarshal([]byte(content), &resp); err != nil {
		return nil, fmt.Errorf("decoding ranking response: %w", err)
	}
	if len(resp.Rankings) == 0 {
		return nil, fmt.Errorf("ranking response contained no entries")
	}

	// Index the pool by ID so we can join the LLM's rankings back
	// onto the canonical candidate records without an O(n*m) loop.
	poolByID := make(map[string]candidateRecord, len(pool))
	for _, c := range pool {
		poolByID[c.ID] = c
	}

	out := make([]Candidate, 0, len(resp.Rankings))
	for _, r := range resp.Rankings {
		rec, ok := poolByID[r.CandidateID]
		if !ok {
			// The model invented a candidate ID. Skip silently
			// rather than failing the whole match — the ranking is
			// still useful even if one entry is garbage — but the
			// skipped entry is visible in logs via the caller.
			continue
		}
		score := r.Score
		if score < 0 {
			score = 0
		} else if score > 1 {
			score = 1
		}
		out = append(out, Candidate{
			ID:        rec.ID,
			Name:      rec.Name,
			Score:     score,
			Reasoning: strings.TrimSpace(r.Reasoning),
		})
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("ranking response matched no candidate IDs from the pool")
	}

	// Enforce descending-score order regardless of what the model
	// emitted. Stable sort so equal scores preserve the model's
	// ordering (which presumably carries some intent).
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})

	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}

	return out, nil
}
