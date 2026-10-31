package tui

import (
	"context"
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// io/tui owns a terminal so behavioral tests are out of scope for the
// contract harness. Smoke-test the static contract only.
func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New)
	subs := h.Plugin().Subscriptions()
	if len(subs) == 0 {
		t.Error("Subscriptions() empty")
	}
	if emits := h.Plugin().Emissions(); len(emits) == 0 {
		t.Error("Emissions() empty")
	}
}

// A user quit (program exits without Stop) must end the session; an exit
// caused by Stop must not re-announce it.
func TestAdapter_UserQuitEndsSession(t *testing.T) {
	a := NewAdapter(nil, nil)
	calls := 0
	a.OnExit(func() { calls++ })

	a.programExited()
	if calls != 1 {
		t.Fatalf("user quit ran the exit handler %d times, want 1", calls)
	}

	_ = a.Stop(context.Background()) // no program: just marks stopping
	a.programExited()
	if calls != 1 {
		t.Fatal("an exit caused by Stop ran the exit handler")
	}
}
