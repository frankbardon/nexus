package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// a2aTestConfig loads a broker config with two profiles, so every assertion
// about namespacing is made against a broker that genuinely fronts more than one
// agent. head carries whatever else a test needs (an `auth:` block).
func a2aTestConfig(t *testing.T, head string) Config {
	t.Helper()
	yaml := head + "listen_addr: \"127.0.0.1:8080\"\n" +
		agentsBlock(oneValidProfile("support", "/tmp/support.yaml")+
			`  research:
    config: "/tmp/research.yaml"
    card:
      name: "Research Agent"
      description: "Reads and summarizes."
      version: "0.1.0"
      skills:
        - id: "summarize"
          name: "Summarize"
          description: "Summarizes a document."
`)
	return mustLoadConfig(t, yaml)
}

// newA2ATestServer wires the A2A ingress the way run() does — registered THROUGH
// the guard, alongside every pre-existing route — over an httptest server.
//
// It deliberately reuses newBrokerTestServer rather than building a bespoke mux:
// the "this is additive" claim is only worth anything if the A2A routes are
// mounted on the same mux, behind the same guard, as /claim and /binaries.
func newA2ATestServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	agents, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	ts, _ := newBrokerTestServer(t, cfg, agents.Register)
	return ts
}

// recordingMux captures the patterns registered through it, which is how the
// "no agents block registers nothing" property is asserted directly rather than
// inferred from a 404.
type recordingMux struct{ patterns []string }

func (m *recordingMux) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
}

// TestA2ARoutesAreNamespacedPerProfile pins the route shape: every profile owns
// its own subtree, and the patterns registered are the ones a card advertises.
func TestA2ARoutesAreNamespacedPerProfile(t *testing.T) {
	cfg := a2aTestConfig(t, "")
	agents, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	rec := &recordingMux{}
	agents.Register(rec)

	got := append([]string(nil), rec.patterns...)
	sort.Strings(got)
	want := []string{
		"/agents/{profile}/a2a/v1/{rest...}",
		"GET /agents/{profile}/.well-known/agent-card.json",
		"HEAD /agents/{profile}/.well-known/agent-card.json",
		"POST /agents/{profile}/a2a",
	}
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("registered patterns =\n %v\nwant\n %v", got, want)
	}

	// Every registered pattern lives under /agents/, which is what makes this
	// additive: no existing broker route starts with that prefix, so nothing can
	// be shadowed.
	for _, p := range rec.patterns {
		if !strings.Contains(p, agentRoutePrefix) {
			t.Errorf("pattern %q is not under %q; it could shadow an existing route", p, agentRoutePrefix)
		}
	}
}

// TestA2ACardIsServedPerProfile: each profile answers with ITS OWN card at its
// own path, which is the whole of "profiles cannot collide" from a client's
// point of view.
func TestA2ACardIsServedPerProfile(t *testing.T) {
	ts := newA2ATestServer(t, a2aTestConfig(t, ""))

	for profile, wantName := range map[string]string{
		"support":  "Support Agent",
		"research": "Research Agent",
	} {
		resp, err := http.Get(ts.URL + agentCardPath(profile))
		if err != nil {
			t.Fatalf("GET card for %s: %v", profile, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET card for %s = %d, want 200: %s", profile, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != a2a.ContentTypeAgentCard {
			t.Errorf("card Content-Type = %q, want %q", ct, a2a.ContentTypeAgentCard)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("card for %s is not JSON: %v", profile, err)
		}
		if doc["name"] != wantName {
			t.Errorf("card at %s names %v, want %q", agentCardPath(profile), doc["name"], wantName)
		}

		// The conditional-request contract: a client that echoes the ETag is told
		// nothing changed rather than being handed the document again.
		req, _ := http.NewRequest(http.MethodGet, ts.URL+agentCardPath(profile), nil)
		req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
		cached, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("conditional GET: %v", err)
		}
		_ = cached.Body.Close()
		if cached.StatusCode != http.StatusNotModified {
			t.Errorf("conditional GET = %d, want 304", cached.StatusCode)
		}
	}
}

// TestA2AJSONRPCAnswersNotImplemented is the operational contract for an
// operation this ingress does NOT drive: the route exists, decodes a real A2A
// call, and refuses it with a well-formed UnsupportedOperationError rather than
// a 404 or an HTML error page.
//
// GetExtendedAgentCard is the subject. It was GetTask until the task store
// landed; the three task reads are dispatched now, and the only operations left
// unimplemented are the push-notification family and the extended card. Section
// 3.3.4 assigns UnsupportedOperationError to exactly this one, and the card
// backs the refusal up by advertising capabilities.extendedAgentCard as false —
// so a client gets a refusal it already knows how to handle.
func TestA2AJSONRPCAnswersNotImplemented(t *testing.T) {
	ts := newA2ATestServer(t, a2aTestConfig(t, ""))

	body := `{"jsonrpc":"2.0","id":7,"method":"GetExtendedAgentCard","params":{}}`
	resp, err := http.Post(ts.URL+agentJSONRPCPath("support"), a2a.ContentTypeJSON, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST jsonrpc: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	// 200: the JSON-RPC binding carries its outcome in the body. A non-200 would
	// tell an intermediary the request failed when it was answered.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST jsonrpc = %d, want 200: %s", resp.StatusCode, raw)
	}
	if v := resp.Header.Get(a2a.HeaderVersion); v != a2a.ProtocolVersion {
		t.Errorf("%s = %q, want %q", a2a.HeaderVersion, v, a2a.ProtocolVersion)
	}

	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("response is not a JSON-RPC envelope: %v (%s)", err, raw)
	}
	if envelope.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", envelope.JSONRPC)
	}
	// The id must be echoed: a client correlating responses by id has no other
	// way to attribute the refusal.
	if string(envelope.ID) != "7" {
		t.Errorf("id = %s, want 7", envelope.ID)
	}
	if envelope.Error == nil {
		t.Fatalf("no error object in %s", raw)
	}
	if want := a2a.NewError(a2a.ErrorTypeUnsupportedOperation, "").Code(); envelope.Error.Code != want {
		t.Errorf("error code = %d, want %d (UnsupportedOperationError)", envelope.Error.Code, want)
	}
	// The message names the OPERATION, so a client with several calls in flight
	// can tell which one was refused.
	if !strings.Contains(envelope.Error.Message, a2a.MethodGetExtendedAgentCard) {
		t.Errorf("error message = %q, want it to name %s", envelope.Error.Message, a2a.MethodGetExtendedAgentCard)
	}
}

// TestA2AImplementedOperationsAndCardAgree pins the invariant the card is built
// on: capabilities are DERIVED from brokerImplementedOperations, so a story that
// flips an operation cannot leave the advertised card behind, and one that
// advertises a capability cannot do so without a handler.
func TestA2AImplementedOperationsAndCardAgree(t *testing.T) {
	cfg := a2aTestConfig(t, "")
	agents, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	card := agents.card("support").card

	// streaming is true because BOTH streaming operations are dispatched: a turn
	// can be streamed as it runs, and an existing task can be subscribed to.
	if !card.Capabilities.Streaming {
		t.Error("capabilities.streaming = false, but the ingress dispatches the streaming operations")
	}
	for _, op := range []string{a2a.MethodSendStreamingMessage, a2a.MethodSubscribeToTask} {
		if !brokerOperationImplemented(op) {
			t.Errorf("%s is not implemented, so capabilities.streaming would be a lie", op)
		}
	}
	// The two capabilities that remain false must have no handler behind them.
	if card.Capabilities.PushNotifications || brokerOperationImplemented(a2a.MethodCreateTaskPushNotificationConfig) {
		t.Error("pushNotifications is advertised or dispatched; neither is built")
	}
	if card.Capabilities.ExtendedAgentCard || brokerOperationImplemented(a2a.MethodGetExtendedAgentCard) {
		t.Error("extendedAgentCard is advertised or dispatched; neither is built")
	}
	// And the reads this story implements are genuinely dispatched.
	for _, op := range []string{a2a.MethodGetTask, a2a.MethodListTasks, a2a.MethodSubscribeToTask} {
		if !brokerOperationImplemented(op) {
			t.Errorf("%s must be implemented by this story", op)
		}
	}
}

// TestA2AJSONRPCRejectsAMalformedCall proves the refusal is not a blanket
// response: a request that is not decodable is told THAT, so a client can get
// its envelope right before the operation exists.
func TestA2AJSONRPCRejectsAMalformedCall(t *testing.T) {
	ts := newA2ATestServer(t, a2aTestConfig(t, ""))

	resp, err := http.Post(ts.URL+agentJSONRPCPath("support"), a2a.ContentTypeJSON, strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("POST jsonrpc: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	var envelope struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("response is not a JSON-RPC envelope: %v (%s)", err, raw)
	}
	if envelope.Error == nil {
		t.Fatalf("malformed call was not refused: %s", raw)
	}
	if want := a2a.NewError(a2a.ErrorTypeJSONParse, "").Code(); envelope.Error.Code != want {
		t.Errorf("error code = %d, want %d (JSONParseError)", envelope.Error.Code, want)
	}
}

// TestA2ARESTAnswersNotImplemented: the same contract on the HTTP+JSON binding,
// where an A2A error carries a real HTTP status and the google.rpc.Status body.
//
// A push-notification route is the subject, and it is the one that still
// exercises THIS package's refusal rather than the codec's: pkg/a2a decodes no
// parameters for it, but the REST route table declares it so a server can answer
// it properly, so the request reaches brokerOperationImplemented and is refused
// by a2aNotImplemented with the "not yet implemented" wording.
func TestA2ARESTAnswersNotImplemented(t *testing.T) {
	ts := newA2ATestServer(t, a2aTestConfig(t, ""))

	resp, err := http.Get(ts.URL + agentRESTPrefix("support") + "/tasks/task-1/pushNotificationConfigs/cfg-1")
	if err != nil {
		t.Fatalf("GET rest: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET rest = %d, want 400 (UnsupportedOperationError's mapped status): %s", resp.StatusCode, raw)
	}
	var body a2a.RESTError
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("response is not a google.rpc.Status body: %v (%s)", err, raw)
	}
	if body.Error.Status != "FAILED_PRECONDITION" {
		t.Errorf("status = %q, want FAILED_PRECONDITION", body.Error.Status)
	}
	if recovered := body.AsError(); recovered.Type != a2a.ErrorTypeUnsupportedOperation {
		t.Errorf("error round-trips to %q, want UnsupportedOperationError", recovered.Type)
	}
	// The message says NOT YET, not merely "unsupported": that is the difference
	// a partner integrating against this broker needs to read.
	if !strings.Contains(body.Error.Message, "not yet implemented") {
		t.Errorf("error message = %q, want it to say the operation is not yet implemented", body.Error.Message)
	}
}

// TestA2ARESTUnknownPathsAndVerbs covers the two ways a REST request can be
// wrong about the URL, each with the answer a client can act on.
func TestA2ARESTUnknownPathsAndVerbs(t *testing.T) {
	ts := newA2ATestServer(t, a2aTestConfig(t, ""))

	// A path that names no operation at all.
	resp, err := http.Get(ts.URL + agentRESTPrefix("support") + "/nonsense")
	if err != nil {
		t.Fatalf("GET rest: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET an unmounted REST path = %d, want 404", resp.StatusCode)
	}

	// A path that names a real operation with the wrong verb: 405 plus Allow,
	// rather than a 404 that would send a client hunting for a typo in a correct
	// URL.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+agentRESTPrefix("support")+a2a.PathSendMessage, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET message:send: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on a POST-only operation = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, http.MethodPost) {
		t.Errorf("Allow = %q, want it to name POST", allow)
	}
}

// TestA2AUnknownProfileIsRefusedOnEveryBinding: a path naming no configured
// profile is a 404 on all three routes, in each binding's own error shape.
//
// It must not fall back to some default agent, for the same reason POST /claim
// refuses an unknown binary: serving a different agent than the one addressed
// produces a conversation that merely behaves oddly.
func TestA2AUnknownProfileIsRefusedOnEveryBinding(t *testing.T) {
	ts := newA2ATestServer(t, a2aTestConfig(t, ""))

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"card", http.MethodGet, agentCardPath("nope")},
		{"jsonrpc", http.MethodPost, agentJSONRPCPath("nope")},
		{"rest", http.MethodPost, agentRESTPrefix("nope") + a2a.PathSendMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader("{}"))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s = %d, want 404: %s", tc.method, tc.path, resp.StatusCode, raw)
			}
			// Whatever the shape, the answer must be machine-readable JSON —
			// never the mux's default HTML 404, which no A2A client can parse.
			var any map[string]any
			if err := json.Unmarshal(raw, &any); err != nil {
				t.Errorf("refusal is not JSON: %v (%s)", err, raw)
			}
		})
	}
}

// TestA2ARoutesAreBehindTheAuthGuard is the acceptance criterion in full: every
// A2A route — the card included — refuses an unauthenticated caller with the
// BROKER's standard error envelope, the same one /claim and /binaries produce,
// and admits a valid credential.
func TestA2ARoutesAreBehindTheAuthGuard(t *testing.T) {
	cfg := a2aTestConfig(t, staticAuthYAML)
	if !cfg.AuthChain.Enabled() {
		t.Fatal("fixture produced a disabled chain; the test would prove nothing")
	}
	ts := newA2ATestServer(t, cfg)

	routes := []struct {
		name   string
		method string
		path   string
	}{
		{"card", http.MethodGet, agentCardPath("support")},
		{"jsonrpc", http.MethodPost, agentJSONRPCPath("support")},
		{"rest", http.MethodPost, agentRESTPrefix("support") + a2a.PathSendMessage},
	}

	for _, route := range routes {
		t.Run(route.name+" without a credential", func(t *testing.T) {
			resp := doAuthed(t, route.method, ts.URL+route.path, "", "{}")
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s = %d, want 401", route.method, route.path, resp.StatusCode)
			}
			// The BROKER's envelope, not an A2A one: the guard is the same
			// middleware that refuses a /claim caller, so there is exactly one
			// authentication answer on this binary.
			var body map[string]string
			raw, _ := io.ReadAll(resp.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("refusal is not the broker's JSON envelope: %v (%s)", err, raw)
			}
			if body["error"] != "authentication required" {
				t.Errorf("error = %q, want the broker's %q", body["error"], "authentication required")
			}
			if ch := resp.Header.Get("WWW-Authenticate"); !strings.Contains(ch, "nexus-broker") {
				t.Errorf("WWW-Authenticate = %q, want the broker's realm", ch)
			}
		})

		t.Run(route.name+" with a bad credential", func(t *testing.T) {
			resp := doAuthed(t, route.method, ts.URL+route.path, "wrong-token", "{}")
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s = %d, want 401", route.method, route.path, resp.StatusCode)
			}
			var body map[string]string
			raw, _ := io.ReadAll(resp.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("refusal is not the broker's JSON envelope: %v (%s)", err, raw)
			}
			if body["error"] != "credential rejected" {
				t.Errorf("error = %q, want the broker's %q", body["error"], "credential rejected")
			}
		})

		t.Run(route.name+" with a valid credential", func(t *testing.T) {
			resp := doAuthed(t, route.method, ts.URL+route.path, "good-token", "{}")
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				t.Fatalf("%s %s = %d for a valid credential", route.method, route.path, resp.StatusCode)
			}
		})
	}
}

// TestNoAgentsBlockRegistersNoRoutes is the other half of the "behaves exactly
// as before" proof.
//
// The mechanism is an ABSENCE of patterns, not a handler that answers 404: with
// no profiles the mux is byte-for-byte the mux it was before the A2A ingress
// existed, so nothing about request routing, pattern precedence or the guard's
// audit labels can have changed.
func TestNoAgentsBlockRegistersNoRoutes(t *testing.T) {
	cfg := mustLoadConfig(t, "listen_addr: \"127.0.0.1:8080\"\n")
	agents, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	if agents.enabled() {
		t.Error("A2AServer reports enabled with no agents block")
	}
	rec := &recordingMux{}
	agents.Register(rec)
	if len(rec.patterns) != 0 {
		t.Fatalf("registered %v with no agents block, want nothing", rec.patterns)
	}

	// And end to end: an A2A path on such a broker is an ordinary mux 404.
	ts := newA2ATestServer(t, cfg)
	resp, err := http.Get(ts.URL + agentCardPath("support"))
	if err != nil {
		t.Fatalf("GET card: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET %s = %d on a broker with no profiles, want 404", agentCardPath("support"), resp.StatusCode)
	}
}

// TestA2AIngressLeavesExistingRoutesAlone is the additive claim, asserted rather
// than assumed: with two profiles mounted, the pre-existing routes answer
// exactly as they did.
func TestA2AIngressLeavesExistingRoutesAlone(t *testing.T) {
	ts := newA2ATestServer(t, a2aTestConfig(t, ""))

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(raw)) != `{"status":"ok"}` {
			t.Errorf("GET /healthz = %d %s", resp.StatusCode, raw)
		}
	})

	t.Run("binaries", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/binaries")
		if err != nil {
			t.Fatalf("GET /binaries: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /binaries = %d: %s", resp.StatusCode, raw)
		}
		var body binariesBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("GET /binaries is not the usual envelope: %v (%s)", err, raw)
		}
		if len(body.Binaries) == 0 {
			t.Error("GET /binaries listed nothing")
		}
	})

	t.Run("leases", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/leases")
		if err != nil {
			t.Fatalf("GET /leases: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /leases = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("release of an unknown lease", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/release/lease-nope", "application/json", nil)
		if err != nil {
			t.Fatalf("POST /release: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST /release/lease-nope = %d, want 404: %s", resp.StatusCode, raw)
		}
		var body map[string]string
		if err := json.Unmarshal(raw, &body); err != nil || body["error"] != unknownLeaseError {
			t.Errorf("refusal changed shape: %s", raw)
		}
	})
}
