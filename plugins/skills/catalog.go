package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
)

// resourceDirs are the subdirectories within a skill that contain resources.
var resourceDirs = []string{"scripts", "references", "assets"}

// BuildCatalogXML generates the <available_skills> XML block for system prompt injection.
func BuildCatalogXML(skills []SkillRecord) string {
	if len(skills) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<available_skills>\n")
	for _, s := range skills {
		b.WriteString(fmt.Sprintf("  <skill name=%q scope=%q>\n", s.Name, s.Scope))
		b.WriteString(fmt.Sprintf("    <description>%s</description>\n", xmlEscape(s.Description)))
		if s.License != "" {
			b.WriteString(fmt.Sprintf("    <license>%s</license>\n", xmlEscape(s.License)))
		}
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</available_skills>")
	return b.String()
}

// BuildSkillContentXML generates the <skill_content> XML wrapper with the skill
// body and a list of available resources.
func BuildSkillContentXML(record SkillRecord) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("<skill_content name=%q scope=%q>\n", record.Name, record.Scope))

	resources, _ := ListResources(record.BaseDir)
	if len(resources) > 0 {
		b.WriteString("  <resources>\n")
		for _, r := range resources {
			b.WriteString(fmt.Sprintf("    <resource>%s</resource>\n", xmlEscape(r)))
		}
		b.WriteString("  </resources>\n")
	}

	b.WriteString("  <body>\n")
	b.WriteString(record.Body)
	b.WriteString("\n  </body>\n")
	b.WriteString("</skill_content>")
	return b.String()
}

// ListResources lists files in the scripts/, references/, and assets/
// subdirectories of a skill's base directory.
func ListResources(baseDir string) ([]string, error) {
	var resources []string
	for _, dir := range resourceDirs {
		fullDir := filepath.Join(baseDir, dir)
		entries, err := os.ReadDir(fullDir)
		if err != nil {
			continue // directory may not exist
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			resources = append(resources, filepath.Join(dir, entry.Name()))
		}
	}
	return resources, nil
}

// xmlEscape delegates to the shared engine.XMLEscape helper.
func xmlEscape(s string) string {
	return engine.XMLEscape(s)
}
