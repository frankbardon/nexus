package runner

import (
	"context"
	"testing"
	"time"

	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
)

// TestFilterByTags is a unit test for the tag superset filter — RunMany
// applies it before dispatch, so its correctness is load-bearing.
func TestFilterByTags(t *testing.T) {
	cases := []*evalcase.Case{
		{ID: "a", Meta: evalcase.Meta{Tags: []string{"react", "mock"}}},
		{ID: "b", Meta: evalcase.Meta{Tags: []string{"mock"}}},
		{ID: "c", Meta: evalcase.Meta{Tags: []string{"react", "skills"}}},
	}

	tt := []struct {
		name string
		want []string
		ids  []string
	}{
		{"no filter", nil, []string{"a", "b", "c"}},
		{"single tag", []string{"react"}, []string{"a", "c"}},
		{"two tags", []string{"react", "mock"}, []string{"a"}},
		{"missing tag", []string{"impossible"}, nil},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			got := filterByTags(cases, tc.want)
			if len(got) != len(tc.ids) {
				t.Fatalf("len: got=%d want=%d (%+v)", len(got), len(tc.ids), got)
			}
			for i, c := range got {
				if c.ID != tc.ids[i] {
					t.Errorf("[%d] got=%s want=%s", i, c.ID, tc.ids[i])
				}
			}
		})
	}
}

// TestOverrideDefaultModelRole confirms the surgical YAML rewrite preserves
// every key except core.models.default.
func TestOverrideDefaultModelRole(t *testing.T) {
	in := []byte(`core:
  log_level: warn
  models:
    default: original
    original:
      provider: x
      model: y
plugins:
  active:
    - foo
`)
	out, err := overrideDefaultModelRole(in, "override")
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if !contains(out, []byte("default: override")) {
		t.Errorf("expected override; got:\n%s", out)
	}
	if !contains(out, []byte("provider: x")) {
		t.Errorf("provider key dropped:\n%s", out)
	}
	if !contains(out, []byte("- foo")) {
		t.Errorf("plugins.active dropped:\n%s", out)
	}
}

// TestRunMany_FailureIsolation builds two synthetic cases — one valid, one
// with a missing journal — and asserts RunMany surfaces both results.
func TestRunMany_FailureIsolation(t *testing.T) {
	good := makeStubCase(t, "good")
	bad := &evalcase.Case{
		ID:         "bad-missing-journal",
		Dir:        t.TempDir(),
		JournalDir: "/nonexistent/journal/dir",
		ConfigYAML: []byte(`core:
  log_level: warn
plugins:
  active: []
`),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results := RunMany(ctx, []*evalcase.Case{good, bad}, MultiOptions{
		Parallelism: 2,
		PerCase:     Options{SessionsRoot: t.TempDir()},
	})

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Stable order = sort by ID. "bad-missing-journal" < "good".
	if results[0].CaseID != "bad-missing-journal" || results[1].CaseID != "good" {
		t.Fatalf("ordering: got %q, %q", results[0].CaseID, results[1].CaseID)
	}
	if results[0].Pass {
		t.Error("expected bad case to fail")
	}
	if !results[1].Pass {
		t.Errorf("expected good case to pass, fails=%v", results[1].Assertions)
	}
}

// TestRunMany_TagFilter ensures tag filtering happens before dispatch — a
// case that would error if dispatched is not even attempted when its tag
// doesn't match.
func TestRunMany_TagFilter(t *testing.T) {
	good := makeStubCase(t, "good")
	good.Meta.Tags = []string{"foo"}
	bad := &evalcase.Case{
		ID:         "bad",
		Dir:        t.TempDir(),
		JournalDir: "/nonexistent",
		ConfigYAML: []byte(`core:
  log_level: warn
plugins: { active: [] }
`),
		Meta: evalcase.Meta{Tags: []string{"bar"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results := RunMany(ctx, []*evalcase.Case{good, bad}, MultiOptions{
		Tags:    []string{"foo"},
		PerCase: Options{SessionsRoot: t.TempDir()},
	})
	if len(results) != 1 {
		t.Fatalf("expected 1 result after tag filter, got %d", len(results))
	}
	if results[0].CaseID != "good" {
		t.Errorf("got %q want good", results[0].CaseID)
	}
}

// makeStubCase builds a Case backed by a freshly-recorded minimal journal
// (one io.input, one llm.response, one turn) so RunMany can drive it.
// Avoids a separate fixture file — keeps the test self-contained.
func makeStubCase(t *testing.T, id string) *evalcase.Case {
	t.Helper()
	dir := t.TempDir()
	journalDir := dir + "/journal"
	if err := writeStubJournal(journalDir, id); err != nil {
		t.Fatalf("write stub journal: %v", err)
	}
	cfg := []byte(`core:
  log_level: warn
  tick_interval: 1h
  models:
    default: mock
    mock:
      provider: nexus.llm.anthropic
      model: mock
      max_tokens: 1024
  sessions:
    root: ` + dir + `/sessions
journal:
  fsync: none
plugins:
  active:
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.memory.capped
  nexus.llm.anthropic:
    api_key: "sk-mock-not-used"
  nexus.agent.react:
    system_prompt: "test"
  nexus.memory.capped:
    max_messages: 10
    persist: false
`)
	return &evalcase.Case{
		ID:         id,
		Dir:        dir,
		JournalDir: journalDir,
		ConfigYAML: cfg,
		Inputs:     []string{"hello"},
		Assertions: evalcase.Assertions{
			Deterministic: []evalcase.Assertion{
				{
					Kind: "event_emitted",
					EventEmitted: &evalcase.EventEmittedSpec{
						Type:  "io.input",
						Count: &evalcase.CountRange{Min: 1, Max: 1},
					},
				},
			},
		},
	}
}

func contains(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
