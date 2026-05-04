// Package metadata implements a declarative per-step model router.
//
// The plugin subscribes high-priority on before:llm.request and rewrites
// LLMRequest.Model based on rules that match against the request's Metadata
// (e.g. _source, task_kind, iteration) and Tags (e.g. tenant, project,
// source_plugin). It is the 80% case for per-step routing — fast,
// predictable, no extra LLM call. The classifier router (idea 09 part 2)
// handles the long tail where rules can't tell whether a step needs the
// strong model.
//
// Rule ordering matters: the first matching rule wins. The terminal `default`
// rule is matched last and applies when nothing else does.
package metadata

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID = "nexus.router.metadata"
)

// New creates a new metadata router instance.
func New() engine.Plugin {
	return &Plugin{}
}

// Plugin implements the metadata-driven router. It is stateless across
// requests — every routing decision is a pure function of the rule set
// and the inbound request's Metadata + Tags.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	rules        []rule
	defaultModel string
	defaultRole  string

	unsubs []func()
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Metadata Router" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	rules, def, defRole, err := parseConfig(ctx.Config)
	if err != nil {
		return fmt.Errorf("router.metadata: %w", err)
	}
	p.rules = rules
	p.defaultModel = def
	p.defaultRole = defRole

	// Priority 50: above the gates (which sit at 10) but below the engine
	// tag seeder (100) so we route on the seeded tag set, not before it
	// exists. Routing is destructive (rewrites Model), so it runs once and
	// before any cost/budget reasoning happens downstream.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	p.logger.Info("metadata router initialized",
		"rules", len(p.rules),
		"default_model", p.defaultModel,
		"default_role", p.defaultRole)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "before:llm.request", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string { return nil }

func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	// Skip when caller already pinned a specific provider — typically the
	// fallback plugin retrying after a failure, where rerouting the model
	// underneath would defeat the retry semantics. Caller-pinned models
	// should still match (a future static config might want to route
	// model-by-model), so we only skip on _target_provider.
	if _, pinned := req.Metadata["_target_provider"].(string); pinned {
		return
	}

	for i, r := range p.rules {
		if !r.matches(req) {
			continue
		}
		applyDecision(req, r.use, r.role, p.logger, i, r.name)
		return
	}

	if p.defaultModel != "" || p.defaultRole != "" {
		applyDecision(req, p.defaultModel, p.defaultRole, p.logger, -1, "default")
	}
}

// applyDecision rewrites the request's Model/Role and records the routing
// decision on the request's Metadata so journal/cost reports can later
// answer "why did we hit Sonnet here?".
func applyDecision(req *events.LLMRequest, model, role string, logger *slog.Logger, idx int, name string) {
	prevModel := req.Model
	prevRole := req.Role
	if model != "" {
		req.Model = model
	}
	if role != "" {
		req.Role = role
	}
	if req.Metadata == nil {
		req.Metadata = make(map[string]any)
	}
	req.Metadata["_routed_by"] = pluginID
	req.Metadata["_routed_rule"] = name
	if prevModel != "" && prevModel != req.Model {
		req.Metadata["_routed_from_model"] = prevModel
	}
	if prevRole != "" && prevRole != req.Role {
		req.Metadata["_routed_from_role"] = prevRole
	}
	logger.Debug("router rule matched",
		"index", idx,
		"rule", name,
		"prev_model", prevModel,
		"prev_role", prevRole,
		"new_model", req.Model,
		"new_role", req.Role)
}

// rule is one routing rule.
type rule struct {
	name string
	// match is a flat map: keys are dotted paths into the request
	// (`metadata.<key>`, `tags.<key>`, `role`, `model`, `iteration`),
	// values are matchers. All matchers must hit for a rule to fire (AND).
	matchers []matcher
	use      string // model id to assign
	role     string // role to assign (alternative to use)
}

func (r rule) matches(req *events.LLMRequest) bool {
	for _, m := range r.matchers {
		if !m(req) {
			return false
		}
	}
	return len(r.matchers) > 0
}

// matcher tests a single field of the request.
type matcher func(*events.LLMRequest) bool

// stringEqMatcher returns a matcher that checks a string field equals want.
func stringEqMatcher(getter func(*events.LLMRequest) string, want string) matcher {
	return func(req *events.LLMRequest) bool {
		return getter(req) == want
	}
}

// intCompareMatcher implements numeric comparators (lt, lte, gt, gte, eq).
func intCompareMatcher(getter func(*events.LLMRequest) (int, bool), op string, want int) matcher {
	return func(req *events.LLMRequest) bool {
		got, ok := getter(req)
		if !ok {
			return false
		}
		switch op {
		case "lt":
			return got < want
		case "lte":
			return got <= want
		case "gt":
			return got > want
		case "gte":
			return got >= want
		case "eq":
			return got == want
		}
		return false
	}
}

// metadataString reads a string-valued key from request.Metadata, accepting
// both string and fmt-stringer-like types.
func metadataString(req *events.LLMRequest, key string) string {
	if req.Metadata == nil {
		return ""
	}
	switch v := req.Metadata[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	}
	return ""
}

// metadataInt reads an integer-valued key from request.Metadata, accepting
// int, int64, and float64 (YAML loads numbers as float64).
func metadataInt(req *events.LLMRequest, key string) (int, bool) {
	if req.Metadata == nil {
		return 0, false
	}
	switch v := req.Metadata[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// tagString reads a key from request.Tags. Tags are always strings.
func tagString(req *events.LLMRequest, key string) string {
	if req.Tags == nil {
		return ""
	}
	return req.Tags[key]
}

// parseConfig converts the plugin's YAML config into the internal rule set.
//
// Accepted shape:
//
//	rules:
//	  - name: "planner-uses-haiku"     # optional; falls back to "rule#N"
//	    match:
//	      metadata._source: "planner"
//	    use: claude-haiku-4-5-20251001
//	  - match:
//	      metadata.task_kind: "react_main"
//	      metadata.iteration: { gte: 3 }
//	    use: claude-opus-4-7
//	  - match:
//	      tags.tenant: "premium"
//	    role: reasoning
//	default_model: claude-sonnet-4-6-20250514
//	default_role: balanced
func parseConfig(cfg map[string]any) ([]rule, string, string, error) {
	rawRules, _ := cfg["rules"].([]any)
	out := make([]rule, 0, len(rawRules))

	for i, raw := range rawRules {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, "", "", fmt.Errorf("rules[%d]: must be a mapping, got %T", i, raw)
		}
		name, _ := entry["name"].(string)
		if name == "" {
			name = fmt.Sprintf("rule#%d", i)
		}
		use, _ := entry["use"].(string)
		role, _ := entry["role"].(string)
		if use == "" && role == "" {
			return nil, "", "", fmt.Errorf("rules[%d] (%s): at least one of `use` or `role` is required", i, name)
		}

		matchRaw, _ := entry["match"].(map[string]any)
		matchers, err := parseMatchers(matchRaw)
		if err != nil {
			return nil, "", "", fmt.Errorf("rules[%d] (%s): %w", i, name, err)
		}
		if len(matchers) == 0 {
			return nil, "", "", fmt.Errorf("rules[%d] (%s): match block must define at least one condition", i, name)
		}

		out = append(out, rule{name: name, matchers: matchers, use: use, role: role})
	}

	defModel, _ := cfg["default_model"].(string)
	defRole, _ := cfg["default_role"].(string)

	return out, defModel, defRole, nil
}

// parseMatchers translates a YAML match block into a slice of matchers.
//
// Supported keys:
//   - `metadata.<key>`: equality (string) or numeric comparator block
//     (`{lt|lte|gt|gte|eq: N}`).
//   - `tags.<key>`: string equality only.
//   - `role` / `model`: top-level shortcuts; equivalent to matching the
//     request's Role / Model fields directly.
func parseMatchers(m map[string]any) ([]matcher, error) {
	out := make([]matcher, 0, len(m))
	for key, raw := range m {
		switch {
		case strings.HasPrefix(key, "metadata."):
			subKey := strings.TrimPrefix(key, "metadata.")
			mat, err := parseFieldMatcher(raw, func(req *events.LLMRequest) string {
				return metadataString(req, subKey)
			}, func(req *events.LLMRequest) (int, bool) {
				return metadataInt(req, subKey)
			})
			if err != nil {
				return nil, fmt.Errorf("match.%s: %w", key, err)
			}
			out = append(out, mat)
		case strings.HasPrefix(key, "tags."):
			subKey := strings.TrimPrefix(key, "tags.")
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("match.%s: tags only support string equality, got %T", key, raw)
			}
			out = append(out, stringEqMatcher(func(req *events.LLMRequest) string {
				return tagString(req, subKey)
			}, s))
		case key == "role":
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("match.role: must be a string, got %T", raw)
			}
			out = append(out, stringEqMatcher(func(req *events.LLMRequest) string {
				return req.Role
			}, s))
		case key == "model":
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("match.model: must be a string, got %T", raw)
			}
			out = append(out, stringEqMatcher(func(req *events.LLMRequest) string {
				return req.Model
			}, s))
		default:
			return nil, fmt.Errorf("match.%s: unsupported key (use metadata.*, tags.*, role, or model)", key)
		}
	}
	return out, nil
}

// parseFieldMatcher handles the polymorphic right-hand side: a bare string
// becomes equality, a single-key map ({lt|lte|gt|gte|eq: N}) becomes a
// numeric comparator.
func parseFieldMatcher(raw any, strGetter func(*events.LLMRequest) string, intGetter func(*events.LLMRequest) (int, bool)) (matcher, error) {
	switch v := raw.(type) {
	case string:
		return stringEqMatcher(strGetter, v), nil
	case bool:
		want := fmt.Sprintf("%v", v)
		return stringEqMatcher(strGetter, want), nil
	case int, int64, float64:
		want, _ := numericInt(v)
		return intCompareMatcher(intGetter, "eq", want), nil
	case map[string]any:
		if len(v) != 1 {
			return nil, fmt.Errorf("comparator block must have exactly one key (lt|lte|gt|gte|eq), got %d", len(v))
		}
		for op, val := range v {
			n, ok := numericInt(val)
			if !ok {
				return nil, fmt.Errorf("comparator %q: value must be numeric, got %T", op, val)
			}
			switch op {
			case "lt", "lte", "gt", "gte", "eq":
				return intCompareMatcher(intGetter, op, n), nil
			default:
				return nil, fmt.Errorf("unknown comparator %q (allowed: lt, lte, gt, gte, eq)", op)
			}
		}
	}
	return nil, fmt.Errorf("unsupported value type %T", raw)
}

func numericInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
