// Package evalcase loads eval cases from disk.
//
// A case is a directory under tests/eval/cases/<id>/ containing:
//
//	case.yaml          # name, description, tags, owner, freshness_days, model_baseline
//	input/
//	  config.yaml      # config for the engine under test
//	  inputs.yaml      # scripted user inputs (drive nexus.io.test)
//	journal/           # full copy of source session journal (header + segments)
//	assertions.yaml    # mix of deterministic + semantic assertions
//
// Cases are pure data — the runner consumes them. This package owns the
// schema and loader; pkg/eval/runner owns execution.
package evalcase

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Case is the in-memory representation of a single eval case.
type Case struct {
	// Dir is the absolute path to the case directory.
	Dir string
	// ID is the case identifier (the directory's basename).
	ID string
	// Meta is the parsed case.yaml.
	Meta Meta
	// ConfigYAML is the raw bytes of input/config.yaml. The runner feeds
	// these to engine.NewFromBytes — keeping it raw lets the engine own
	// validation and tilde expansion.
	ConfigYAML []byte
	// Inputs are the scripted user inputs from input/inputs.yaml.
	Inputs []string
	// JournalDir is the absolute path to the journal directory.
	JournalDir string
	// Assertions is the parsed assertions.yaml.
	Assertions Assertions
}

// Meta is case.yaml.
type Meta struct {
	Name          string    `yaml:"name"`
	Description   string    `yaml:"description"`
	Tags          []string  `yaml:"tags"`
	Owner         string    `yaml:"owner"`
	FreshnessDays int       `yaml:"freshness_days"`
	ModelBaseline string    `yaml:"model_baseline"`
	RecordedAt    time.Time `yaml:"recorded_at,omitempty"`
}

// inputsFile is the on-disk schema of input/inputs.yaml.
type inputsFile struct {
	Inputs []string `yaml:"inputs"`
}

// Load opens a case directory and parses every artifact. Errors carry the
// path that failed so debugging a malformed bundle is straightforward.
func Load(dir string) (*Case, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving case dir %q: %w", dir, err)
	}
	if info, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("stat case dir %q: %w", abs, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("case path %q is not a directory", abs)
	}

	c := &Case{
		Dir: abs,
		ID:  filepath.Base(abs),
	}

	metaPath := filepath.Join(abs, "case.yaml")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", metaPath, err)
	}
	if err := yaml.Unmarshal(metaBytes, &c.Meta); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", metaPath, err)
	}
	if c.Meta.Name == "" {
		return nil, fmt.Errorf("%s: missing required field 'name'", metaPath)
	}

	cfgPath := filepath.Join(abs, "input", "config.yaml")
	cfgBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", cfgPath, err)
	}
	c.ConfigYAML = cfgBytes

	inputsPath := filepath.Join(abs, "input", "inputs.yaml")
	inputBytes, err := os.ReadFile(inputsPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", inputsPath, err)
	}
	var inputs inputsFile
	if err := yaml.Unmarshal(inputBytes, &inputs); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", inputsPath, err)
	}
	c.Inputs = inputs.Inputs

	journalDir := filepath.Join(abs, "journal")
	if info, err := os.Stat(journalDir); err != nil {
		return nil, fmt.Errorf("stat journal dir %q: %w", journalDir, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("journal path %q is not a directory", journalDir)
	}
	c.JournalDir = journalDir

	assertionsPath := filepath.Join(abs, "assertions.yaml")
	assertBytes, err := os.ReadFile(assertionsPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", assertionsPath, err)
	}
	asserts, err := ParseAssertions(assertBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", assertionsPath, err)
	}
	c.Assertions = asserts
	return c, nil
}
