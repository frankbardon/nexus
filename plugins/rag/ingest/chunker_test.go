package ingest

import (
	"strings"
	"testing"
)

func TestChunker_ShortTextSingleChunk(t *testing.T) {
	c := newChunker(1000, 200)
	got := c.chunk("hello world")
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(got))
	}
	if got[0] != "hello world" {
		t.Errorf("unexpected chunk content %q", got[0])
	}
}

func TestChunker_EmptyInput(t *testing.T) {
	c := newChunker(1000, 200)
	if got := c.chunk(""); len(got) != 0 {
		t.Fatalf("expected zero chunks for empty input, got %d", len(got))
	}
	if got := c.chunk("   \n\n  "); len(got) != 0 {
		t.Fatalf("expected zero chunks for whitespace input, got %d", len(got))
	}
}

func TestChunker_ParagraphSplit(t *testing.T) {
	// Three paragraphs, each already under size.
	text := strings.Repeat("sentence. ", 20) + "\n\n" +
		strings.Repeat("another. ", 20) + "\n\n" +
		strings.Repeat("third para. ", 20)
	c := newChunker(250, 50)
	got := c.chunk(text)
	if len(got) < 2 {
		t.Fatalf("expected ≥2 chunks, got %d", len(got))
	}
	for _, ch := range got {
		if len(ch) > 250+50 { // allow overlap slack
			t.Errorf("chunk exceeds size+overlap: %d", len(ch))
		}
	}
}

func TestChunker_OverlapPreservesContext(t *testing.T) {
	text := strings.Repeat("word ", 500)
	c := newChunker(200, 50)
	got := c.chunk(text)
	if len(got) < 2 {
		t.Fatalf("expected ≥2 chunks for long input, got %d", len(got))
	}
	// With overlap > 0, consecutive chunks should share some trailing words
	// from the previous chunk at the start of the next. Check the second
	// chunk begins with content that appears in the first.
	first, second := got[0], got[1]
	tail := first
	if len(tail) > 50 {
		tail = first[len(first)-50:]
	}
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return
	}
	// The overlap tail should appear somewhere near the start of the next chunk.
	if !strings.Contains(second[:min(len(second), 100)], tail[:min(len(tail), 10)]) {
		t.Logf("first tail: %q", tail)
		t.Logf("second head: %q", second[:min(len(second), 100)])
		t.Errorf("expected overlap context at start of second chunk")
	}
}

func TestChunker_HardSplitWhenNoSeparators(t *testing.T) {
	// No separators at all — a long run of non-space chars.
	text := strings.Repeat("a", 750)
	c := newChunker(200, 50)
	got := c.chunk(text)
	if len(got) < 3 {
		t.Fatalf("expected ≥3 chunks for 750 chars at size 200, got %d", len(got))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
