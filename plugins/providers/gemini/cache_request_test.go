package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

// cacheTestTools is the tool set every cache-shape test request carries.
var cacheTestTools = []events.ToolDef{{
	Name:        "lookup",
	Description: "Look something up.",
	Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
}}

// cacheTestMessages is a conversation whose stable prefix is the first two
// turns (user, model functionCall); the functionResponse turn and the
// follow-up after it are the dynamic tail.
func cacheTestMessages() []events.Message {
	return []events.Message{
		{Role: "system", Content: "You are terse."},
		{Role: "user", Content: "find it"},
		{Role: "assistant", ToolCalls: []events.ToolCallRequest{{ID: "c1", Name: "lookup", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Content: `{"found":true}`},
	}
}

func newCacheRequestPlugin(t *testing.T) *Plugin {
	t.Helper()
	cs, err := newCacheState(map[string]any{"cache": map[string]any{"enabled": true}}, quietLogger())
	if err != nil {
		t.Fatalf("newCacheState: %v", err)
	}
	return &Plugin{
		logger: quietLogger(),
		auth:   &authState{mode: authModeAPIKey, apiKey: "k"},
		cache:  cs,
	}
}

// populateFor records a cache entry for exactly the prefix the body builder
// will hash for msgs, the way createCachedContentAt does after a create.
func populateFor(t *testing.T, p *Plugin, model string, msgs []events.Message, tools []events.ToolDef) {
	t.Helper()
	system, contents, err := p.convertMessages(msgs)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	p.cache.populate(model, system, tools, contents, "cachedContents/abc", time.Hour)
}

func marshalBody(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// On a hit the body carries cachedContent and only what the cache does not
// cover: no systemInstruction, no tools, and contents trimmed to the turns
// after the cached prefix.
func TestBuildRequestBody_CacheHitOmitsCoveredFields(t *testing.T) {
	p := newCacheRequestPlugin(t)
	msgs := cacheTestMessages()
	populateFor(t, p, "gemini-2.5-flash", msgs, cacheTestTools)

	body, err := p.buildRequestBody(context.Background(), "gemini-2.5-flash", 0, "", events.LLMRequest{
		Messages: msgs,
		Tools:    cacheTestTools,
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	wire := marshalBody(t, body)

	if wire["cachedContent"] != "cachedContents/abc" {
		t.Fatalf("cachedContent = %v, want cachedContents/abc", wire["cachedContent"])
	}
	for _, key := range []string{"systemInstruction", "tools", "toolConfig"} {
		if _, ok := wire[key]; ok {
			t.Errorf("%s sent alongside cachedContent; the cache covers it", key)
		}
	}
	contents, _ := wire["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents has %d entries, want 1 (the functionResponse tail): %v", len(contents), contents)
	}
	first, _ := contents[0].(map[string]any)
	parts, _ := first["parts"].([]any)
	part, _ := parts[0].(map[string]any)
	if _, ok := part["functionResponse"]; !ok {
		t.Fatalf("tail turn is %v, want the functionResponse turn", first)
	}
}

// A miss leaves the full body intact.
func TestBuildRequestBody_CacheMissSendsFullBody(t *testing.T) {
	p := newCacheRequestPlugin(t)
	msgs := cacheTestMessages()

	body, err := p.buildRequestBody(context.Background(), "gemini-2.5-flash", 0, "", events.LLMRequest{
		Messages: msgs,
		Tools:    cacheTestTools,
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	wire := marshalBody(t, body)
	if _, ok := wire["cachedContent"]; ok {
		t.Fatal("cachedContent set on a miss")
	}
	for _, key := range []string{"systemInstruction", "tools"} {
		if _, ok := wire[key]; !ok {
			t.Errorf("%s missing from an uncached body", key)
		}
	}
	if contents, _ := wire["contents"].([]any); len(contents) != 3 {
		t.Fatalf("contents has %d entries, want all 3", len(contents))
	}
}

// A request that needs a toolConfig cannot use an entry created without one —
// toolConfig may not ride alongside cachedContent — so it goes out uncached.
func TestBuildRequestBody_CacheSkippedWithToolConfig(t *testing.T) {
	p := newCacheRequestPlugin(t)
	msgs := cacheTestMessages()
	populateFor(t, p, "gemini-2.5-flash", msgs, cacheTestTools)

	body, err := p.buildRequestBody(context.Background(), "gemini-2.5-flash", 0, "", events.LLMRequest{
		Messages:   msgs,
		Tools:      cacheTestTools,
		ToolChoice: &events.ToolChoice{Mode: "required"},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	if _, ok := body["cachedContent"]; ok {
		t.Fatal("cachedContent set on a request carrying toolConfig")
	}
	if _, ok := body["toolConfig"]; !ok {
		t.Fatal("toolConfig dropped")
	}
	if _, ok := body["tools"]; !ok {
		t.Fatal("tools dropped from an uncached body")
	}
}

// A request that is exactly the cached prefix has no tail to send, and
// generateContent requires at least one content, so it goes out uncached.
func TestBuildRequestBody_CacheSkippedWithEmptyTail(t *testing.T) {
	p := newCacheRequestPlugin(t)
	msgs := cacheTestMessages()[:3] // no functionResponse: the whole run is the prefix
	populateFor(t, p, "gemini-2.5-flash", msgs, cacheTestTools)

	body, err := p.buildRequestBody(context.Background(), "gemini-2.5-flash", 0, "", events.LLMRequest{
		Messages: msgs,
		Tools:    cacheTestTools,
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	if _, ok := body["cachedContent"]; ok {
		t.Fatal("cachedContent set with nothing after the cached prefix")
	}
	if contents, _ := body["contents"].([]map[string]any); len(contents) != 2 {
		t.Fatalf("contents has %d entries, want the full 2", len(contents))
	}
}

// createCachedContentAt stores only the prefix the hash covers and records its
// length, so a later hit strips exactly that many contents.
func TestCreateCachedContentAt_StoresOnlyPrefix(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "cachedContents/xyz"})
	}))
	defer srv.Close()

	p := newCacheRequestPlugin(t)
	p.client = srv.Client()
	p.codeExecution = true

	system, contents, err := p.convertMessages(cacheTestMessages())
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if _, err := p.createCachedContentAt(context.Background(), srv.URL, "gemini-2.5-flash", system, cacheTestTools, contents, time.Minute); err != nil {
		t.Fatalf("createCachedContentAt: %v", err)
	}

	if got, _ := sent["contents"].([]any); len(got) != 2 {
		t.Fatalf("cache create sent %d contents, want the 2-turn prefix", len(got))
	}
	tools, _ := sent["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("cache create sent %d tool groups, want functionDeclarations + codeExecution", len(tools))
	}
	name, covered := p.cache.lookupWith(true, "gemini-2.5-flash", system, cacheTestTools, contents)
	if name != "cachedContents/xyz" || covered != 2 {
		t.Fatalf("lookupWith = (%q, %d), want (cachedContents/xyz, 2)", name, covered)
	}
}

// blockingTransport never answers; it returns only when the request's context
// is done, standing in for a slow Files API upload.
type blockingTransport struct{ called chan struct{} }

func (b blockingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case b.called <- struct{}{}:
	default:
	}
	<-r.Context().Done()
	return nil, r.Context().Err()
}

// The Files API upload for an oversize part runs under the request context,
// so cancelling the request aborts the upload instead of leaving it to run.
func TestBuildRequestBody_CancelAbortsUpload(t *testing.T) {
	tr := blockingTransport{called: make(chan struct{}, 1)}
	p := &Plugin{
		client: &http.Client{Transport: tr},
		logger: quietLogger(),
		auth:   &authState{mode: authModeAPIKey, apiKey: "k"},
		cache:  &cacheState{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-tr.called
		cancel()
	}()

	big := make([]byte, inlineDataLimit+1)
	done := make(chan error, 1)
	go func() {
		_, err := p.buildRequestBody(ctx, "gemini-2.5-flash", 0, "", events.LLMRequest{
			Messages: []events.Message{{
				Role:  "user",
				Parts: []events.MessagePart{{Type: "image", MimeType: "image/png", Data: big}},
			}},
		})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("upload did not abort on cancellation")
	}
}
