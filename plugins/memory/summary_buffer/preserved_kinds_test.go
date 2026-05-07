package summary_buffer

import (
	"reflect"
	"testing"
)

func TestParsePreservedKinds_Trailer(t *testing.T) {
	got := parsePreservedKinds("paragraph one\n\n## Preserved Kinds: decision, rationale, error\n")
	want := []string{"decision", "rationale", "error"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParsePreservedKinds_NoTrailer(t *testing.T) {
	if got := parsePreservedKinds("just a summary"); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestParsePreservedKinds_TrailerNoNewline(t *testing.T) {
	got := parsePreservedKinds("body\n## Preserved Kinds: decision")
	want := []string{"decision"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestMissingRequiredKinds(t *testing.T) {
	if !missingRequiredKinds([]string{"rationale"}, []string{"decision", "rationale"}) {
		t.Fatal("expected missing decision")
	}
	if missingRequiredKinds([]string{"decision", "rationale", "error"}, []string{"decision", "rationale"}) {
		t.Fatal("must not flag when all required present")
	}
}
