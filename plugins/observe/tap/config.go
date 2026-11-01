package tap

import (
	"fmt"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

const (
	defaultBind              = "127.0.0.1:8095"
	defaultRingSize          = 8192
	defaultClientBuffer      = 1024
	defaultHeartbeatInterval = 15 * time.Second
)

type config struct {
	bind string
	// chain is never nil after parseConfig; a chain with no validators means
	// the listener admits every caller (loopback default).
	chain *nexusauth.Chain
	// include / exclude are the operator ceiling: an event outside it is
	// never marshaled, never enters the ring, and no client can ask for it.
	include      []string
	exclude      []string
	ringSize     int
	clientBuffer int
	// heartbeat <= 0 disables keepalive comments.
	heartbeat time.Duration
}

func parseConfig(raw map[string]any) (config, error) {
	cfg := config{
		bind:         defaultBind,
		ringSize:     defaultRingSize,
		clientBuffer: defaultClientBuffer,
		heartbeat:    defaultHeartbeatInterval,
	}

	if v, ok := raw["bind"].(string); ok && strings.TrimSpace(v) != "" {
		cfg.bind = strings.TrimSpace(v)
	}

	cfg.chain = nexusauth.NewChain()
	if rawAuth, ok := raw["auth"]; ok && rawAuth != nil {
		m, ok := rawAuth.(map[string]any)
		if !ok {
			return config{}, fmt.Errorf("auth: want a mapping, got %T", rawAuth)
		}
		chain, err := nexusauth.ChainFromMap(m)
		if err != nil {
			return config{}, fmt.Errorf("auth: %w", err)
		}
		cfg.chain = chain
	}

	var err error
	if cfg.include, err = stringList(raw, "include_events"); err != nil {
		return config{}, err
	}
	if cfg.exclude, err = stringList(raw, "exclude_events"); err != nil {
		return config{}, err
	}

	if cfg.ringSize, err = positiveInt(raw, "ring_size", cfg.ringSize); err != nil {
		return config{}, err
	}
	if cfg.clientBuffer, err = positiveInt(raw, "client_buffer", cfg.clientBuffer); err != nil {
		return config{}, err
	}

	// An unparseable interval is an error rather than a silent fallback: the
	// failure it guards against (an idle proxy cutting the stream) is
	// invisible until it happens. "0" turns keepalives off.
	if v, ok := raw["heartbeat_interval"].(string); ok && strings.TrimSpace(v) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return config{}, fmt.Errorf("heartbeat_interval %q is not a duration: %w", v, err)
		}
		cfg.heartbeat = d
	}
	return cfg, nil
}

func stringList(raw map[string]any, key string) ([]string, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: want a list of strings, got %T", key, v)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("%s: want a list of strings, got element %T", key, it)
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func positiveInt(raw map[string]any, key string, def int) (int, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return def, nil
	}
	var n int
	switch x := v.(type) {
	case int:
		n = x
	case int64:
		n = int(x)
	case float64:
		n = int(x)
	default:
		return 0, fmt.Errorf("%s: want an integer, got %T", key, v)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s: must be positive, got %d", key, n)
	}
	return n, nil
}

// typeFilter is an include/exclude pair over event types. A pattern is an
// exact type or ends in "*" for a prefix match — "llm.*" (the
// nexus.observe.otel rule), "before:*", or "*" for everything. Empty include
// admits all.
type typeFilter struct {
	include []string
	exclude []string
}

func (f typeFilter) match(eventType string) bool {
	if len(f.include) > 0 && !anyPattern(f.include, eventType) {
		return false
	}
	return !anyPattern(f.exclude, eventType)
}

func anyPattern(patterns []string, eventType string) bool {
	for _, p := range patterns {
		if p == eventType {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(eventType, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// splitPatterns parses a comma-separated query parameter into patterns.
func splitPatterns(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
