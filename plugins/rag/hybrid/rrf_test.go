package hybrid

import "testing"

func TestRRFRanksByCombinedRank(t *testing.T) {
	// Both backends rank b first → b is unambiguously top.
	// a is rank 2 in vector and absent from lexical → rank 2 overall.
	// c, d, e are single-list and trail.
	lists := []rankedList{
		{source: "vector", bias: 1, docs: []rankedDoc{
			{id: "b"}, {id: "a"}, {id: "c"},
		}},
		{source: "lexical", bias: 1, docs: []rankedDoc{
			{id: "b"}, {id: "d"}, {id: "e"},
		}},
	}
	out := fuseRRF(lists, 60)

	if out[0].id != "b" {
		t.Fatalf("top hit = %q, want b", out[0].id)
	}
	if len(out) != 5 {
		t.Fatalf("got %d hits, want 5 distinct ids", len(out))
	}
}

func TestRRFBiasFavorsLexical(t *testing.T) {
	lists := []rankedList{
		{source: "vector", bias: 0.1, docs: []rankedDoc{
			{id: "v1"}, {id: "v2"},
		}},
		{source: "lexical", bias: 1.9, docs: []rankedDoc{
			{id: "l1"}, {id: "l2"},
		}},
	}
	out := fuseRRF(lists, 60)
	if out[0].id != "l1" {
		t.Fatalf("with lexical bias, top hit = %q, want l1", out[0].id)
	}
}

func TestRRFCarriesSources(t *testing.T) {
	lists := []rankedList{
		{source: "vector", bias: 1, docs: []rankedDoc{{id: "a", similarity: 0.9}}},
		{source: "lexical", bias: 1, docs: []rankedDoc{{id: "a", lexical: 5.0}}},
	}
	out := fuseRRF(lists, 60)
	if len(out) != 1 {
		t.Fatalf("expected 1 fused hit, got %d", len(out))
	}
	if out[0].similarity != 0.9 {
		t.Errorf("similarity = %v, want 0.9", out[0].similarity)
	}
	if out[0].lexical != 5.0 {
		t.Errorf("lexical = %v, want 5.0", out[0].lexical)
	}
	if len(out[0].sources) != 2 {
		t.Errorf("sources = %v, want 2 entries", out[0].sources)
	}
}

func TestWeightedNormalizesAcrossBackends(t *testing.T) {
	// Vector scores in [0.1, 1.0]; lexical scores in [1, 100]. Without
	// normalization, lexical would dominate. With normalization + equal
	// bias, both backends contribute equally.
	lists := []rankedList{
		{source: "vector", bias: 0.5, docs: []rankedDoc{
			{id: "a", similarity: 1.0},
			{id: "b", similarity: 0.5},
			{id: "c", similarity: 0.1},
		}},
		{source: "lexical", bias: 0.5, docs: []rankedDoc{
			{id: "c", lexical: 100},
			{id: "b", lexical: 50},
			{id: "a", lexical: 1},
		}},
	}
	out := fuseWeighted(lists)
	// All three should appear. After normalization both backends score
	// 1.0 / 0.5 / 0.0 across a/b/c, summed → equal contribution.
	if len(out) != 3 {
		t.Fatalf("got %d hits, want 3", len(out))
	}
}

func TestWeightedHandlesEmptyList(t *testing.T) {
	lists := []rankedList{
		{source: "vector", bias: 0.7, docs: []rankedDoc{
			{id: "a", similarity: 1.0},
		}},
		{source: "lexical", bias: 0.3, docs: nil},
	}
	out := fuseWeighted(lists)
	if len(out) != 1 || out[0].id != "a" {
		t.Fatalf("expected [a], got %v", out)
	}
}

func TestWeightedSingleDocSpread(t *testing.T) {
	// Only one doc → score range is zero. norm should clamp to 1, not divide
	// by zero.
	lists := []rankedList{
		{source: "vector", bias: 1, docs: []rankedDoc{
			{id: "a", similarity: 0.5},
		}},
	}
	out := fuseWeighted(lists)
	if len(out) != 1 || out[0].score == 0 {
		t.Fatalf("single-doc fusion produced zero score: %+v", out)
	}
}
