package jsonschema

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"schema": map[string]any{"type": "object"},
	}))
	h.AssertSubscribesTo("before:io.output", "llm.response")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"llm.request", "io.output"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
