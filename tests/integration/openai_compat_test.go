//go:build integration

package integration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestOpenAICompat_BaseURLOverrideRoutesToCustomServer validates that the
// nexus.llm.openai plugin honours a base_url override and sends its requests
// to the configured non-OpenAI server. The test stands up a httptest server
// that mimics the OpenAI streaming SSE protocol just enough for one round
// trip, then asserts the agent received the synthetic content.
func TestOpenAICompat_BaseURLOverrideRoutesToCustomServer(t *testing.T) {
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)

		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/v1/chat/completions") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		writeChunk := func(s string) {
			_, _ = w.Write([]byte("data: " + s + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}

		writeChunk(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"llama3","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		writeChunk(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"llama3","choices":[{"index":0,"delta":{"content":"COMPAT OK"},"finish_reason":null}]}`)
		writeChunk(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"llama3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
		writeChunk("[DONE]")
	}))
	defer srv.Close()

	cfg := copyConfig(t, "configs/test-openai-compat.yaml", map[string]any{
		"nexus.llm.openai": map[string]any{
			"api_key":  "not-needed",
			"base_url": srv.URL + "/v1/chat/completions",
		},
	})

	h := testharness.New(t, cfg, testharness.WithTimeout(20*time.Second))
	h.Run()

	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("expected at least one HTTP request to the override server, got none")
	}

	// Final assistant output should contain the chunked content.
	var found bool
	for _, e := range h.Events() {
		if e.Type != "io.output" {
			continue
		}
		if out, ok := e.Payload.(events.AgentOutput); ok && out.Role == "assistant" {
			if strings.Contains(out.Content, "COMPAT OK") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected assistant output to contain 'COMPAT OK' from custom server")
	}
}
