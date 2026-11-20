package events

// Schema-version constants for decision.* payloads. See doc.go.
const (
	DecisionAskVersion = 1
)

// Question types a decision.provider may be asked. They are Nexus names, not
// any one vendor's: an adapter maps them onto its wire vocabulary (Jev's
// System One API calls a yes/no question "noul"; the Vercel gateway calls it
// "boolean").
const (
	// DecisionYesNo asks for the probability that a statement is true.
	DecisionYesNo = "yes_no"
	// DecisionChoice asks the model to pick one key out of Choices.
	DecisionChoice = "choice"
	// DecisionScore asks for a position on the ordered Levels scale.
	DecisionScore = "score"
)

// DecisionAsk puts typed questions about a piece of state to a decision
// model — a classifier that returns calibrated probabilities rather than
// generated text. The asker fires it as a pointer payload on "decision.ask";
// the capability-resolved decision.provider fills Answers / Provider / Error
// in place before Emit returns, the same synchronous fill pattern as
// SearchRequest.
//
// This is deliberately not an LLMRequest: there are no messages, no tool
// calls and no streaming, and the answer is a typed value per question.
type DecisionAsk struct {
	SchemaVersion int `json:"_schema_version"`

	// State is what the questions are about: a string, or any JSON-encodable
	// map or slice.
	State any `json:"state"`
	// Questions maps an asker-chosen key to a question. Answers comes back
	// under the same keys.
	Questions map[string]DecisionQuestion `json:"questions"`
	// Model overrides the provider's configured model id. Optional.
	Model string `json:"model,omitempty"`

	// Populated by the provider handler. Callers treat them as zero on input.
	Answers      map[string]DecisionAnswer `json:"answers,omitempty"`
	ServedModel  string                    `json:"served_model,omitempty"` // model version the provider reported
	InputTokens  int                       `json:"input_tokens,omitempty"`
	OutputTokens int                       `json:"output_tokens,omitempty"`
	Provider     string                    `json:"provider,omitempty"` // plugin ID of the adapter that answered
	Error        string                    `json:"error,omitempty"`    // non-empty when the call failed
}

// DecisionQuestion is one typed question. Which criteria fields apply depends
// on Type.
type DecisionQuestion struct {
	Type         string `json:"type"` // DecisionYesNo | DecisionChoice | DecisionScore
	Instructions string `json:"instructions"`

	// TrueMeans / FalseMeans optionally describe each side of a yes/no question.
	TrueMeans  string `json:"true_means,omitempty"`
	FalseMeans string `json:"false_means,omitempty"`
	// Choices maps each option key to its description (choice only).
	Choices map[string]string `json:"choices,omitempty"`
	// Levels lists score levels lowest first, 2–10 entries (score only).
	Levels []string `json:"levels,omitempty"`
}

// DecisionAnswer is the typed answer to one question.
type DecisionAnswer struct {
	Type string `json:"type"`

	// Probability is P(yes) in [0,1] (yes/no only).
	Probability float64 `json:"probability,omitempty"`
	// Choice is the selected key (choice only).
	Choice string `json:"choice,omitempty"`
	// Score is the expected level index, fractional allowed (score only).
	Score float64 `json:"score,omitempty"`
	// Confidence is the provider's confidence in Choice / Score.
	Confidence float64 `json:"confidence,omitempty"`
	// Probabilities is the full distribution over choice keys or score
	// levels, when the provider returns one.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}
