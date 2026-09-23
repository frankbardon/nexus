package openai

import (
	"bytes"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/openaiconform"
)

// This file is this provider's driver for the shared OpenAI Responses
// conformance corpus (pkg/openaiconform).
//
// The corpus exists because nexus.llm.batch carries a SECOND Responses
// serializer and a second reply parser — deliberately, because the builders
// here are unexported methods on plugin state the coordinator neither has nor
// should have, and reaching across would be the plugin-to-plugin call CLAUDE.md
// forbids. The corpus is the only thing keeping the two from drifting: a wire
// fix applied here and not there surfaces as an HTTP 400 on batched traffic
// only, months later.
//
// So: the vectors and the expected bodies are NOT here, and must not be moved
// here. Everything in this file is the adapter that hands this package's own
// builders one vector at a time. A conformance failure means either this
// surface drifted or the corpus's expectation is wrong — never that the vector
// should be relaxed to match what this package happens to produce.
//
// The per-feature tests in responses_test.go and responses_reply_test.go are
// unaffected and stay: they cover the provider-only halves (multimodal Items,
// prompt decoration, tool_choice, tool filtering, reasoning-Item replay,
// degradation) that the coordinator does not implement and the corpus therefore
// does not carry.

// conformReasoning maps the corpus's resolved reasoning statement onto this
// provider's own type. Resolution — which config layer won — is each surface's
// own business and each surface's own tests; what must not diverge is what a
// settled configuration puts on the wire.
func conformReasoning(v openaiconform.Vector) reasoningConfig {
	mode := reasoningModeOff
	if v.Reasoning.Mode == openaiconform.ReasoningModeEffort {
		mode = reasoningModeEffort
	}
	return reasoningConfig{
		Mode:    mode,
		Effort:  v.Reasoning.Effort,
		Summary: v.Reasoning.Summary,
	}
}

// conformBuild is the request driver. It deliberately uses a plugin with no
// prompt registry: the corpus pins serialization, and prompt decoration is a
// provider-only concern the batch coordinator does not have.
func conformBuild(p *Plugin) openaiconform.BuildFunc {
	return func(t *testing.T, v openaiconform.Vector) map[string]any {
		t.Helper()
		if p.prompts != nil {
			t.Fatal("the conformance driver must build with no prompt registry")
		}
		return p.buildResponsesBodyWith(v.Model, v.MaxTokens, v.LLMRequest(), conformReasoning(v))
	}
}

// The synchronous provider's request serializer against the shared corpus.
func TestResponsesConformance_Request(t *testing.T) {
	openaiconform.RunRequestSuite(t, openaiconform.SurfaceProvider, conformBuild(quietPlugin()))
}

// The same corpus under an Azure auth mode, which is how the "model is always
// present" invariant is asserted where it actually bites.
//
// The chat path strips `model` in Azure modes because the deployment lives in
// the URL; the Azure Responses route is /openai/v1/responses with the
// deployment carried in the body's `model` instead, so inheriting that strip
// here would send a request with no model at all. Running the WHOLE corpus
// under the Azure mode — rather than one bespoke assertion — means every future
// vector gets the guarantee for free.
func TestResponsesConformance_RequestUnderAzure(t *testing.T) {
	for _, mode := range []authMode{authModeAzureKey, authModeAzureAAD} {
		t.Run(string(mode), func(t *testing.T) {
			p := quietPlugin()
			p.auth = &authState{mode: mode}
			openaiconform.RunRequestSuite(t, openaiconform.SurfaceProvider, conformBuild(p))
		})
	}
}

// The reply parser against the shared corpus.
//
// The driver goes through handleResponsesSyncResponse and a real bus rather
// than calling convertResponsesReply directly, because the failed-run check —
// a run that failed inside an HTTP 200 — lives in the handler, and a driver
// that reimplemented that check would pass a corpus the production path no
// longer satisfies.
func TestResponsesConformance_Reply(t *testing.T) {
	openaiconform.RunReplySuite(t, openaiconform.SurfaceProvider, func(t *testing.T, rv openaiconform.ReplyVector) (*events.LLMResponse, error) {
		t.Helper()
		bus := engine.NewEventBus()
		p := quietPlugin()
		p.bus = bus

		var got *events.LLMResponse
		var failed error
		bus.Subscribe("llm.response", func(e engine.Event[any]) {
			if r, ok := e.Payload.(events.LLMResponse); ok {
				resp := r
				got = &resp
			}
		})
		bus.Subscribe("core.error", func(e engine.Event[any]) {
			if info, ok := e.Payload.(events.ErrorInfo); ok {
				failed = info.Err
			}
		})

		p.handleResponsesSyncResponse(bytes.NewReader(rv.RawReply()), "req-conform", nil, nil, reasoningConfig{})

		if failed != nil {
			return nil, failed
		}
		return got, nil
	})
}
