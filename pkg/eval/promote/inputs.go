// Package promote turns a real session directory into a deterministic eval
// case. It copies the journal verbatim, reconstructs scripted user inputs
// from journaled io.input events, and synthesizes a starter assertions.yaml
// the case author can tighten by hand.
//
// The promote package owns no engine state — it is a pure-disk transform —
// so it can be exercised from tests without a running bus.
package promote

import (
	"fmt"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
)

// ExtractInputs returns the user-typed strings carried by io.input envelopes
// in the order they appear in the journal. Non-io.input envelopes are
// ignored. Empty input list yields an empty slice (not nil) so the YAML
// always serializes a deterministic shape.
//
// The journal stores io.input payloads as either typed events.UserInput
// (when the writer ran with the live bus) or as map[string]any (post-reload
// from disk, since journal.Reader unmarshals payloads to interface{}). Both
// shapes are honored.
func ExtractInputs(envs []journal.Envelope) []string {
	out := make([]string, 0)
	for _, e := range envs {
		if e.Type != "io.input" {
			continue
		}
		s, ok := readUserInputContent(e.Payload)
		if !ok {
			continue
		}
		out = append(out, s)
	}
	return out
}

// readUserInputContent extracts the .Content field from a journaled io.input
// payload. Honors both the typed events.UserInput and the post-reload
// map[string]any shape (where Reader.Iter returns the payload as
// map[string]any after JSON unmarshal).
func readUserInputContent(payload any) (string, bool) {
	switch v := payload.(type) {
	case events.UserInput:
		return v.Content, true
	case *events.UserInput:
		if v == nil {
			return "", false
		}
		return v.Content, true
	case map[string]any:
		if raw, ok := v["Content"]; ok {
			if s, ok := raw.(string); ok {
				return s, true
			}
		}
		// Some on-disk envelopes use the lowercase JSON tag form. The shipped
		// events.UserInput has no `json` tags so Go field names are the
		// canonical shape, but cover both for safety against future schema
		// drift.
		if raw, ok := v["content"]; ok {
			if s, ok := raw.(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

// inputsYAML is the minimal on-disk schema written to input/inputs.yaml. It
// matches pkg/eval/case/case.go:inputsFile so a round-trip via Load works
// without further conversion.
type inputsYAML struct {
	Inputs []string `yaml:"inputs"`
}

// inputsYAMLBytes returns the YAML body for input/inputs.yaml given the
// extracted slice. Always emits the `inputs:` key (possibly with an empty
// list) so the schema is stable.
func inputsYAMLBytes(inputs []string) ([]byte, error) {
	return marshalYAML(inputsYAML{Inputs: inputs})
}

// marshalYAML wraps yaml.Marshal so callers don't import yaml.v3 directly.
// Centralized for ease of swap-out and to avoid scattering yaml.v3 across
// every helper file.
func marshalYAML(v any) ([]byte, error) {
	out, err := yamlMarshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal yaml: %w", err)
	}
	return out, nil
}
