package client

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
)

var slugSanitizeRE = regexp.MustCompile(`[^a-z0-9_]+`)

const slugMaxRunes = 48

// resourceSlug produces a deterministic, filesystem/tool-name-safe slug for an
// MCP resource. Inputs in priority order: title, name, then URI. A short hash
// suffix from the URI guarantees stability across server restarts when name
// changes (or is empty) while still scoping collisions per server.
func resourceSlug(title, name, uri string) string {
	base := strings.ToLower(firstNonEmpty(title, name, uri))
	base = slugSanitizeRE.ReplaceAllString(base, "_")
	base = strings.Trim(base, "_")
	if base == "" {
		base = "resource"
	}
	if len([]rune(base)) > slugMaxRunes {
		base = string([]rune(base)[:slugMaxRunes])
	}

	sum := sha1.Sum([]byte(uri))
	suffix := hex.EncodeToString(sum[:])[:8]
	return base + "_" + suffix
}

// promptSlug normalises a prompt name into a slash-command-safe form (no
// spaces, lowercase). Prompts are user-typed so we keep the readable name
// rather than appending a hash.
func promptSlug(name string) string {
	out := strings.ToLower(name)
	out = slugSanitizeRE.ReplaceAllString(out, "_")
	return strings.Trim(out, "_")
}

// toolName produces the Nexus-namespaced catalog name for an MCP tool.
func toolName(server, raw string) string {
	return "mcp__" + server + "__" + raw
}

// readResourceToolName returns the generic per-server resource-read tool name.
func readResourceToolName(server string) string {
	return "mcp__" + server + "__read_resource"
}

// listResourcesToolName returns the generic per-server resource-list tool name.
func listResourcesToolName(server string) string {
	return "mcp__" + server + "__list_resources"
}

// staticResourceToolName composes the catalog name for an auto-registered
// static MCP resource.
func staticResourceToolName(server, slug string) string {
	return "mcp__" + server + "__resource__" + slug
}

// templateResourceToolName composes the catalog name for an auto-registered
// resource template.
func templateResourceToolName(server, slug string) string {
	return "mcp__" + server + "__template__" + slug
}
