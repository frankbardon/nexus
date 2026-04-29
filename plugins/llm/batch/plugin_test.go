package batch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// silentLogger returns a logger that swallows everything; tests don't need
// the noise.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestPlugin builds a minimal Plugin wired against an httptest server so
// the unit tests don't talk to real provider endpoints. fn lets the test
// register handlers on an http.ServeMux before the server boots.
func newTestPlugin(t *testing.T, fn func(mux *http.ServeMux)) (*Plugin, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	if fn != nil {
		fn(mux)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := &Plugin{
		logger:               silentLogger(),
		client:               srv.Client(),
		dataDir:              t.TempDir(),
		pollInterval:         20 * time.Millisecond,
		defaultMaxTokens:     1024,
		anthropicAPIKey:      "test-anthropic-key",
		openaiAPIKey:         "test-openai-key",
		anthropicBaseURL:     srv.URL + "/anthropic/batches",
		openaiFilesBaseURL:   srv.URL + "/openai/files",
		openaiBatchesBaseURL: srv.URL + "/openai/batches",
		active:               make(map[string]*activeBatch),
		pollers:              make(map[string]context.CancelFunc),
	}
	return p, srv
}

// =====================================================================
// State persistence
// =====================================================================

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	state := &batchState{
		Provider:    "anthropic",
		BatchID:     "msgbatch_abc123",
		SubmittedAt: time.Now().UTC().Truncate(time.Second),
		OriginalReqs: []events.BatchRequest{
			{
				CustomID: "req-1",
				Request: events.LLMRequest{
					Model:    "claude-sonnet-4-5-20250514",
					Messages: []events.Message{{Role: "user", Content: "hi"}},
				},
			},
		},
		Metadata: map[string]any{"job": "label-batch"},
	}

	if err := saveBatch(dir, state); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadBatches(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 state file, got %d", len(loaded))
	}
	got := loaded[0]
	if got.Provider != state.Provider || got.BatchID != state.BatchID {
		t.Fatalf("got mismatched record: %+v", got)
	}
	if len(got.OriginalReqs) != 1 || got.OriginalReqs[0].CustomID != "req-1" {
		t.Fatalf("custom id round-trip lost: %+v", got.OriginalReqs)
	}
	if got.Metadata["job"] != "label-batch" {
		t.Fatalf("metadata round-trip lost")
	}

	if err := deleteBatch(dir, state.BatchID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	loaded, err = loadBatches(dir)
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected 0 state files after delete, got %d", len(loaded))
	}
}

func TestStateFilenameSanitize(t *testing.T) {
	// Provider-issued ids may contain chars we don't want on disk; the
	// sanitizer should replace anything outside [A-Za-z0-9-_.] with '_'.
	got := stateFilename("msgbatch_01HX:slash/dot.")
	if strings.Contains(got, "/") || strings.Contains(got, ":") {
		t.Fatalf("filename contains forbidden chars: %q", got)
	}
}

func TestLoadBatchesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	loaded, err := loadBatches(dir)
	if err != nil {
		t.Fatalf("expected nil error on missing dir, got %v", err)
	}
	if loaded != nil {
		t.Fatalf("expected nil slice, got %v", loaded)
	}
}

// =====================================================================
// Anthropic submit
// =====================================================================

func TestSubmitAnthropic_BodyShape(t *testing.T) {
	var captured map[string]any
	p, _ := newTestPlugin(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/anthropic/batches", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			if r.Header.Get("x-api-key") != "test-anthropic-key" {
				t.Errorf("missing x-api-key header")
			}
			if r.Header.Get("anthropic-version") == "" {
				t.Errorf("missing anthropic-version header")
			}
			if r.Header.Get("anthropic-beta") == "" {
				t.Errorf("missing anthropic-beta header")
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &captured); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msgbatch_xyz"}`))
		})
	})

	id, err := p.submitAnthropic(context.Background(), []events.BatchRequest{
		{
			CustomID: "req-a",
			Request: events.LLMRequest{
				Model:    "claude-sonnet-4-5-20250514",
				Messages: []events.Message{{Role: "user", Content: "hello"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("submitAnthropic: %v", err)
	}
	if id != "msgbatch_xyz" {
		t.Fatalf("expected msgbatch_xyz, got %q", id)
	}

	requests, ok := captured["requests"].([]any)
	if !ok || len(requests) != 1 {
		t.Fatalf("expected 1 request, got %#v", captured["requests"])
	}
	first, _ := requests[0].(map[string]any)
	if first["custom_id"] != "req-a" {
		t.Fatalf("custom_id mismatch: %#v", first)
	}
	params, _ := first["params"].(map[string]any)
	if params["model"] != "claude-sonnet-4-5-20250514" {
		t.Fatalf("model mismatch: %#v", params)
	}
	if _, ok := params["max_tokens"]; !ok {
		t.Fatalf("expected max_tokens in params, got %#v", params)
	}
}

// =====================================================================
// OpenAI submit (two-step: files upload + batches create)
// =====================================================================

func TestSubmitOpenAI_TwoStepFlow(t *testing.T) {
	var (
		fileHits  atomic.Int32
		batchHits atomic.Int32
	)
	p, _ := newTestPlugin(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/openai/files", func(w http.ResponseWriter, r *http.Request) {
			fileHits.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("files expected POST, got %s", r.Method)
			}
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				t.Errorf("expected multipart Content-Type, got %q", r.Header.Get("Content-Type"))
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatalf("parse multipart: %v", err)
			}
			if got := r.FormValue("purpose"); got != "batch" {
				t.Errorf("expected purpose=batch, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"file-xyz"}`))
		})
		mux.HandleFunc("/openai/batches", func(w http.ResponseWriter, r *http.Request) {
			batchHits.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("batches expected POST, got %s", r.Method)
			}
			body, _ := io.ReadAll(r.Body)
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got["input_file_id"] != "file-xyz" {
				t.Errorf("expected input_file_id=file-xyz, got %v", got["input_file_id"])
			}
			if got["completion_window"] != "24h" {
				t.Errorf("expected 24h window, got %v", got["completion_window"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"batch_abc"}`))
		})
	})

	id, err := p.submitOpenAI(context.Background(), []events.BatchRequest{
		{
			CustomID: "req-x",
			Request: events.LLMRequest{
				Model:    "gpt-4o-mini",
				Messages: []events.Message{{Role: "user", Content: "hi"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("submitOpenAI: %v", err)
	}
	if id != "batch_abc" {
		t.Fatalf("expected batch_abc, got %q", id)
	}
	if fileHits.Load() != 1 || batchHits.Load() != 1 {
		t.Fatalf("expected exactly one hit per endpoint, got files=%d batches=%d", fileHits.Load(), batchHits.Load())
	}
}

// =====================================================================
// Status mapping
// =====================================================================

func TestStatusMappingAnthropic(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"processing_status":"in_progress","request_counts":{"processing":3,"succeeded":1,"errored":0}}`, statusInProgress},
		{`{"processing_status":"ended","request_counts":{"processing":0,"succeeded":4,"errored":0}}`, statusCompleted},
		{`{"processing_status":"ended","request_counts":{"processing":0,"succeeded":0,"errored":4}}`, statusFailed},
		{`{"processing_status":"canceled","request_counts":{"processing":0,"succeeded":0,"errored":0,"canceled":4}}`, statusCancelled},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			body := c.body
			p, _ := newTestPlugin(t, func(mux *http.ServeMux) {
				mux.HandleFunc("/anthropic/batches/", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				})
			})
			got, _, err := p.statusAnthropic(context.Background(), "msgbatch_test")
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if got != c.want {
				t.Fatalf("expected %q, got %q", c.want, got)
			}
		})
	}
}

func TestStatusMappingOpenAI(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"status":"validating","request_counts":{"total":3}}`, statusInProgress},
		{`{"status":"in_progress","request_counts":{"total":3,"completed":1}}`, statusInProgress},
		{`{"status":"finalizing","request_counts":{"total":3,"completed":3}}`, statusInProgress},
		{`{"status":"completed","output_file_id":"file-out","request_counts":{"total":3,"completed":3}}`, statusCompleted},
		{`{"status":"failed","request_counts":{"total":3,"failed":3}}`, statusFailed},
		{`{"status":"cancelled"}`, statusCancelled},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			body := c.body
			p, _ := newTestPlugin(t, func(mux *http.ServeMux) {
				mux.HandleFunc("/openai/batches/", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				})
			})
			got, _, _, _, err := p.statusOpenAI(context.Background(), "batch_test")
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if got != c.want {
				t.Fatalf("expected %q, got %q", c.want, got)
			}
		})
	}
}

// =====================================================================
// Results parsing
// =====================================================================

func TestResultsAnthropic_Mixed(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"custom_id":"a","result":{"type":"succeeded","message":{"id":"msg_1","model":"claude-sonnet-4-5","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":3,"output_tokens":1}}}}`,
		`{"custom_id":"b","result":{"type":"errored","error":{"type":"invalid_request","message":"bad model"}}}`,
		`{"custom_id":"c","result":{"type":"expired"}}`,
	}, "\n") + "\n"

	p, _ := newTestPlugin(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/anthropic/batches/msgbatch_test/results", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(jsonl))
		})
	})

	results, err := p.resultsAnthropic(context.Background(), "msgbatch_test")
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].CustomID != "a" || results[0].Response == nil || results[0].Response.Content != "hello" {
		t.Fatalf("first result mismatch: %#v", results[0])
	}
	if results[1].Error == "" || !strings.Contains(results[1].Error, "bad model") {
		t.Fatalf("expected errored entry to surface message, got %#v", results[1])
	}
	if results[2].Error != "expired" {
		t.Fatalf("expected expired, got %q", results[2].Error)
	}
}

func TestResultsOpenAI_Mixed(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"id":"r1","custom_id":"a","response":{"status_code":200,"body":{"id":"chatcmpl_1","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}},"error":null}`,
		`{"id":"r2","custom_id":"b","error":{"code":"invalid_request","message":"missing model"}}`,
	}, "\n") + "\n"

	p, _ := newTestPlugin(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/openai/files/file-out/content", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(jsonl))
		})
	})

	results, err := p.resultsOpenAI(context.Background(), "file-out")
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Response == nil || results[0].Response.Content != "hi there" {
		t.Fatalf("first result mismatch: %#v", results[0])
	}
	if !strings.Contains(results[1].Error, "missing model") {
		t.Fatalf("expected error message to surface: %#v", results[1])
	}
}

// =====================================================================
// Restart resilience
// =====================================================================

// TestResumePersistedBatches seeds a state file in a temp data dir and asserts
// Init reattaches a poller against that batch.
func TestResumePersistedBatches(t *testing.T) {
	dir := t.TempDir()

	// Pre-seed a state file the resume path will pick up.
	state := &batchState{
		Provider:    "anthropic",
		BatchID:     "msgbatch_resume",
		SubmittedAt: time.Now().UTC(),
		OriginalReqs: []events.BatchRequest{
			{CustomID: "r1", Request: events.LLMRequest{Model: "claude-sonnet-4-5-20250514"}},
		},
	}
	if err := saveBatch(dir, state); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	// Mock backend: status returns "completed" immediately, then results
	// returns one entry. The poller should fire pollOnce, hit completed,
	// fetch results, emit BatchResults, and tear down.
	var statusHits, resultsHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/anthropic/batches/msgbatch_resume", func(w http.ResponseWriter, r *http.Request) {
		statusHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"processing_status":"ended","request_counts":{"processing":0,"succeeded":1,"errored":0}}`))
	})
	mux.HandleFunc("/anthropic/batches/msgbatch_resume/results", func(w http.ResponseWriter, r *http.Request) {
		resultsHits.Add(1)
		_, _ = w.Write([]byte(`{"custom_id":"r1","result":{"type":"succeeded","message":{"id":"m","model":"claude","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}}}` + "\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	bus := &captureBus{}
	p := &Plugin{
		bus:              bus,
		logger:           silentLogger(),
		client:           srv.Client(),
		dataDir:          dir,
		pollInterval:     5 * time.Millisecond,
		defaultMaxTokens: 1024,
		anthropicAPIKey:  "test",
		anthropicBaseURL: srv.URL + "/anthropic/batches",
		active:           make(map[string]*activeBatch),
		pollers:          make(map[string]context.CancelFunc),
	}

	if err := p.resumePersistedBatches(); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// Wait for the poller to run and finalize.
	if !waitFor(t, 2*time.Second, func() bool {
		return bus.hasResult("msgbatch_resume")
	}) {
		t.Fatalf("never received results event; status hits=%d, results hits=%d", statusHits.Load(), resultsHits.Load())
	}

	// State file should be gone after finalize.
	loaded, _ := loadBatches(dir)
	if len(loaded) != 0 {
		t.Fatalf("expected state file deleted after completion, got %d", len(loaded))
	}

	// Cleanly tear down any lingering pollers (waiting on wg).
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// =====================================================================
// Concurrent batches
// =====================================================================

func TestConcurrentBatches_IndependentPollers(t *testing.T) {
	mux := http.NewServeMux()

	// Both batches return "completed" and their results are distinguishable
	// by custom_id.
	mux.HandleFunc("/anthropic/batches/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/anthropic/batches/")
		if strings.HasSuffix(path, "/results") {
			id := strings.TrimSuffix(path, "/results")
			line := `{"custom_id":"` + id + `","result":{"type":"succeeded","message":{"id":"m","model":"claude","stop_reason":"end_turn","content":[{"type":"text","text":"` + id + `"}],"usage":{"input_tokens":1,"output_tokens":1}}}}` + "\n"
			_, _ = w.Write([]byte(line))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"processing_status":"ended","request_counts":{"processing":0,"succeeded":1,"errored":0}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	bus := &captureBus{}
	p := &Plugin{
		bus:              bus,
		logger:           silentLogger(),
		client:           srv.Client(),
		dataDir:          t.TempDir(),
		pollInterval:     5 * time.Millisecond,
		defaultMaxTokens: 1024,
		anthropicAPIKey:  "test",
		anthropicBaseURL: srv.URL + "/anthropic/batches",
		active:           make(map[string]*activeBatch),
		pollers:          make(map[string]context.CancelFunc),
	}

	for _, id := range []string{"batch1", "batch2"} {
		ab := &activeBatch{Provider: "anthropic", BatchID: id, SubmittedAt: time.Now()}
		p.startPoller(ab)
	}

	if !waitFor(t, 2*time.Second, func() bool {
		return bus.hasResult("batch1") && bus.hasResult("batch2")
	}) {
		t.Fatalf("expected both batches to finalize")
	}

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// =====================================================================
// Shutdown
// =====================================================================

// TestShutdownStopsPollersWithoutDeletingState confirms that Shutdown drops
// active pollers but leaves the state files behind for next-boot resume.
func TestShutdownStopsPollersWithoutDeletingState(t *testing.T) {
	// Backend that NEVER returns "completed" — in_progress forever.
	mux := http.NewServeMux()
	mux.HandleFunc("/anthropic/batches/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"processing_status":"in_progress","request_counts":{"processing":1}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	state := &batchState{
		Provider:    "anthropic",
		BatchID:     "msgbatch_persist",
		SubmittedAt: time.Now().UTC(),
	}
	if err := saveBatch(dir, state); err != nil {
		t.Fatalf("seed: %v", err)
	}

	bus := &captureBus{}
	p := &Plugin{
		bus:              bus,
		logger:           silentLogger(),
		client:           srv.Client(),
		dataDir:          dir,
		pollInterval:     5 * time.Millisecond,
		defaultMaxTokens: 1024,
		anthropicAPIKey:  "test",
		anthropicBaseURL: srv.URL + "/anthropic/batches",
		active:           make(map[string]*activeBatch),
		pollers:          make(map[string]context.CancelFunc),
	}

	if err := p.resumePersistedBatches(); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// Give the poller a couple of ticks to start running, then shut down.
	time.Sleep(20 * time.Millisecond)

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// State file should still be there for next-boot resume.
	loaded, err := loadBatches(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 state file after shutdown, got %d", len(loaded))
	}
}

// =====================================================================
// Wire-format adapter sanity checks
// =====================================================================

func TestBuildAnthropicMessageBody_RejectsEmptyModel(t *testing.T) {
	_, err := buildAnthropicMessageBody(events.LLMRequest{}, 1024)
	if err == nil {
		t.Fatalf("expected error for empty model")
	}
}

func TestBuildAnthropicMessageBody_DefaultMaxTokens(t *testing.T) {
	body, err := buildAnthropicMessageBody(events.LLMRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	}, 4096)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if body["max_tokens"] != 4096 {
		t.Fatalf("expected default max_tokens=4096, got %v", body["max_tokens"])
	}
}

func TestBuildAnthropicMessageBody_SystemSplitOut(t *testing.T) {
	body, err := buildAnthropicMessageBody(events.LLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []events.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hi"},
		},
	}, 1024)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if body["system"] != "be terse" {
		t.Fatalf("expected system field, got %#v", body)
	}
	msgs, _ := body["messages"].([]map[string]any)
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Fatalf("expected only user message, got %#v", msgs)
	}
}

func TestBuildOpenAIChatBody_BasicShape(t *testing.T) {
	body, err := buildOpenAIChatBody(events.LLMRequest{
		Model: "gpt-4o-mini",
		Messages: []events.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hi"},
		},
		ResponseFormat: &events.ResponseFormat{
			Type:   "json_schema",
			Name:   "answer",
			Schema: map[string]any{"type": "object"},
			Strict: true,
		},
	}, 1024)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if body["model"] != "gpt-4o-mini" {
		t.Fatalf("expected model field, got %#v", body)
	}
	rf, ok := body["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("expected json_schema response_format, got %#v", body["response_format"])
	}
}

// =====================================================================
// Test bus + helpers
// =====================================================================

// captureBus is a minimal EventBus implementation that records every event
// emitted. We don't need full bus semantics — just the assertion path.
type captureBus struct {
	mu     sync.Mutex
	events []events.BatchResults
	status []events.BatchStatus
}

func (b *captureBus) Emit(eventType string, payload any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch eventType {
	case "llm.batch.results":
		if r, ok := payload.(events.BatchResults); ok {
			b.events = append(b.events, r)
		}
	case "llm.batch.status":
		if s, ok := payload.(events.BatchStatus); ok {
			b.status = append(b.status, s)
		}
	}
	return nil
}

func (b *captureBus) EmitEvent(_ engine.Event[any]) error { return nil }
func (b *captureBus) EmitAsync(_ string, _ any) <-chan error {
	ch := make(chan error, 1)
	ch <- nil
	close(ch)
	return ch
}
func (b *captureBus) Subscribe(_ string, _ engine.HandlerFunc, _ ...engine.SubscribeOption) func() {
	return func() {}
}
func (b *captureBus) SubscribeAll(_ engine.HandlerFunc) func()       { return func() {} }
func (b *captureBus) SubscribeAllReplay(_ engine.HandlerFunc) func() { return func() {} }
func (b *captureBus) EmitVetoable(_ string, _ any) (engine.VetoResult, error) {
	return engine.VetoResult{}, nil
}
func (b *captureBus) Drain(_ context.Context) error { return nil }

func (b *captureBus) hasResult(batchID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.events {
		if r.BatchID == batchID {
			return true
		}
	}
	return false
}

// waitFor polls fn until it returns true or the timeout elapses. Returns the
// final state of fn() so the caller can fail the test cleanly.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fn()
}

// Compile-time check: captureBus implements engine.EventBus.
var _ engine.EventBus = (*captureBus)(nil)

// Compile-time check: errors.New is referenced so we don't get unused-import
// flags when the tests below don't use it. (Reserved for future error-path
// tests.)
var _ = errors.New
