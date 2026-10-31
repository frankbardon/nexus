package client

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/frankbardon/nexus/pkg/engine"
)

// recordHandler is a slog.Handler that keeps every record (with its attrs,
// including those added via With) so tests can assert level + fields.
type recordHandler struct {
	mu    *sync.Mutex
	recs  *[]capturedRecord
	attrs []slog.Attr
}

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func newRecordHandler() *recordHandler {
	return &recordHandler{mu: &sync.Mutex{}, recs: &[]capturedRecord{}}
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	c := capturedRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	for _, a := range h.attrs {
		c.attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		c.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	*h.recs = append(*h.recs, c)
	h.mu.Unlock()
	return nil
}

func (h *recordHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordHandler{mu: h.mu, recs: h.recs, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *recordHandler) WithGroup(string) slog.Handler { return h }

func (h *recordHandler) atLevel(l slog.Level) []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []capturedRecord
	for _, r := range *h.recs {
		if r.level == l {
			out = append(out, r)
		}
	}
	return out
}

// fakeSubscriber stands in for *mcp.ClientSession.
type fakeSubscriber struct {
	res      *mcp.InitializeResult
	err      error
	unsubErr error
	calls    []string
	unsubs   []string
}

func (f *fakeSubscriber) InitializeResult() *mcp.InitializeResult { return f.res }

func (f *fakeSubscriber) Subscribe(_ context.Context, p *mcp.SubscribeParams) error {
	f.calls = append(f.calls, p.URI)
	return f.err
}

func (f *fakeSubscriber) Unsubscribe(_ context.Context, p *mcp.UnsubscribeParams) error {
	f.unsubs = append(f.unsubs, p.URI)
	return f.unsubErr
}

func newSubscribeTestServer(h *recordHandler) *server {
	cfg := ServerConfig{Name: "srv", Timeout: time.Second}
	cfg.Resources.SubscribeUpdates = true
	return newServer(cfg, &Plugin{logger: slog.New(h)})
}

func resourcesFor(uris ...string) []*mcp.Resource {
	out := make([]*mcp.Resource, 0, len(uris))
	for _, u := range uris {
		out = append(out, &mcp.Resource{URI: u, Name: u})
	}
	return out
}

func TestSubscribe_SkipsWhenCapabilityNotAdvertised(t *testing.T) {
	cases := map[string]*mcp.InitializeResult{
		"nil result":       nil,
		"nil capabilities": {ProtocolVersion: "2026-07-28"},
		"no resources cap": {ProtocolVersion: "2026-07-28", Capabilities: &mcp.ServerCapabilities{}},
		"subscribe false": {ProtocolVersion: "2025-11-25", Capabilities: &mcp.ServerCapabilities{
			Resources: &mcp.ResourceCapabilities{ListChanged: true},
		}},
	}
	for name, res := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRecordHandler()
			s := newSubscribeTestServer(h)
			fake := &fakeSubscriber{res: res, err: errors.New("must not be called")}

			s.subscribeWith(context.Background(), fake, resourcesFor("test://a", "test://b"))

			if len(fake.calls) != 0 {
				t.Fatalf("Subscribe called %v on a server without resources.subscribe", fake.calls)
			}
			if w := h.atLevel(slog.LevelWarn); len(w) != 0 {
				t.Fatalf("unsupported path must skip cleanly, got warnings: %+v", w)
			}
		})
	}
}

func TestSubscribe_FailureOnAdvertisingServerWarnsOncePerServer(t *testing.T) {
	h := newRecordHandler()
	s := newSubscribeTestServer(h)
	fake := &fakeSubscriber{
		res: &mcp.InitializeResult{ProtocolVersion: "2025-11-25", Capabilities: &mcp.ServerCapabilities{
			Resources: &mcp.ResourceCapabilities{Subscribe: true},
		}},
		err: errors.New("method not found"),
	}

	// Two refreshes over two URIs: four failures, one Warn.
	s.subscribeWith(context.Background(), fake, resourcesFor("test://a", "test://b"))
	s.subscribeWith(context.Background(), fake, resourcesFor("test://a", "test://b"))

	if len(fake.calls) != 4 {
		t.Fatalf("Subscribe calls = %v, want 4", fake.calls)
	}
	warns := h.atLevel(slog.LevelWarn)
	if len(warns) != 1 {
		t.Fatalf("got %d warnings, want exactly 1: %+v", len(warns), warns)
	}
	if warns[0].attrs["server"] != "srv" || warns[0].attrs["uri"] != "test://a" {
		t.Fatalf("warning must name server and URI, got attrs %v", warns[0].attrs)
	}
	if !strings.Contains(warns[0].attrs["error"], "method not found") {
		t.Fatalf("warning must carry the error, got attrs %v", warns[0].attrs)
	}
}

func TestSubscribe_DisabledByConfig(t *testing.T) {
	h := newRecordHandler()
	s := newSubscribeTestServer(h)
	s.cfg.Resources.SubscribeUpdates = false
	// No session is set; with the flag off nothing must even look for one.
	s.syncSubscriptions(context.Background(), resourcesFor("test://a"))
	if len(h.atLevel(slog.LevelWarn)) != 0 {
		t.Fatal("subscribe_updates: false must not log")
	}
}

// TestSubscribe_InProcessUnsupportedServerSkips drives the real SDK over the
// in-memory transport: the shared test server registers a resource but no
// SubscribeHandler, so it advertises resources without subscribe. Boot must
// not warn (and must not attempt a subscription the server would reject).
func TestSubscribe_InProcessUnsupportedServerSkips(t *testing.T) {
	newTestInProcessServer(t)
	h := newRecordHandler()
	p := bootInProcessWithLogger(t, inprocessKey, slog.New(h))

	sess := p.servers["host"].getSession()
	if sess == nil {
		t.Fatal("server not connected")
	}
	if supportsResourceSubscribe(sess.InitializeResult()) {
		t.Fatal("test server unexpectedly advertises resources.subscribe")
	}
	if w := h.atLevel(slog.LevelWarn); len(w) != 0 {
		t.Fatalf("unexpected warnings on unsupported server: %+v", w)
	}
	var skipped bool
	for _, r := range h.atLevel(slog.LevelDebug) {
		if strings.Contains(r.msg, "does not advertise resources.subscribe") {
			skipped = true
			t.Logf("negotiated protocol %s", r.attrs["protocol_version"])
		}
	}
	if !skipped {
		t.Fatal("expected the capability gate to log a Debug skip")
	}
}

// TestSubscribe_InProcessSupportedServerSubscribes asserts the gate lets a
// server that does advertise resources.subscribe through, over whichever
// protocol the SDK negotiates.
func TestSubscribe_InProcessSupportedServerSubscribes(t *testing.T) {
	const key = "test-host-server-subscribe"
	got := make(chan string, 4)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-host", Version: "v0"}, &mcp.ServerOptions{
		SubscribeHandler: func(_ context.Context, req *mcp.SubscribeRequest) error {
			got <- req.Params.URI
			return nil
		},
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	srv.AddResource(&mcp.Resource{URI: "test://greeting", Name: "greeting", MIMEType: "text/plain"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{}, nil
		})
	RegisterInProcessServer(key, srv)
	t.Cleanup(func() { UnregisterInProcessServer(key) })

	h := newRecordHandler()
	bootInProcessWithLogger(t, key, slog.New(h))

	select {
	case uri := <-got:
		if uri != "test://greeting" {
			t.Fatalf("subscribed to %q, want test://greeting", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never received a subscription for test://greeting")
	}
	if w := h.atLevel(slog.LevelWarn); len(w) != 0 {
		t.Fatalf("unexpected warnings: %+v", w)
	}
}

func bootInProcessWithLogger(t *testing.T, key string, logger *slog.Logger) *Plugin {
	t.Helper()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:    engine.NewEventBus(),
		Logger: logger,
		Config: map[string]any{
			"servers": []any{
				map[string]any{
					"name":      "host",
					"transport": "inprocess",
					"server":    key,
					"lifecycle": "engine",
				},
			},
		},
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func subscribingFake() *fakeSubscriber {
	return &fakeSubscriber{res: &mcp.InitializeResult{ProtocolVersion: "2025-11-25", Capabilities: &mcp.ServerCapabilities{
		Resources: &mcp.ResourceCapabilities{Subscribe: true, ListChanged: true},
	}}}
}

func trackedURIs(s *server) map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]struct{}, len(s.subscribedURIs))
	for u := range s.subscribedURIs {
		out[u] = struct{}{}
	}
	return out
}

// TestSubscribe_ListChangedUnsubscribesDroppedURIs: a refresh that no longer
// lists a subscribed URI sends Unsubscribe for it and stops tracking it,
// while the URIs still listed stay subscribed.
func TestSubscribe_ListChangedUnsubscribesDroppedURIs(t *testing.T) {
	h := newRecordHandler()
	s := newSubscribeTestServer(h)
	fake := subscribingFake()

	s.syncSubscriptionsWith(context.Background(), fake, resourcesFor("test://a", "test://b"))
	if got := trackedURIs(s); len(got) != 2 {
		t.Fatalf("tracked = %v, want a and b", got)
	}
	if len(fake.unsubs) != 0 {
		t.Fatalf("first sync must not unsubscribe, got %v", fake.unsubs)
	}

	s.syncSubscriptionsWith(context.Background(), fake, resourcesFor("test://a"))
	if len(fake.unsubs) != 1 || fake.unsubs[0] != "test://b" {
		t.Fatalf("unsubscribes = %v, want [test://b]", fake.unsubs)
	}
	got := trackedURIs(s)
	if _, ok := got["test://b"]; ok || len(got) != 1 {
		t.Fatalf("tracked = %v, want only test://a", got)
	}

	// The overflow path (nil keep set) drops every remaining subscription.
	s.syncSubscriptionsWith(context.Background(), fake, nil)
	if len(fake.unsubs) != 2 || fake.unsubs[1] != "test://a" {
		t.Fatalf("unsubscribes = %v, want test://a dropped on overflow", fake.unsubs)
	}
	if got := trackedURIs(s); len(got) != 0 {
		t.Fatalf("tracked = %v, want empty", got)
	}
}

// TestSubscribe_UnsubscribeFailureIsLoggedNotFatal: a failed Unsubscribe is
// logged and the URI still leaves local tracking.
func TestSubscribe_UnsubscribeFailureIsLoggedNotFatal(t *testing.T) {
	h := newRecordHandler()
	s := newSubscribeTestServer(h)
	fake := subscribingFake()
	fake.unsubErr = errors.New("boom")

	s.syncSubscriptionsWith(context.Background(), fake, resourcesFor("test://a", "test://b"))
	s.syncSubscriptionsWith(context.Background(), fake, resourcesFor("test://a"))

	if _, ok := trackedURIs(s)["test://b"]; ok {
		t.Fatal("test://b must leave tracking even when Unsubscribe fails")
	}
	warns := h.atLevel(slog.LevelWarn)
	if len(warns) != 1 || warns[0].attrs["uri"] != "test://b" || !strings.Contains(warns[0].attrs["error"], "boom") {
		t.Fatalf("want one Warn naming test://b and the error, got %+v", warns)
	}
}

// TestSubscribe_FailedSubscribeIsNotTracked: only acknowledged
// subscriptions are tracked, so a dropped URI that never subscribed is not
// unsubscribed.
func TestSubscribe_FailedSubscribeIsNotTracked(t *testing.T) {
	h := newRecordHandler()
	s := newSubscribeTestServer(h)
	fake := subscribingFake()
	fake.err = errors.New("nope")

	s.syncSubscriptionsWith(context.Background(), fake, resourcesFor("test://a"))
	fake.err = nil
	s.syncSubscriptionsWith(context.Background(), fake, nil)

	if len(fake.unsubs) != 0 {
		t.Fatalf("unsubscribed %v, but nothing was ever subscribed", fake.unsubs)
	}
}

// TestSubscribe_InProcessListChangedDropsSubscription drives the real SDK
// over the in-memory transport: the server lists two subscribable resources,
// then removes one, which pushes resources/list_changed. The client's
// refresh must stop tracking the dropped URI's subscription.
func TestSubscribe_InProcessListChangedDropsSubscription(t *testing.T) {
	const key = "test-host-server-list-changed"
	subscribed := make(chan string, 8)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-host", Version: "v0"}, &mcp.ServerOptions{
		SubscribeHandler: func(_ context.Context, req *mcp.SubscribeRequest) error {
			subscribed <- req.Params.URI
			return nil
		},
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	read := func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	}
	srv.AddResource(&mcp.Resource{URI: "test://keep", Name: "keep", MIMEType: "text/plain"}, read)
	srv.AddResource(&mcp.Resource{URI: "test://drop", Name: "drop", MIMEType: "text/plain"}, read)
	RegisterInProcessServer(key, srv)
	t.Cleanup(func() { UnregisterInProcessServer(key) })

	h := newRecordHandler()
	p := bootInProcessWithLogger(t, key, slog.New(h))
	s := p.servers["host"]

	waitFor(t, func() bool { return len(trackedURIs(s)) == 2 }, "both URIs subscribed")

	srv.RemoveResources("test://drop")

	waitFor(t, func() bool {
		got := trackedURIs(s)
		_, dropped := got["test://drop"]
		_, kept := got["test://keep"]
		return !dropped && kept && len(got) == 1
	}, "test://drop unsubscribed after resources/list_changed")

	var logged bool
	for _, r := range h.atLevel(slog.LevelDebug) {
		if strings.Contains(r.msg, "unsubscribed resource dropped") && r.attrs["uri"] == "test://drop" {
			logged = true
		}
	}
	if !logged {
		t.Fatal("expected a Debug record for the unsubscribe of test://drop")
	}
	if w := h.atLevel(slog.LevelWarn); len(w) != 0 {
		t.Fatalf("unexpected warnings: %+v", w)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
