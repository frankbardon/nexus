package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// runHITL is the "nexus hitl" subcommand multiplexer. Routes to per-action
// handlers. Each handler resolves the registry directory from the active
// nexus config (matching what the running engine watches) and either lists
// pending requests or writes a response file the engine's fsnotify watcher
// will pick up.
func runHITL(args []string) int {
	if len(args) == 0 {
		hitlUsage()
		return 2
	}
	action, rest := args[0], args[1:]
	switch action {
	case "list":
		return runHITLList(rest)
	case "respond":
		return runHITLRespond(rest)
	case "cancel":
		return runHITLCancel(rest)
	default:
		hitlUsage()
		fmt.Fprintf(os.Stderr, "\nerror: unknown action %q\n", action)
		return 2
	}
}

func hitlUsage() {
	fmt.Fprintln(os.Stderr, `usage: nexus hitl <action> [flags] [<request-id>]

actions:
  list      Print pending HITL requests in the registry directory.
  respond   Write a response YAML for a pending request.
  cancel    Cancel a pending request with an optional reason.

shorthand subcommands:
  nexus approve <request-id>    Equivalent to: nexus hitl respond <id> --choice allow
  nexus reject  <request-id>    Equivalent to: nexus hitl respond <id> --choice reject

each action accepts:
  --config <path>   path to nexus config (default: nexus.yaml)`)
}

// runHITLList prints one line per pending request.
func runHITLList(args []string) int {
	fs := flag.NewFlagSet("hitl list", flag.ExitOnError)
	configPath := fs.String("config", "nexus.yaml", "path to config file")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus hitl list [--config NEXUS.YAML]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	dir, err := resolveHITLRegistryDir(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	entries, err := readHITLRequests(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "no pending HITL requests.")
		return 0
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].RequestID < entries[j].RequestID
	})
	for _, e := range entries {
		fmt.Printf("%s  %s  %s\n", e.RequestID, displayActionKind(e.ActionKind), truncatePrompt(e.Prompt, 60))
	}
	return 0
}

// runHITLRespond writes a response YAML so the running engine's fsnotify
// watcher dispatches it. Supports --choice / --free-text / --edit.
func runHITLRespond(args []string) int {
	fs := flag.NewFlagSet("hitl respond", flag.ExitOnError)
	configPath := fs.String("config", "nexus.yaml", "path to config file")
	choice := fs.String("choice", "", "choice id (required for choices/both modes)")
	freeText := fs.String("free-text", "", "freeform answer")
	editPath := fs.String("edit", "", "path to JSON or YAML file with edited_payload")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus hitl respond <request-id> --choice <id> | --free-text <text> [--edit FILE]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	requestID := fs.Arg(0)
	if *choice == "" && *freeText == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "\nerror: provide at least one of --choice or --free-text")
		return 2
	}

	dir, err := resolveHITLRegistryDir(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	resp := events.HITLResponse{
		RequestID: requestID,
		ChoiceID:  *choice,
		FreeText:  *freeText,
	}
	if *editPath != "" {
		payload, err := readEditedPayload(*editPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		resp.EditedPayload = payload
	}

	return writeHITLResponseExit(dir, requestID, resp)
}

// runHITLCancel writes a cancelled response so the engine releases the
// pending request with the operator's reason.
func runHITLCancel(args []string) int {
	fs := flag.NewFlagSet("hitl cancel", flag.ExitOnError)
	configPath := fs.String("config", "nexus.yaml", "path to config file")
	reason := fs.String("reason", "", "cancellation reason (recommended)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus hitl cancel <request-id> [--reason \"...\"]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	requestID := fs.Arg(0)
	dir, err := resolveHITLRegistryDir(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	resp := events.HITLResponse{
		RequestID:    requestID,
		Cancelled:    true,
		CancelReason: *reason,
	}
	return writeHITLResponseExit(dir, requestID, resp)
}

// runHITLApprove is the canonical "allow" shorthand: nexus approve <id>.
func runHITLApprove(args []string) int {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	configPath := fs.String("config", "nexus.yaml", "path to config file")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus approve <request-id>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	requestID := fs.Arg(0)
	dir, err := resolveHITLRegistryDir(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return writeHITLResponseExit(dir, requestID, events.HITLResponse{
		RequestID: requestID,
		ChoiceID:  "allow",
	})
}

// runHITLReject is the canonical "reject" shorthand: nexus reject <id>.
func runHITLReject(args []string) int {
	fs := flag.NewFlagSet("reject", flag.ExitOnError)
	configPath := fs.String("config", "nexus.yaml", "path to config file")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus reject <request-id>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	requestID := fs.Arg(0)
	dir, err := resolveHITLRegistryDir(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return writeHITLResponseExit(dir, requestID, events.HITLResponse{
		RequestID: requestID,
		ChoiceID:  "reject",
	})
}

// writeHITLResponseExit writes the response YAML and prints a one-line
// confirmation. Returns the exit code for main().
func writeHITLResponseExit(dir, requestID string, resp events.HITLResponse) int {
	if err := validateHITLRequestID(requestID); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}
	path, err := writeHITLResponseYAML(dir, requestID, resp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	fmt.Printf("wrote %s\n", path)
	return 0
}

// resolveHITLRegistryDir reads the nexus config, locates the
// nexus.control.hitl plugin's `registry.dir`, expands `~`, and verifies
// `registry.enabled` is true. Mirrors the plugin's Init() parsing exactly.
func resolveHITLRegistryDir(configPath string) (string, error) {
	cfg, err := engine.LoadConfig(engine.ExpandPath(configPath))
	if err != nil {
		return "", fmt.Errorf("load config %s: %w", configPath, err)
	}
	pluginCfg, ok := cfg.Plugins.Configs["nexus.control.hitl"]
	if !ok {
		return "", fmt.Errorf("nexus.control.hitl is not configured in %s — registry disabled", configPath)
	}
	regCfg, ok := pluginCfg["registry"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("nexus.control.hitl.registry is missing — registry disabled in config")
	}
	enabled, _ := regCfg["enabled"].(bool)
	if !enabled {
		return "", fmt.Errorf("nexus.control.hitl.registry.enabled is false — registry disabled in config")
	}
	dir, _ := regCfg["dir"].(string)
	if dir == "" {
		dir = "~/.nexus/hitl"
	}
	return engine.ExpandPath(dir), nil
}

// validateHITLRequestID rejects IDs that would let a writer escape the
// registry directory. Mirrors the plugin-side check so the CLI fails early
// before producing a stray file.
func validateHITLRequestID(id string) error {
	if id == "" {
		return fmt.Errorf("request id is empty")
	}
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("request id %q contains a path separator", id)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("request id %q is invalid", id)
	}
	return nil
}

// writeHITLResponseYAML writes a response YAML to <dir>/<id>.response.yaml
// using an atomic same-directory rename so the engine's fsnotify watcher
// never observes a partial file. The on-disk shape matches the plugin's
// responseFile struct (request_id, choice_id, free_text, edited_payload,
// cancelled, cancel_reason).
func writeHITLResponseYAML(dir, requestID string, resp events.HITLResponse) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("registry dir %s: %w", dir, err)
	}
	rec := struct {
		RequestID     string         `yaml:"request_id"`
		ChoiceID      string         `yaml:"choice_id,omitempty"`
		FreeText      string         `yaml:"free_text,omitempty"`
		EditedPayload map[string]any `yaml:"edited_payload,omitempty"`
		Cancelled     bool           `yaml:"cancelled,omitempty"`
		CancelReason  string         `yaml:"cancel_reason,omitempty"`
	}{
		RequestID:     requestID,
		ChoiceID:      resp.ChoiceID,
		FreeText:      resp.FreeText,
		EditedPayload: resp.EditedPayload,
		Cancelled:     resp.Cancelled,
		CancelReason:  resp.CancelReason,
	}
	data, err := yaml.Marshal(&rec)
	if err != nil {
		return "", fmt.Errorf("marshal response: %w", err)
	}
	path := filepath.Join(dir, requestID+".response.yaml")

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("tempfile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("rename to %s: %w", path, err)
	}
	return path, nil
}

// hitlRequestRow is the trimmed subset of the on-disk request file we need
// for `nexus hitl list`. Mirrors the plugin's requestFile YAML keys.
type hitlRequestRow struct {
	RequestID  string `yaml:"request_id"`
	ActionKind string `yaml:"action_kind"`
	Prompt     string `yaml:"prompt"`
}

// readHITLRequests parses every *.request.yaml in dir into a hitlRequestRow.
// Files that fail to parse are skipped — a single bad file shouldn't blank
// the operator's listing.
func readHITLRequests(dir string) ([]hitlRequestRow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	out := make([]hitlRequestRow, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, ".request.yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var row hitlRequestRow
		if err := yaml.Unmarshal(data, &row); err != nil {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// readEditedPayload accepts either YAML or JSON. JSON is parsed first
// because YAML unmarshaling treats JSON as a superset, but we want
// explicit error messaging if the file claims .json but is malformed.
func readEditedPayload(path string) (map[string]any, error) {
	data, err := os.ReadFile(engine.ExpandPath(path))
	if err != nil {
		return nil, fmt.Errorf("read edit file %s: %w", path, err)
	}
	out := map[string]any{}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" {
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("parse JSON %s: %w", path, err)
		}
		return out, nil
	}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse YAML %s: %w", path, err)
	}
	return out, nil
}

func displayActionKind(k string) string {
	if k == "" {
		return "(unknown)"
	}
	return k
}

func truncatePrompt(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
