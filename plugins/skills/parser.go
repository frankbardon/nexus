package skills

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillRecord holds all parsed data from a SKILL.md file.
type SkillRecord struct {
	Name             string            `yaml:"name"`
	Description      string            `yaml:"description"`
	Location         string            `yaml:"-"`
	BaseDir          string            `yaml:"-"`
	License          string            `yaml:"license"`
	Compatibility    string            `yaml:"compatibility"`
	Metadata         map[string]string `yaml:"metadata"`
	AllowedTools     []string          `yaml:"allowed_tools"`
	OutputSchema     map[string]any    `yaml:"output_schema"`      // inline JSON Schema for structured output
	OutputSchemaFile string            `yaml:"output_schema_file"` // path to .json schema file (relative to skill dir)
	Class            string            `yaml:"class"`              // Semantic class for progressive discovery.
	Subclass         string            `yaml:"subclass"`           // Optional grouping within class.
	Tags             []string          `yaml:"tags"`               // Cross-cutting metadata for filtering.
	Body             string            `yaml:"-"`
	Scope            string            `yaml:"-"` // "project", "user", "builtin", "config"
	Trusted          bool              `yaml:"-"`
}

// ParseSkillFile reads and parses a SKILL.md file, extracting YAML
// frontmatter and the markdown body.
func ParseSkillFile(path string) (*SkillRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading skill file %s: %w", path, err)
	}

	content := string(data)
	record := &SkillRecord{Location: path}

	// Extract YAML frontmatter between --- delimiters.
	if strings.HasPrefix(strings.TrimSpace(content), "---") {
		trimmed := strings.TrimSpace(content)
		// Find the closing --- after the opening one.
		rest := trimmed[3:] // skip the opening ---
		endIdx := strings.Index(rest, "\n---")
		if endIdx == -1 {
			// No closing delimiter; treat entire content as body.
			record.Body = content
		} else {
			frontmatter := rest[:endIdx]
			record.Body = strings.TrimSpace(rest[endIdx+4:]) // skip \n---

			if err := yaml.Unmarshal([]byte(frontmatter), record); err != nil {
				return nil, fmt.Errorf("parsing frontmatter in %s: %w", path, err)
			}
		}
	} else {
		record.Body = content
	}

	// Validate required fields.
	if record.Name == "" {
		return nil, fmt.Errorf("skill file %s missing required field: name", path)
	}
	if record.Description == "" {
		return nil, fmt.Errorf("skill file %s missing required field: description", path)
	}

	return record, nil
}
