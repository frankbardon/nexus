package mock

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_SubscribesToEmbeddingsRequest(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("embeddings.request")
}

func TestContract_FillsRequestPayload(t *testing.T) {
	h := contract.NewContract(t, New)

	req := &events.EmbeddingsRequest{Texts: []string{"foo", "bar"}}
	h.Inject("embeddings.request", req)

	if req.Provider != "nexus.embeddings.mock" {
		t.Errorf("provider = %q, want nexus.embeddings.mock", req.Provider)
	}
	if req.Model == "" {
		t.Error("model not stamped")
	}
	if len(req.Vectors) != 2 {
		t.Fatalf("vectors len = %d, want 2", len(req.Vectors))
	}
	for i, v := range req.Vectors {
		if len(v) == 0 {
			t.Errorf("vector %d empty", i)
		}
	}
}

func TestContract_DeterministicAcrossInstances(t *testing.T) {
	a := contract.NewContract(t, New)
	b := contract.NewContract(t, New)

	reqA := &events.EmbeddingsRequest{Texts: []string{"identical-input"}}
	reqB := &events.EmbeddingsRequest{Texts: []string{"identical-input"}}

	a.Inject("embeddings.request", reqA)
	b.Inject("embeddings.request", reqB)

	if len(reqA.Vectors[0]) != len(reqB.Vectors[0]) {
		t.Fatalf("vector dim mismatch: %d vs %d", len(reqA.Vectors[0]), len(reqB.Vectors[0]))
	}
	for i := range reqA.Vectors[0] {
		if reqA.Vectors[0][i] != reqB.Vectors[0][i] {
			t.Fatalf("vector %d differs at idx %d: %v vs %v",
				0, i, reqA.Vectors[0][i], reqB.Vectors[0][i])
		}
	}
}

func TestContract_HonorsRequestProviderPin(t *testing.T) {
	h := contract.NewContract(t, New)
	req := &events.EmbeddingsRequest{Texts: []string{"x"}, Provider: "someone-else"}
	h.Inject("embeddings.request", req)
	if req.Provider != "someone-else" {
		t.Errorf("provider should not be overwritten when pinned, got %q", req.Provider)
	}
	if len(req.Vectors) != 0 {
		t.Errorf("vectors should not be filled when provider is pinned, got %d", len(req.Vectors))
	}
}

func TestContract_NoUndeclaredEmissions(t *testing.T) {
	h := contract.NewContract(t, New)
	req := &events.EmbeddingsRequest{Texts: []string{"y"}}
	h.Inject("embeddings.request", req)
	h.AssertNoUndeclaredEmissions()
}
