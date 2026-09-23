package batch

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/openaiconform"
)

// This file is the batch coordinator's driver for the shared OpenAI Responses
// conformance corpus (pkg/openaiconform).
//
// The serializer in openai_responses.go is the SECOND one in this repository;
// nexus.llm.openai carries the first. The duplication is deliberate and
// recorded in openai_surface.go — the provider's builders are unexported
// methods on its own plugin state, and reaching across would be a
// plugin-to-plugin call. The corpus is what replaces the compile coupling the
// duplication gives up: both surfaces are held to one set of vectors, one set
// of named wire invariants and one explicit list of the fields that may
// legitimately differ.
//
// So: the vectors and the expected bodies are NOT here and must not be moved
// here. A conformance failure means either this surface drifted or the
// expectation is wrong — never that the vector should be relaxed to match what
// this package happens to produce. If this surface legitimately gains a key the
// provider does not have, it goes in openaiconform.RequestDivergences with a
// rationale, where a reviewer sees it.

// conformReasoning maps the corpus's resolved reasoning statement onto this
// coordinator's own type. Resolution — which config layer won — is each
// surface's own business and is covered by each surface's own tests; what must
// not diverge is what a settled configuration puts on the wire.
func conformReasoning(v openaiconform.Vector) openaiReasoning {
	mode := openaiReasoningModeOff
	if v.Reasoning.Mode == openaiconform.ReasoningModeEffort {
		mode = openaiReasoningModeEffort
	}
	return openaiReasoning{
		Mode:    mode,
		Effort:  v.Reasoning.Effort,
		Summary: v.Reasoning.Summary,
	}
}

// The coordinator's Responses request serializer against the shared corpus.
func TestOpenAIResponsesConformance_Request(t *testing.T) {
	openaiconform.RunRequestSuite(t, openaiconform.SurfaceBatch, func(t *testing.T, v openaiconform.Vector) map[string]any {
		t.Helper()
		// The default cap is deliberately a value no vector uses: the corpus
		// pins the RESOLVED cap reaching the body, so a builder that fell
		// back to its default must fail rather than coincide.
		body, err := buildOpenAIResponsesBody(v.LLMRequest(), 1, conformReasoning(v))
		if err != nil {
			t.Fatalf("building the body for vector %s: %v", v.ID, err)
		}
		return body
	})
}

// The coordinator's Responses reply decoder against the shared corpus.
//
// Every fixture is also run past the surface sniffer, because on this side the
// decoder is only reached if the sniffer routes to it: a result line whose
// shape were misread would be decoded by the Chat Completions path and arrive
// as an empty success, which no amount of conformance on the decoder itself
// would catch.
func TestOpenAIResponsesConformance_Reply(t *testing.T) {
	openaiconform.RunReplySuite(t, openaiconform.SurfaceBatch, func(t *testing.T, rv openaiconform.ReplyVector) (*events.LLMResponse, error) {
		t.Helper()
		raw := rv.RawReply()
		if !openaiResultBodyIsResponses(raw) {
			t.Fatalf("the surface sniffer read fixture %s as a Chat Completions reply; it would never reach the Responses decoder", rv.ID)
		}
		return decodeOpenAIResponsesBody(raw)
	})
}
