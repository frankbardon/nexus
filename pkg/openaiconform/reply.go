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

//go:embed replies/*.json
var replyFS embed.FS

// Usage is the token accounting both parsers must agree on.
//
// It is the five counters both surfaces map out of the Responses usage block.
// events.Usage's ModalityBreakdown is deliberately not here: it is declared in
// ReplyDivergences as provider-only, and CheckReply enforces its ABSENCE on the
// other surface rather than comparing it.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

// ReplyExpect is what both parsers must produce from one fixture.
type ReplyExpect struct {
	Model        string     `json:"model"`
	Content      string     `json:"content"`
	FinishReason string     `json:"finish_reason"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	Usage        Usage      `json:"usage"`

	// ReasoningItems is how many `reasoning` Items the fixture carries.
	//
	// It is an expectation on the provider surface — where the Items are
	// captured onto Metadata, because the NEXT request of a tool loop must
	// replay them verbatim — and a prohibition on every other, per the
	// Metadata[openai_reasoning_items] entry in ReplyDivergences.
	ReasoningItems int `json:"reasoning_items,omitempty"`
}

// ReplyVector is one raw Responses reply and the LLMResponse it must become.
type ReplyVector struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Rationale string `json:"rationale"`

	// Reply is the reply body verbatim, as bytes, because that is what a
	// parser is actually handed. Keeping it raw means a field renamed in
	// either parser's struct tags fails here.
	Reply json.RawMessage `json:"reply"`

	// Error says the fixture must be reported as a failed run rather than as
	// a successful empty reply: a Responses run can fail inside an HTTP 200,
	// and a coordinator that read that as an empty success would record a
	// finished line with no output.
	Error bool `json:"error,omitempty"`

	Expect ReplyExpect `json:"expect"`
}

// RawReply returns the fixture body as bytes.
func (rv ReplyVector) RawReply() []byte {
	return append([]byte(nil), rv.Reply...)
}

var (
	repliesOnce sync.Once
	repliesVal  []ReplyVector
	repliesErr  error
)

// ReplyVectors returns the whole reply corpus, ordered by id.
func ReplyVectors() []ReplyVector {
	repliesOnce.Do(func() { repliesVal, repliesErr = loadReplies() })
	if repliesErr != nil {
		panic(fmt.Sprintf("openaiconform: %v", repliesErr))
	}
	out := make([]ReplyVector, len(repliesVal))
	copy(out, repliesVal)
	return out
}

func loadReplies() ([]ReplyVector, error) {
	entries, err := fs.ReadDir(replyFS, "replies")
	if err != nil {
		return nil, fmt.Errorf("reading the reply directory: %w", err)
	}
	var out []ReplyVector
	seen := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".json" {
			continue
		}
		name := path.Join("replies", e.Name())
		raw, err := replyFS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		var rv ReplyVector
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rv); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", name, err)
		}
		switch {
		case rv.ID == "":
			return nil, fmt.Errorf("%s: id is required", name)
		case rv.Title == "":
			return nil, fmt.Errorf("%s: title is required", name)
		case rv.Rationale == "":
			return nil, fmt.Errorf("%s: rationale is required", name)
		case len(rv.Reply) == 0:
			return nil, fmt.Errorf("%s: reply is required", name)
		case !rv.Error && rv.Expect.FinishReason == "":
			return nil, fmt.Errorf("%s: a successful fixture must expect a finish reason", name)
		}
		if prev, dup := seen[rv.ID]; dup {
			return nil, fmt.Errorf("%s and %s both declare reply vector id %q", prev, name, rv.ID)
		}
		seen[rv.ID] = name
		out = append(out, rv)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the reply corpus is empty")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// CheckReply compares one surface's parse of a fixture against the expectation.
//
// Both surfaces produce an events.LLMResponse, so this compares the struct
// itself rather than a projection: there is no key set to drift, and the fields
// that legitimately differ are governed by ReplyDivergences in both directions.
//
// Like CheckBody it is pure, and returns every disagreement rather than the
// first.
func CheckReply(rv ReplyVector, surface Surface, resp *events.LLMResponse, err error) []error {
	if !surface.Known() {
		return []error{fmt.Errorf("%q is not a declared surface (want one of %v)", surface, Surfaces())}
	}

	if rv.Error {
		if err == nil {
			return []error{fmt.Errorf("the fixture is a run that FAILED inside an HTTP 200 and this surface reported success (%s). Reading it as an empty successful reply is how a failed line arrives looking finished", render(resp))}
		}
		return nil
	}
	if err != nil {
		return []error{fmt.Errorf("parsing failed: %v", err)}
	}
	if resp == nil {
		return []error{fmt.Errorf("no response was produced")}
	}

	var errs []error
	if resp.SchemaVersion != events.LLMResponseVersion {
		errs = append(errs, fmt.Errorf("`SchemaVersion` = %d, want %d — every producer stamps the current version", resp.SchemaVersion, events.LLMResponseVersion))
	}
	if resp.Model != rv.Expect.Model {
		errs = append(errs, fmt.Errorf("`Model` = %q, want %q", resp.Model, rv.Expect.Model))
	}
	if resp.Content != rv.Expect.Content {
		errs = append(errs, fmt.Errorf("`Content` = %q, want %q", resp.Content, rv.Expect.Content))
	}
	if resp.FinishReason != rv.Expect.FinishReason {
		errs = append(errs, fmt.Errorf("`FinishReason` = %q, want %q. The Responses lifecycle (status + incomplete_details.reason) is TRANSLATED into the vocabulary the chat path already publishes, so flipping `api:` does not change what a consumer reads", resp.FinishReason, rv.Expect.FinishReason))
	}
	errs = append(errs, diffToolCalls(rv.Expect.ToolCalls, resp.ToolCalls)...)
	errs = append(errs, diffUsage(rv.Expect.Usage, resp.Usage)...)
	errs = append(errs, checkReplyDivergences(rv, surface, *resp)...)
	return errs
}

func diffToolCalls(want []ToolCall, got []events.ToolCallRequest) []error {
	if len(want) != len(got) {
		return []error{fmt.Errorf("`ToolCalls` has %d entries, want %d (got %s)", len(got), len(want), render(got))}
	}
	var errs []error
	for i := range want {
		if got[i].ID != want[i].ID {
			errs = append(errs, fmt.Errorf("`ToolCalls[%d].ID` = %q, want %q — call_id is the pairing handle a later function_call_output references, and the Item's own id is only the fallback", i, got[i].ID, want[i].ID))
		}
		if got[i].Name != want[i].Name {
			errs = append(errs, fmt.Errorf("`ToolCalls[%d].Name` = %q, want %q", i, got[i].Name, want[i].Name))
		}
		if got[i].Arguments != want[i].Arguments {
			errs = append(errs, fmt.Errorf("`ToolCalls[%d].Arguments` = %q, want %q", i, got[i].Arguments, want[i].Arguments))
		}
	}
	return errs
}

func diffUsage(want Usage, got events.Usage) []error {
	var errs []error
	cmp := []struct {
		field     string
		want, got int
	}{
		{"PromptTokens", want.PromptTokens, got.PromptTokens},
		{"CompletionTokens", want.CompletionTokens, got.CompletionTokens},
		{"TotalTokens", want.TotalTokens, got.TotalTokens},
		{"CachedTokens", want.CachedTokens, got.CachedTokens},
		{"ReasoningTokens", want.ReasoningTokens, got.ReasoningTokens},
	}
	for _, c := range cmp {
		if c.want != c.got {
			errs = append(errs, fmt.Errorf("`Usage.%s` = %d, want %d — the Responses block names the same counters differently (input/output rather than prompt/completion) and both surfaces must map them the same way", c.field, c.got, c.want))
		}
	}
	return errs
}

// checkReplyDivergences enforces the allowlist in both directions on the reply
// side: a field declared provider-only must be populated there and absent
// everywhere else.
func checkReplyDivergences(rv ReplyVector, surface Surface, resp events.LLMResponse) []error {
	var errs []error
	for _, d := range ReplyDivergences() {
		allowed := d.allows(surface)
		switch d.Key {
		case "Metadata[" + ReasoningItemsMetadataKey + "]":
			n := countReasoningItems(resp.Metadata)
			if allowed {
				if n != rv.Expect.ReasoningItems {
					errs = append(errs, fmt.Errorf("`Metadata[%s]` carries %d Items, want %d. Under `store: false` each Item holds an opaque encrypted_content blob, and the next request of a tool loop must replay every one verbatim or the model starts the round it needed reasoning for without any",
						ReasoningItemsMetadataKey, n, rv.Expect.ReasoningItems))
				}
				continue
			}
			if n != 0 {
				errs = append(errs, divergenceViolation(d, surface, fmt.Sprintf("%d Items", n)))
			}
		case "CostUSD":
			if allowed {
				continue
			}
			if resp.CostUSD != 0 {
				errs = append(errs, divergenceViolation(d, surface, fmt.Sprintf("%v", resp.CostUSD)))
			}
		case "Usage.ModalityBreakdown":
			if allowed {
				continue
			}
			if len(resp.Usage.ModalityBreakdown) != 0 {
				errs = append(errs, divergenceViolation(d, surface, render(resp.Usage.ModalityBreakdown)))
			}
		default:
			errs = append(errs, fmt.Errorf("ReplyDivergences declares %q, which CheckReply does not know how to enforce; a divergence nothing checks is an allowlist entry that allows everything", d.Key))
		}
	}
	return errs
}

func divergenceViolation(d Divergence, surface Surface, got string) error {
	return fmt.Errorf("%s is declared surface-specific to %v and %s populated it anyway (%s).\n      why the divergence exists: %s\n      If this surface now legitimately carries it, widen the Only list in ReplyDivergences",
		d.Key, d.Only, surface, got, d.Why)
}

// countReasoningItems reads the captured Items back out of metadata, accepting
// both shapes: the capture path produces []map[string]any and the same value
// comes back as []any once it has been through JSON, which is every persisted
// and every replayed turn.
func countReasoningItems(meta map[string]any) int {
	if meta == nil {
		return 0
	}
	switch items := meta[ReasoningItemsMetadataKey].(type) {
	case []map[string]any:
		return len(items)
	case []any:
		return len(items)
	default:
		return 0
	}
}
