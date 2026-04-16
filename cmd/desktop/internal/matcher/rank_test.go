package matcher

import (
	"strings"
	"testing"
)

// testPool is a small fixed pool used across every parseRanking test.
// Kept minimal (3 entries) so the expected outputs are obvious; the
// real candidate pool in candidates.go is not appropriate for tests
// because changing it to add a candidate would break every test that
// asserts on a specific count.
func testPool() []candidateRecord {
	return []candidateRecord{
		{ID: "c1", Name: "Alice"},
		{ID: "c2", Name: "Bob"},
		{ID: "c3", Name: "Carol"},
	}
}

func TestParseRanking_HappyPath(t *testing.T) {
	content := `{
		"rankings": [
			{"candidate_id": "c1", "score": 0.92, "reasoning": "Strong fit on skills and location."},
			{"candidate_id": "c2", "score": 0.75, "reasoning": "Solid backend match."},
			{"candidate_id": "c3", "score": 0.60, "reasoning": "Partial skills match."}
		]
	}`

	got, err := parseRanking(content, testPool(), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(got))
	}

	// Check descending order and field mapping.
	wantOrder := []string{"c1", "c2", "c3"}
	for i, c := range got {
		if c.ID != wantOrder[i] {
			t.Errorf("position %d: got ID %q, want %q", i, c.ID, wantOrder[i])
		}
	}

	// Check that pool metadata (Name) was joined in.
	if got[0].Name != "Alice" {
		t.Errorf("expected Name to be joined from pool, got %q", got[0].Name)
	}
	if got[0].Score != 0.92 {
		t.Errorf("expected score 0.92, got %f", got[0].Score)
	}
	if got[0].Reasoning != "Strong fit on skills and location." {
		t.Errorf("unexpected reasoning: %q", got[0].Reasoning)
	}
}

func TestParseRanking_MarkdownFence(t *testing.T) {
	// Simulates the model ignoring the "no code fences" instruction.
	// The parser tolerates one surrounding fence specifically because
	// this is the most common LLM output drift and the easiest to
	// handle cleanly.
	content := "```json\n" +
		`{"rankings": [{"candidate_id": "c1", "score": 0.9, "reasoning": "ok"}]}` +
		"\n```"

	got, err := parseRanking(content, testPool(), 0)
	if err != nil {
		t.Fatalf("markdown-fenced JSON should parse, got error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
}

func TestParseRanking_EmptyContent(t *testing.T) {
	_, err := parseRanking("", testPool(), 0)
	if err == nil {
		t.Fatal("expected error on empty content")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected error mentioning empty, got %q", err.Error())
	}
}

func TestParseRanking_MalformedJSON(t *testing.T) {
	_, err := parseRanking(`{this is not valid JSON`, testPool(), 0)
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Errorf("expected error mentioning decoding, got %q", err.Error())
	}
}

func TestParseRanking_EmptyRankingsArray(t *testing.T) {
	_, err := parseRanking(`{"rankings": []}`, testPool(), 0)
	if err == nil {
		t.Fatal("expected error on empty rankings array")
	}
	if !strings.Contains(err.Error(), "no entries") {
		t.Errorf("expected error mentioning no entries, got %q", err.Error())
	}
}

func TestParseRanking_AllHallucinatedIDs(t *testing.T) {
	// Model invented candidate IDs that are not in the pool at all.
	// Parser should return an error because nothing matched — the
	// ranking is useless without any real candidates.
	content := `{
		"rankings": [
			{"candidate_id": "ghost1", "score": 0.9, "reasoning": "x"},
			{"candidate_id": "ghost2", "score": 0.8, "reasoning": "y"}
		]
	}`

	_, err := parseRanking(content, testPool(), 0)
	if err == nil {
		t.Fatal("expected error when all IDs are hallucinated")
	}
	if !strings.Contains(err.Error(), "matched no candidate IDs") {
		t.Errorf("expected 'matched no candidate IDs' error, got %q", err.Error())
	}
}

func TestParseRanking_PartialHallucination(t *testing.T) {
	// Mix of real and hallucinated IDs. Parser should silently skip
	// the fake ones and return the real ones — partial result is
	// still useful.
	content := `{
		"rankings": [
			{"candidate_id": "c1", "score": 0.9, "reasoning": "real"},
			{"candidate_id": "ghost", "score": 0.85, "reasoning": "fake"},
			{"candidate_id": "c2", "score": 0.7, "reasoning": "real"}
		]
	}`

	got, err := parseRanking(content, testPool(), 0)
	if err != nil {
		t.Fatalf("partial hallucination should return real entries, got error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 real candidates, got %d", len(got))
	}
	if got[0].ID != "c1" || got[1].ID != "c2" {
		t.Errorf("expected [c1, c2], got [%s, %s]", got[0].ID, got[1].ID)
	}
}

func TestParseRanking_ScoreClamping(t *testing.T) {
	// Out-of-range scores get clamped to [0, 1] without failing the
	// whole parse. This is defensive behavior for when the model
	// ignores the scoring rubric and emits > 1.0 for an especially
	// strong candidate or < 0 for a disqualifying fit.
	content := `{
		"rankings": [
			{"candidate_id": "c1", "score": 1.5, "reasoning": "over"},
			{"candidate_id": "c2", "score": -0.2, "reasoning": "under"},
			{"candidate_id": "c3", "score": 0.5, "reasoning": "normal"}
		]
	}`

	got, err := parseRanking(content, testPool(), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(got))
	}

	// After clamping, c1 is 1.0, c3 is 0.5, c2 is 0.0 — descending
	// order should be c1, c3, c2.
	if got[0].ID != "c1" || got[0].Score != 1.0 {
		t.Errorf("expected c1 clamped to 1.0, got ID=%q score=%f", got[0].ID, got[0].Score)
	}
	if got[1].ID != "c3" || got[1].Score != 0.5 {
		t.Errorf("expected c3 at 0.5, got ID=%q score=%f", got[1].ID, got[1].Score)
	}
	if got[2].ID != "c2" || got[2].Score != 0.0 {
		t.Errorf("expected c2 clamped to 0.0, got ID=%q score=%f", got[2].ID, got[2].Score)
	}
}

func TestParseRanking_OutOfOrderScores(t *testing.T) {
	// The model emits candidates in arbitrary order; the parser
	// must enforce descending-score ordering before returning.
	content := `{
		"rankings": [
			{"candidate_id": "c2", "score": 0.3, "reasoning": "low"},
			{"candidate_id": "c1", "score": 0.9, "reasoning": "high"},
			{"candidate_id": "c3", "score": 0.6, "reasoning": "mid"}
		]
	}`

	got, err := parseRanking(content, testPool(), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantIDs := []string{"c1", "c3", "c2"}
	for i, c := range got {
		if c.ID != wantIDs[i] {
			t.Errorf("position %d: got ID %q, want %q", i, c.ID, wantIDs[i])
		}
	}
}

func TestParseRanking_TopKTruncation(t *testing.T) {
	// TopK=2 on a 3-candidate pool should return only the top two
	// after sorting. The cut happens after sorting, not before.
	content := `{
		"rankings": [
			{"candidate_id": "c1", "score": 0.9, "reasoning": "a"},
			{"candidate_id": "c2", "score": 0.7, "reasoning": "b"},
			{"candidate_id": "c3", "score": 0.5, "reasoning": "c"}
		]
	}`

	got, err := parseRanking(content, testPool(), 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected TopK=2 truncation, got %d candidates", len(got))
	}
	if got[0].ID != "c1" || got[1].ID != "c2" {
		t.Errorf("expected top 2 [c1, c2], got [%s, %s]", got[0].ID, got[1].ID)
	}
}

func TestParseRanking_TopKZeroMeansAll(t *testing.T) {
	// TopK=0 is the "no limit" sentinel — all three candidates
	// should come back. This is the default the plugin uses when
	// the caller does not set MatchRequest.TopK.
	content := `{
		"rankings": [
			{"candidate_id": "c1", "score": 0.9, "reasoning": "a"},
			{"candidate_id": "c2", "score": 0.7, "reasoning": "b"},
			{"candidate_id": "c3", "score": 0.5, "reasoning": "c"}
		]
	}`

	got, err := parseRanking(content, testPool(), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("TopK=0 should return all candidates, got %d", len(got))
	}
}
