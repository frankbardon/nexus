package shell

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("tool.invoke")
	if emits := h.Plugin().Emissions(); len(emits) == 0 {
		t.Error("Emissions() empty")
	}
}

func TestContract_RegistersShellToolOnReady(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertEmitted("tool.register")
}
