package voice

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"asr": map[string]any{"api_key": "sk-mock-not-used"},
		"tts": map[string]any{"api_key": "sk-mock-not-used"},
	}))
	h.AssertSubscribesTo("voice.audio.input.chunk", "llm.response")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"io.input", "voice.audio.output.chunk", "cancel.request"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
