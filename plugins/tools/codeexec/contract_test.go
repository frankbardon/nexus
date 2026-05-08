package codeexec

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("tool.invoke", "tool.result", "tool.register",
		"skill.loaded", "skill.deactivate")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"tool.register", "before:tool.invoke", "tool.invoke",
		"before:tool.result", "tool.result",
		"code.exec.request", "code.exec.stdout", "code.exec.result",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
