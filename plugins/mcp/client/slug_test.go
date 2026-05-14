package client

import (
	"strings"
	"testing"
)

func TestResourceSlug_StableAcrossRestarts(t *testing.T) {
	a := resourceSlug("Project README", "readme", "file:///project/README.md")
	b := resourceSlug("Project README", "readme", "file:///project/README.md")
	if a != b {
		t.Fatalf("slug not stable: %q vs %q", a, b)
	}
}

func TestResourceSlug_DifferentURIsDifferentSlugs(t *testing.T) {
	a := resourceSlug("README", "readme", "file:///proj-a/README.md")
	b := resourceSlug("README", "readme", "file:///proj-b/README.md")
	if a == b {
		t.Fatalf("expected different slugs for different URIs, both = %q", a)
	}
}

func TestResourceSlug_FallsBackToURIWhenNamesEmpty(t *testing.T) {
	s := resourceSlug("", "", "file:///x.txt")
	if s == "" {
		t.Fatalf("expected non-empty slug")
	}
}

func TestResourceSlug_TruncatesLongBase(t *testing.T) {
	long := strings.Repeat("a", 200)
	s := resourceSlug("", long, "file:///x")
	// base capped at 48 runes, plus "_" and 8-char hash = 57 chars max.
	if len(s) > 60 {
		t.Fatalf("slug too long: %d chars (%q)", len(s), s)
	}
}

func TestPromptSlug_NormalisesCase(t *testing.T) {
	if promptSlug("Review PR") != "review_pr" {
		t.Fatalf("got %q", promptSlug("Review PR"))
	}
}

func TestToolName_NamespacesBoth(t *testing.T) {
	if got := toolName("fs", "read_file"); got != "mcp__fs__read_file" {
		t.Fatalf("got %q", got)
	}
}
