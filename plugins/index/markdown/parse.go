package markdownindex

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// frontmatter holds the YAML frontmatter fields we fold into the index and
// surface as result metadata. Everything else in the block is ignored.
type frontmatter struct {
	Title  string   `yaml:"title"`
	Type   string   `yaml:"type"`
	Source string   `yaml:"source"`
	Tags   []string `yaml:"tags"`
}

// section is one indexed unit: a markdown `##`+ heading and its body text.
// Heading is "" for the preamble that precedes the first heading.
type section struct {
	Heading string
	Text    string // full section text, heading line included
}

// splitFrontmatter separates a leading `---\n…\n---` YAML block from the body.
// Returns parsed frontmatter (zero value when absent or unparseable) and the
// remaining markdown body. Frontmatter parsing is best-effort: a malformed
// block yields an empty frontmatter and the original text as body.
func splitFrontmatter(raw string) (frontmatter, string) {
	var fm frontmatter
	s := strings.TrimPrefix(raw, "\ufeff") // strip UTF-8 BOM if present
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return fm, raw
	}
	nl := strings.IndexByte(s, '\n')
	rest := s[nl+1:]
	// Closing fence is the first line that is exactly "---".
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, raw
	}
	yamlBlock := rest[:end]
	body := rest[end+len("\n---"):]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:] // drop to the end of the closing fence line
	} else {
		body = ""
	}
	_ = yaml.Unmarshal([]byte(yamlBlock), &fm) // best-effort
	return fm, body
}

// splitSections splits a markdown body on `##`..`######` ATX headings. Text
// before the first heading becomes the preamble (section 0) when non-empty;
// level-1 (`#`) titles are left inside the preamble. Each section keeps its
// heading line as its first line. Empty/whitespace-only sections are dropped.
func splitSections(body string) []section {
	var sections []section
	var cur section
	started := false

	flush := func() {
		if strings.TrimSpace(cur.Text) != "" {
			sections = append(sections, cur)
		}
	}

	for _, ln := range strings.Split(body, "\n") {
		if isHeading(ln) {
			if started {
				flush()
			}
			cur = section{Heading: headingText(ln), Text: ln + "\n"}
			started = true
			continue
		}
		cur.Text += ln + "\n"
		started = true
	}
	flush()
	return sections
}

// isHeading reports whether ln is a level 2-6 ATX heading ("## ", "### ", …).
func isHeading(ln string) bool {
	t := strings.TrimSpace(ln)
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n < 2 || n > 6 || n >= len(t) {
		return false
	}
	return t[n] == ' ' || t[n] == '\t'
}

// headingText strips the leading hashes and surrounding space from a heading.
func headingText(ln string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "#"))
}
