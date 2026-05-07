package realtime

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo(
		"llm.stream.chunk", "llm.stream.end",
		"before:tool.invoke", "voice.audio.output.chunk",
		"cancel.complete", "hitl.requested",
	)
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"io.input", "before:io.input", "voice.audio.input.chunk",
		"cancel.request", "hitl.responded",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
