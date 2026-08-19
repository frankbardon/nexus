package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	// Empty YAML => all defaults. Compared with reflect.DeepEqual rather than ==
	// because Config carries the raw auth map, which makes it non-comparable.
	cfg, err := LoadConfigFromBytes([]byte(""))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	want := DefaultConfig()
	// The one field a load produces that DefaultConfig does not: the binary
	// registry is SYNTHESIZED at load, not defaulted, because the folding rules
	// have to be able to tell an operator-declared `binaries.nexus` from a value
	// we put there ourselves. Zero config still resolves to exactly the historical
	// spawn target.
	want.Binaries = map[string]BinaryEntry{reservedBinaryName: {Path: defaultNexusBinaryPath}}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("defaults mismatch:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	yaml := `
listen_addr: "127.0.0.1:9000"
nexus_binary_path: "/opt/nexus/bin/nexus"
max_concurrent: 32
idle_timeout: 2m
queue_wait_timeout: 10s
release_grace: 20s
reattach_window: 90s
`
	cfg, err := LoadConfigFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.NexusBinaryPath != "/opt/nexus/bin/nexus" {
		t.Errorf("NexusBinaryPath = %q", cfg.NexusBinaryPath)
	}
	if cfg.MaxConcurrent != 32 {
		t.Errorf("MaxConcurrent = %d", cfg.MaxConcurrent)
	}
	if cfg.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout = %v", cfg.IdleTimeout)
	}
	if cfg.QueueWaitTimeout != 10*time.Second {
		t.Errorf("QueueWaitTimeout = %v", cfg.QueueWaitTimeout)
	}
	if cfg.ReleaseGrace != 20*time.Second {
		t.Errorf("ReleaseGrace = %v", cfg.ReleaseGrace)
	}
	if cfg.ReattachWindow != 90*time.Second {
		t.Errorf("ReattachWindow = %v", cfg.ReattachWindow)
	}
}

// TestLoadConfigReattachWindowDefault pins the restart-recovery bound: absent, it
// takes the default, and a non-positive value falls back to the same default at
// use rather than disabling the reaper — "wait forever" would reintroduce the
// orphaned lease this key exists to remove.
func TestLoadConfigReattachWindowDefault(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(``))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.ReattachWindow != defaultReattachWindow {
		t.Errorf("ReattachWindow = %v, want %v", cfg.ReattachWindow, defaultReattachWindow)
	}

	zero, err := LoadConfigFromBytes([]byte("reattach_window: 0s\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if zero.ReattachWindow != 0 {
		t.Errorf("ReattachWindow = %v, want the configured 0 carried through", zero.ReattachWindow)
	}
	// The fallback lives at the use site, so the reaper still bounds the wait.
	reg := NewRegistry(discardLogger(), 8)
	id, err := reg.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	// Nothing was restored, so the reaper is a no-op regardless — this asserts the
	// zero value does not make it hang or panic.
	reapUnreattached(context.Background(), discardLogger(), reg, nil, zero.ReattachWindow, 0)
	if !reg.Has(id) {
		t.Error("an ordinary lease was reaped by the reattach reaper")
	}
}

func TestLoadConfigExpandsBinaryPath(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`nexus_binary_path: "~/bin/nexus"`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.NexusBinaryPath == "~/bin/nexus" {
		t.Errorf("expected ~ to be expanded, got %q", cfg.NexusBinaryPath)
	}
}

// TestLoadConfigBinariesSynthesizesReservedEntry pins the zero-config guarantee:
// a broker.yaml that has never heard of `binaries:` still resolves a registry,
// and the reserved entry in it points at exactly the path the broker has spawned
// since before the registry existed.
func TestLoadConfigBinariesSynthesizesReservedEntry(t *testing.T) {
	for _, yaml := range []string{``, "listen_addr: \":7777\"\n", "binaries: {}\n", "binaries:\n"} {
		cfg, err := LoadConfigFromBytes([]byte(yaml))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes(%q): %v", yaml, err)
		}
		entry, ok := cfg.Binaries[reservedBinaryName]
		if !ok {
			t.Fatalf("for %q: registry = %+v, want a %q entry", yaml, cfg.Binaries, reservedBinaryName)
		}
		if entry.Path != defaultNexusBinaryPath {
			t.Errorf("for %q: %s path = %q, want %q", yaml, reservedBinaryName, entry.Path, defaultNexusBinaryPath)
		}
		if len(cfg.Warnings) != 0 {
			t.Errorf("for %q: warnings = %v, want none (nothing deprecated was used)", yaml, cfg.Warnings)
		}
	}
}

// TestLoadConfigBinariesReservedEntrySurvivesOtherEntries is the other half of
// the reservation: an operator's `binaries:` block adds variants, it does not
// replace the base binary. A broker that could lose its `nexus` entry would
// start refusing claims that name no binary at all.
func TestLoadConfigBinariesReservedEntrySurvivesOtherEntries(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`
binaries:
  vision:
    path: "/opt/nexus/bin/nexus-vision"
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if len(cfg.Binaries) != 2 {
		t.Fatalf("registry = %+v, want the declared entry plus the reserved one", cfg.Binaries)
	}
	if got := cfg.Binaries[reservedBinaryName].Path; got != defaultNexusBinaryPath {
		t.Errorf("%s path = %q, want %q", reservedBinaryName, got, defaultNexusBinaryPath)
	}
	if got := cfg.Binaries["vision"].Path; got != "/opt/nexus/bin/nexus-vision" {
		t.Errorf("vision path = %q", got)
	}
}

// TestLoadConfigBinaryEntryFields covers the descriptive and per-variant spawn
// fields round-tripping off the YAML.
func TestLoadConfigBinaryEntryFields(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`
binaries:
  vision:
    path: "/opt/nexus/bin/nexus-vision"
    label: "Nexus (vision)"
    description: "Multimodal build with the image tools compiled in"
    args: ["-profile", "vision"]
    env:
      NEXUS_VISION: "1"
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	entry := cfg.Binaries["vision"]
	if entry.Label != "Nexus (vision)" {
		t.Errorf("Label = %q", entry.Label)
	}
	if entry.Description != "Multimodal build with the image tools compiled in" {
		t.Errorf("Description = %q", entry.Description)
	}
	if !reflect.DeepEqual(entry.Args, []string{"-profile", "vision"}) {
		t.Errorf("Args = %v", entry.Args)
	}
	if !reflect.DeepEqual(entry.Env, map[string]string{"NEXUS_VISION": "1"}) {
		t.Errorf("Env = %v", entry.Env)
	}
}

// TestLoadConfigBinaryEntryExpandsPath pins the project-wide path rule for the
// new key: every config-supplied filesystem path goes through engine.ExpandPath,
// so `~` works in a registry entry exactly as it does in nexus_binary_path.
func TestLoadConfigBinaryEntryExpandsPath(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte("binaries:\n  vision:\n    path: \"~/bin/nexus-vision\"\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	got := cfg.Binaries["vision"].Path
	if strings.HasPrefix(got, "~") {
		t.Errorf("expected ~ to be expanded, got %q", got)
	}
	if !strings.HasSuffix(got, "/bin/nexus-vision") {
		t.Errorf("vision path = %q, want the configured suffix preserved", got)
	}
}

// TestLoadConfigBinaryAliasFoldsIntoRegistry is the backward-compatibility
// criterion: an existing deployment that only knows nexus_binary_path boots
// unchanged, its value becomes the reserved entry, and the operator is told
// which key replaces it.
func TestLoadConfigBinaryAliasFoldsIntoRegistry(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte("nexus_binary_path: \"/opt/nexus/bin/nexus\"\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if got := cfg.Binaries[reservedBinaryName].Path; got != "/opt/nexus/bin/nexus" {
		t.Errorf("%s path = %q, want the alias value folded in", reservedBinaryName, got)
	}
	if len(cfg.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one deprecation warning", cfg.Warnings)
	}
	// The warning has to name BOTH the dead key and its replacement; "deprecated"
	// with no migration target is not actionable.
	for _, want := range []string{keyNexusBinaryPath, "binaries." + reservedBinaryName + ".path"} {
		if !strings.Contains(cfg.Warnings[0], want) {
			t.Errorf("warning %q does not name %q", cfg.Warnings[0], want)
		}
	}

	// The alias path is expanded on the way into the registry, like any other.
	tilde, err := LoadConfigFromBytes([]byte("nexus_binary_path: \"~/bin/nexus\"\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if got := tilde.Binaries[reservedBinaryName].Path; strings.HasPrefix(got, "~") {
		t.Errorf("expected ~ to be expanded in the folded entry, got %q", got)
	}
}

// TestLoadConfigBinaryAliasAndEntryConflictFails: two keys naming one spawn
// target is a config bug, and the broker refuses the boot rather than picking a
// winner — whichever way the tie broke, half the operators hitting it would
// silently spawn the binary they did not mean.
func TestLoadConfigBinaryAliasAndEntryConflictFails(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`
nexus_binary_path: "/opt/nexus/bin/nexus"
binaries:
  nexus:
    path: "/usr/local/bin/nexus"
`))
	if err == nil {
		t.Fatalf("LoadConfigFromBytes succeeded (registry=%+v); want a boot failure", cfg.Binaries)
	}
	for _, want := range []string{keyNexusBinaryPath, "binaries." + reservedBinaryName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestLoadConfigBinaryEntryWithoutPathFails: an entry that names nothing
// spawnable must fail the boot, naming the entry, rather than the first claim
// that selects it.
func TestLoadConfigBinaryEntryWithoutPathFails(t *testing.T) {
	for _, yaml := range []string{
		"binaries:\n  vision: {}\n",
		"binaries:\n  vision:\n    path: \"\"\n",
		"binaries:\n  vision:\n    path: \"   \"\n",
		"binaries:\n  vision:\n    label: \"Nexus (vision)\"\n",
	} {
		cfg, err := LoadConfigFromBytes([]byte(yaml))
		if err == nil {
			t.Fatalf("LoadConfigFromBytes(%q) succeeded (registry=%+v); want a boot failure", yaml, cfg.Binaries)
		}
		if !strings.Contains(err.Error(), "vision") {
			t.Errorf("error %q does not name the offending entry", err)
		}
	}
}

// TestLoadConfigBinaryEmptyNameFails: the map key is the name a claim selects
// by, so a nameless entry is unreachable and is refused rather than carried.
func TestLoadConfigBinaryEmptyNameFails(t *testing.T) {
	for _, yaml := range []string{
		"binaries:\n  \"\":\n    path: \"/opt/nexus/bin/nexus-x\"\n",
		"binaries:\n  \"   \":\n    path: \"/opt/nexus/bin/nexus-x\"\n",
	} {
		cfg, err := LoadConfigFromBytes([]byte(yaml))
		if err == nil {
			t.Fatalf("LoadConfigFromBytes(%q) succeeded (registry=%+v); want a boot failure", yaml, cfg.Binaries)
		}
		if !strings.Contains(err.Error(), "binaries") {
			t.Errorf("error %q does not name binaries", err)
		}
	}
}

// TestLoadConfigBinaryEmptyAliasFails: `nexus_binary_path: ""` written
// explicitly is not the same as omitting it. Defaulting it back to "nexus"
// would silently undo whatever the operator was in the middle of doing.
func TestLoadConfigBinaryEmptyAliasFails(t *testing.T) {
	if _, err := LoadConfigFromBytes([]byte("nexus_binary_path: \"\"\n")); err == nil {
		t.Fatal("LoadConfigFromBytes succeeded for an empty nexus_binary_path; want a boot failure")
	} else if !strings.Contains(err.Error(), keyNexusBinaryPath) {
		t.Errorf("error %q does not name %q", err, keyNexusBinaryPath)
	}
}

// TestLoadConfigBinaryTrimmedNameCollisionFails: names are compared trimmed, so
// `"nexus ":` cannot smuggle in a second entry indistinguishable from the
// reserved one in every log line and operator surface.
func TestLoadConfigBinaryTrimmedNameCollisionFails(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`
binaries:
  vision:
    path: "/opt/a"
  "vision ":
    path: "/opt/b"
`))
	if err == nil {
		t.Fatalf("LoadConfigFromBytes succeeded (registry=%+v); want a boot failure", cfg.Binaries)
	}
	if !strings.Contains(err.Error(), "vision") {
		t.Errorf("error %q does not name the colliding entry", err)
	}
}

// writeBinaryFixture writes a fake spawn target into dir and returns its path.
//
// Fixtures are always built under t.TempDir() and PATH is always overridden with
// t.Setenv, so nothing in this file can pass or fail because of what happens to
// be installed on the machine running the tests.
func writeBinaryFixture(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatalf("writing binary fixture: %v", err)
	}
	// WriteFile's mode is masked by the process umask, so set it explicitly —
	// otherwise the "executable" fixture can land at 0644 on a strict umask and
	// the test asserts the opposite of what it means to.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod binary fixture: %v", err)
	}
	return path
}

// resolveOne is the single-entry shorthand the resolution tests are written
// against: it exercises the same map-walking entry point the boot path uses.
func resolveOne(name, path string) (string, error) {
	binaries := map[string]BinaryEntry{name: {Path: path}}
	if err := resolveBinaryRegistry(binaries); err != nil {
		return "", err
	}
	return binaries[name].ResolvedPath, nil
}

// TestResolveBinaryRegistryRejectsMissingFile is the headline criterion: a typo
// or a build that was never produced stops the boot, and the error names the
// entry so an operator knows which line of broker.yaml to fix.
func TestResolveBinaryRegistryRejectsMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nexus-vision")
	resolved, err := resolveOne("vision", missing)
	if err == nil {
		t.Fatalf("resolveBinaryRegistry succeeded (resolved=%q); want a load failure", resolved)
	}
	for _, want := range []string{"vision", missing} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestResolveBinaryRegistryRejectsDirectory: pointing an entry at a directory is
// the classic "I gave it the install dir, not the binary" mistake, and exec()
// would fail with a confusing EACCES at claim time.
func TestResolveBinaryRegistryRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	resolved, err := resolveOne("vision", dir)
	if err == nil {
		t.Fatalf("resolveBinaryRegistry succeeded (resolved=%q); want a load failure", resolved)
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error %q does not say the path is a directory", err)
	}
	if !strings.Contains(err.Error(), "vision") {
		t.Errorf("error %q does not name the offending entry", err)
	}
}

// TestResolveBinaryRegistryRejectsNonExecutableFile covers the build artifact
// that exists but was never chmod +x'd — the one failure mode that looks
// completely fine in an `ls` of the deploy directory.
func TestResolveBinaryRegistryRejectsNonExecutableFile(t *testing.T) {
	path := writeBinaryFixture(t, t.TempDir(), "nexus-vision", 0o644)
	resolved, err := resolveOne("vision", path)
	if err == nil {
		t.Fatalf("resolveBinaryRegistry succeeded (resolved=%q); want a load failure", resolved)
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("error %q does not say the file is not executable", err)
	}
	for _, want := range []string{"vision", path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestResolveBinaryRegistryResolvesBareNameThroughPath pins the PATH contract,
// for a NON-reserved entry: bare names are allowed for every variant, not just
// `nexus`, so an operator installing builds into /usr/local/bin need not spell
// out directories.
func TestResolveBinaryRegistryResolvesBareNameThroughPath(t *testing.T) {
	dir := t.TempDir()
	want := writeBinaryFixture(t, dir, "nexus-vision", 0o755)
	t.Setenv("PATH", dir)

	got, err := resolveOne("vision", "nexus-vision")
	if err != nil {
		t.Fatalf("resolveBinaryRegistry: %v", err)
	}
	if got != want {
		t.Errorf("ResolvedPath = %q, want the PATH fixture %q", got, want)
	}
}

// TestResolveBinaryRegistryBareNameNotOnPathFails is the other half: a name that
// PATH cannot answer is a boot failure naming the entry and the name, not a
// deferred surprise at claim time.
func TestResolveBinaryRegistryBareNameNotOnPathFails(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	resolved, err := resolveOne("vision", "nexus-vision")
	if err == nil {
		t.Fatalf("resolveBinaryRegistry succeeded (resolved=%q); want a load failure", resolved)
	}
	for _, want := range []string{"vision", "nexus-vision", "PATH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestResolveBinaryRegistryExpandsTilde: `~` works in a registry entry exactly
// as it does in every other Nexus path key, and the EXPANDED path is what gets
// stat'd — a `~` reaching the filesystem would be a directory literally named
// "~", which is never what the operator meant.
func TestResolveBinaryRegistryExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "builds"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := writeBinaryFixture(t, filepath.Join(home, "builds"), "nexus-vision", 0o755)

	got, err := resolveOne("vision", "~/builds/nexus-vision")
	if err != nil {
		t.Fatalf("resolveBinaryRegistry: %v", err)
	}
	if got != want {
		t.Errorf("ResolvedPath = %q, want %q", got, want)
	}
}

// TestResolveBinaryRegistryYieldsAbsolutePaths pins the claim-time guarantee: a
// valid registry leaves every entry holding an absolute path, so spawning does no
// filesystem work and cannot be affected by the process working directory.
func TestResolveBinaryRegistryYieldsAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	base := writeBinaryFixture(t, dir, "nexus", 0o755)
	vision := writeBinaryFixture(t, dir, "nexus-vision", 0o755)
	t.Setenv("PATH", dir)

	binaries := map[string]BinaryEntry{
		reservedBinaryName: {Path: reservedBinaryName}, // bare, via PATH
		"vision":           {Path: vision},             // already absolute
	}
	if err := resolveBinaryRegistry(binaries); err != nil {
		t.Fatalf("resolveBinaryRegistry: %v", err)
	}
	for name, entry := range binaries {
		if !filepath.IsAbs(entry.ResolvedPath) {
			t.Errorf("%s ResolvedPath = %q, want an absolute path", name, entry.ResolvedPath)
		}
	}
	if got := binaries[reservedBinaryName].ResolvedPath; got != base {
		t.Errorf("%s ResolvedPath = %q, want %q", reservedBinaryName, got, base)
	}
	if got := binaries["vision"].ResolvedPath; got != vision {
		t.Errorf("vision ResolvedPath = %q, want %q", got, vision)
	}
}

// TestLoadConfigResolvesBinaryRegistry runs the whole boot path — a real file on
// disk through LoadConfig — because that, not the byte-level parser, is what
// run() calls. The reserved entry is resolved too, even though the config never
// mentions it.
func TestLoadConfigResolvesBinaryRegistry(t *testing.T) {
	dir := t.TempDir()
	base := writeBinaryFixture(t, dir, "nexus", 0o755)
	vision := writeBinaryFixture(t, dir, "nexus-vision", 0o755)
	t.Setenv("PATH", dir)

	configPath := filepath.Join(dir, "broker.yaml")
	yaml := "binaries:\n  vision:\n    path: " + fmt.Sprintf("%q", vision) + "\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing broker.yaml: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Binaries[reservedBinaryName].ResolvedPath; got != base {
		t.Errorf("%s ResolvedPath = %q, want the PATH-resolved %q", reservedBinaryName, got, base)
	}
	if got := cfg.Binaries["vision"].ResolvedPath; got != vision {
		t.Errorf("vision ResolvedPath = %q, want %q", got, vision)
	}
	// The configured path is retained verbatim alongside the resolved one, so the
	// boot log can show both and an operator can see a surprising PATH answer.
	if got := cfg.Binaries[reservedBinaryName].Path; got != defaultNexusBinaryPath {
		t.Errorf("%s Path = %q, want the configured %q retained", reservedBinaryName, got, defaultNexusBinaryPath)
	}
}

// TestLoadConfigZeroConfigResolvesReservedEntryThroughPath is the deliberate
// behaviour change this story introduces: a zero-config broker resolves `nexus`
// through PATH at BOOT. If PATH cannot answer, startup fails — where previously
// it deferred the failure to the first claim.
func TestLoadConfigZeroConfigResolvesReservedEntryThroughPath(t *testing.T) {
	dir := t.TempDir()
	base := writeBinaryFixture(t, dir, "nexus", 0o755)
	configPath := filepath.Join(dir, "broker.yaml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatalf("writing broker.yaml: %v", err)
	}

	t.Setenv("PATH", dir)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Binaries[reservedBinaryName].ResolvedPath; got != base {
		t.Errorf("%s ResolvedPath = %q, want %q", reservedBinaryName, got, base)
	}

	t.Setenv("PATH", t.TempDir()) // same config, a PATH with no nexus on it
	if _, err := LoadConfig(configPath); err == nil {
		t.Fatal("LoadConfig succeeded with no nexus on PATH; want a boot failure")
	} else if !strings.Contains(err.Error(), reservedBinaryName) {
		t.Errorf("error %q does not name the %q entry", err, reservedBinaryName)
	}
}

// TestLoadConfigFromBytesLeavesRegistryUnresolved pins the deliberate split: the
// byte-level parser validates the DOCUMENT and touches no filesystem, which is
// what lets the rest of this suite load config from string literals naming paths
// that do not exist.
func TestLoadConfigFromBytesLeavesRegistryUnresolved(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte("binaries:\n  vision:\n    path: \"/nope/nexus-vision\"\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if got := cfg.Binaries["vision"].ResolvedPath; got != "" {
		t.Errorf("vision ResolvedPath = %q, want empty until LoadConfig resolves it", got)
	}
}

// TestLoadConfigExpandsStateDir pins the path rule for the durability key: every
// config-supplied filesystem path goes through engine.ExpandPath, so an operator
// can write `~/.nexus/broker` wherever a path is accepted.
func TestLoadConfigExpandsStateDir(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`state_dir: "~/.nexus/broker-state"`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if strings.HasPrefix(cfg.StateDir, "~") {
		t.Errorf("expected ~ to be expanded, got %q", cfg.StateDir)
	}
	if !strings.HasSuffix(cfg.StateDir, "/.nexus/broker-state") {
		t.Errorf("StateDir = %q, want the configured suffix preserved", cfg.StateDir)
	}
}

// TestLoadConfigStateDirDefaultsToDisabled pins the compatibility guarantee: a
// broker.yaml that never mentions state_dir persists nothing, so an existing
// deployment behaves exactly as it did before durability existed. A whitespace
// -only value reads as unset too, rather than as a directory named " ".
func TestLoadConfigStateDirDefaultsToDisabled(t *testing.T) {
	for _, yaml := range []string{
		``,
		"listen_addr: \":7777\"\n",
		"state_dir: \"\"\n",
		"state_dir: \"   \"\n",
	} {
		cfg, err := LoadConfigFromBytes([]byte(yaml))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes(%q): %v", yaml, err)
		}
		if cfg.StateDir != "" {
			t.Errorf("StateDir = %q for %q, want empty (persistence disabled)", cfg.StateDir, yaml)
		}
	}
}

// TestLoadConfigBrokerID: an explicit broker_id is carried verbatim; absent it
// stays empty and the store generates and persists one.
func TestLoadConfigBrokerID(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte("broker_id: \"broker-eu-1\"\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.BrokerID != "broker-eu-1" {
		t.Errorf("BrokerID = %q, want broker-eu-1", cfg.BrokerID)
	}
	if def := DefaultConfig(); def.BrokerID != "" {
		t.Errorf("DefaultConfig().BrokerID = %q, want empty", def.BrokerID)
	}
}

// TestLoadConfigAuthAbsentDisablesAuth pins the backward-compatibility
// guarantee: a broker.yaml with no auth block loads clean and yields a chain
// that is disabled (not permissive, not nil).
func TestLoadConfigAuthAbsentDisablesAuth(t *testing.T) {
	for _, yaml := range []string{
		``,
		"listen_addr: \":7777\"\n",
		"auth:\n", // present but empty: a commented-out block mid-edit
		"auth: {}\n",
		"auth:\n  validators: []\n",
	} {
		cfg, err := LoadConfigFromBytes([]byte(yaml))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes(%q): %v", yaml, err)
		}
		if cfg.AuthChain == nil {
			t.Fatalf("AuthChain is nil for %q; load must always build one", yaml)
		}
		if cfg.AuthChain.Enabled() {
			t.Errorf("AuthChain.Enabled() = true for %q, want disabled", yaml)
		}
	}
}

// TestLoadConfigAuthStaticBuildsChain proves a well-formed auth block produces
// an enabled chain, in configured order.
func TestLoadConfigAuthStaticBuildsChain(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`
auth:
  validators:
    - type: static
      tokens:
        - token: "s3cret"
          principal: "ci-runner"
          tenant: "acme"
          scopes: "broker.claim broker.release"
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if !cfg.AuthChain.Enabled() {
		t.Fatal("AuthChain.Enabled() = false, want enabled")
	}
	if got := cfg.AuthChain.Names(); !reflect.DeepEqual(got, []string{"static"}) {
		t.Errorf("Names() = %v, want [static]", got)
	}
	// The raw block is retained so it stays inspectable/loggable.
	if cfg.Auth == nil {
		t.Error("Auth = nil, want the raw block retained")
	}
}

// TestLoadConfigMalformedAuthBlockFails is the security-relevant half of the
// contract: a malformed auth block must be a BOOT FAILURE naming the offending
// key, never a silent fallback to "auth disabled".
func TestLoadConfigMalformedAuthBlockFails(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantKeys []string // substrings the error must mention
	}{
		{
			name:     "auth is not a mapping",
			yaml:     "auth: \"on\"\n",
			wantKeys: []string{"auth"},
		},
		{
			name:     "unknown key inside auth",
			yaml:     "auth:\n  validatorz: []\n",
			wantKeys: []string{"validatorz"},
		},
		{
			name:     "validators is not a list",
			yaml:     "auth:\n  validators: {}\n",
			wantKeys: []string{"validators"},
		},
		{
			name:     "validator entry missing type",
			yaml:     "auth:\n  validators:\n    - tokens: []\n",
			wantKeys: []string{"validators[0]", "type"},
		},
		{
			name:     "unknown validator type",
			yaml:     "auth:\n  validators:\n    - type: telepathy\n",
			wantKeys: []string{"telepathy"},
		},
		{
			name:     "static validator without tokens",
			yaml:     "auth:\n  validators:\n    - type: static\n",
			wantKeys: []string{"tokens"},
		},
		{
			name:     "misspelled token key",
			yaml:     "auth:\n  validators:\n    - type: static\n      tokens:\n        - tokne: x\n          principal: p\n",
			wantKeys: []string{"tokne"},
		},
		{
			name:     "token without a principal",
			yaml:     "auth:\n  validators:\n    - type: static\n      tokens:\n        - token: x\n",
			wantKeys: []string{"principal"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("LoadConfigFromBytes succeeded; want a boot failure (chain enabled=%v)",
					cfg.AuthChain.Enabled())
			}
			for _, key := range tc.wantKeys {
				if !strings.Contains(err.Error(), key) {
					t.Errorf("error %q does not name %q", err, key)
				}
			}
		})
	}
}

// TestLoadConfigAdminScope covers the broker-only `auth.admin_scope` key: it
// defaults, it can be overridden, it can be switched OFF with an empty value, and
// — the load-bearing part — it is lifted out of the block before nexusauth sees
// it. nexusauth rejects unknown keys, so a config carrying admin_scope alongside
// validators must still build a chain; if the lift regressed, every such config
// would fail to boot.
func TestLoadConfigAdminScope(t *testing.T) {
	const withValidators = `
auth:
  admin_scope: %s
  validators:
    - type: static
      tokens:
        - token: "t"
          principal: "p"
`
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"absent keeps the default", "", defaultAdminScope},
		{"absent alongside an auth block keeps the default", "auth:\n  validators: []\n", defaultAdminScope},
		{"explicit value wins", fmt.Sprintf(withValidators, `"ops.admin"`), "ops.admin"},
		{"surrounding whitespace is trimmed", fmt.Sprintf(withValidators, `"  ops.admin  "`), "ops.admin"},
		{"empty string disables the operator view", fmt.Sprintf(withValidators, `""`), ""},
		{"null disables the operator view", fmt.Sprintf(withValidators, ``), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("LoadConfigFromBytes: %v", err)
			}
			if cfg.AdminScope != tc.want {
				t.Errorf("AdminScope = %q, want %q", cfg.AdminScope, tc.want)
			}
			// The key must not survive in the block handed to nexusauth.
			if _, present := cfg.Auth[keyAdminScope]; present {
				t.Errorf("%s left in the raw auth block; nexusauth would reject it", keyAdminScope)
			}
		})
	}

	// A non-string value is a boot failure naming the key, not a silent ignore
	// that would leave the operator view wide open on the default scope.
	if _, err := LoadConfigFromBytes([]byte("auth:\n  admin_scope: 42\n")); err == nil {
		t.Fatal("LoadConfigFromBytes succeeded for a non-string admin_scope; want a boot failure")
	} else if !strings.Contains(err.Error(), keyAdminScope) {
		t.Errorf("error %q does not name %q", err, keyAdminScope)
	}
}

// TestLoadConfigAdvertiseAddrDefaultsToUnset pins the backward-compatibility
// half of E3-S1: a broker.yaml that never heard of advertise_addr must leave both
// derived fields empty, which is what makes clientWSHost behave exactly as it did
// before the key existed.
func TestLoadConfigAdvertiseAddrDefaultsToUnset(t *testing.T) {
	for _, yaml := range []string{``, "listen_addr: \":8080\"\n", "advertise_addr: \"\"\n", "advertise_addr:\n"} {
		cfg, err := LoadConfigFromBytes([]byte(yaml))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes(%q): %v", yaml, err)
		}
		if cfg.AdvertiseHost != "" || cfg.AdvertiseScheme != "" {
			t.Errorf("for %q: AdvertiseScheme/Host = %q/%q, want both empty",
				yaml, cfg.AdvertiseScheme, cfg.AdvertiseHost)
		}
	}
}

// TestLoadConfigAdvertiseAddrParsed covers every accepted form: the bare
// host:port (which must NOT set a scheme, so ws:// stays the default) and the
// scheme-qualified forms, including http/https normalization and the
// port-optional case.
func TestLoadConfigAdvertiseAddrParsed(t *testing.T) {
	cases := []struct {
		raw        string
		wantScheme string
		wantHost   string
	}{
		{"broker.example.com:8443", "", "broker.example.com:8443"},
		{"  broker.example.com:8443  ", "", "broker.example.com:8443"}, // trimmed
		{"10.0.0.7:8080", "", "10.0.0.7:8080"},
		{"[2001:db8::1]:8080", "", "[2001:db8::1]:8080"},
		{"ws://broker.example.com:8080", "ws", "broker.example.com:8080"},
		{"wss://broker.example.com:443", "wss", "broker.example.com:443"},
		{"wss://broker.example.com", "wss", "broker.example.com"}, // port optional with a scheme
		{"http://broker.example.com:8080", "ws", "broker.example.com:8080"},
		{"https://broker.example.com", "wss", "broker.example.com"},
		{"wss://broker.example.com/", "wss", "broker.example.com"}, // bare trailing slash is not a path
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes([]byte("advertise_addr: " + fmt.Sprintf("%q", tc.raw) + "\n"))
			if err != nil {
				t.Fatalf("LoadConfigFromBytes: %v", err)
			}
			if cfg.AdvertiseScheme != tc.wantScheme {
				t.Errorf("AdvertiseScheme = %q, want %q", cfg.AdvertiseScheme, tc.wantScheme)
			}
			if cfg.AdvertiseHost != tc.wantHost {
				t.Errorf("AdvertiseHost = %q, want %q", cfg.AdvertiseHost, tc.wantHost)
			}
			// The raw value is retained verbatim (modulo nothing) for logging.
			if cfg.AdvertiseAddr != tc.raw {
				t.Errorf("AdvertiseAddr = %q, want the raw value %q", cfg.AdvertiseAddr, tc.raw)
			}
		})
	}
}

// TestLoadConfigMalformedAdvertiseAddrFails is the story's boot-failure
// criterion: a value that could not produce a dialable URL must stop startup,
// naming the key, rather than surfacing as clients failing to connect later.
func TestLoadConfigMalformedAdvertiseAddrFails(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"bare host without a port", "broker.example.com"},
		{"port only", ":8080"},
		{"wildcard bare", "0.0.0.0:8080"},
		{"wildcard v6 bare", "[::]:8080"},
		{"wildcard in url form", "ws://0.0.0.0:8080"},
		{"scheme with no host", "wss://"},
		{"unsupported scheme", "tcp://broker.example.com:8080"},
		{"carries a path", "wss://broker.example.com/gateway"},
		{"carries a query", "wss://broker.example.com?x=1"},
		{"carries userinfo", "wss://user:pw@broker.example.com"},
		{"too many colons", "a:b:c"},
		{"not parseable as a url", "ws://[::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes([]byte("advertise_addr: " + fmt.Sprintf("%q", tc.raw) + "\n"))
			if err == nil {
				t.Fatalf("LoadConfigFromBytes(%q) succeeded (host=%q); want a boot failure",
					tc.raw, cfg.AdvertiseHost)
			}
			if !strings.Contains(err.Error(), "advertise_addr") {
				t.Errorf("error %q does not name advertise_addr", err)
			}
		})
	}
}

func TestLoadConfigPartialOverrideKeepsDefaults(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`listen_addr: ":7777"`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.ListenAddr != ":7777" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	def := DefaultConfig()
	if cfg.MaxConcurrent != def.MaxConcurrent {
		t.Errorf("MaxConcurrent = %d, want default %d", cfg.MaxConcurrent, def.MaxConcurrent)
	}
	if cfg.IdleTimeout != def.IdleTimeout {
		t.Errorf("IdleTimeout = %v, want default %v", cfg.IdleTimeout, def.IdleTimeout)
	}
}

// TestA2ATaskSettingsResolve covers the `a2a.tasks:` block: defaults when a key
// is absent, DISABLED when it is written as zero, and a boot failure for a value
// that has no reading.
//
// The absent/zero distinction is the whole reason the block is parsed through
// pointers, and it is the one a test has to pin: reading an absent key as zero
// would silently switch retention off for every broker that never configured it.
func TestA2ATaskSettingsResolve(t *testing.T) {
	for _, c := range []struct {
		name          string
		yaml          string
		wantTTL       time.Duration
		wantPerCtx    int
		wantInput     time.Duration
		wantLoadError string
	}{
		{
			name:       "absent block keeps every default",
			yaml:       "listen_addr: \":8080\"\n",
			wantTTL:    defaultA2ATaskTTL,
			wantPerCtx: defaultA2ATasksPerContext,
			wantInput:  defaultA2AInputTimeout,
		},
		{
			name:       "one key set leaves the others alone",
			yaml:       "a2a:\n  tasks:\n    ttl: 1h\n",
			wantTTL:    time.Hour,
			wantPerCtx: defaultA2ATasksPerContext,
			wantInput:  defaultA2AInputTimeout,
		},
		{
			name:       "zero disables rather than defaulting",
			yaml:       "a2a:\n  tasks:\n    ttl: 0s\n    max_per_context: 0\n    input_timeout: 0s\n",
			wantTTL:    0,
			wantPerCtx: 0,
			wantInput:  0,
		},
		{
			name:       "every knob set",
			yaml:       "a2a:\n  tasks:\n    ttl: 72h\n    max_per_context: 10\n    input_timeout: 90s\n",
			wantTTL:    72 * time.Hour,
			wantPerCtx: 10,
			wantInput:  90 * time.Second,
		},
		{
			name:          "a negative ttl is refused",
			yaml:          "a2a:\n  tasks:\n    ttl: -1h\n",
			wantLoadError: "a2a.tasks.ttl",
		},
		{
			name:          "a negative cap is refused",
			yaml:          "a2a:\n  tasks:\n    max_per_context: -1\n",
			wantLoadError: "a2a.tasks.max_per_context",
		},
		{
			name:          "a negative input timeout is refused",
			yaml:          "a2a:\n  tasks:\n    input_timeout: -5m\n",
			wantLoadError: "a2a.tasks.input_timeout",
		},
		{
			// A duration is a duration STRING, never a bare number: "600" reads as
			// ten minutes to an operator and six hundred nanoseconds to Go, and
			// silently picking either would be worse than an error naming the key.
			name:          "a bare number is refused",
			yaml:          "a2a:\n  tasks:\n    ttl: 600\n",
			wantLoadError: "time.Duration",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes([]byte(c.yaml))
			if c.wantLoadError != "" {
				if err == nil {
					t.Fatalf("the config loaded; want an error naming %s", c.wantLoadError)
				}
				if !strings.Contains(err.Error(), c.wantLoadError) {
					t.Fatalf("error = %v, want it to name %s", err, c.wantLoadError)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfigFromBytes: %v", err)
			}
			if cfg.A2ATaskRetention.ttl != c.wantTTL {
				t.Errorf("ttl = %v, want %v", cfg.A2ATaskRetention.ttl, c.wantTTL)
			}
			if cfg.A2ATaskRetention.maxPerContext != c.wantPerCtx {
				t.Errorf("max_per_context = %d, want %d", cfg.A2ATaskRetention.maxPerContext, c.wantPerCtx)
			}
			if cfg.A2AInputTimeout != c.wantInput {
				t.Errorf("input_timeout = %v, want %v", cfg.A2AInputTimeout, c.wantInput)
			}
		})
	}
}

// TestLoadConfigInheritEnv covers the broker-level pass-through list: what a
// valid document yields, and the three shapes that fail the boot.
//
// The list is returned trimmed, de-duplicated and SORTED because it decides a
// spawned process's environment, and that environment is already assembled in
// sorted order so a boot is reproducible.
func TestLoadConfigInheritEnv(t *testing.T) {
	t.Run("trims, dedupes and sorts", func(t *testing.T) {
		cfg, err := LoadConfigFromBytes([]byte(`
inherit_env:
  - OPENAI_API_KEY
  - "  ANTHROPIC_API_KEY  "
  - OPENAI_API_KEY
`))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes: %v", err)
		}
		want := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
		if !reflect.DeepEqual(cfg.InheritEnv, want) {
			t.Errorf("InheritEnv = %v, want %v", cfg.InheritEnv, want)
		}
	})

	t.Run("defaults to empty", func(t *testing.T) {
		cfg, err := LoadConfigFromBytes([]byte("listen_addr: \":9090\"\n"))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes: %v", err)
		}
		if len(cfg.InheritEnv) != 0 {
			t.Errorf("InheritEnv = %v, want empty: an undeclared broker must forward nothing beyond the always-pass set", cfg.InheritEnv)
		}
	})

	for name, tc := range map[string]struct{ yaml, wants string }{
		"empty entry": {
			yaml:  "inherit_env:\n  - \"\"\n",
			wants: "is empty",
		},
		"name=value pair": {
			yaml:  "inherit_env:\n  - \"FOO=bar\"\n",
			wants: "not a variable name",
		},
		"broker-owned name": {
			yaml:  "inherit_env:\n  - NEXUS_BROKER_SPAWN_SECRET\n",
			wants: "cannot be inherited",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfigFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("LoadConfigFromBytes accepted %q", tc.yaml)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wants)
			}
		})
	}
}

// TestLoadConfig_RunAsEntryOverridesBrokerDefault pins the placement decision:
// the broker-level key is a DEFAULT, and an entry that declares its own replaces
// it outright rather than merging field by field.
//
// Wholesale replacement matters because a merged credential — a uid from the
// entry and a gid from the broker block — is one nobody wrote down, and it would
// be the credential a whole variant's files end up owned by.
func TestLoadConfig_RunAsEntryOverridesBrokerDefault(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`
run_as:
  uid: 1000
  gid: 1000
binaries:
  vision:
    path: /opt/builds/nexus-vision
    run_as:
      uid: 1002
      gid: 1003
  support:
    path: /opt/builds/nexus-support
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}

	want := map[string][2]int{
		// The entry's own.
		"vision": {1002, 1003},
		// The broker default, folded in.
		"support":          {1000, 1000},
		reservedBinaryName: {1000, 1000},
	}
	for name, ids := range want {
		entry, ok := cfg.Binaries[name]
		if !ok {
			t.Fatalf("registry has no %q entry", name)
		}
		if entry.RunAs == nil || entry.RunAs.UID == nil || entry.RunAs.GID == nil {
			t.Fatalf("%s: run_as = %+v, want uid %d gid %d", name, entry.RunAs, ids[0], ids[1])
		}
		if *entry.RunAs.UID != ids[0] || *entry.RunAs.GID != ids[1] {
			t.Errorf("%s: run_as = %d:%d, want %d:%d", name, *entry.RunAs.UID, *entry.RunAs.GID, ids[0], ids[1])
		}
	}

	// Folded as a COPY: resolveRunAsHomes writes a per-entry answer onto the spec,
	// so two entries sharing the broker default must not share one struct.
	if cfg.Binaries["support"].RunAs == cfg.Binaries[reservedBinaryName].RunAs {
		t.Error("two entries share one RunAsSpec; a per-entry resolution would leak between them")
	}
}

// TestLoadConfig_RunAsWithoutRunAsIsUnchanged is the compatibility assertion: a
// config that never mentions the key leaves every entry with no credential at
// all, which is what makes the spawn path byte-identical to what it was.
func TestLoadConfig_RunAsWithoutRunAsIsUnchanged(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`
binaries:
  vision:
    path: /opt/builds/nexus-vision
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.RunAs != nil {
		t.Errorf("cfg.RunAs = %+v, want nil", cfg.RunAs)
	}
	for name, entry := range cfg.Binaries {
		if entry.RunAs != nil {
			t.Errorf("%s: run_as = %+v, want nil when the key is never written", name, entry.RunAs)
		}
	}
}

// TestLoadConfig_RunAsInvalidValueFailsBoot checks that every malformed
// credential is a BOOT failure naming where it was written — the same precedent
// the registry already sets for a missing or non-executable path.
//
// A half-written block is refused rather than completed: a uid with no gid leaves
// instances in the broker's primary group, which looks like a boundary in the
// config and is not one on disk.
func TestLoadConfig_RunAsInvalidValueFailsBoot(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantParts []string
	}{
		{
			name: "negative uid on an entry",
			yaml: "binaries:\n  vision:\n    path: /opt/nexus-vision\n    run_as:\n      uid: -1\n      gid: 20\n",
			// Names the entry, the field, and the offending value.
			wantParts: []string{"binaries: vision: run_as.uid", "-1"},
		},
		{
			name:      "uid without gid on an entry",
			yaml:      "binaries:\n  vision:\n    path: /opt/nexus-vision\n    run_as:\n      uid: 1002\n",
			wantParts: []string{"binaries: vision: run_as", "gid"},
		},
		{
			name:      "gid without uid on an entry",
			yaml:      "binaries:\n  vision:\n    path: /opt/nexus-vision\n    run_as:\n      gid: 1002\n",
			wantParts: []string{"binaries: vision: run_as", "uid"},
		},
		{
			name:      "empty block on an entry",
			yaml:      "binaries:\n  vision:\n    path: /opt/nexus-vision\n    run_as: {}\n",
			wantParts: []string{"binaries: vision: run_as", "neither uid nor gid"},
		},
		{
			name:      "out-of-range gid on an entry",
			yaml:      "binaries:\n  vision:\n    path: /opt/nexus-vision\n    run_as:\n      uid: 1002\n      gid: 4294967296\n",
			wantParts: []string{"binaries: vision: run_as.gid", "4294967296"},
		},
		{
			name:      "negative uid at broker level",
			yaml:      "run_as:\n  uid: -5\n  gid: 20\n",
			wantParts: []string{"run_as.uid", "-5"},
		},
		{
			name:      "half-written block at broker level",
			yaml:      "run_as:\n  gid: 20\n",
			wantParts: []string{"run_as", "uid"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatal("LoadConfigFromBytes accepted an invalid run_as")
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error = %v, want it to mention %q", err, part)
				}
			}
		})
	}
}

// TestResolveRunAsHomes_UsesTheEntrysDeclaredHome checks the escape hatch and,
// with it, the property that makes the boot survivable in a container with no
// passwd entry for the uid: an entry whose `env` sets HOME is never looked up.
func TestResolveRunAsHomes_UsesTheEntrysDeclaredHome(t *testing.T) {
	// A uid no passwd database is going to hold, so a lookup would certainly fail.
	uid, gid := 4294967290, 4294967290
	binaries := map[string]BinaryEntry{
		"vision": {
			Path:         "/opt/builds/nexus-vision",
			ResolvedPath: "/opt/builds/nexus-vision",
			Env:          map[string]string{"HOME": "/var/lib/nexus/vision"},
			RunAs:        &RunAsSpec{UID: &uid, GID: &gid},
		},
	}
	if err := resolveRunAsHomes(binaries); err != nil {
		t.Fatalf("resolveRunAsHomes: %v", err)
	}
	if got := binaries["vision"].RunAs.ResolvedHome; got != "" {
		t.Errorf("ResolvedHome = %q, want empty: the entry's env sets HOME and that value stands", got)
	}
}

// TestResolveRunAsHomes_UnresolvableUIDFailsBoot pins the other side: run_as
// without a HOME story is broken on arrival, so a uid the broker cannot resolve
// a home for refuses the boot and names the key that fixes it, rather than
// spawning instances that fail at their first write.
func TestResolveRunAsHomes_UnresolvableUIDFailsBoot(t *testing.T) {
	uid, gid := 4294967290, 4294967290
	if _, err := runAsHomeDir(uid); err == nil {
		t.Skipf("uid %d resolves on this host, so there is no failure to observe", uid)
	}

	binaries := map[string]BinaryEntry{
		"vision": {
			Path:         "/opt/builds/nexus-vision",
			ResolvedPath: "/opt/builds/nexus-vision",
			RunAs:        &RunAsSpec{UID: &uid, GID: &gid},
		},
	}
	err := resolveRunAsHomes(binaries)
	if err == nil {
		t.Fatal("resolveRunAsHomes accepted a uid with no resolvable home")
	}
	for _, part := range []string{"binaries: vision", "run_as.uid", "binaries.vision.env.HOME"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error = %v, want it to mention %q", err, part)
		}
	}
}

// TestResolveRunAsHomes_ResolvesFromPasswd exercises the real lookup against the
// one uid every host is guaranteed to be able to answer for: the broker's own.
func TestResolveRunAsHomes_ResolvesFromPasswd(t *testing.T) {
	uid := os.Getuid()
	gid := os.Getgid()
	want, err := runAsHomeDir(uid)
	if err != nil {
		t.Skipf("this host cannot resolve its own uid %d in the passwd database: %v", uid, err)
	}

	binaries := map[string]BinaryEntry{
		reservedBinaryName: {
			Path:         "nexus",
			ResolvedPath: "/usr/local/bin/nexus",
			RunAs:        &RunAsSpec{UID: &uid, GID: &gid},
		},
	}
	if err := resolveRunAsHomes(binaries); err != nil {
		t.Fatalf("resolveRunAsHomes: %v", err)
	}
	if got := binaries[reservedBinaryName].RunAs.ResolvedHome; got != want {
		t.Errorf("ResolvedHome = %q, want %q", got, want)
	}
}
