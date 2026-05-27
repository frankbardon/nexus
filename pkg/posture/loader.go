package posture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadDir scans dir for *.yaml / *.yml files and decodes each into an
// AgentPosture. Returns the parsed postures in alphabetical filename order so
// repeated calls are stable. Files that fail to parse are surfaced in errs;
// successful entries still come back in postures, so a single malformed file
// does not block the rest from registering.
func LoadDir(dir string) (postures []AgentPosture, errs []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{fmt.Errorf("posture: read dir %s: %w", dir, err)}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
			continue
		}
		full := filepath.Join(dir, name)
		p, lerr := LoadFile(full)
		if lerr != nil {
			errs = append(errs, lerr)
			continue
		}
		postures = append(postures, p)
	}
	return postures, errs
}

// LoadFile decodes a single posture YAML file.
func LoadFile(path string) (AgentPosture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentPosture{}, fmt.Errorf("posture: read %s: %w", path, err)
	}
	var p AgentPosture
	if err := yaml.Unmarshal(data, &p); err != nil {
		return AgentPosture{}, fmt.Errorf("posture: parse %s: %w", path, err)
	}
	if p.Name == "" {
		// Fall back to the basename so loaders without explicit `name:` still
		// land in the registry under the on-disk identifier.
		base := filepath.Base(path)
		base = strings.TrimSuffix(base, filepath.Ext(base))
		p.Name = base
	}
	p.Version = HashPosture(p)
	return p, nil
}
