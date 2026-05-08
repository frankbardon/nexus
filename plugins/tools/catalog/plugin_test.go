package catalog

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_DeclaresExpectedSubscriptions(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("tool.register", "tool.catalog.query")
}

func TestContract_AdvertisesToolCatalogCapability(t *testing.T) {
	h := contract.NewContract(t, New)
	caps := h.Plugin().Capabilities()
	if len(caps) != 1 || caps[0].Name != "tool.catalog" {
		t.Errorf("Capabilities() = %v, want [tool.catalog]", caps)
	}
}

func TestContract_RegisterAndQueryRoundTrip(t *testing.T) {
	h := contract.NewContract(t, New)

	h.Inject("tool.register", events.ToolDef{Name: "alpha", Description: "alpha tool"})
	h.Inject("tool.register", events.ToolDef{Name: "beta", Description: "beta tool"})

	q := &events.ToolCatalogQuery{}
	h.Inject("tool.catalog.query", q)

	if len(q.Tools) != 2 {
		t.Fatalf("expected 2 tools registered, got %d (%v)", len(q.Tools), q.Tools)
	}
}

func TestContract_ReRegisterReplacesByName(t *testing.T) {
	h := contract.NewContract(t, New)

	h.Inject("tool.register", events.ToolDef{Name: "shell", Description: "v1"})
	h.Inject("tool.register", events.ToolDef{Name: "shell", Description: "v2"})

	q := &events.ToolCatalogQuery{}
	h.Inject("tool.catalog.query", q)

	if len(q.Tools) != 1 {
		t.Fatalf("expected 1 entry after re-register, got %d", len(q.Tools))
	}
	if q.Tools[0].Description != "v2" {
		t.Errorf("expected v2 to win, got %q", q.Tools[0].Description)
	}
}
