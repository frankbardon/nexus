package gemini

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestNewCacheState_Defaults(t *testing.T) {
	cs := newCacheState(map[string]any{}, slog.Default())
	if cs.enabled {
		t.Fatal("expected disabled by default")
	}
	if cs.minTokens != 32768 {
		t.Fatalf("minTokens default %d", cs.minTokens)
	}
	if cs.ttl != time.Hour {
		t.Fatalf("ttl default %v", cs.ttl)
	}
}

func TestNewCacheState_FromConfig(t *testing.T) {
	cs := newCacheState(map[string]any{
		"cache": map[string]any{
			"enabled":     true,
			"min_tokens":  10000,
			"ttl":         "30m",
			"max_entries": 8,
		},
	}, slog.Default())
	if !cs.enabled {
		t.Fatal("enabled should be true")
	}
	if cs.minTokens != 10000 {
		t.Fatalf("minTokens %d", cs.minTokens)
	}
	if cs.ttl != 30*time.Minute {
		t.Fatalf("ttl %v", cs.ttl)
	}
	if cs.maxEntries != 8 {
		t.Fatalf("maxEntries %d", cs.maxEntries)
	}
}

func TestPrefixHash_StableAcrossToolOrder(t *testing.T) {
	cs := newCacheState(map[string]any{"cache": map[string]any{"enabled": true}}, slog.Default())

	t1 := []events.ToolDef{{Name: "b", Description: "B"}, {Name: "a", Description: "A"}}
	t2 := []events.ToolDef{{Name: "a", Description: "A"}, {Name: "b", Description: "B"}}

	h1 := cs.prefixHash("gemini-2.5-flash", "system", t1, nil)
	h2 := cs.prefixHash("gemini-2.5-flash", "system", t2, nil)

	if h1 != h2 {
		t.Fatalf("hash should be tool-order-stable: %s vs %s", h1, h2)
	}
}

func TestPrefixHash_StopsAtFunctionResponse(t *testing.T) {
	cs := newCacheState(map[string]any{"cache": map[string]any{"enabled": true}}, slog.Default())

	stableContents := []map[string]any{
		{"role": "user", "parts": []map[string]any{{"text": "stable"}}},
	}
	withTool := append([]map[string]any{}, stableContents...)
	withTool = append(withTool,
		map[string]any{"role": "user", "parts": []map[string]any{{"functionResponse": map[string]any{"name": "x"}}}},
		map[string]any{"role": "user", "parts": []map[string]any{{"text": "different tail"}}},
	)

	h1 := cs.prefixHash("model", "sys", nil, stableContents)
	h2 := cs.prefixHash("model", "sys", nil, withTool)

	if h1 != h2 {
		t.Fatalf("trailing functionResponse turn must not affect prefix hash: %s vs %s", h1, h2)
	}
}

func TestCachePopulateLookup(t *testing.T) {
	cs := newCacheState(map[string]any{"cache": map[string]any{"enabled": true, "ttl": "1h"}}, slog.Default())
	contents := []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "hi"}}}}

	if got := cs.lookup("m", "s", nil, contents); got != "" {
		t.Fatalf("expected empty before populate, got %q", got)
	}

	cs.populate("m", "s", nil, contents, "cachedContents/abc", time.Hour)

	got := cs.lookup("m", "s", nil, contents)
	if got != "cachedContents/abc" {
		t.Fatalf("expected hit, got %q", got)
	}
}

func TestCacheExpiry(t *testing.T) {
	cs := newCacheState(map[string]any{"cache": map[string]any{"enabled": true}}, slog.Default())
	contents := []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "hi"}}}}

	cs.populate("m", "s", nil, contents, "cachedContents/expired", -1*time.Second)

	if got := cs.lookup("m", "s", nil, contents); got != "" {
		t.Fatalf("expected expired entry to miss, got %q", got)
	}
}

func TestCacheInvalidate(t *testing.T) {
	cs := newCacheState(map[string]any{"cache": map[string]any{"enabled": true}}, slog.Default())
	contents := []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "hi"}}}}

	cs.populate("m", "s", nil, contents, "cachedContents/abc", time.Hour)
	cs.invalidate("cachedContents/abc")

	if got := cs.lookup("m", "s", nil, contents); got != "" {
		t.Fatalf("expected miss after invalidate, got %q", got)
	}
}

func TestCreateCachedContent_Disabled(t *testing.T) {
	p := &Plugin{
		client: http.DefaultClient,
		logger: slog.Default(),
		auth:   &authState{mode: authModeAPIKey, apiKey: "k"},
		cache:  newCacheState(map[string]any{}, slog.Default()),
	}
	if _, err := p.createCachedContent(context.Background(), "m", "s", nil, nil, time.Hour); err == nil {
		t.Fatal("expected error when cache disabled")
	}
}

func TestCreateCachedContentAt_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "models/gemini-2.5-flash" {
			t.Errorf("unexpected model: %v", body["model"])
		}
		if body["ttl"] != "60s" {
			t.Errorf("unexpected ttl: %v", body["ttl"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "cachedContents/xyz",
		})
	}))
	defer srv.Close()

	p := &Plugin{
		client: srv.Client(),
		logger: slog.Default(),
		auth:   &authState{mode: authModeAPIKey, apiKey: "k"},
		cache: newCacheState(map[string]any{
			"cache": map[string]any{"enabled": true, "ttl": "1h"},
		}, slog.Default()),
	}

	contents := []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "hello"}}}}
	name, err := p.createCachedContentAt(context.Background(), srv.URL, "gemini-2.5-flash", "sys", nil, contents, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if name != "cachedContents/xyz" {
		t.Fatalf("unexpected cache name: %s", name)
	}

	// Cache should now contain the entry under the prefix hash.
	if got := p.cache.lookup("gemini-2.5-flash", "sys", nil, contents); got != "cachedContents/xyz" {
		t.Fatalf("expected cache hit after create, got %q", got)
	}
}

func TestCachedContentsURL(t *testing.T) {
	a := &authState{mode: authModeAPIKey}
	want := "https://generativelanguage.googleapis.com/v1beta/cachedContents"
	if got := a.cachedContentsURL(); got != want {
		t.Fatalf("api-key URL: got %q want %q", got, want)
	}

	v := &authState{mode: authModeVertex, projectID: "myproj", location: "us-central1"}
	wantV := "https://us-central1-aiplatform.googleapis.com/v1/projects/myproj/locations/us-central1/cachedContents"
	if got := v.cachedContentsURL(); got != wantV {
		t.Fatalf("vertex URL: got %q want %q", got, wantV)
	}
}
