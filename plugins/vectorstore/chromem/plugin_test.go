package chromem

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	subs := h.Plugin().Subscriptions()
	if len(subs) == 0 {
		t.Error("Subscriptions() empty")
	}
	caps := h.Plugin().Capabilities()
	if len(caps) == 0 {
		t.Error("vectorstore should advertise vector.store capability")
	}
}
