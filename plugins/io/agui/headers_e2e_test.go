package agui

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// postWithHeaders is post() plus arbitrary extra request headers, which is the
// whole subject of this file.
func postWithHeaders(t *testing.T, url, body string, extra map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// runOneTurn drives a stub agent turn as soon as io.input lands, and returns
// the UserInput that started it.
func runOneTurn(t *testing.T, bus engine.EventBus, url, body string, extra map[string]string) events.UserInput {
	t.Helper()
	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})
	got := make(chan events.UserInput, 1)
	go func() {
		in := <-inputSeen
		got <- in
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-hdr"}
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "hi", Role: "assistant"})
		_ = bus.Emit("agent.turn.end", turn)
	}()

	resp := postWithHeaders(t, url, body, extra)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if _, err := agui.NewSSEReader(resp.Body).ReadAll(); err != nil {
		t.Fatalf("read sse: %v", err)
	}
	return <-got
}

// The acceptance test: an X-Nexus-* header on the POST reaches the turn's
// io.input AND the session's reserved label namespace, and an unprefixed
// header does neither.
func TestE2E_RequestHeadersReachInputAndSession(t *testing.T) {
	p, bus, session, url := newSessionTestPlugin(t, nil)

	var mu sync.Mutex
	setValues := map[string]string{}
	bus.Subscribe("session.tag.set", func(e engine.Event[any]) {
		if s, ok := e.Payload.(events.SessionTagSet); ok {
			mu.Lock()
			setValues[s.Key] = s.Value
			mu.Unlock()
		}
	})

	body := `{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":"hello"}]}`
	in := runOneTurn(t, bus, url, body, map[string]string{
		"X-Nexus-Timezone":  "Europe/Amsterdam",
		"X-Nexus-Tenant-ID": "acme",
		"X-Trace-Id":        "should-not-appear",
		"Authorization":     "Bearer should-not-appear",
	})

	if in.Headers["timezone"] != "Europe/Amsterdam" {
		t.Errorf("io.input Headers[timezone] = %q, want Europe/Amsterdam (all: %v)", in.Headers["timezone"], in.Headers)
	}
	if in.Headers["tenant-id"] != "acme" {
		t.Errorf("io.input Headers[tenant-id] = %q, want acme", in.Headers["tenant-id"])
	}
	if len(in.Headers) != 2 {
		t.Errorf("io.input Headers = %v, want only the two X-Nexus-* headers", in.Headers)
	}

	mu.Lock()
	bound := setValues[engine.RequestHeaderLabelKey("timezone")]
	mu.Unlock()
	if bound != "Europe/Amsterdam" {
		t.Errorf("session.tag.set for the header label = %q, want Europe/Amsterdam", bound)
	}

	// endRun clears the namespace as the handler returns.
	waitFor(t, func() bool { return p.currentRun() == nil })
	left, err := session.RequestHeaders()
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("headers still bound after the run ended: %v", left)
	}
}

// A second run that drops a header must not inherit the first run's value.
func TestE2E_RequestHeadersDoNotLeakBetweenRuns(t *testing.T) {
	p, bus, _, url := newSessionTestPlugin(t, nil)

	body1 := `{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":"one"}]}`
	first := runOneTurn(t, bus, url, body1, map[string]string{"X-Nexus-Tenant": "acme"})
	if first.Headers["tenant"] != "acme" {
		t.Fatalf("first run Headers = %v, want tenant=acme", first.Headers)
	}
	waitFor(t, func() bool { return p.currentRun() == nil })

	body2 := `{"threadId":"t1","runId":"r2","messages":[{"id":"m2","role":"user","content":"two"}]}`
	second := runOneTurn(t, bus, url, body2, nil)
	if len(second.Headers) != 0 {
		t.Errorf("second run inherited headers: %v", second.Headers)
	}
}
