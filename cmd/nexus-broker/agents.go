package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
)

// AgentProfile is one named public agent this broker fronts: a Nexus config to
// boot, a binary registry entry to boot it with, and the Agent Card that
// describes the result to the world.
//
// It exists because POST /claim cannot be the A2A front door. A claim carries
// the FULL nexus config as inline YAML (see claimRequest.Config), which no
// third-party A2A client can supply — it does not know Nexus exists, let alone
// which plugins to activate. A profile moves that decision broker-side: the
// client names an agent by URL, and the operator decided long ago what running
// that agent means.
//
// The map key — not a field — is the profile name, so a profile cannot disagree
// with its own name, exactly as BinaryEntry cannot. The name is also a PATH
// SEGMENT (every route this profile publishes is namespaced under it), which is
// why it is validated more strictly than a registry entry name.
//
// Rejected alternative: carrying the Nexus config through A2A Message.metadata.
// That works only for Nexus-aware clients, which defeats the entire point of
// speaking a standard protocol.
type AgentProfile struct {
	// Binary names which entry of the `binaries:` registry this profile spawns.
	//
	// OMITTED MEANS THE RESERVED `nexus` ENTRY, matching claimRequest.Binary
	// exactly: an omitted binary has one meaning in this broker, not two. An
	// UNKNOWN name is a BOOT failure rather than a fallback to the reserved
	// entry — also matching /claim, which answers 400 rather than quietly
	// spawning the base binary for a caller that asked for a vision build. The
	// difference is only in when it is caught: a claim's binary arrives per
	// request, a profile's is written in the broker's own config, so the honest
	// moment to refuse it is startup.
	Binary string `yaml:"binary"`

	// Config is the path to the Nexus config file instances of this profile
	// boot with. Required: a profile with no config names nothing spawnable,
	// and there is no default worth guessing — the config IS the agent.
	//
	// Funneled through engine.ExpandPath, so `~` works here as it does in every
	// other Nexus path key. Resolved to an absolute path and stat()ed at boot
	// (see resolveAgentProfiles), so a typo fails the boot naming the profile
	// rather than the first A2A request that happens to select it.
	Config string `yaml:"config"`

	// Card is the hand-authored half of this profile's Agent Card: identity,
	// provider, modes and skills. The derived half — supportedInterfaces,
	// capabilities, securitySchemes — is computed from what the broker actually
	// serves and overwrites anything stated here. See buildAgentCard.
	Card AgentCardSpec `yaml:"card"`

	// ResolvedConfig is the ABSOLUTE, verified path to Config: after tilde
	// expansion, after filepath.Abs, and after a stat that confirmed a readable
	// regular file.
	//
	// Not a YAML key. It follows BinaryEntry.ResolvedPath's precedent for the
	// same two reasons: a missing file fails the BOOT naming the profile, and
	// the answer is computed once per process rather than per request.
	//
	// Empty after LoadConfigFromBytes alone, which parses but deliberately
	// touches no filesystem — see LoadConfig.
	ResolvedConfig string `yaml:"-"`
}

// AgentCardSpec is the hand-authored half of a profile's Agent Card.
//
// The keys are spelled exactly as `nexus.io.a2a`'s inline `card:` block spells
// them, deliberately: an operator who has already authored a card for a
// standalone serving instance must be able to paste it here unchanged. What is
// missing is the same in both places — interfaces, capabilities and security
// have no keys, because an operator must not be able to state one that is false.
type AgentCardSpec struct {
	// Name is the agent's public name. Required.
	Name string `yaml:"name"`

	// Description is the agent's public one-paragraph description. Required.
	Description string `yaml:"description"`

	// Version is the AGENT's version, not the protocol's. Required.
	Version string `yaml:"version"`

	// DocumentationURL points at human-readable docs for this agent.
	DocumentationURL string `yaml:"documentation_url"`

	// IconURL points at an icon for client UIs.
	IconURL string `yaml:"icon_url"`

	// Provider names the organization behind the agent.
	Provider *AgentCardProvider `yaml:"provider"`

	// DefaultInputModes / DefaultOutputModes are the media types the agent
	// accepts and produces when a message does not say otherwise.
	DefaultInputModes  []string `yaml:"default_input_modes"`
	DefaultOutputModes []string `yaml:"default_output_modes"`

	// Skills is the public capability listing. At least one is required by the
	// A2A specification, and it is deliberately hand-authored rather than
	// derived from the instance's tool catalog: a catalog churns with every
	// plugin an operator enables, and a discovery document that churned with it
	// would leak internal structure and break clients that keyed off it. The
	// broker could not derive one in any case — no instance is running when the
	// card is built.
	Skills []AgentCardSkill `yaml:"skills"`
}

// empty reports whether the operator wrote no card at all, so the error can say
// "the card is missing" rather than listing every required field inside it.
func (s AgentCardSpec) empty() bool {
	return s.Name == "" && s.Description == "" && s.Version == "" &&
		s.DocumentationURL == "" && s.IconURL == "" && s.Provider == nil &&
		len(s.DefaultInputModes) == 0 && len(s.DefaultOutputModes) == 0 && len(s.Skills) == 0
}

// AgentCardProvider is the card's provider block.
type AgentCardProvider struct {
	// Organization is the provider's name. Required whenever the block is
	// present at all — an anonymous provider block states nothing.
	Organization string `yaml:"organization"`
	// URL is the provider's public URL.
	URL string `yaml:"url"`
}

// AgentCardSkill is one entry of the card's public skill listing.
type AgentCardSkill struct {
	// ID, Name and Description are all required by the specification.
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// Tags, Examples and the per-skill mode overrides are optional.
	Tags        []string `yaml:"tags"`
	Examples    []string `yaml:"examples"`
	InputModes  []string `yaml:"input_modes"`
	OutputModes []string `yaml:"output_modes"`
}

// sortedAgentNames returns a profile map's keys in a stable order.
//
// It delegates to sortedBinaryNames, which is generic over the value type for
// exactly this reason: every walk of a config-declared map in this binary goes
// through one helper so a config with two bad entries fails on the same one
// every boot, and so the startup log lists profiles in a fixed order.
func sortedAgentNames[T any](m map[string]T) []string { return sortedBinaryNames(m) }

// foldAgentProfiles validates the declared `agents:` block against the already
// folded binary registry, returning the resolved profile map.
//
// A nil or empty block returns a NIL map, not an empty one. That is the
// difference between "this broker has no A2A ingress" and "this broker has an
// A2A ingress with nothing on it", and it is what keeps a broker with no
// `agents:` block byte-for-byte the broker it was before profiles existed: no
// routes are registered, no card is built, and the boot log says nothing new.
func foldAgentProfiles(declared map[string]AgentProfile, binaries map[string]BinaryEntry) (map[string]AgentProfile, error) {
	if len(declared) == 0 {
		return nil, nil
	}

	out := make(map[string]AgentProfile, len(declared))
	for _, raw := range sortedAgentNames(declared) {
		profile := declared[raw]

		// Trimmed for the same reason binary names are: `"support ":` must not
		// smuggle in a second profile that is indistinguishable from the first
		// in every log line, card and URL.
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("agents: a profile has an empty name; every profile must be keyed by the name its A2A routes are published under")
		}
		if err := validateProfileName(name); err != nil {
			return nil, err
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("agents: %q is declared more than once (names are compared with surrounding whitespace trimmed)", name)
		}

		binary := strings.TrimSpace(profile.Binary)
		if binary == "" {
			binary = reservedBinaryName
		}
		if _, known := binaries[binary]; !known {
			return nil, fmt.Errorf("agents: %s: binary %q is not in the binaries registry; declare it under binaries: or name one of: %s",
				name, binary, strings.Join(sortedBinaryNames(binaries), ", "))
		}
		profile.Binary = binary

		configPath := strings.TrimSpace(profile.Config)
		if configPath == "" {
			return nil, fmt.Errorf("agents: %s: config is required and must name the nexus config file instances of this profile boot with", name)
		}
		profile.Config = engine.ExpandPath(configPath)

		if err := validateCardSpec(name, profile.Card); err != nil {
			return nil, err
		}

		out[name] = profile
	}
	return out, nil
}

// validateProfileName enforces the character set a profile name may use.
//
// This is stricter than the binary registry's rule, and the reason is
// structural rather than stylistic: a binary name is a value inside a JSON
// body, whereas a profile name is a PATH SEGMENT in every URL the profile
// publishes and in the absolute URLs its own Agent Card advertises. A name
// carrying a slash would silently restructure the route tree; one carrying a
// percent, a colon or a space would round-trip differently through URL encoding
// than through the card, so a client would dial a URL the broker never
// registered.
//
// The permitted set is therefore the unreserved URL characters that need no
// encoding at all (RFC 3986 §2.3), minus '~' which reads as a home directory
// everywhere else in Nexus config.
func validateProfileName(name string) error {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("agents: %q is not a usable profile name: a profile name is a URL path segment, so it may contain only letters, digits, '-', '_' and '.'", name)
		}
	}
	// A leading '.' would make the routes look like a dotfile and, more
	// practically, "." and ".." are path traversal in every client that joins
	// URLs textually.
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("agents: %q is not a usable profile name: it must not start with '.'", name)
	}
	return nil
}

// validateCardSpec checks the hand-authored card fields the A2A specification
// marks REQUIRED.
//
// pkg/a2a.ValidateAgentCard re-checks all of this when the full card is
// assembled, and that check remains the authority — this one exists on top of
// it because its errors name YAML keys an operator can go and edit
// ("agents.support.card.skills[0].id") rather than JSON field paths from a
// document the operator never wrote in that form.
func validateCardSpec(profile string, card AgentCardSpec) error {
	if card.empty() {
		return fmt.Errorf("agents: %s: card is required: an A2A agent MUST publish an agent card, and the broker will not invent a name, description or skill list on an operator's behalf", profile)
	}
	for _, required := range []struct {
		key   string
		value string
	}{
		{"name", card.Name},
		{"description", card.Description},
		{"version", card.Version},
	} {
		if strings.TrimSpace(required.value) == "" {
			return fmt.Errorf("agents: %s: card.%s is required", profile, required.key)
		}
	}
	if card.Provider != nil && strings.TrimSpace(card.Provider.Organization) == "" {
		return fmt.Errorf("agents: %s: card.provider.organization is required whenever a provider block is present", profile)
	}
	if len(card.Skills) == 0 {
		return fmt.Errorf("agents: %s: card.skills must list at least one skill; the skill list is this agent's public capability contract", profile)
	}
	for i, skill := range card.Skills {
		for _, required := range []struct {
			key   string
			value string
		}{
			{"id", skill.ID},
			{"name", skill.Name},
			{"description", skill.Description},
		} {
			if strings.TrimSpace(required.value) == "" {
				return fmt.Errorf("agents: %s: card.skills[%d].%s is required", profile, i, required.key)
			}
		}
	}
	return nil
}

// resolveAgentProfiles resolves and verifies every profile's Nexus config file,
// recording the answer on each profile's ResolvedConfig. It mutates the map in
// place; on the first failure it returns and the caller refuses the boot.
//
// This is the environmental half of profile validation, and it lives with
// resolveBinaryRegistry for the same reason: the same bytes legitimately
// resolve differently on two machines, so it cannot live in the pure parser.
// The accepted tradeoff is identical too — a broker whose profile names a
// config that is momentarily absent will not come up, which is better than one
// that boots and then fails every A2A request for that agent.
func resolveAgentProfiles(agents map[string]AgentProfile) error {
	for _, name := range sortedAgentNames(agents) {
		profile := agents[name]
		resolved, err := resolveAgentConfigPath(name, profile.Config)
		if err != nil {
			return err
		}
		profile.ResolvedConfig = resolved
		agents[name] = profile
	}
	return nil
}

// resolveAgentConfigPath turns one profile's configured config path into an
// absolute path that is known to be a readable regular file, or explains why it
// is not.
//
// It stats rather than parses. Whether the file is a VALID Nexus config is the
// engine's judgement, made by the instance that boots it; the broker's job here
// is only to catch the mistake it can catch cheaply and unambiguously — a path
// that names nothing, or names a directory.
func resolveAgentConfigPath(name, path string) (string, error) {
	candidate := engine.ExpandPath(strings.TrimSpace(path))
	resolved, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("agents: %s: cannot resolve config %q to an absolute path: %w", name, candidate, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("agents: %s: no such config file %s%s", name, resolved, configuredAs(resolved, path))
		}
		return "", fmt.Errorf("agents: %s: cannot stat config %s%s: %w", name, resolved, configuredAs(resolved, path), err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("agents: %s: config %s is a directory, not a file%s", name, resolved, configuredAs(resolved, path))
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("agents: %s: config %s is not a regular file (mode %s)%s", name, resolved, info.Mode(), configuredAs(resolved, path))
	}
	// Opened, not just stat()ed: a config the broker cannot READ is as fatal as
	// one that does not exist, and permissions are the common way that happens.
	f, err := os.Open(resolved)
	if err != nil {
		return "", fmt.Errorf("agents: %s: cannot read config %s%s: %w", name, resolved, configuredAs(resolved, path), err)
	}
	_ = f.Close()
	return resolved, nil
}

// resolveA2ABaseURL returns the absolute origin — scheme://host[:port] — that
// this broker's Agent Cards advertise their endpoints under.
//
// An Agent Card MUST carry absolute interface URLs: a client fetches the card
// precisely to learn where to send messages, so a relative or wrong URL is a
// card that cannot be used. The broker already has a key that answers "where do
// clients reach this broker" — advertise_addr, which POST /claim's ws_url is
// derived from — so this reuses it rather than adding a second, driftable
// answer to the same question. ws:// maps to http://, wss:// to https://.
//
// With advertise_addr unset it falls back to listen_addr, but ONLY when
// listen_addr names a dialable host. A wildcard bind (":8080", "0.0.0.0:8080")
// names no host at all, and a card advertising "http://:8080/agents/x/a2a"
// would be a confidently wrong answer handed to every client that fetches it.
// So that case is a boot failure naming advertise_addr, which is the key that
// fixes it.
//
// Profiles are the only consumer, so the whole question is skipped when there
// are none.
func resolveA2ABaseURL(advertiseScheme, advertiseHost, listenAddr string) (string, error) {
	if advertiseHost != "" {
		scheme := "http"
		if advertiseScheme == "wss" {
			scheme = "https"
		}
		return scheme + "://" + advertiseHost, nil
	}

	addr := strings.TrimSpace(listenAddr)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("agents: cannot derive the public URL for the agent cards from listen_addr %q: %w; set advertise_addr to the address clients use to reach this broker", listenAddr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		return "", fmt.Errorf("agents: listen_addr %q names no dialable host, so the agent cards would advertise a URL no client can use; set advertise_addr to the address clients use to reach this broker", listenAddr)
	}
	return "http://" + net.JoinHostPort(host, port), nil
}
