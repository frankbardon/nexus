package anthropic

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/providers/fallback"
)

// This is the behaviour the per-role `retry:` axis exists for, end to end: a
// role whose chain has somewhere to fall through to wants FEWER attempts at the
// primary, because every retry there is time not spent on the entry that might
// actually answer. A terminal role wants the opposite.
//
// The test drives the real fallback coordinator on a real bus against the real
// handleRequest, with only the HTTP transport substituted. It asserts two
// things a value-arrives test cannot: that the loop honoured the SERVING
// entry's budget rather than the plugin's, and that giving up sooner is what
// gets the request to the second entry sooner.

// alwaysUnavailable answers every request with a retryable 503 and counts the
// attempts. Substituting the transport rather than the URL keeps the provider's
// own endpoint and auth code on the path.
type alwaysUnavailable struct {
	mu   sync.Mutex
	hits int
}

func (a *alwaysUnavailable) RoundTrip(r *http.Request) (*http.Response, error) {
	a.mu.Lock()
	a.hits++
	a.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Status:     "503 Service Unavailable",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`)),
		Request:    r,
	}, nil
}

func (a *alwaysUnavailable) attempts() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hits
}

// fallthroughRun wires the provider and the fallback coordinator onto one bus,
// runs a single turn for the role, and reports how many HTTP attempts the
// primary entry got, how long the whole thing took, and the request the
// coordinator re-emitted at the next entry (nil if it never fell through).
func fallthroughRun(t *testing.T, primaryRetry map[string]any) (attempts int, elapsed time.Duration, next *events.LLMRequest) {
	t.Helper()

	primary := map[string]any{"provider": pluginID, "model": "claude-opus-4-7"}
	if primaryRetry != nil {
		primary["retry"] = primaryRetry
	}
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			primary,
			map[string]any{"provider": pluginID, "model": "claude-haiku-4-5"},
		},
	})

	bus := engine.NewEventBus()
	logger := silentTestLogger()

	coordinator := fallback.New()
	if err := coordinator.Init(engine.PluginContext{Bus: bus, Logger: logger, Models: models}); err != nil {
		t.Fatalf("fallback Init: %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Shutdown(t.Context()) })

	var mu sync.Mutex
	var reemitted *events.LLMRequest
	bus.Subscribe("llm.request", func(e engine.Event[any]) {
		req, ok := e.Payload.(events.LLMRequest)
		if !ok {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		reemitted = &req
	}, engine.WithSource("test.sink"))

	transport := &alwaysUnavailable{}

	// A deliberately slow plugin-level policy, so the difference between
	// honouring it and honouring the role's is wall-clock visible. Constant
	// backoff keeps the arithmetic exact.
	pluginBlock := map[string]any{
		"max_retries":   6,
		"backoff":       "constant",
		"initial_delay": "30ms",
		"max_delay":     "30ms",
	}
	rc, err := parseRetryConfig(map[string]any{"retry": pluginBlock}, nil)
	if err != nil {
		t.Fatalf("parseRetryConfig: %v", err)
	}

	p := &Plugin{
		logger:   logger,
		bus:      bus,
		models:   models,
		client:   &http.Client{Transport: transport},
		auth:     &authState{mode: authModeAPIKey, apiKey: "test-key"},
		retry:    rc,
		retryRaw: rawRetryBlock(map[string]any{"retry": pluginBlock}),
		thinking: thinkingConfig{Mode: thinkingModeOff},
	}
	if err := validateRoleRetry(models, p.retryRaw, logger); err != nil {
		t.Fatalf("validateRoleRetry: %v", err)
	}

	req := events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "balanced",
		Messages:      []events.Message{{Role: "user", Content: "hello"}},
	}
	if _, err := bus.EmitVetoable("before:llm.request", &req); err != nil {
		t.Fatalf("before:llm.request: %v", err)
	}

	start := time.Now()
	p.handleRequest(req)
	elapsed = time.Since(start)

	mu.Lock()
	defer mu.Unlock()
	return transport.attempts(), elapsed, reemitted
}

// The headline: entry 0 asking for one retry falls through to entry 1 after two
// attempts, where the plugin's own policy would have taken seven — and the
// whole turn is correspondingly quicker.
func TestRoleRetry_FallbackChainFallsThroughSooner(t *testing.T) {
	fastAttempts, fastElapsed, fastNext := fallthroughRun(t, map[string]any{"max_retries": 1})
	slowAttempts, slowElapsed, slowNext := fallthroughRun(t, nil)

	if fastAttempts != 2 {
		t.Fatalf("attempts with the role's max_retries: 1 = %d, want 2 (the call plus one retry)", fastAttempts)
	}
	if slowAttempts != 7 {
		t.Fatalf("attempts with no role block = %d, want 7 (the plugin's max_retries: 6)", slowAttempts)
	}

	// Both must actually reach the second entry — falling through faster is
	// only interesting if it still falls through.
	for name, got := range map[string]*events.LLMRequest{"role-capped": fastNext, "plugin-default": slowNext} {
		if got == nil {
			t.Fatalf("%s: the coordinator never re-emitted at the next chain entry", name)
		}
		if got.Model != "claude-haiku-4-5" {
			t.Fatalf("%s: fell through to model %q, want claude-haiku-4-5", name, got.Model)
		}
		if target, _ := got.Metadata["_target_provider"].(string); target != pluginID {
			t.Fatalf("%s: _target_provider = %q, want %q", name, target, pluginID)
		}
	}

	if fastElapsed >= slowElapsed {
		t.Fatalf("the role-capped run took %v and the plugin-default one %v; the point of the axis is that the first is quicker",
			fastElapsed, slowElapsed)
	}
}
