//go:build integration

package integration

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// skillsConfig builds a config copy with scan_paths rewritten to an absolute
// path. The base config uses a relative path which only resolves correctly
// when the cwd is the repo root; tests run from tests/integration/.
func skillsConfig(t *testing.T) string {
	t.Helper()
	root := findRoot(t)
	return copyConfig(t, "configs/test-skills.yaml", map[string]any{
		"nexus.skills": map[string]any{
			"scan_paths":               []string{filepath.Join(root, "tests/fixtures/skills")},
			"trust_project":            "always",
			"max_active_skills":        3,
			"catalog_in_system_prompt": true,
			"disabled_skills":          []string{},
		},
	})
}

// TestSkills_Boot validates the skills plugin boots and pulls in its
// dependencies cleanly.
func TestSkills_Boot(t *testing.T) {
	h := testharness.New(t, skillsConfig(t), testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.skills",
		"nexus.agent.react",
	)
}

// TestSkills_DiscoversFixtures validates that the plugin scans the configured
// fixture path and emits a skill.discover event listing the bundled skills.
func TestSkills_DiscoversFixtures(t *testing.T) {
	h := testharness.New(t, skillsConfig(t), testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertEventEmitted("skill.discover")

	// tests/fixtures/skills/ ships with these three skills.
	wantSkills := map[string]bool{
		"code-review":  false,
		"doc-analysis": false,
		"git-workflow": false,
	}

	var lastCatalog *events.SkillCatalog
	for _, e := range h.Events() {
		if e.Type != "skill.discover" {
			continue
		}
		cat, ok := e.Payload.(events.SkillCatalog)
		if !ok {
			continue
		}
		lastCatalog = &cat
	}
	if lastCatalog == nil {
		t.Fatal("no skill.discover event with SkillCatalog payload found")
	}
	for _, s := range lastCatalog.Skills {
		if _, want := wantSkills[s.Name]; want {
			wantSkills[s.Name] = true
		}
	}
	for name, found := range wantSkills {
		if !found {
			t.Errorf("expected skill %q in catalog, but missing. Catalog: %+v", name, lastCatalog.Skills)
		}
	}
}
