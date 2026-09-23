package anthropic

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// BackoffStrategy defines how delays increase between retries.
type BackoffStrategy string

const (
	BackoffConstant    BackoffStrategy = "constant"
	BackoffLinear      BackoffStrategy = "linear"
	BackoffExponential BackoffStrategy = "exponential"
	BackoffJitter      BackoffStrategy = "exponential_jitter"
)

// retryConfig controls automatic retry behavior for API requests.
type retryConfig struct {
	// Enabled turns retry on/off. Default: false.
	Enabled bool
	// MaxRetries is the maximum number of retry attempts. Default: 3.
	MaxRetries int
	// InitialDelay is the base delay before the first retry. Default: 1s.
	InitialDelay time.Duration
	// MaxDelay caps the delay between retries. Default: 60s.
	MaxDelay time.Duration
	// Backoff is the backoff strategy. Default: exponential_jitter.
	Backoff BackoffStrategy
	// Multiplier scales delay growth for linear and exponential strategies. Default: 2.0.
	Multiplier float64
	// RetryableStatuses is the set of HTTP status codes that trigger a retry.
	// Default: 429, 500, 502, 503, 529.
	RetryableStatuses map[int]bool
}

func defaultRetryConfig() retryConfig {
	return retryConfig{
		Enabled:      false,
		MaxRetries:   3,
		InitialDelay: 1 * time.Second,
		MaxDelay:     60 * time.Second,
		Backoff:      BackoffJitter,
		Multiplier:   2.0,
		RetryableStatuses: map[int]bool{
			429: true, // rate limited
			500: true, // internal server error
			502: true, // bad gateway
			503: true, // service unavailable
			529: true, // overloaded
		},
	}
}

// retryBlockKeys is the complete set of keys a `retry:` block may carry. It
// duplicates schema.json's property list on purpose: a block written on a
// `core.models` role never passes through schema.json — core stores those maps
// without looking inside them — so this is the only thing that ever rejects a
// typo there.
var retryBlockKeys = map[string]struct{}{
	"enabled":       {},
	"max_retries":   {},
	"initial_delay": {},
	"max_delay":     {},
	"backoff":       {},
	"multiplier":    {},
	"statuses":      {},
}

// retryBlockKeyList names the accepted keys in error messages.
const retryBlockKeyList = "enabled, max_retries, initial_delay, max_delay, backoff, multiplier, statuses"

// validateRetryBlock rejects a `retry:` block that schema.json would have
// rejected — an unknown key, or a key of the wrong type. Everything else stays
// lenient, which is this parser's long-standing contract: an unrecognised
// `backoff` word and an unparseable duration are warned about and defaulted
// rather than refused.
//
// At plugin level schema.json has already caught both classes, so this is
// belt-and-braces there. On a merged role block it is the only guard.
func validateRetryBlock(raw map[string]any) error {
	var unknown []string
	for k := range raw {
		if _, ok := retryBlockKeys[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("anthropic: retry block has unknown key(s) %s; accepted keys are %s",
			strings.Join(unknown, ", "), retryBlockKeyList)
	}

	if v, present := raw["enabled"]; present {
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("anthropic: retry.enabled must be a bool, got %s", typeName(v))
		}
	}
	// YAML decoders surface integers as int or float64 depending on path, so
	// both shapes are accepted everywhere a number is read.
	if v, present := raw["max_retries"]; present {
		n, ok := retryNumber(v)
		if !ok {
			return fmt.Errorf("anthropic: retry.max_retries must be an int, got %s", typeName(v))
		}
		if n < 0 {
			return fmt.Errorf("anthropic: retry.max_retries must not be negative, got %v", v)
		}
	}
	if v, present := raw["multiplier"]; present {
		n, ok := retryNumber(v)
		if !ok {
			return fmt.Errorf("anthropic: retry.multiplier must be a number, got %s", typeName(v))
		}
		if n < 0 {
			return fmt.Errorf("anthropic: retry.multiplier must not be negative, got %v", v)
		}
	}
	for _, key := range []string{"initial_delay", "max_delay", "backoff"} {
		if v, present := raw[key]; present {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("anthropic: retry.%s must be a string, got %s", key, typeName(v))
			}
		}
	}
	if v, present := raw["statuses"]; present {
		list, ok := v.([]any)
		if !ok {
			return fmt.Errorf("anthropic: retry.statuses must be a list of ints, got %s", typeName(v))
		}
		for _, s := range list {
			if _, ok := retryNumber(s); !ok {
				return fmt.Errorf("anthropic: retry.statuses entries must be ints, got %s", typeName(s))
			}
		}
	}
	return nil
}

// retryNumber normalises the int/float64 split YAML decoding leaves behind.
func retryNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// parseRetryConfig reads retry settings from a config map carrying a `retry:`
// block. An absent block resolves to the defaults with retrying off; a present
// one turns it on, whatever the individual keys say.
//
// The error is reserved for the two things schema.json would have caught and a
// `core.models` role block bypasses: an unknown key and a key of the wrong
// type. Value-level surprises stay soft, matching the rest of the plugin's
// parsing style — an unrecognised `backoff` word keeps the default, with a
// warning, rather than failing the boot.
//
// logger may be nil, which suppresses those warnings. That is what the
// per-request path wants: validateRoleRetry has already run every role's merged
// block through here once at Init with the real logger attached.
func parseRetryConfig(cfg map[string]any, logger *slog.Logger) (retryConfig, error) {
	rc := defaultRetryConfig()

	retryCfg, ok := cfg["retry"].(map[string]any)
	if !ok {
		return rc, nil
	}

	if err := validateRetryBlock(retryCfg); err != nil {
		return defaultRetryConfig(), err
	}

	rc.Enabled = true

	if v, ok := retryNumber(retryCfg["max_retries"]); ok {
		rc.MaxRetries = int(v)
	}

	rc.InitialDelay = retryDuration(retryCfg, "initial_delay", rc.InitialDelay, logger)
	rc.MaxDelay = retryDuration(retryCfg, "max_delay", rc.MaxDelay, logger)

	if v, ok := retryCfg["backoff"].(string); ok {
		switch BackoffStrategy(v) {
		case BackoffConstant, BackoffLinear, BackoffExponential, BackoffJitter:
			rc.Backoff = BackoffStrategy(v)
		default:
			if logger != nil {
				logger.Warn("anthropic: retry.backoff is not a known strategy; keeping the default",
					"backoff", v,
					"default", string(rc.Backoff),
				)
			}
		}
	}

	if v, ok := retryNumber(retryCfg["multiplier"]); ok && v > 0 {
		rc.Multiplier = v
	}

	if v, ok := retryCfg["statuses"].([]any); ok {
		rc.RetryableStatuses = make(map[int]bool, len(v))
		for _, s := range v {
			if code, ok := retryNumber(s); ok {
				rc.RetryableStatuses[int(code)] = true
			}
		}
	}

	return rc, nil
}

// retryDuration reads one Go-duration key, keeping def when the key is absent
// and warning-then-keeping it when the value will not parse.
func retryDuration(raw map[string]any, key string, def time.Duration, logger *slog.Logger) time.Duration {
	v, ok := raw[key].(string)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		if logger != nil {
			logger.Warn("anthropic: retry duration is not a Go duration; keeping the default",
				"key", key,
				"value", v,
				"default", def,
			)
		}
		return def
	}
	return d
}

// doWithRetry executes an HTTP request with retry logic. The caller must close
// the response body on success. The makeFn creates a fresh *http.Request for
// each attempt (required because request bodies are consumed on send).
//
// rc is passed in rather than read off the plugin because retry is the one
// per-entry axis that is call-time *behaviour* rather than a request field:
// nothing about it is serialized into the body, so the only way a role's
// `retry:` block can mean anything is for the loop around this call to be
// driven by the resolved configuration. Callers get theirs from
// Plugin.resolveRetry.
func (p *Plugin) doWithRetry(ctx context.Context, rc retryConfig, makeFn func() (*http.Request, error)) (*http.Response, error) {
	if !rc.Enabled {
		req, err := makeFn()
		if err != nil {
			return nil, err
		}
		return p.client.Do(req)
	}

	var lastErr error

	for attempt := 0; attempt <= rc.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := p.backoffDelay(attempt-1, rc)
			p.logger.Warn("retrying API request",
				"attempt", attempt,
				"max_retries", rc.MaxRetries,
				"delay", delay,
			)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := makeFn()
		if err != nil {
			return nil, fmt.Errorf("anthropic: failed to create HTTP request: %w", err)
		}

		resp, err := p.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}

		if !rc.RetryableStatuses[resp.StatusCode] {
			return resp, nil
		}

		// Check for Retry-After header on 429 responses.
		if resp.StatusCode == http.StatusTooManyRequests {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil {
					delay := time.Duration(secs) * time.Second
					if delay > rc.MaxDelay {
						delay = rc.MaxDelay
					}
					p.logger.Warn("respecting Retry-After header",
						"delay", delay,
						"attempt", attempt,
					)
					resp.Body.Close()
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(delay):
					}
					continue
				}
			}
		}

		lastErr = fmt.Errorf("API returned status %d", resp.StatusCode)
		resp.Body.Close()
	}

	return nil, fmt.Errorf("anthropic: max retries (%d) exceeded: %w", rc.MaxRetries, lastErr)
}

// backoffDelay computes the wait duration for a given retry attempt.
func (p *Plugin) backoffDelay(attempt int, rc retryConfig) time.Duration {
	var delay time.Duration

	switch rc.Backoff {
	case BackoffConstant:
		delay = rc.InitialDelay

	case BackoffLinear:
		delay = rc.InitialDelay + time.Duration(float64(attempt)*rc.Multiplier*float64(rc.InitialDelay))

	case BackoffExponential:
		delay = time.Duration(float64(rc.InitialDelay) * math.Pow(rc.Multiplier, float64(attempt)))

	case BackoffJitter:
		base := float64(rc.InitialDelay) * math.Pow(rc.Multiplier, float64(attempt))
		jitter := rand.Float64() * base
		delay = time.Duration(base + jitter)

	default:
		delay = rc.InitialDelay
	}

	if delay > rc.MaxDelay {
		delay = rc.MaxDelay
	}

	return delay
}

// --- per-role retry ---------------------------------------------------------

// rawRetryBlock lifts the plugin-level `retry:` block off the plugin config so
// it can be merged with a `core.models` role's block later. nil means the
// plugin set no block at all, which parseRetryConfig reads as retrying off.
//
// The map is not copied: everything reachable from it is treated as read-only,
// and mergeRetryBlock always builds a fresh map rather than writing into either
// input.
func rawRetryBlock(cfg map[string]any) map[string]any {
	block, ok := cfg["retry"].(map[string]any)
	if !ok {
		return nil
	}
	return block
}

// mergeRetryBlock merges a `core.models` role's `retry:` block over the
// plugin-level one, key by key. The role wins on every key it mentions, and a
// plugin-level key the role is silent about survives — so a role that only
// wants a shorter `max_retries` keeps the plugin's backoff shape and status
// list.
//
// A nil role block means the role said nothing, and the plugin block stands
// unchanged. A set-but-empty role block (`retry: {}`) is a statement rather
// than a gap: it overrides no individual key, but the merged block is non-nil,
// so on a deployment whose plugin block is absent entirely it still turns
// retrying on with the built-in defaults rather than leaving it off.
//
// Neither input is mutated and the result aliases neither: the plugin config
// map and the registry's block are both shared with the loaded configuration.
func mergeRetryBlock(plugin, role map[string]any) map[string]any {
	if role == nil {
		return plugin
	}
	merged := make(map[string]any, len(plugin)+len(role))
	for k, v := range plugin {
		merged[k] = v
	}
	for k, v := range role {
		merged[k] = v
	}
	return merged
}

// parseMergedRetry runs a merged block back through parseRetryConfig — the one
// parser — by handing it the block wrapped in the shape it expects. A merged
// block is an ordinary `retry:` block and gets exactly the same validation,
// defaulting and warning treatment the plugin-level one does.
//
// A nil logger suppresses the warnings, which is what the per-request path
// wants: validateRoleRetry has already run every role's merged block through
// here once at Init with the real logger attached.
func parseMergedRetry(plugin, role map[string]any, logger *slog.Logger) (retryConfig, error) {
	merged := mergeRetryBlock(plugin, role)
	if merged == nil {
		return defaultRetryConfig(), nil
	}
	return parseRetryConfig(map[string]any{"retry": merged}, logger)
}

// resolveRetry picks the retry configuration for one request.
//
// This is the single lookup point for the retry block, and unlike every other
// per-entry axis it feeds no part of the request body: its one consumer is the
// loop in doWithRetry. Precedence is the unified rule, most specific first:
//
//  1. req.Overrides.Retry — the block of the chain entry actually being served.
//     It arrives either stamped by the fallback or fanout coordinator (which
//     alone knows a non-first entry is in play) or recovered from the registry
//     by engine.ResolveModelConfig on the paths no coordinator touches. Either
//     way it is merged over, not substituted for, the plugin block.
//  2. the plugin-level `retry:` block, which is what a request carrying no
//     override of its own gets, unchanged.
//
// There is no `effort` analogue here: retrying has no shared cross-provider
// axis on a `core.models` entry, only the native block.
//
// An unparseable merged block is unreachable through Init, which sweeps every
// role this provider could serve and refuses to boot on one. The degradation
// here exists for a request whose Overrides.Retry was hand-set by something
// other than the registry: fall back to the plugin-level configuration rather
// than silently retrying to a policy nobody wrote.
func (p *Plugin) resolveRetry(req events.LLMRequest) retryConfig {
	if req.Overrides.Retry == nil {
		return p.retry
	}
	// Nil logger: the warnings are boot-time facts, already said once by
	// validateRoleRetry. Repeating them per request would flood the log of a
	// busy role.
	rc, err := parseMergedRetry(p.retryRaw, req.Overrides.Retry, nil)
	if err != nil {
		logger := p.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("anthropic: ignoring an invalid per-request retry block; using the plugin-level configuration",
			"role", req.Role,
			"error", err,
		)
		return p.retry
	}
	return rc
}

// validateRoleRetry checks every `core.models` role this provider could serve,
// at Init, by merging its `retry:` block over the plugin-level one and parsing
// the result.
//
// It exists because a block on a `core.models` entry bypasses the plugin's
// schema.json entirely — core stores these maps without looking inside them, so
// nothing else ever checks them. Without this sweep a role whose merged block
// carries a typo (`max_retry: 1`, say) would boot clean and retry on the
// plugin's policy, invisibly, for as long as the deployment ran.
//
// The error names the role, which is the useful half of the answer. The sweep
// itself — sorted roles, whole chain, foreign entries skipped — is
// engine.WalkRoleEntries; see there for why each of those matters.
func validateRoleRetry(models *engine.ModelRegistry, plugin map[string]any, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	return engine.WalkRoleEntries(models, pluginID, func(role string, cfg engine.ModelConfig) error {
		if cfg.Retry == nil {
			return nil
		}
		// The role's logger, so a warning raised by the merged block says which
		// role raised it. Only entries that actually set a block get here, so
		// the plugin block's own warnings — already said once by Init — are not
		// repeated per role.
		if _, err := parseMergedRetry(plugin, cfg.Retry, logger.With("role", role)); err != nil {
			return fmt.Errorf("core.models role %q: %w", role, err)
		}
		return nil
	})
}
