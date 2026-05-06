package engine

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	jsc "github.com/santhosh-tekuri/jsonschema/v6"
	jsckind "github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// ConfigSchemaProvider is an optional interface a plugin implements to expose
// a JSON Schema describing the shape of its config block. The engine type-
// asserts on plugin instances during boot and validates the raw config map
// against the schema before Init is called. Plugins that do not implement
// the interface are skipped with an info log.
//
// The schema is canonical for the plugin's config — the same map[string]any
// passed to Init is what gets validated. Schemas should declare
// `additionalProperties: false` at every object level so unknown keys
// (typos) fail loudly. See docs/src/configuration/reference.md for the
// authoring guide.
type ConfigSchemaProvider interface {
	ConfigSchema() []byte
}

//go:embed engine_schema.json
var engineConfigSchemaBytes []byte

// SmokeValidateConfig runs the same schema validation pass that lifecycle
// Boot performs, but without instantiating any of the plugin's Init logic.
// Tests use it to verify that every shipped YAML config continues to satisfy
// every plugin's schema across the active set as declared in YAML. Returns
// the aggregate error verbatim from the validator so failures point at the
// offending key. Auto-activated requirements are NOT included — they receive
// engine-supplied defaults (already known-valid) and would require running
// expandRequirements which is part of Boot proper.
func SmokeValidateConfig(eng *Engine) error {
	if eng == nil || eng.Config == nil || eng.Registry == nil {
		return nil
	}
	plugins := make(map[string]Plugin, len(eng.Config.Plugins.Active))
	active := make([]string, 0, len(eng.Config.Plugins.Active))
	for _, id := range eng.Config.Plugins.Active {
		factory, ok := eng.Registry.Get(PluginBaseID(id))
		if !ok {
			return fmt.Errorf("plugin %q not registered", id)
		}
		plugins[id] = factory()
		active = append(active, id)
	}
	return validateConfigSchemas(eng.Config, plugins, active, eng.Logger)
}

// configValidationResult aggregates findings across one validation pass so the
// operator sees every typo/type error at once instead of one-at-a-time.
type configValidationResult struct {
	errors   []string
	warnings []string
}

func (r *configValidationResult) addError(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *configValidationResult) addWarning(format string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
}

func (r *configValidationResult) merge(other *configValidationResult) {
	if other == nil {
		return
	}
	r.errors = append(r.errors, other.errors...)
	r.warnings = append(r.warnings, other.warnings...)
}

// validateConfigSchemas runs schema validation across the engine top-level
// config and every plugin instance that advertises a ConfigSchemaProvider.
// All errors are aggregated into a single message so operators see every
// issue at once. Warnings (deprecated keys, missing schemas) are logged via
// the engine logger but do not fail boot.
func validateConfigSchemas(cfg *Config, plugins map[string]Plugin, activeIDs []string, logger *slog.Logger) error {
	if cfg == nil {
		return nil
	}

	result := &configValidationResult{}

	if engineRes := validateEngineConfig(cfg); engineRes != nil {
		result.merge(engineRes)
	}

	withSchema := 0
	withoutSchema := 0
	for _, id := range activeIDs {
		p, ok := plugins[id]
		if !ok || p == nil {
			continue
		}
		csp, ok := p.(ConfigSchemaProvider)
		if !ok {
			withoutSchema++
			if logger != nil {
				logger.Debug("plugin config not validated (no schema)", "plugin", id)
			}
			continue
		}
		schemaBytes := csp.ConfigSchema()
		if len(schemaBytes) == 0 {
			withoutSchema++
			if logger != nil {
				logger.Debug("plugin config not validated (empty schema)", "plugin", id)
			}
			continue
		}
		withSchema++

		pluginCfg := cfg.Plugins.Configs[id]
		if pluginCfg == nil {
			pluginCfg = map[string]any{}
		}
		pres := validateOneConfig(fmt.Sprintf("plugins.%s", id), schemaBytes, pluginCfg, logger, id)
		result.merge(pres)
	}

	if logger != nil {
		logger.Debug("config schema validation complete",
			"plugins_validated", withSchema,
			"plugins_unvalidated", withoutSchema,
			"errors", len(result.errors),
			"warnings", len(result.warnings))
	}

	for _, w := range result.warnings {
		if logger != nil {
			logger.Warn("config validation: " + w)
		}
	}

	if len(result.errors) == 0 {
		return nil
	}
	return formatValidationErrors(result.errors)
}

// validateEngineConfig walks the engine top-level YAML (rebuilt from the
// typed Config) and checks it against engineConfigSchemaBytes.
func validateEngineConfig(cfg *Config) *configValidationResult {
	res := &configValidationResult{}

	// Reconstruct the top-level shape. We can't reuse cfg.Raw directly because
	// per-plugin configs use yaml:"-" — we want only the engine-owned keys
	// (core, engine, plugins.active, capabilities, journal). Rebuilding by
	// hand keeps the schema honest about what the engine itself owns.
	top := map[string]any{
		"core": map[string]any{
			"log_level":             cfg.Core.LogLevel,
			"tick_interval":         durationToString(cfg.Core.TickInterval),
			"max_concurrent_events": cfg.Core.MaxConcurrentEvents,
			"agent_id":              cfg.Core.AgentID,
			"sessions": map[string]any{
				"root":      cfg.Core.Sessions.Root,
				"retention": cfg.Core.Sessions.Retention,
				"id_format": cfg.Core.Sessions.IDFormat,
			},
			"storage": map[string]any{
				"root":            cfg.Core.Storage.Root,
				"busy_timeout_ms": cfg.Core.Storage.BusyTimeoutMs,
				"cache_size_kb":   cfg.Core.Storage.CacheSizeKB,
				"pool_max_idle":   cfg.Core.Storage.PoolMaxIdle,
				"pool_max_open":   cfg.Core.Storage.PoolMaxOpen,
			},
			"logging": map[string]any{
				"bootstrap_stderr": cfg.Core.Logging.BootstrapStderr,
				"buffer_size":      cfg.Core.Logging.BufferSize,
			},
		},
		"engine": map[string]any{
			"shutdown": map[string]any{
				"drain_timeout": durationToString(cfg.Engine.Shutdown.DrainTimeout),
			},
		},
		"plugins": map[string]any{
			"active": cfg.Plugins.Active,
		},
		"journal": map[string]any{
			"fsync":          cfg.Journal.Fsync,
			"retain_days":    cfg.Journal.RetainDays,
			"rotate_size_mb": cfg.Journal.RotateSizeMB,
		},
	}
	if cfg.Core.ModelsRaw != nil {
		top["core"].(map[string]any)["models"] = cfg.Core.ModelsRaw
	}
	if len(cfg.Capabilities) > 0 {
		caps := make(map[string]any, len(cfg.Capabilities))
		for k, v := range cfg.Capabilities {
			caps[k] = v
		}
		top["capabilities"] = caps
	}

	// Drop empty sub-maps so the schema's "if present, must be valid" semantics
	// don't trip on zero-valued sections the user never set.
	stripEmptyBranches(top)

	pres := validateOneConfig("(engine)", engineConfigSchemaBytes, top, nil, "")
	res.merge(pres)
	return res
}

// validateOneConfig compiles the schema, validates the config, and returns
// any errors/warnings. logger and pluginID are optional; when set, deprecated-
// key warnings are logged with plugin context.
func validateOneConfig(scope string, schemaBytes []byte, cfg map[string]any, logger *slog.Logger, pluginID string) *configValidationResult {
	res := &configValidationResult{}

	schema, err := compileSchemaBytes(schemaBytes)
	if err != nil {
		res.addError("%s: invalid schema: %v", scope, err)
		return res
	}

	// Convert YAML-decoded map[string]any into JSON-shaped any tree the
	// validator expects. yaml.v3 already gives us map[string]any (not
	// map[interface{}]interface{}), but nested values may include time.Duration
	// from the typed Config rebuild; fall through json round-trip to normalize.
	normalized, err := jsonRoundTrip(cfg)
	if err != nil {
		res.addError("%s: cannot normalize config for validation: %v", scope, err)
		return res
	}

	if err := schema.Validate(normalized); err != nil {
		ve, ok := err.(*jsc.ValidationError)
		if !ok {
			res.addError("%s: %v", scope, err)
			return res
		}
		collectValidationErrors(scope, ve, schemaBytes, cfg, res)
	}

	// Walk the schema for `deprecated: true` keys present in cfg and warn.
	deprecated := collectDeprecated(schemaBytes)
	if len(deprecated) > 0 {
		flagDeprecatedKeys(scope, normalized, deprecated, res)
	}

	return res
}

// compileSchemaBytes wraps the jsonschema/v6 compiler.
func compileSchemaBytes(schemaBytes []byte) (*jsc.Schema, error) {
	c := jsc.NewCompiler()
	parsed, err := jsc.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	if err := c.AddResource("schema.json", parsed); err != nil {
		return nil, fmt.Errorf("add resource: %w", err)
	}
	return c.Compile("schema.json")
}

// collectValidationErrors flattens the jsonschema error tree into operator-
// friendly strings. We special-case `additionalProperties` failures so the
// reported message names the unknown key directly (typo-detection UX).
func collectValidationErrors(scope string, ve *jsc.ValidationError, schemaBytes []byte, cfg map[string]any, res *configValidationResult) {
	if ve == nil {
		return
	}
	if len(ve.Causes) == 0 {
		path := joinPath(scope, ve.InstanceLocation)
		// additionalProperties: ErrorKind carries the offending key list directly
		// — preferred over message-string parsing. Suggestions come from the
		// schema's declared properties at the same path, not the cfg map (the
		// cfg holds the unknown key itself, which would make it suggest itself).
		if ap, ok := ve.ErrorKind.(*jsckind.AdditionalProperties); ok && len(ap.Properties) > 0 {
			known := knownPropertiesAtPath(schemaBytes, ve.InstanceLocation)
			for _, unknown := range ap.Properties {
				if suggestion := suggestKey(unknown, known); suggestion != "" {
					res.addError(`%s: unknown key %q (did you mean %q?)`, path, unknown, suggestion)
				} else {
					res.addError(`%s: unknown key %q`, path, unknown)
				}
			}
			return
		}
		res.addError("%s: %s", path, readableValidatorMessage(ve))
		return
	}
	for _, c := range ve.Causes {
		collectValidationErrors(scope, c, schemaBytes, cfg, res)
	}
}

// readableValidatorMessage returns the bottom-most leaf message from a
// ValidationError, stripping the noisy "jsonschema: ..." preamble where it
// shows up.
func readableValidatorMessage(ve *jsc.ValidationError) string {
	msg := ve.Error()
	if idx := strings.Index(msg, "/"); idx >= 0 {
		// Trim leading "jsonschema validation failed with " preamble when present.
		if pre := strings.Index(msg, "with "); pre >= 0 && pre < idx {
			msg = msg[pre+5:]
		}
	}
	if newline := strings.Index(msg, "\n"); newline > 0 {
		// Take the most specific (last) leaf line if multi-line.
		lines := strings.Split(strings.TrimSpace(msg), "\n")
		msg = strings.TrimSpace(lines[len(lines)-1])
		msg = strings.TrimPrefix(msg, "- ")
	}
	return msg
}

// knownPropertiesAtPath walks the schema bytes to the same instance path the
// validator inspected and returns the declared `properties` keys at that
// branch. Suggestions are sourced from the schema, not the cfg, so a typo
// like `naem` does not "match itself" via the cfg snapshot.
func knownPropertiesAtPath(schemaBytes []byte, instanceLocation []string) []string {
	var schema map[string]any
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		return nil
	}
	cur := schema
	for _, part := range instanceLocation {
		props, _ := cur["properties"].(map[string]any)
		if props == nil {
			return nil
		}
		next, ok := props[part].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	props, _ := cur["properties"].(map[string]any)
	if props == nil {
		return nil
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	return keys
}

// suggestKey returns the closest known key by Levenshtein distance, capped
// at 3 edits and only when distance is strictly less than half the
// candidate's length to avoid wild guesses.
func suggestKey(unknown string, known []string) string {
	if len(known) == 0 {
		return ""
	}
	best := ""
	bestDist := len(unknown) + 1
	for _, k := range known {
		d := levenshtein(unknown, k)
		threshold := 3
		if len(k)/2 < threshold {
			threshold = len(k) / 2
		}
		if d <= threshold && d < bestDist {
			best = k
			bestDist = d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			min3 := prev[j] + 1
			if curr[j-1]+1 < min3 {
				min3 = curr[j-1] + 1
			}
			if prev[j-1]+cost < min3 {
				min3 = prev[j-1] + cost
			}
			curr[j] = min3
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

// collectDeprecated walks the raw schema bytes and returns every JSON path
// (dotted) annotated with `deprecated: true`. Used at validation time to warn
// when a deprecated key is set in the config.
func collectDeprecated(schemaBytes []byte) []string {
	var schema map[string]any
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		return nil
	}
	var out []string
	walkSchemaForDeprecated(schema, "", &out)
	return out
}

func walkSchemaForDeprecated(node any, path string, out *[]string) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if dep, _ := m["deprecated"].(bool); dep && path != "" {
		*out = append(*out, path)
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for k, v := range props {
			child := k
			if path != "" {
				child = path + "." + k
			}
			walkSchemaForDeprecated(v, child, out)
		}
	}
	if items, ok := m["items"]; ok {
		walkSchemaForDeprecated(items, path+"[]", out)
	}
}

// flagDeprecatedKeys reports a warning per deprecated key actually present in
// the config tree. Recursive: walks the cfg tree once per path.
func flagDeprecatedKeys(scope string, cfg any, deprecated []string, res *configValidationResult) {
	for _, p := range deprecated {
		if hasPath(cfg, strings.Split(p, ".")) {
			res.addWarning("%s: %s is deprecated", scope, p)
		}
	}
}

func hasPath(v any, parts []string) bool {
	cur := v
	for _, part := range parts {
		// Skip array indices (we marked them with "[]" but don't traverse).
		if part == "[]" {
			arr, ok := cur.([]any)
			if !ok || len(arr) == 0 {
				return false
			}
			cur = arr[0]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		nx, present := m[part]
		if !present {
			return false
		}
		cur = nx
	}
	return true
}

// jsonRoundTrip normalizes a config map by marshaling/unmarshaling through JSON.
// Strips Go-only types (time.Duration, etc.) and surfaces the same shape the
// jsonschema validator expects.
func jsonRoundTrip(in map[string]any) (any, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// joinPath produces a dotted error path from the scope and the validator's
// instance location.
func joinPath(scope string, instLoc []string) string {
	if len(instLoc) == 0 {
		return scope
	}
	return scope + "." + strings.Join(instLoc, ".")
}

// formatValidationErrors renders the operator-facing aggregate error.
func formatValidationErrors(errs []string) error {
	sort.Strings(errs)
	var b strings.Builder
	b.WriteString("config validation failed:\n")
	for _, e := range errs {
		b.WriteString("  - ")
		b.WriteString(e)
		b.WriteString("\n")
	}
	plural := "errors"
	if len(errs) == 1 {
		plural = "error"
	}
	fmt.Fprintf(&b, "%d %s; aborting boot", len(errs), plural)
	return fmt.Errorf("%s", b.String())
}

// stripEmptyBranches recursively removes empty maps, nil values, and zero-
// valued scalars (empty string, zero numerics) from m so the engine schema's
// strict shape doesn't trip on zero-valued sub-blocks the user never set in
// YAML. The validator focuses on YAML the user actually wrote rather than
// reconstructed Go defaults. Booleans pass through — "false" can be a
// meaningful user override.
func stripEmptyBranches(m map[string]any) {
	for k, v := range m {
		switch x := v.(type) {
		case map[string]any:
			stripEmptyBranches(x)
			if len(x) == 0 {
				delete(m, k)
			}
		case []any:
			if len(x) == 0 {
				delete(m, k)
			}
		case string:
			if x == "" {
				delete(m, k)
			}
		case int:
			if x == 0 {
				delete(m, k)
			}
		case int64:
			if x == 0 {
				delete(m, k)
			}
		case float64:
			if x == 0 {
				delete(m, k)
			}
		case nil:
			delete(m, k)
		}
	}
}

// durationToString returns "" for the zero duration so the schema's
// optional-string-duration shape accepts an unset value, otherwise the Go
// canonical Duration.String() form ("30s", "5m", "1h30m").
func durationToString(d interface{}) string {
	switch v := d.(type) {
	case nil:
		return ""
	default:
		// Use String() via fmt to avoid a hard import on time here.
		s := fmt.Sprintf("%v", v)
		if s == "0s" || s == "0" {
			return ""
		}
		return s
	}
}
