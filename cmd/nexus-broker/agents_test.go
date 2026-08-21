package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentProfileYAML renders a loadable broker config around one or more profile
// blocks, so a test can vary exactly one thing at a time.
//
// It states a dialable listen_addr because profiles need an origin to advertise
// in their cards, and the DEFAULT bind (":8080") deliberately does not supply
// one — see resolveA2ABaseURL. Tests that are about that rule build their own
// document with agentsBlock instead.
func agentProfileYAML(profileBlock string) string {
	return "listen_addr: \"127.0.0.1:8080\"\n" + agentsBlock(profileBlock)
}

// agentsBlock renders just the `agents:` block, for tests that control the rest
// of the document themselves.
func agentsBlock(profileBlock string) string {
	return "agents:\n" + profileBlock
}

// oneValidProfile is the profile body every "this should load" test starts
// from: a valid card, an omitted binary (which must mean the reserved entry),
// and a config path the caller substitutes.
func oneValidProfile(name, configPath string) string {
	return "  " + name + `:
    config: "` + configPath + `"
    card:
      name: "Support Agent"
      description: "Answers customer questions."
      version: "1.2.0"
      skills:
        - id: "answer"
          name: "Answer questions"
          description: "Answers a customer question from the knowledge base."
`
}

// writeAgentConfig writes a throwaway nexus config file and returns its path.
// Its CONTENTS are irrelevant to the broker, which stats the file and hands the
// path to a spawned instance — parsing it is the engine's job, not this
// binary's.
func writeAgentConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte("plugins:\n  active: []\n"), 0o600); err != nil {
		t.Fatalf("write agent config: %v", err)
	}
	return path
}

// mustLoadAgentsConfig loads a config carrying one valid profile, all the way
// through LoadConfig (not LoadConfigFromBytes), so the filesystem half of
// profile resolution is exercised too.
func mustLoadAgentsConfig(t *testing.T, yaml string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write broker config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// TestAgentProfilesLoad is the happy path: two profiles, one naming a declared
// binary and one omitting `binary` entirely, both resolved with their config
// paths made absolute.
func TestAgentProfilesLoad(t *testing.T) {
	supportConfig := writeAgentConfig(t)
	researchConfig := writeAgentConfig(t)
	// The reserved entry is resolved through PATH at boot, so the registry has
	// to name something that exists on this machine for LoadConfig to succeed.
	nexusBinary := writeBinaryFixture(t, t.TempDir(), "nexus", 0o755)

	yaml := `
listen_addr: "127.0.0.1:8080"
binaries:
  nexus:
    path: "` + nexusBinary + `"
  vision:
    path: "` + nexusBinary + `"
` + agentsBlock(oneValidProfile("support", supportConfig)+
		`  research:
    binary: vision
    config: "`+researchConfig+`"
    card:
      name: "Research Agent"
      description: "Reads and summarizes."
      version: "0.1.0"
      provider:
        organization: "Acme"
        url: "https://acme.example"
      default_input_modes: ["text/plain"]
      default_output_modes: ["text/plain"]
      skills:
        - id: "summarize"
          name: "Summarize"
          description: "Summarizes a document."
          tags: ["research", "writing"]
`)

	cfg := mustLoadAgentsConfig(t, yaml)

	if len(cfg.Agents) != 2 {
		t.Fatalf("Agents = %d profiles, want 2: %+v", len(cfg.Agents), cfg.Agents)
	}

	support := cfg.Agents["support"]
	// An omitted `binary` must mean the reserved entry, exactly as an omitted
	// claim `binary` does. Anything else would give one word two meanings in one
	// broker.
	if support.Binary != reservedBinaryName {
		t.Errorf("support.Binary = %q, want %q (an omitted binary is the reserved entry)", support.Binary, reservedBinaryName)
	}
	if !filepath.IsAbs(support.ResolvedConfig) {
		t.Errorf("support.ResolvedConfig = %q, want an absolute path", support.ResolvedConfig)
	}
	if support.ResolvedConfig != supportConfig {
		t.Errorf("support.ResolvedConfig = %q, want %q", support.ResolvedConfig, supportConfig)
	}
	if support.Card.Name != "Support Agent" || support.Card.Version != "1.2.0" {
		t.Errorf("support card mis-parsed: %+v", support.Card)
	}
	if len(support.Card.Skills) != 1 || support.Card.Skills[0].ID != "answer" {
		t.Errorf("support skills mis-parsed: %+v", support.Card.Skills)
	}

	research := cfg.Agents["research"]
	if research.Binary != "vision" {
		t.Errorf("research.Binary = %q, want %q", research.Binary, "vision")
	}
	if research.Card.Provider == nil || research.Card.Provider.Organization != "Acme" {
		t.Errorf("research provider mis-parsed: %+v", research.Card.Provider)
	}
	if len(research.Card.Skills) != 1 || len(research.Card.Skills[0].Tags) != 2 {
		t.Errorf("research skills mis-parsed: %+v", research.Card.Skills)
	}
}

// TestAgentProfileConfigPathIsExpanded pins the ExpandPath contract: a profile
// config written with a leading ~ resolves against the home directory, exactly
// as every other Nexus path key does.
func TestAgentProfileConfigPathIsExpanded(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this machine: %v", err)
	}
	yaml := agentProfileYAML(oneValidProfile("support", "~/nexus-agent-does-not-exist.yaml"))

	cfg, err := LoadConfigFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	want := filepath.Join(home, "nexus-agent-does-not-exist.yaml")
	if got := cfg.Agents["support"].Config; got != want {
		t.Errorf("Config = %q, want %q (~ must be expanded through engine.ExpandPath)", got, want)
	}
}

// TestAgentProfileUnknownBinaryFailsLoad is the acceptance criterion in its own
// right: a profile naming a binary the registry does not have is a STARTUP
// failure, not a silent fallback to the reserved entry.
//
// The reasoning is claimRequest.Binary's, one layer earlier: quietly spawning
// the base binary for an agent an operator bound to a vision build produces a
// session that merely behaves oddly, which is far harder to diagnose than a
// refusal. A claim catches it per request; a profile is broker config, so the
// honest moment is boot.
func TestAgentProfileUnknownBinaryFailsLoad(t *testing.T) {
	yaml := agentProfileYAML(`  support:
    binary: "vision"
    config: "/tmp/agent.yaml"
    card:
      name: "Support"
      description: "Answers questions."
      version: "1.0.0"
      skills:
        - id: "answer"
          name: "Answer"
          description: "Answers."
`)

	_, err := LoadConfigFromBytes([]byte(yaml))
	if err == nil {
		t.Fatal("LoadConfigFromBytes accepted a profile naming an unregistered binary")
	}
	// The message must name the offending value AND the alternatives — an
	// operator staring at a crash-looping container has nothing else to go on.
	for _, want := range []string{"vision", "binaries", "nexus"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestAgentProfileMissingConfigFileFailsLoad pins the environmental half: a
// profile whose config path names nothing on disk fails the boot naming the
// profile.
func TestAgentProfileMissingConfigFileFailsLoad(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there.yaml")
	yaml := "listen_addr: \"127.0.0.1:8080\"\nbinaries:\n  nexus:\n    path: \"" +
		writeBinaryFixture(t, t.TempDir(), "nexus", 0o755) + "\"\n" +
		agentsBlock(oneValidProfile("support", missing))

	brokerPath := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(brokerPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write broker config: %v", err)
	}

	// It must parse: the document is well-formed, and LoadConfigFromBytes is a
	// pure function of the bytes that deliberately touches no filesystem.
	if _, err := LoadConfigFromBytes([]byte(yaml)); err != nil {
		t.Fatalf("LoadConfigFromBytes rejected a well-formed document: %v", err)
	}

	_, err := LoadConfig(brokerPath)
	if err == nil {
		t.Fatal("LoadConfig accepted a profile whose config file does not exist")
	}
	if !strings.Contains(err.Error(), "support") || !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q names neither the profile nor the missing path %q", err, missing)
	}
}

// TestAgentProfileConfigDirectoryFailsLoad covers the other half of the same
// check: a path that exists but is a directory is not a config file.
func TestAgentProfileConfigDirectoryFailsLoad(t *testing.T) {
	dir := t.TempDir()
	agents := map[string]AgentProfile{"support": {Config: dir}}
	err := resolveAgentProfiles(agents)
	if err == nil {
		t.Fatal("resolveAgentProfiles accepted a directory as a config file")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error %q does not say the path is a directory", err)
	}
}

// TestAgentProfileDuplicateNamesFailLoad covers both spellings of "declared
// twice".
//
// An exact duplicate is caught by yaml.v3 itself, which refuses a mapping key
// defined twice. A duplicate that differs only in surrounding whitespace is
// two distinct YAML keys and one profile name, so the broker has to catch that
// one — otherwise `"support ":` would publish routes indistinguishable from
// `support:`'s in every log line and URL, with one silently winning.
func TestAgentProfileDuplicateNamesFailLoad(t *testing.T) {
	cases := map[string]string{
		"exact duplicate": agentProfileYAML(
			oneValidProfile("support", "/tmp/a.yaml") + oneValidProfile("support", "/tmp/b.yaml")),
		"duplicate after trimming": agentProfileYAML(
			oneValidProfile("support", "/tmp/a.yaml") + oneValidProfile(`"support "`, "/tmp/b.yaml")),
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfigFromBytes([]byte(yaml))
			if err == nil {
				t.Fatal("LoadConfigFromBytes accepted a profile declared twice")
			}
			if !strings.Contains(err.Error(), "support") {
				t.Errorf("error %q does not name the duplicated profile", err)
			}
		})
	}
}

// TestAgentProfileNameMustBeURLSafe pins the stricter-than-binaries naming rule.
// A profile name is a PATH SEGMENT in every URL the profile publishes and in the
// absolute URLs its own card advertises, so a name that does not survive a URL
// round-trip would have a client dialing a route the broker never registered.
func TestAgentProfileNameMustBeURLSafe(t *testing.T) {
	for _, name := range []string{"sup port", "sup/port", "sup:port", "sup%20port", ".hidden", "sup?port"} {
		t.Run(name, func(t *testing.T) {
			yaml := agentProfileYAML(oneValidProfile(`"`+name+`"`, "/tmp/agent.yaml"))
			if _, err := LoadConfigFromBytes([]byte(yaml)); err == nil {
				t.Fatalf("LoadConfigFromBytes accepted the unusable profile name %q", name)
			}
		})
	}
	// The permitted set must still be usable: these are ordinary names an
	// operator would reach for.
	for _, name := range []string{"support", "support-eu", "support_v2", "support.v2", "a1"} {
		t.Run("valid "+name, func(t *testing.T) {
			yaml := agentProfileYAML(oneValidProfile(name, "/tmp/agent.yaml"))
			cfg, err := LoadConfigFromBytes([]byte(yaml))
			if err != nil {
				t.Fatalf("LoadConfigFromBytes rejected the usable profile name %q: %v", name, err)
			}
			if _, ok := cfg.Agents[name]; !ok {
				t.Errorf("profile %q missing from %v", name, cfg.Agents)
			}
		})
	}
}

// TestAgentProfileRequiredFieldsFailLoad covers the fields a profile cannot omit,
// each with an error that names the YAML key an operator must go and fix.
func TestAgentProfileRequiredFieldsFailLoad(t *testing.T) {
	cases := map[string]struct {
		body     string
		wantsKey string
	}{
		"no config": {
			body: `  support:
    card:
      name: "Support"
      description: "Answers."
      version: "1.0.0"
      skills:
        - id: "a"
          name: "A"
          description: "A."
`,
			wantsKey: "config",
		},
		"no card": {
			body: `  support:
    config: "/tmp/agent.yaml"
`,
			wantsKey: "card",
		},
		"no card name": {
			body: `  support:
    config: "/tmp/agent.yaml"
    card:
      description: "Answers."
      version: "1.0.0"
      skills:
        - id: "a"
          name: "A"
          description: "A."
`,
			wantsKey: "card.name",
		},
		"no skills": {
			body: `  support:
    config: "/tmp/agent.yaml"
    card:
      name: "Support"
      description: "Answers."
      version: "1.0.0"
`,
			wantsKey: "card.skills",
		},
		"skill without id": {
			body: `  support:
    config: "/tmp/agent.yaml"
    card:
      name: "Support"
      description: "Answers."
      version: "1.0.0"
      skills:
        - name: "A"
          description: "A."
`,
			wantsKey: "card.skills[0].id",
		},
		"provider without organization": {
			body: `  support:
    config: "/tmp/agent.yaml"
    card:
      name: "Support"
      description: "Answers."
      version: "1.0.0"
      provider:
        url: "https://acme.example"
      skills:
        - id: "a"
          name: "A"
          description: "A."
`,
			wantsKey: "card.provider.organization",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfigFromBytes([]byte(agentProfileYAML(tc.body)))
			if err == nil {
				t.Fatalf("LoadConfigFromBytes accepted a profile with no %s", tc.wantsKey)
			}
			if !strings.Contains(err.Error(), tc.wantsKey) {
				t.Errorf("error %q does not name the missing key %q", err, tc.wantsKey)
			}
		})
	}
}

// TestNoAgentsBlockLeavesConfigUntouched is half the "behaves exactly as before"
// proof, at the config layer: with no `agents:` block the map stays NIL, the
// derived base URL stays empty, and nothing about the load changed.
//
// Nil rather than empty is load-bearing: the A2A ingress registers routes only
// when profiles exist, and TestLoadConfigDefaults compares a zero-config load
// against DefaultConfig field for field.
func TestNoAgentsBlockLeavesConfigUntouched(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte("listen_addr: \":8080\"\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.Agents != nil {
		t.Errorf("Agents = %v, want nil for a config with no agents block", cfg.Agents)
	}
	if cfg.A2ABaseURL != "" {
		t.Errorf("A2ABaseURL = %q, want empty with no profiles", cfg.A2ABaseURL)
	}
	// An empty block is the same statement as an absent one.
	cfg, err = LoadConfigFromBytes([]byte("agents:\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes with an empty agents block: %v", err)
	}
	if cfg.Agents != nil {
		t.Errorf("Agents = %v, want nil for an empty agents block", cfg.Agents)
	}
}

// TestA2ABaseURLResolution pins where the absolute URLs in a card come from.
//
// A card MUST carry absolute interface URLs — a client fetches it precisely to
// learn where to send messages — so this is the difference between a usable card
// and a confidently wrong one.
func TestA2ABaseURLResolution(t *testing.T) {
	cases := []struct {
		name       string
		scheme     string
		host       string
		listenAddr string
		want       string
		wantErr    bool
	}{
		{name: "advertise host:port", scheme: "ws", host: "broker.example:8080", listenAddr: ":8080", want: "http://broker.example:8080"},
		{name: "advertise wss becomes https", scheme: "wss", host: "broker.example", listenAddr: ":8080", want: "https://broker.example"},
		{name: "falls back to a dialable listen_addr", listenAddr: "127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{name: "wildcard listen_addr is refused", listenAddr: ":8080", wantErr: true},
		{name: "0.0.0.0 listen_addr is refused", listenAddr: "0.0.0.0:8080", wantErr: true},
		{name: "malformed listen_addr is refused", listenAddr: "not-an-address", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveA2ABaseURL(tc.scheme, tc.host, tc.listenAddr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveA2ABaseURL(%q, %q, %q) = %q, want an error", tc.scheme, tc.host, tc.listenAddr, got)
				}
				if !strings.Contains(err.Error(), "advertise_addr") {
					t.Errorf("error %q does not name the key that fixes it", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveA2ABaseURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveA2ABaseURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAgentProfilesRequireADialableOrigin proves the base-URL rule reaches the
// operator as a BOOT failure rather than as a card full of unusable URLs: a
// wildcard bind with profiles configured and no advertise_addr does not start.
func TestAgentProfilesRequireADialableOrigin(t *testing.T) {
	yaml := "listen_addr: \":8080\"\n" + agentsBlock(oneValidProfile("support", "/tmp/agent.yaml"))
	_, err := LoadConfigFromBytes([]byte(yaml))
	if err == nil {
		t.Fatal("LoadConfigFromBytes accepted profiles on a wildcard bind with no advertise_addr")
	}
	if !strings.Contains(err.Error(), "advertise_addr") {
		t.Errorf("error %q does not name advertise_addr", err)
	}

	// The same config with advertise_addr set loads, and the profiles inherit
	// the address clients were already told to use.
	yaml = "listen_addr: \":8080\"\nadvertise_addr: \"wss://broker.example\"\n" +
		agentsBlock(oneValidProfile("support", "/tmp/agent.yaml"))
	cfg, err := LoadConfigFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.A2ABaseURL != "https://broker.example" {
		t.Errorf("A2ABaseURL = %q, want %q", cfg.A2ABaseURL, "https://broker.example")
	}
}
