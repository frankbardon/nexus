package openaiconform

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"sync"

	"github.com/frankbardon/nexus/pkg/events"
)

//go:embed vectors/*.json
var vectorFS embed.FS

// Surface names one implementation under test. The value is the plugin id, so
// a failure message says which deployment the drift would reach.
type Surface string

// The two surfaces that build Responses bodies and parse Responses replies.
const (
	// SurfaceProvider is plugins/providers/openai — the synchronous provider.
	SurfaceProvider Surface = "nexus.llm.openai"
	// SurfaceBatch is plugins/llm/batch — the batch coordinator's OpenAI
	// adapter.
	SurfaceBatch Surface = "nexus.llm.batch"
)

var allSurfaces = []Surface{SurfaceProvider, SurfaceBatch}

// Surfaces returns every declared surface.
func Surfaces() []Surface {
	out := make([]Surface, len(allSurfaces))
	copy(out, allSurfaces)
	return out
}

// Known reports whether s is a declared surface.
func (s Surface) Known() bool {
	for _, known := range allSurfaces {
		if s == known {
			return true
		}
	}
	return false
}

// Message is one conversation turn, in the subset both surfaces serialize.
//
// It is the corpus's own type rather than events.Message on purpose. The corpus
// is data — JSON documents a reviewer reads without reading Go — and a DTO with
// explicit snake_case tags keeps the vector files legible where the engine
// struct (which carries no json tags and several fields no vector may use)
// would not. LLMRequest converts.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is one assistant tool call, on the way out in a request vector and
// on the way back in a reply expectation.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is one function tool definition.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ResponseFormat is the structured-output request, in the shape
// events.ResponseFormat carries it.
type ResponseFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name,omitempty"`
	Schema map[string]any `json:"schema,omitempty"`
	Strict bool           `json:"strict,omitempty"`
}

// Reasoning is the RESOLVED reasoning configuration for a vector — what each
// surface's own precedence rules already settled on, not the config that
// produced it.
//
// Resolution is out of scope here and deliberately so: the two surfaces reach
// it through different config layers (a provider plugin block plus a role's
// `reasoning:` on one side, the coordinator's block plus the same role's on the
// other), and each already tests its own precedence. What must not diverge is
// what a settled configuration puts on the wire.
type Reasoning struct {
	// Mode is "off" or "effort", spelled as both surfaces spell it. Empty
	// means off.
	Mode string `json:"mode,omitempty"`
	// Effort is the depth dial, or empty for the model's own default.
	Effort string `json:"effort,omitempty"`
	// Summary is the reasoning-summary verbosity, or empty for none.
	Summary string `json:"summary,omitempty"`
}

// Reasoning modes, spelled as both implementations spell them.
const (
	ReasoningModeOff    = "off"
	ReasoningModeEffort = "effort"
)

// On reports whether this configuration puts a `reasoning` object on the wire
// path — which is also what decides whether the sampling-parameter strip runs.
func (r Reasoning) On() bool { return r.Mode == ReasoningModeEffort }

// Vector is one canonical request and the whole body it must produce.
//
// Body is the SHARED expectation: every key both surfaces must write, with the
// exact value. Keys a surface may legitimately add or omit are not silently
// absent from it — they are declared in RequestDivergences, which CheckBody
// enforces in both directions.
type Vector struct {
	// ID names the vector, and names the subtest, so a failure reports the
	// behaviour that drifted rather than a line number.
	ID string `json:"id"`
	// Title is the one-line human summary.
	Title string `json:"title"`
	// Rationale says why the expectation is what it is. It is printed on
	// failure, because the first instinct on a red conformance test is to
	// change the expectation.
	Rationale string `json:"rationale"`

	// Model is the resolved model id, which both surfaces receive already
	// resolved (a param on one, req.Model on the other).
	Model string `json:"model"`
	// MaxTokens is the resolved output cap. Always positive: neither surface
	// is asked what it does with zero, because neither can be reached with
	// one.
	MaxTokens int `json:"max_tokens"`
	// Temperature is the sampling temperature, or nil for unset.
	Temperature *float64 `json:"temperature,omitempty"`
	// Reasoning is the resolved reasoning configuration.
	Reasoning Reasoning `json:"reasoning"`

	Messages       []Message       `json:"messages"`
	Tools          []Tool          `json:"tools,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	// Body is the complete expected request body, minus the declared
	// divergences.
	Body map[string]any `json:"body"`
}

// LLMRequest builds the engine request a surface is handed.
//
// Model and MaxTokens ride the request as well as being vector fields because
// the batch builder reads them off the request while the provider takes them as
// resolved parameters; a driver uses whichever its own entry point wants.
func (v Vector) LLMRequest() events.LLMRequest {
	req := events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Model:         v.Model,
		MaxTokens:     v.MaxTokens,
		Temperature:   v.Temperature,
	}
	for _, m := range v.Messages {
		msg := events.Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, events.ToolCallRequest{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: tc.Arguments,
			})
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range v.Tools {
		req.Tools = append(req.Tools, events.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}
	if v.ResponseFormat != nil {
		req.ResponseFormat = &events.ResponseFormat{
			Type:   v.ResponseFormat.Type,
			Name:   v.ResponseFormat.Name,
			Schema: v.ResponseFormat.Schema,
			Strict: v.ResponseFormat.Strict,
		}
	}
	return req
}

var (
	vectorsOnce sync.Once
	vectorsVal  []Vector
	vectorsErr  error
)

// Vectors returns the whole request corpus, ordered by id.
//
// A malformed vector panics rather than being skipped: a corpus that silently
// drops a document it could not read is a corpus that silently stops testing.
func Vectors() []Vector {
	vectorsOnce.Do(func() { vectorsVal, vectorsErr = loadVectors() })
	if vectorsErr != nil {
		panic(fmt.Sprintf("openaiconform: %v", vectorsErr))
	}
	out := make([]Vector, len(vectorsVal))
	copy(out, vectorsVal)
	return out
}

func loadVectors() ([]Vector, error) {
	entries, err := fs.ReadDir(vectorFS, "vectors")
	if err != nil {
		return nil, fmt.Errorf("reading the vector directory: %w", err)
	}
	var out []Vector
	seen := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".json" {
			continue
		}
		name := path.Join("vectors", e.Name())
		raw, err := vectorFS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		var v Vector
		// Strict decoding: a typo in a vector is a failure rather than an
		// expectation that silently never runs.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", name, err)
		}
		if err := validateVector(v); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if prev, dup := seen[v.ID]; dup {
			return nil, fmt.Errorf("%s and %s both declare vector id %q", prev, name, v.ID)
		}
		seen[v.ID] = name
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the vector corpus is empty")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// validateVector rejects a vector that could not possibly hold a surface to
// anything — an empty expectation, a missing rationale, a model the body does
// not agree with.
func validateVector(v Vector) error {
	switch {
	case v.ID == "":
		return fmt.Errorf("id is required")
	case v.Title == "":
		return fmt.Errorf("vector %q: title is required", v.ID)
	case v.Rationale == "":
		return fmt.Errorf("vector %q: rationale is required — it is printed on failure, and a failure with no rationale invites weakening the vector", v.ID)
	case v.Model == "":
		return fmt.Errorf("vector %q: model is required", v.ID)
	case v.MaxTokens <= 0:
		return fmt.Errorf("vector %q: max_tokens must be positive", v.ID)
	case len(v.Messages) == 0:
		return fmt.Errorf("vector %q: at least one message is required", v.ID)
	case len(v.Body) == 0:
		return fmt.Errorf("vector %q: body is the expectation; an empty one asserts nothing", v.ID)
	}
	if v.Reasoning.Mode != "" && v.Reasoning.Mode != ReasoningModeOff && v.Reasoning.Mode != ReasoningModeEffort {
		return fmt.Errorf("vector %q: reasoning.mode %q is not %s or %s", v.ID, v.Reasoning.Mode, ReasoningModeOff, ReasoningModeEffort)
	}
	for _, d := range RequestDivergences() {
		if _, ok := v.Body[d.Key]; ok {
			return fmt.Errorf("vector %q: body declares %q, which RequestDivergences says is surface-specific; a divergent key belongs in the divergence list, never in the shared expectation", v.ID, d.Key)
		}
	}
	return nil
}
