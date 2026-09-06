// Package session_tags registers an opt-in LLM-facing tool surface over the
// general-namespace session tag store (pkg/engine/session_tags.go): the
// agent itself can set, read, delete, and list session labels through
// session_tag_set/get/delete/list.
//
// This plugin is deliberately restricted to the general (non-reserved)
// namespace. Every write goes through the vetoable
// before:session.tag.set/before:session.tag.delete bus path exactly like any
// other caller's would — see engine.installSessionTagHandlers, the one
// enforcement point that rejects a reserved ("_"-prefixed) Key unconditionally.
// This plugin adds no special-casing, no pre-check, and no bypass around that
// enforcement: a reserved-key set/delete attempt simply gets vetoed the same
// way it would for any other emitter, and the veto reason is surfaced back to
// the caller as an ordinary tool error.
//
// Reads (session_tag_get/session_tag_list) apply their own filter on top of
// SessionMetadata().Labels, since reads never touch the bus path at all:
// session_tag_get returns not-found for a reserved key as if it doesn't
// exist — it must never leak the key's presence or value through a
// different error message or code path — and session_tag_list omits reserved
// keys entirely from its result set.
//
// Off by default. An operator opts in by listing "nexus.tool.session_tags"
// under plugins.active, same convention as nexus.embeddings.mock and other
// optional tool plugins.
package session_tags

import (
	"context"
	"log/slog"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.tool.session_tags"
	pluginName = "Session Tags Tool"
	version    = "0.1.0"
)

// Plugin exposes session_tag_set/get/delete/list as LLM-facing tools over
// the general-namespace session tag store.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	enabled map[string]bool
	unsubs  []func()
}

// New creates a new session tags tool plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	// All tools enabled by default.
	p.enabled = map[string]bool{
		"session_tag_set":    true,
		"session_tag_get":    true,
		"session_tag_delete": true,
		"session_tag_list":   true,
	}

	// Allow per-tool enable/disable via config, mirroring nexus.tool.file's
	// "tools.<tool_name>" convention.
	if tools, ok := ctx.Config["tools"].(map[string]any); ok {
		for name, v := range tools {
			if _, known := p.enabled[name]; !known {
				p.logger.Warn("session_tags: ignoring unknown tool in config", "tool", name)
				continue
			}
			if enabled, ok := v.(bool); ok {
				p.enabled[name] = enabled
			}
		}
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	return nil
}

func (p *Plugin) registerTool(def events.ToolDef) {
	if !p.enabled[def.Name] {
		p.logger.Info("session_tags: tool disabled by config", "tool", def.Name)
		return
	}
	_ = p.bus.Emit("tool.register", def)
}

func (p *Plugin) Ready() error {
	p.registerTool(events.ToolDef{
		Name: "session_tag_set",
		Description: "Set a general-namespace session tag (key/value label attached to " +
			"this session). Keys starting with an underscore are reserved for the host " +
			"and cannot be written this way — the call fails if you try.",
		Class:    "session",
		Subclass: "tags",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{
					"type":        "string",
					"description": "The tag key to set. Must not start with an underscore.",
				},
				"value": map[string]any{
					"type":        "string",
					"description": "The tag value.",
				},
			},
			"required": []string{"key", "value"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":   map[string]any{"type": "string"},
				"value": map[string]any{"type": "string"},
			},
			"required": []string{"key", "value"},
		},
	})

	p.registerTool(events.ToolDef{
		Name:        "session_tag_get",
		Description: "Read a single general-namespace session tag by key. Returns not found if the key isn't set.",
		Class:       "session",
		Subclass:    "tags",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{
					"type":        "string",
					"description": "The tag key to read.",
				},
			},
			"required": []string{"key"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":   map[string]any{"type": "string"},
				"value": map[string]any{"type": "string"},
				"found": map[string]any{"type": "boolean"},
			},
			"required": []string{"key", "found"},
		},
	})

	p.registerTool(events.ToolDef{
		Name:        "session_tag_delete",
		Description: "Delete a general-namespace session tag by key. Keys starting with an underscore are reserved and cannot be deleted this way.",
		Class:       "session",
		Subclass:    "tags",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{
					"type":        "string",
					"description": "The tag key to delete.",
				},
			},
			"required": []string{"key"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{"type": "string"},
			},
			"required": []string{"key"},
		},
	})

	p.registerTool(events.ToolDef{
		Name:        "session_tag_list",
		Description: "List every general-namespace session tag currently set on this session. Reserved (host-only) tags never appear here.",
		Class:       "session",
		Subclass:    "tags",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tags": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Key/value map of every non-reserved session tag.",
				},
			},
			"required": []string{"tags"},
		},
	})

	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"before:tool.result",
		"tool.result",
		"tool.register",
		"before:session.tag.set",
		"before:session.tag.delete",
	}
}

func (p *Plugin) handleEvent(event engine.Event[any]) {
	if event.Type != "tool.invoke" {
		return
	}

	tc, ok := event.Payload.(events.ToolCall)
	if !ok {
		return
	}

	if !p.enabled[tc.Name] {
		return
	}

	switch tc.Name {
	case "session_tag_set":
		p.handleSet(tc)
	case "session_tag_get":
		p.handleGet(tc)
	case "session_tag_delete":
		p.handleDelete(tc)
	case "session_tag_list":
		p.handleList(tc)
	}
}

func (p *Plugin) handleSet(tc events.ToolCall) {
	key, _ := tc.Arguments["key"].(string)
	value, _ := tc.Arguments["value"].(string)
	if key == "" {
		p.emitResult(tc, "", "key argument is required", nil)
		return
	}
	if p.session == nil {
		p.emitResult(tc, "", "no active session", nil)
		return
	}

	// Rides the general-namespace vetoable request path exactly like any
	// other caller — no special-casing, no elevated privilege for this
	// plugin. installSessionTagHandlers (pkg/engine/session_tags.go) is the
	// one place a reserved key is rejected; this call is subject to it the
	// same as everything else.
	req := &events.SessionTagSetRequest{
		SchemaVersion: events.SessionTagSetRequestVersion,
		Key:           key,
		Value:         value,
	}
	veto, err := p.bus.EmitVetoable("before:session.tag.set", req)
	if err != nil {
		p.emitResult(tc, "", err.Error(), nil)
		return
	}
	if veto.Vetoed {
		p.emitResult(tc, "", veto.Reason, nil)
		return
	}

	p.emitResult(tc, "session tag set", "", map[string]any{
		"key":   key,
		"value": value,
	})
}

func (p *Plugin) handleDelete(tc events.ToolCall) {
	key, _ := tc.Arguments["key"].(string)
	if key == "" {
		p.emitResult(tc, "", "key argument is required", nil)
		return
	}
	if p.session == nil {
		p.emitResult(tc, "", "no active session", nil)
		return
	}

	req := &events.SessionTagDeleteRequest{
		SchemaVersion: events.SessionTagDeleteRequestVersion,
		Key:           key,
	}
	veto, err := p.bus.EmitVetoable("before:session.tag.delete", req)
	if err != nil {
		p.emitResult(tc, "", err.Error(), nil)
		return
	}
	if veto.Vetoed {
		p.emitResult(tc, "", veto.Reason, nil)
		return
	}

	p.emitResult(tc, "session tag deleted", "", map[string]any{
		"key": key,
	})
}

func (p *Plugin) handleGet(tc events.ToolCall) {
	key, _ := tc.Arguments["key"].(string)
	if key == "" {
		p.emitResult(tc, "", "key argument is required", nil)
		return
	}
	if p.session == nil {
		p.emitResult(tc, "", "no active session", nil)
		return
	}

	// A reserved key is reported not-found, exactly as if it were never
	// set — this must never distinguish "reserved" from "absent" in its
	// response, or the response itself leaks the key's presence.
	if engine.IsReservedLabelKey(key) {
		p.emitResult(tc, "", "", map[string]any{
			"key":   key,
			"found": false,
		})
		return
	}

	meta, err := p.session.SessionMetadata()
	if err != nil {
		p.emitResult(tc, "", err.Error(), nil)
		return
	}

	value, found := meta.Labels[key]
	if !found {
		p.emitResult(tc, "", "", map[string]any{
			"key":   key,
			"found": false,
		})
		return
	}

	p.emitResult(tc, value, "", map[string]any{
		"key":   key,
		"value": value,
		"found": true,
	})
}

func (p *Plugin) handleList(tc events.ToolCall) {
	if p.session == nil {
		p.emitResult(tc, "", "no active session", nil)
		return
	}

	meta, err := p.session.SessionMetadata()
	if err != nil {
		p.emitResult(tc, "", err.Error(), nil)
		return
	}

	// Reserved keys are filtered out entirely — not redacted, not marked,
	// simply absent — so the tool result carries no signal that a reserved
	// namespace even exists.
	tags := make(map[string]string, len(meta.Labels))
	for k, v := range meta.Labels {
		if engine.IsReservedLabelKey(k) {
			continue
		}
		tags[k] = v
	}

	p.emitResult(tc, "", "", map[string]any{
		"tags": tags,
	})
}

func (p *Plugin) emitResult(tc events.ToolCall, output, errMsg string, structured map[string]any) {
	result := events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
		Name:             tc.Name,
		Output:           output,
		Error:            errMsg,
		OutputStructured: structured,
		TurnID:           tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}
