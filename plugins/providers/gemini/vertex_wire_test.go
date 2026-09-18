package gemini

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"

	// Side-effect import: registers the "google-adc" credential source these
	// tests open by name. Same reasoning as auth_test.go — non-test gemini code
	// must never import googleadc, and internal/optincheck is the canary that
	// holds that line.
	_ "github.com/frankbardon/nexus/pkg/nexuscreds/googleadc"
)

// This file is the cross-cutting suite for the credential seam: the per-package
// tests in pkg/nexuscreds, pkg/nexuscreds/googleadc and auth_test.go all stop at
// "a token was minted" or "applyAuth set a header on a request object". Nothing
// there proves the assembled provider actually puts that token on a request it
// sends, against the URL its resolved project and region built.
//
// The seam used here is test-only and needs no production hook: Plugin.client is
// package-private, so a test in this package can swap its Transport after Init
// and watch every byte the provider would have sent to Google. The credential
// path is untouched by that swap — the ADC token source owns its own transport
// and reaches the fake metadata server through GCE_METADATA_HOST, exactly as it
// does on a real pod.

// wireTap is an httptest server plus the RoundTripper that points a plugin's
// HTTP client at it. It records what arrived on the wire and what URL the
// plugin had addressed before the redirect, which is where the resolved project
// and region show up.
type wireTap struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []tappedRequest
}

type tappedRequest struct {
	// outboundURL is the URL the plugin built, captured before the redirect —
	// so the assertions can see the real -aiplatform.googleapis.com host.
	outboundURL string
	// The rest is read off the request the server actually received.
	method        string
	authorization string
	googAPIKey    string
	body          string
}

// newWireTap starts the tap. response is the JSON body served for every
// request; callers pass a well-formed Gemini reply so the provider's own
// decode path runs and an llm.response comes back out on the bus.
func newWireTap(t *testing.T, response string) *wireTap {
	t.Helper()

	w := &wireTap{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		w.mu.Lock()
		// The redirect transport stashes the pre-rewrite URL in a header it
		// strips nothing else from; see redirectTo.
		w.requests = append(w.requests, tappedRequest{
			outboundURL:   r.Header.Get(outboundURLHeader),
			method:        r.Method,
			authorization: r.Header.Get("Authorization"),
			googAPIKey:    r.Header.Get("x-goog-api-key"),
			body:          string(body),
		})
		w.mu.Unlock()

		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(response))
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// outboundURLHeader carries the pre-redirect URL to the tap's handler. It is
// added by the test transport only, after the provider has finished building
// and signing the request, so it cannot influence anything under test. It
// deliberately stays out of the reserved X-Nexus-* namespace pkg/nexusheaders
// owns.
const outboundURLHeader = "X-Test-Outbound-URL"

// redirect points p's HTTP client at the tap. It must be called after Init,
// since Init is where the client is constructed.
func (w *wireTap) redirect(t *testing.T, p *Plugin) {
	t.Helper()
	if p.client == nil {
		t.Fatal("plugin has no HTTP client — Init did not run")
	}
	base, err := url.Parse(w.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = redirectTo{base: base, rt: w.srv.Client().Transport}
}

func (w *wireTap) taken() []tappedRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]tappedRequest, len(w.requests))
	copy(out, w.requests)
	return out
}

// only asserts exactly one request reached the wire and returns it.
func (w *wireTap) only(t *testing.T) tappedRequest {
	t.Helper()
	got := w.taken()
	if len(got) != 1 {
		t.Fatalf("wire saw %d requests, want exactly 1", len(got))
	}
	return got[0]
}

type redirectTo struct {
	base *url.URL
	rt   http.RoundTripper
}

func (r redirectTo) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set(outboundURLHeader, req.URL.String())
	clone.URL.Scheme = r.base.Scheme
	clone.URL.Host = r.base.Host
	clone.Host = ""
	return r.rt.RoundTrip(clone)
}

// okResponse is a minimal well-formed Gemini reply.
const okResponse = `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},` +
	`"finishReason":"STOP"}],"modelVersion":"gemini-2.5-flash"}`

// tapped boots the plugin through the contract harness with cfg, redirects its
// client at a fresh tap, and returns both. The contract harness is used rather
// than a bare Init because it brings a real bus and records every emission, so
// the same boot proves the declared Subscriptions/Emissions still hold under
// this auth mode.
func tapped(t *testing.T, cfg map[string]any) (*contract.ContractHarness, *wireTap) {
	t.Helper()
	h := contract.NewContract(t, New, contract.WithPluginConfig(cfg))
	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin under test is %T, want *gemini.Plugin", h.Plugin())
	}
	tap := newWireTap(t, okResponse)
	tap.redirect(t, p)
	return h, tap
}

// ping is the request driven through the bus. Model and MaxTokens are set
// explicitly because the contract harness supplies no ModelRegistry.
func ping() events.LLMRequest {
	return events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Model:         "gemini-2.5-flash",
		MaxTokens:     64,
		Messages:      []events.Message{{Role: "user", Content: "ping"}},
	}
}

// assertAnswered asserts the turn completed: an llm.response carrying the
// tapped reply came back out, and the plugin emitted nothing it did not
// declare.
func assertAnswered(t *testing.T, h *contract.ContractHarness) {
	t.Helper()
	h.AssertEmitted("llm.response")
	h.AssertNoUndeclaredEmissions()

	for _, ev := range h.PluginEmissions() {
		if ev.Type != "llm.response" {
			continue
		}
		resp, ok := ev.Payload.(events.LLMResponse)
		if !ok {
			t.Fatalf("llm.response payload is %T", ev.Payload)
		}
		if resp.Content != "pong" {
			t.Fatalf("llm.response content = %q, want the tapped reply", resp.Content)
		}
		return
	}
	t.Fatal("no llm.response payload captured")
}

// --- the pod-identity path, end to end ----------------------------------------

// TestVertexPodIdentity_SignsTheOutboundRequestFromMetadataAlone is the
// headline case for this effort: the exact configuration a GKE deployment
// writes — auth: vertex and nothing else GCP-specific — must reach Vertex with
// a bearer token, a project and a region that nobody typed anywhere.
//
// Everything here is resolved rather than configured: the project and region
// come off the fake metadata server, the credential comes from ADC falling
// through to that same server's token endpoint, and the URL is assembled from
// both. No key material exists anywhere in the test.
func TestVertexPodIdentity_SignsTheOutboundRequestFromMetadataAlone(t *testing.T) {
	newFakeMetadata(t)
	isolateADC(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	h, tap := tapped(t, map[string]any{"auth": "vertex"})

	h.Inject("llm.request", ping())

	req := tap.only(t)

	// The half the unit tests do not reach: the token is on a request that was
	// actually sent, not on one that was merely constructed.
	if want := "Bearer " + testMetadataToken; req.authorization != want {
		t.Errorf("Authorization on the wire = %q, want %q", req.authorization, want)
	}
	if req.googAPIKey != "" {
		t.Errorf("vertex mode put an x-goog-api-key on the wire: %q", req.googAPIKey)
	}
	if req.method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.method)
	}

	// The URL proves both resolution chains ran: the region came from the
	// metadata zone (deliberately not us-central1) and the project from the
	// metadata project-id.
	wantURL := "https://" + testMetadataRegion + "-aiplatform.googleapis.com" +
		"/v1/projects/" + testMetadataProjectID +
		"/locations/" + testMetadataRegion +
		"/publishers/google/models/gemini-2.5-flash:generateContent"
	if req.outboundURL != wantURL {
		t.Errorf("outbound URL = %q, want %q", req.outboundURL, wantURL)
	}

	if !strings.Contains(req.body, "ping") {
		t.Errorf("request body did not carry the prompt: %s", req.body)
	}

	assertAnswered(t, h)
}

// TestVertexPodIdentity_ContractIsUnchangedUnderVertex holds the guarantee that
// the auth rework did not move the plugin's event surface. contract_test.go
// pins the surface in api_key mode; this asserts vertex declares and honours
// the identical one, so a future credential change cannot quietly add or drop
// an event for Vertex deployments only.
func TestVertexPodIdentity_ContractIsUnchangedUnderVertex(t *testing.T) {
	newFakeMetadata(t)
	isolateADC(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	vertex, _ := tapped(t, map[string]any{"auth": "vertex"})
	apiKey := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"api_key": "test-key-not-real",
	}))

	vertex.AssertSubscribesTo("llm.request", "cancel.active")

	if got, want := subscriptionTypes(vertex.Plugin()), subscriptionTypes(apiKey.Plugin()); !equalStrings(got, want) {
		t.Errorf("vertex Subscriptions() = %v, want the api_key surface %v", got, want)
	}
	if got, want := vertex.Plugin().Emissions(), apiKey.Plugin().Emissions(); !equalStrings(got, want) {
		t.Errorf("vertex Emissions() = %v, want the api_key surface %v", got, want)
	}
}

func subscriptionTypes(p engine.Plugin) []string {
	out := make([]string, 0, len(p.Subscriptions()))
	for _, s := range p.Subscriptions() {
		out = append(out, s.EventType)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- the backwards-compatibility path -----------------------------------------

// TestVertexKeyFile_SignsTheOutboundRequestWithNoConfigEdit is the promise made
// to deployments that already exist: a service_account_json config written
// before the nexuscreds seam landed still authenticates, unedited. The key file
// is a throwaway RSA key whose token_uri points at a local server, so the whole
// exchange stays in-process.
func TestVertexKeyFile_SignsTheOutboundRequestWithNoConfigEdit(t *testing.T) {
	pinNotOnGCE(t)
	isolateADC(t)

	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ya29.from-key-file",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer tokens.Close()

	h, tap := tapped(t, map[string]any{
		"auth":                 "vertex",
		"project_id":           "myproj",
		"service_account_json": writeServiceAccountKey(t, tokens.URL),
	})

	h.Inject("llm.request", ping())

	req := tap.only(t)
	if want := "Bearer ya29.from-key-file"; req.authorization != want {
		t.Errorf("Authorization on the wire = %q, want %q", req.authorization, want)
	}
	// Off GCE with no location configured, the default stands — and the
	// configured project, not a metadata one, builds the URL.
	wantURL := "https://us-central1-aiplatform.googleapis.com" +
		"/v1/projects/myproj/locations/us-central1" +
		"/publishers/google/models/gemini-2.5-flash:generateContent"
	if req.outboundURL != wantURL {
		t.Errorf("outbound URL = %q, want %q", req.outboundURL, wantURL)
	}

	assertAnswered(t, h)
}

// --- the regression guard -----------------------------------------------------

// TestAPIKeyMode_IsUnchangedOnTheWire guards the path this effort did not
// intend to touch. The public Generative Language endpoint authenticates with a
// header, not a bearer token, and nothing about the credential seam may leak
// into it.
func TestAPIKeyMode_IsUnchangedOnTheWire(t *testing.T) {
	pinNotOnGCE(t)

	h, tap := tapped(t, map[string]any{"api_key": "test-key-not-real"})

	h.Inject("llm.request", ping())

	req := tap.only(t)
	if req.googAPIKey != "test-key-not-real" {
		t.Errorf("x-goog-api-key on the wire = %q, want the configured key", req.googAPIKey)
	}
	if req.authorization != "" {
		t.Errorf("api_key mode put an Authorization header on the wire: %q", req.authorization)
	}
	wantURL := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"
	if req.outboundURL != wantURL {
		t.Errorf("outbound URL = %q, want %q", req.outboundURL, wantURL)
	}

	assertAnswered(t, h)
}

// --- the diagnosable failure --------------------------------------------------

// TestVertexUnknownCredentialSource_FailsBoot asserts the failure at the level
// an operator meets it. auth_test.go pins the message resolveAuth produces;
// what matters to a deployment is that Init itself refuses, so the pod dies at
// boot with the reason instead of serving traffic that cannot authenticate.
func TestVertexUnknownCredentialSource_FailsBoot(t *testing.T) {
	pinNotOnGCE(t)

	_, err := initWithCapturedLog(t, map[string]any{
		"auth":        "vertex",
		"project_id":  "myproj",
		"credentials": "vault",
	})
	if err == nil {
		t.Fatal("Init must fail when the configured credential source is not registered")
	}
	msg := err.Error()
	for _, want := range []string{"vault", "blank-import", defaultCredentialSource} {
		if !strings.Contains(msg, want) {
			t.Errorf("boot error should mention %q, got: %v", want, err)
		}
	}
}
