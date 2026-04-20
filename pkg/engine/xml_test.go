package engine

import (
	"strings"
	"testing"
)

func TestXMLTag(t *testing.T) {
	var b strings.Builder
	XMLTag(&b, "section", "name", "test")
	got := b.String()
	want := "<section name=\"test\">\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestXMLTag_NoAttrs(t *testing.T) {
	var b strings.Builder
	XMLTag(&b, "block")
	got := b.String()
	want := "<block>\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestXMLClose(t *testing.T) {
	var b strings.Builder
	XMLClose(&b, "section")
	got := b.String()
	want := "</section>\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestXMLWrap(t *testing.T) {
	got := XMLWrap("task", "do something", "id", "1")
	want := "<task id=\"1\">\ndo something\n</task>\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestXMLWrap_TrailingNewline(t *testing.T) {
	got := XMLWrap("task", "content\n")
	want := "<task>\ncontent\n</task>\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestXMLWrap_Empty(t *testing.T) {
	got := XMLWrap("task", "")
	if got != "" {
		t.Fatalf("expected empty string for empty content, got %q", got)
	}
}

func TestXMLCDATA(t *testing.T) {
	got := XMLCDATA("hello <world>")
	want := "<![CDATA[hello <world>]]>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestXMLCDATA_NestedClose(t *testing.T) {
	got := XMLCDATA("data]]>more")
	want := "<![CDATA[data]]]]><![CDATA[>more]]>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestXMLEscape(t *testing.T) {
	got := XMLEscape(`<a & "b">`)
	want := "&lt;a &amp; &quot;b&quot;&gt;"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
