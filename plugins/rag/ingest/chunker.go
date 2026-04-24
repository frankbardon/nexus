package ingest

import "strings"

// chunker splits text into overlapping windows using a recursive-character
// strategy: try the biggest separator first (paragraph breaks), fall back to
// smaller ones (lines, sentences, spaces) until chunks fit within size.
// Overlap preserves context across chunk boundaries for retrieval quality.
//
// Kept internal to this plugin per the "don't genericize early" rule — a
// second caller will justify promoting it to pkg/rag/.
type chunker struct {
	size    int
	overlap int
	seps    []string
}

func newChunker(size, overlap int) *chunker {
	if size <= 0 {
		size = 1000
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 4
	}
	return &chunker{
		size:    size,
		overlap: overlap,
		seps:    []string{"\n\n", "\n", ". ", " ", ""},
	}
}

// chunk splits text into overlapping chunks. Never returns empty chunks.
func (c *chunker) chunk(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(text) <= c.size {
		return []string{text}
	}

	raw := c.split(text, c.seps)
	return c.mergeWithOverlap(raw)
}

// split recursively breaks text by the first separator that reduces chunks
// below c.size. The empty-string separator at the tail guarantees termination
// (it slices by character).
func (c *chunker) split(text string, seps []string) []string {
	if len(text) <= c.size {
		return []string{text}
	}
	if len(seps) == 0 {
		// Shouldn't happen because the last sep is "", but be safe.
		return []string{text}
	}

	sep := seps[0]
	rest := seps[1:]

	var parts []string
	if sep == "" {
		// Hard split by size.
		for i := 0; i < len(text); i += c.size {
			end := i + c.size
			if end > len(text) {
				end = len(text)
			}
			parts = append(parts, text[i:end])
		}
		return parts
	}

	for _, p := range strings.Split(text, sep) {
		if p == "" {
			continue
		}
		if len(p) <= c.size {
			parts = append(parts, p)
			continue
		}
		parts = append(parts, c.split(p, rest)...)
	}
	return parts
}

// mergeWithOverlap concatenates adjacent parts until they would exceed size,
// then starts a new chunk carrying the last c.overlap characters of the
// previous chunk as context.
func (c *chunker) mergeWithOverlap(parts []string) []string {
	var chunks []string
	var cur strings.Builder

	flush := func() {
		s := strings.TrimSpace(cur.String())
		if s != "" {
			chunks = append(chunks, s)
		}
		cur.Reset()
	}

	for _, p := range parts {
		// If adding p would overflow, flush and seed the next chunk with
		// an overlap tail from the flushed content.
		if cur.Len() > 0 && cur.Len()+len(p)+1 > c.size {
			prev := cur.String()
			flush()
			if c.overlap > 0 && len(prev) > c.overlap {
				cur.WriteString(prev[len(prev)-c.overlap:])
				cur.WriteString(" ")
			}
		}
		cur.WriteString(p)
		cur.WriteString(" ")
	}
	flush()
	return chunks
}
