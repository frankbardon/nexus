//go:build integration

// Package integration: OTEL trace export end-to-end test.
//
// Notes on the OTLP export shape (verified against
// plugins/observe/otel/exporter.go):
//   - The plugin uses the upstream OpenTelemetry SDK exporters
//     (otlptracehttp / otlptracegrpc) — it does NOT write to a file or a
//     bus event we can intercept directly.
//   - Spans are batched (sdktrace.WithBatcher) and flushed on plugin
//     Shutdown, which the harness triggers via engine.Stop.
//   - For HTTP/protobuf transport the SDK POSTs serialized
//     ExportTraceServiceRequest payloads to "<endpoint>/v1/traces" with
//     Content-Type "application/x-protobuf". WithEndpoint takes only
//     "host:port" (no scheme), and WithInsecure forces plain HTTP.
//
// The test stands up an httptest.Server, extracts its host:port, points the
// otel plugin at it via a templated config, runs a minimal scripted agent
// loop in mock mode, and asserts the collector received at least one POST
// to /v1/traces during the lifecycle. We intentionally do not decode the
// protobuf payload — the assertion target is "spans were exported", and
// pulling in protobuf decoding would couple the test to OTLP schema
// versioning. Span content is already covered by the unit tests in
// plugins/observe/otel/plugin_test.go (TestExtractAttributes_*).
package integration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// otlpRequest captures one POST received by the fake collector.
type otlpRequest struct {
	Path        string
	ContentType string
	BodyLen     int
}

// newOTLPCollector returns an httptest.Server that records every request
// it receives. The captured slice is goroutine-safe.
func newOTLPCollector(t *testing.T) (*httptest.Server, func() []otlpRequest) {
	t.Helper()

	var (
		mu       sync.Mutex
		captured []otlpRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain body so the SDK's HTTP client treats this as a clean response.
		buf := make([]byte, 0, 1024)
		if r.Body != nil {
			tmp := make([]byte, 4096)
			for {
				n, err := r.Body.Read(tmp)
				if n > 0 {
					buf = append(buf, tmp[:n]...)
				}
				if err != nil {
					break
				}
			}
			_ = r.Body.Close()
		}

		mu.Lock()
		captured = append(captured, otlpRequest{
			Path:        r.URL.Path,
			ContentType: r.Header.Get("Content-Type"),
			BodyLen:     len(buf),
		})
		mu.Unlock()

		// OTLP/HTTP success response: empty ExportTraceServiceResponse.
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(nil)
	}))

	t.Cleanup(srv.Close)

	get := func() []otlpRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]otlpRequest, len(captured))
		copy(out, captured)
		return out
	}
	return srv, get
}

// hostPort strips the "http://" prefix from an httptest.Server URL,
// returning just "127.0.0.1:PORT" — the form otlptracehttp.WithEndpoint
// expects.
func hostPort(t *testing.T, serverURL string) string {
	t.Helper()
	const prefix = "http://"
	if !strings.HasPrefix(serverURL, prefix) {
		t.Fatalf("unexpected httptest URL: %s", serverURL)
	}
	return strings.TrimPrefix(serverURL, prefix)
}

// TestOTEL_ExportsSpans validates the full export chain: the otel plugin
// boots, batches spans during a minimal mock agent loop, and flushes them
// to the configured OTLP/HTTP collector on shutdown. The fake collector
// asserts at least one POST to /v1/traces was received with a non-empty
// protobuf body.
func TestOTEL_ExportsSpans(t *testing.T) {
	srv, getRequests := newOTLPCollector(t)
	endpoint := hostPort(t, srv.URL)

	cfg := copyConfig(t, "configs/test-otel.yaml", map[string]any{
		"nexus.observe.otel": map[string]any{
			"endpoint":     endpoint,
			"protocol":     "http",
			"insecure":     true,
			"service_name": "nexus-otel-test",
			"exclude_events": []any{
				"core.tick",
			},
		},
	})

	h := testharness.New(t, cfg, testharness.WithTimeout(20*time.Second))
	h.Run()

	// Tier 1: the plugin must have booted alongside a minimal agent stack.
	h.AssertBooted(
		"nexus.io.test",
		"nexus.llm.anthropic",
		"nexus.agent.react",
		"nexus.observe.otel",
	)
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")

	// Allow a brief window for the SDK's batch processor to drain. The
	// plugin's Shutdown call inside engine.Stop already triggers a flush,
	// but the HTTP POST itself completes asynchronously relative to the
	// caller. 500ms is generous on local loopback.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(getRequests()) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	reqs := getRequests()
	if len(reqs) == 0 {
		t.Fatalf("expected at least one OTLP export to %s, got 0", srv.URL)
	}

	// At least one request must be a POST to /v1/traces with a non-empty
	// protobuf payload. The SDK may issue multiple batches; we only need
	// one valid one.
	var sawTracesPost bool
	for _, r := range reqs {
		if r.Path == "/v1/traces" && r.BodyLen > 0 {
			sawTracesPost = true
			if !strings.Contains(r.ContentType, "protobuf") &&
				!strings.Contains(r.ContentType, "json") {
				t.Errorf("OTLP request had unexpected Content-Type %q (want protobuf or json)", r.ContentType)
			}
			break
		}
	}
	if !sawTracesPost {
		t.Errorf("no POST to /v1/traces with non-empty body; got %d request(s): %+v", len(reqs), reqs)
	}
}

// TestOTEL_NoSpansWhenPluginInactive is a control: with the otel plugin
// removed from plugins.active, the collector receives nothing. This guards
// against accidental in-process exporters left wired up by another plugin.
func TestOTEL_NoSpansWhenPluginInactive(t *testing.T) {
	srv, getRequests := newOTLPCollector(t)

	cfg := copyConfig(t, "configs/test-otel.yaml", map[string]any{
		"active": []any{
			"nexus.io.test",
			"nexus.llm.anthropic",
			"nexus.agent.react",
			"nexus.gate.endless_loop",
			"nexus.memory.capped",
			// nexus.observe.otel intentionally omitted
		},
		"nexus.observe.otel": map[string]any{
			"endpoint": hostPort(t, srv.URL),
			"protocol": "http",
			"insecure": true,
		},
	})

	h := testharness.New(t, cfg, testharness.WithTimeout(20*time.Second))
	h.Run()

	if reqs := getRequests(); len(reqs) > 0 {
		t.Errorf("expected zero exports without nexus.observe.otel active; got %d: %+v", len(reqs), reqs)
	}
}
