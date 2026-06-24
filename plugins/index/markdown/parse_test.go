package markdownindex

import "testing"

func TestSplitFrontmatter(t *testing.T) {
	raw := "---\n" +
		"title: \"BERA Score\"\n" +
		"type: reference\n" +
		"source: zendesk\n" +
		"tags:\n  - external\n  - bera-metrics\n" +
		"---\n" +
		"# BERA Score\n\nThe composite metric.\n"

	fm, body := splitFrontmatter(raw)
	if fm.Title != "BERA Score" {
		t.Errorf("title = %q, want %q", fm.Title, "BERA Score")
	}
	if fm.Type != "reference" {
		t.Errorf("type = %q, want reference", fm.Type)
	}
	if fm.Source != "zendesk" {
		t.Errorf("source = %q, want zendesk", fm.Source)
	}
	if len(fm.Tags) != 2 || fm.Tags[0] != "external" || fm.Tags[1] != "bera-metrics" {
		t.Errorf("tags = %v, want [external bera-metrics]", fm.Tags)
	}
	if got, want := body, "# BERA Score\n\nThe composite metric.\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestSplitFrontmatterAbsent(t *testing.T) {
	raw := "# Title\n\nNo frontmatter here.\n"
	fm, body := splitFrontmatter(raw)
	if fm.Title != "" {
		t.Errorf("expected empty title, got %q", fm.Title)
	}
	if body != raw {
		t.Errorf("body should be unchanged, got %q", body)
	}
}

func TestSplitSections(t *testing.T) {
	body := "# Title\n\nIntro paragraph.\n\n" +
		"## Overview\n\nOverview text.\n\n" +
		"## Why This Matters\n\nBecause reasons.\n"

	secs := splitSections(body)
	if len(secs) != 3 {
		t.Fatalf("got %d sections, want 3", len(secs))
	}
	// Preamble (level-1 title + intro) stays as section 0 with empty heading.
	if secs[0].Heading != "" {
		t.Errorf("section 0 heading = %q, want empty (preamble)", secs[0].Heading)
	}
	if secs[1].Heading != "Overview" {
		t.Errorf("section 1 heading = %q, want Overview", secs[1].Heading)
	}
	if secs[2].Heading != "Why This Matters" {
		t.Errorf("section 2 heading = %q, want Why This Matters", secs[2].Heading)
	}
}

func TestSplitSectionsLeadingHeading(t *testing.T) {
	body := "## First\n\nbody one\n\n## Second\n\nbody two\n"
	secs := splitSections(body)
	if len(secs) != 2 {
		t.Fatalf("got %d sections, want 2 (no empty preamble)", len(secs))
	}
	if secs[0].Heading != "First" || secs[1].Heading != "Second" {
		t.Errorf("headings = %q,%q want First,Second", secs[0].Heading, secs[1].Heading)
	}
}

func TestIsHeading(t *testing.T) {
	cases := map[string]bool{
		"## Overview":   true,
		"### Deep":      true,
		"# Title":       false, // level 1 is not a split point
		"###### Six":    true,
		"####### Seven": false, // >6 hashes is not a heading
		"##NoSpace":     false,
		"text ## mid":   false,
	}
	for in, want := range cases {
		if got := isHeading(in); got != want {
			t.Errorf("isHeading(%q) = %v, want %v", in, got, want)
		}
	}
}
