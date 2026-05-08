package pdf

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	// default_mode: document skips the pdftotext PATH lookup so the test
	// works on CI environments without poppler installed.
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"default_mode": "document",
	}))
	h.AssertSubscribesTo("tool.invoke")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"before:tool.result", "tool.result", "tool.register"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
