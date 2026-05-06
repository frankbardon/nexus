package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// fakePlugin is a stand-in for tests that need a Plugin without booting any
// real implementation. ConfigSchema is wired separately via fakeSchemaPlugin
// so a plugin without ConfigSchema is also representable.
type fakePlugin struct {
	id string
}

func (p *fakePlugin) ID() string                     { return p.id }
func (p *fakePlugin) Name() string                   { return p.id }
func (p *fakePlugin) Version() string                { return "0.0.0" }
func (p *fakePlugin) Dependencies() []string         { return nil }
func (p *fakePlugin) Requires() []Requirement        { return nil }
func (p *fakePlugin) Capabilities() []Capability     { return nil }
func (p *fakePlugin) Init(PluginContext) error       { return nil }
func (p *fakePlugin) Ready() error                   { return nil }
func (p *fakePlugin) Shutdown(context.Context) error { return nil }
func (p *fakePlugin) Subscriptions() []EventSubscription {
	return nil
}
func (p *fakePlugin) Emissions() []string { return nil }

// fakeSchemaPlugin attaches a ConfigSchema to a fakePlugin so the validator
// recognizes it as a ConfigSchemaProvider.
type fakeSchemaPlugin struct {
	fakePlugin
	schema []byte
}

func (p *fakeSchemaPlugin) ConfigSchema() []byte { return p.schema }

const minimalSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "name":  {"type": "string"},
    "count": {"type": "integer", "minimum": 1},
    "old":   {"type": "boolean", "deprecated": true}
  }
}`

// captureLogs returns a fresh logger plus a buffer the caller can inspect.
func captureLogs() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, buf
}

func newCfgWith(pluginID string, raw map[string]any) *Config {
	cfg := DefaultConfig()
	cfg.Plugins.Configs = map[string]map[string]any{
		pluginID: raw,
	}
	return cfg
}

func TestValidateConfigSchemas_ValidPasses(t *testing.T) {
	logger, _ := captureLogs()
	pid := "test.valid"
	plugins := map[string]Plugin{
		pid: &fakeSchemaPlugin{fakePlugin: fakePlugin{id: pid}, schema: []byte(minimalSchema)},
	}
	cfg := newCfgWith(pid, map[string]any{"name": "ok", "count": 3})

	if err := validateConfigSchemas(cfg, plugins, []string{pid}, logger); err != nil {
		t.Fatalf("expected nil, got error: %v", err)
	}
}

func TestValidateConfigSchemas_UnknownKeyFails(t *testing.T) {
	logger, _ := captureLogs()
	pid := "test.unknown"
	plugins := map[string]Plugin{
		pid: &fakeSchemaPlugin{fakePlugin: fakePlugin{id: pid}, schema: []byte(minimalSchema)},
	}
	cfg := newCfgWith(pid, map[string]any{"name": "ok", "wat": 5})

	err := validateConfigSchemas(cfg, plugins, []string{pid}, logger)
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), `"wat"`) {
		t.Fatalf("expected error to mention 'wat', got: %v", err)
	}
}

func TestValidateConfigSchemas_TypeMismatchFails(t *testing.T) {
	logger, _ := captureLogs()
	pid := "test.typemismatch"
	plugins := map[string]Plugin{
		pid: &fakeSchemaPlugin{fakePlugin: fakePlugin{id: pid}, schema: []byte(minimalSchema)},
	}
	cfg := newCfgWith(pid, map[string]any{"count": "not-an-int"})

	err := validateConfigSchemas(cfg, plugins, []string{pid}, logger)
	if err == nil {
		t.Fatal("expected type mismatch error")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Fatalf("expected error to mention 'count', got: %v", err)
	}
}

func TestValidateConfigSchemas_DeprecatedKeyWarns(t *testing.T) {
	logger, buf := captureLogs()
	pid := "test.deprecated"
	plugins := map[string]Plugin{
		pid: &fakeSchemaPlugin{fakePlugin: fakePlugin{id: pid}, schema: []byte(minimalSchema)},
	}
	cfg := newCfgWith(pid, map[string]any{"name": "ok", "old": true})

	if err := validateConfigSchemas(cfg, plugins, []string{pid}, logger); err != nil {
		t.Fatalf("expected nil with only deprecated key, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "deprecated") || !strings.Contains(out, "old") {
		t.Fatalf("expected deprecation warning in logs, got: %s", out)
	}
}

func TestValidateConfigSchemas_NoSchemaIsDebug(t *testing.T) {
	logger, buf := captureLogs()
	pid := "test.noschema"
	plugins := map[string]Plugin{
		pid: &fakePlugin{id: pid},
	}
	cfg := newCfgWith(pid, map[string]any{"anything": 1})

	if err := validateConfigSchemas(cfg, plugins, []string{pid}, logger); err != nil {
		t.Fatalf("expected nil for plugin without schema, got: %v", err)
	}
	if !strings.Contains(buf.String(), "no schema") {
		t.Fatalf("expected debug log mentioning 'no schema', got: %s", buf.String())
	}
}

func TestValidateConfigSchemas_MultiplePluginErrorsAggregated(t *testing.T) {
	logger, _ := captureLogs()
	a, b := "test.a", "test.b"
	plugins := map[string]Plugin{
		a: &fakeSchemaPlugin{fakePlugin: fakePlugin{id: a}, schema: []byte(minimalSchema)},
		b: &fakeSchemaPlugin{fakePlugin: fakePlugin{id: b}, schema: []byte(minimalSchema)},
	}
	cfg := DefaultConfig()
	cfg.Plugins.Configs = map[string]map[string]any{
		a: {"unknown_a": 1},
		b: {"unknown_b": 2},
	}

	err := validateConfigSchemas(cfg, plugins, []string{a, b}, logger)
	if err == nil {
		t.Fatal("expected aggregate error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown_a") || !strings.Contains(msg, "unknown_b") {
		t.Fatalf("expected both plugin errors aggregated, got: %s", msg)
	}
	if !strings.Contains(msg, "2 errors") {
		t.Fatalf("expected '2 errors' in summary, got: %s", msg)
	}
}

func TestValidateEngineConfig_BadDrainTimeoutShape(t *testing.T) {
	cfg := DefaultConfig()
	// Insert a structurally-broken drain_timeout via the raw plugins block —
	// the engine schema only owns engine.shutdown.drain_timeout, so we
	// validate the engine-only path directly.
	res := validateEngineConfig(cfg)
	if res == nil {
		t.Fatal("expected validateEngineConfig to return result")
	}
	// Default should be valid.
	if len(res.errors) != 0 {
		t.Fatalf("default config should validate clean, got: %v", res.errors)
	}

	// Now corrupt the engine block via the typed config: a malformed
	// drain_timeout via reflection of the typed struct is hard; instead
	// inject directly through a nested raw assertion by wrapping
	// validateOneConfig with a known-bad map.
	bad := map[string]any{
		"engine": map[string]any{
			"shutdown": map[string]any{
				"drain_timeout": []string{"this is wrong"},
			},
		},
	}
	pres := validateOneConfig("(engine)", engineConfigSchemaBytes, bad, nil, "")
	if pres == nil || len(pres.errors) == 0 {
		t.Fatalf("expected validation error for bad drain_timeout shape; got: %+v", pres)
	}
}

func TestValidateConfigSchemas_DidYouMeanSuggests(t *testing.T) {
	logger, _ := captureLogs()
	pid := "test.suggest"
	plugins := map[string]Plugin{
		pid: &fakeSchemaPlugin{fakePlugin: fakePlugin{id: pid}, schema: []byte(minimalSchema)},
	}
	// "naem" → "name" (one transposition).
	cfg := newCfgWith(pid, map[string]any{"naem": "ok"})

	err := validateConfigSchemas(cfg, plugins, []string{pid}, logger)
	if err == nil {
		t.Fatal("expected unknown-key error with did-you-mean")
	}
	if !strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("expected did-you-mean suggestion, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"name"`) {
		t.Fatalf("expected suggestion to mention 'name', got: %v", err)
	}
}
