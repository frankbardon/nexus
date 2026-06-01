package workspace

import (
	"errors"
	"strings"
)

// rawContract maps the YAML frontmatter shape we accept on a stage
// contract.md or verifier .md file. Validation runs on the populated
// struct so type mismatches surface as actionable errors.
type rawContract struct {
	ID        string        `yaml:"id"`
	Display   string        `yaml:"display"`
	Turns     TurnConfig    `yaml:"turns"`
	HumanGate HumanGate     `yaml:"human_gate"`
	OnError   ErrorPolicy   `yaml:"on_error"`
	Loop      *LoopConfig   `yaml:"loop"`
	FanOut    *FanOutConfig `yaml:"fan_out"`
	Output    OutputSpec    `yaml:"output"`
	Inputs    InputScope    `yaml:"inputs"`
	Agent     AgentSpec     `yaml:"agent"`
	Verifiers []string      `yaml:"verifiers"`
}

// splitContract divides a contract.md (or SKILL.md) into (frontmatter,
// body). The file must begin with '---', contain a closing '---' on its
// own line. The body may be empty for skill descriptors but the caller
// enforces non-empty when relevant.
func splitContract(data []byte) (frontmatter []byte, body string, err error) {
	text := string(data)
	if !strings.HasPrefix(text, "---") {
		return nil, "", errors.New("must begin with '---' YAML front-matter delimiter")
	}
	rest := strings.TrimPrefix(text, "---")
	rest = strings.TrimPrefix(rest, "\r")
	rest = strings.TrimPrefix(rest, "\n")

	end := findFrontmatterEnd(rest)
	if end < 0 {
		return nil, "", errors.New("YAML front-matter missing closing '---' on its own line")
	}

	frontmatter = []byte(rest[:end])
	body = strings.TrimSpace(rest[end:])
	if i := strings.Index(body, "\n"); i >= 0 {
		body = strings.TrimSpace(body[i+1:])
	} else {
		body = ""
	}
	return frontmatter, body, nil
}

// findFrontmatterEnd returns the byte offset of the line beginning with
// '---' that closes the front-matter, or -1 if not found.
func findFrontmatterEnd(s string) int {
	pos := 0
	for {
		idx := strings.Index(s[pos:], "\n---")
		if idx < 0 {
			return -1
		}
		abs := pos + idx + 1 // start of "---..." line
		tail := s[abs:]
		end := strings.IndexAny(tail, "\r\n")
		var lineEnd int
		if end < 0 {
			lineEnd = len(tail)
		} else {
			lineEnd = end
		}
		if strings.TrimSpace(tail[:lineEnd]) == "---" {
			return abs
		}
		pos = abs + 3
	}
}

// firstNonEmptyLine returns the first non-blank line of body, trimmed.
// Returns "" if body is empty or all blank.
func firstNonEmptyLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		// strip a leading markdown heading marker for nicer display
		t = strings.TrimLeft(t, "#")
		t = strings.TrimSpace(t)
		if t != "" {
			return t
		}
	}
	return ""
}

// truncateDisplay collapses display strings to the 80-char limit.
func truncateDisplay(s string) string {
	const max = 80
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
