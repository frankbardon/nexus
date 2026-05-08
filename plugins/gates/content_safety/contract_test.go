package contentsafety

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("before:io.output")
	if got := h.Plugin().Emissions(); len(got) != 1 || got[0] != "io.output" {
		t.Errorf("Emissions() = %v, want [io.output]", got)
	}
}
