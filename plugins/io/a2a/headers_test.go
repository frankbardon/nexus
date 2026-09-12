package a2a

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
)

// withRequestHeader sets one arbitrary header on the request under test.
func withRequestHeader(key, value string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(key, value) }
}

// newSessionTestPlugin is newTestPlugin with a real SessionWorkspace attached,
// so the reserved "_header.*" binding actually lands somewhere readable.
func newSessionTestPlugin(t *testing.T) (*Plugin, engine.EventBus, *engine.SessionWorkspace) {
	t.Helper()
	bus := engine.NewEventBus()
	session, err := engine.NewSessionWorkspace(filepath.Join(t.TempDir(), "sessions"), bus)
	if err != nil {
		t.Fatalf("new session workspace: %v", err)
	}
	p, ok := New().(*Plugin)
	if !ok {
		t.Fatal("New() did not return *Plugin")
	}
	if err := p.Init(engine.PluginContext{
		Config:  testConfig(t, nil),
		Bus:     bus,
		Logger:  discardLogger(),
		Storage: testStorage(t),
		Session: session,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })
	return p, bus, session
}

// A2A carries out-of-band caller context on the transport, so the header must
// reach the turn's io.input while nothing unprefixed does.
func TestSendMessageCarriesRequestHeadersOntoTheBus(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	log := playAgent(t, bus, scriptedTurn("ok"))

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		withRequestHeader("X-Nexus-Tenant-ID", "acme"),
		withRequestHeader("X-Nexus-Timezone", "Europe/Amsterdam"),
		withRequestHeader("Authorization", "Bearer should-not-appear"),
		jsonrpcBody(t, a2a.MethodSendStreamingMessage, sendMessageParams("hello", "ctx-hdr")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	prompts := log.all()
	if len(prompts) != 1 {
		t.Fatalf("io.input emissions = %d, want exactly one", len(prompts))
	}
	got := prompts[0].Headers
	if got["tenant-id"] != "acme" || got["timezone"] != "Europe/Amsterdam" {
		t.Errorf("io.input Headers = %v, want the two X-Nexus-* headers", got)
	}
	if len(got) != 2 {
		t.Errorf("io.input Headers = %v, want nothing outside the X-Nexus-* namespace", got)
	}
}

// The same values must also land in the reserved session labels, which is the
// read seam for a plugin that never sees io.input.
func TestSendMessageBindsRequestHeadersToTheSession(t *testing.T) {
	p, bus, session := newSessionTestPlugin(t)

	// Read the labels from inside the turn: endTurn clears them once it
	// settles, so a read afterwards would legitimately see nothing.
	seen := make(chan map[string]string, 1)
	bus.Subscribe("io.input", func(engine.Event[any]) {
		hdrs, err := session.RequestHeaders()
		if err != nil {
			t.Errorf("RequestHeaders: %v", err)
		}
		select {
		case seen <- hdrs:
		default:
		}
	}, engine.WithSource("test.reader"))
	playAgent(t, bus, scriptedTurn("ok"))

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		withRequestHeader("X-Nexus-Tenant", "acme"),
		jsonrpcBody(t, a2a.MethodSendStreamingMessage, sendMessageParams("hello", "ctx-bind")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	hdrs := <-seen
	if hdrs["tenant"] != "acme" {
		t.Errorf("session request headers during the turn = %v, want tenant=acme", hdrs)
	}
}
