package a2aclient_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
)

func TestCardResolvesFromBaseURL(t *testing.T) {
	agent := newAgent(t, agentConfig{})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	card, err := client.Card(context.Background())
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	if card.Name != "test-agent" {
		t.Fatalf("card name = %q, want test-agent", card.Name)
	}

	seen := agent.seen()
	if len(seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(seen))
	}
	if seen[0].Path != a2a.AgentCardPath {
		t.Fatalf("card fetched from %q, want %q", seen[0].Path, a2a.AgentCardPath)
	}

	// A second resolution is served from cache: discovery is a boot-time cost,
	// not a per-call one.
	if _, err := client.Card(context.Background()); err != nil {
		t.Fatalf("second Card: %v", err)
	}
	if got := len(agent.seen()); got != 1 {
		t.Fatalf("card fetched %d times, want 1", got)
	}
}

func TestCapabilitiesInspection(t *testing.T) {
	agent := newAgent(t, agentConfig{
		securitySchemes: map[string]a2a.SecurityScheme{
			"bearer": a2a.BearerScheme("JWT", "a bearer token"),
			"apikey": a2a.APIKeyScheme("X-Key", a2a.APIKeyInHeader, "an api key"),
		},
	})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	caps, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.Streaming {
		t.Fatal("Streaming = false, want true")
	}
	if caps.PushNotifications || caps.ExtendedAgentCard {
		t.Fatal("push/extended should be false on the default card")
	}
	if !caps.SupportsBinding(a2a.BindingJSONRPC) || !caps.SupportsBinding(a2a.BindingHTTPJSON) {
		t.Fatalf("bindings = %v, want both HTTP bindings", caps.Bindings)
	}
	if caps.SupportsBinding(a2a.BindingGRPC) {
		t.Fatal("gRPC should not be reported: the card exposes no such interface")
	}
	if len(caps.SecuritySchemes) != 2 {
		t.Fatalf("security schemes = %d, want 2", len(caps.SecuritySchemes))
	}
	// Sorted by name, so the presentation is stable.
	if caps.SecuritySchemes[0].Name != "apikey" || caps.SecuritySchemes[1].Name != "bearer" {
		t.Fatalf("schemes not sorted by name: %+v", caps.SecuritySchemes)
	}
	if got := caps.SecuritySchemes[1].Kind; got != a2a.SecuritySchemeHTTPAuth {
		t.Fatalf("bearer kind = %q, want %q", got, a2a.SecuritySchemeHTTPAuth)
	}
	if !caps.RequiresAuth() {
		t.Fatal("RequiresAuth = false, want true: the card declares requirements")
	}
	if _, ok := caps.Scheme("bearer"); !ok {
		t.Fatal("Scheme(bearer) not found")
	}
	if len(caps.SkillIDs) != 1 || caps.SkillIDs[0] != "chat" {
		t.Fatalf("skills = %v, want [chat]", caps.SkillIDs)
	}
}

func TestCapabilitiesReportsNoStreaming(t *testing.T) {
	agent := newAgent(t, agentConfig{noStreaming: true})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	caps, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Streaming {
		t.Fatal("Streaming = true, want false")
	}
}

func TestCardUnparseableIsTyped(t *testing.T) {
	agent := newAgent(t, agentConfig{cardBody: []byte("this is not json")})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.Card(context.Background())
	var cardErr *a2aclient.CardError
	if !errors.As(err, &cardErr) {
		t.Fatalf("error = %v (%T), want *CardError", err, err)
	}
	if cardErr.Stage != "decode" {
		t.Fatalf("stage = %q, want decode", cardErr.Stage)
	}
	if !strings.Contains(cardErr.URL, a2a.AgentCardPath) {
		t.Fatalf("card error URL = %q, want the well-known path", cardErr.URL)
	}
}

func TestCardFailsValidationIsTyped(t *testing.T) {
	// Structurally valid JSON, but a card with no interfaces and no skills is
	// not something a client can act on.
	agent := newAgent(t, agentConfig{cardBody: []byte(`{"name":"x"}`)})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.Card(context.Background())
	var cardErr *a2aclient.CardError
	if !errors.As(err, &cardErr) {
		t.Fatalf("error = %v (%T), want *CardError", err, err)
	}
	if cardErr.Stage != "validate" {
		t.Fatalf("stage = %q, want validate", cardErr.Stage)
	}
	var protoErr *a2a.Error
	if !errors.As(err, &protoErr) {
		t.Fatalf("validation cause = %v, want an *a2a.Error", cardErr.Err)
	}
}

func TestCardValidationCanBeRelaxed(t *testing.T) {
	agent := newAgent(t, agentConfig{cardBody: []byte(
		`{"name":"x","supportedInterfaces":[{"url":"` + "http://example.invalid/a2a" +
			`","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{"streaming":true}}`)})
	client, err := a2aclient.New(agent.URL(), a2aclient.WithCardValidation(false))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	card, err := client.Card(context.Background())
	if err != nil {
		t.Fatalf("Card with validation off: %v", err)
	}
	if card.Name != "x" {
		t.Fatalf("card name = %q", card.Name)
	}
}

func TestCardHTTPErrorIsTyped(t *testing.T) {
	agent := newAgent(t, agentConfig{cardStatus: http.StatusForbidden})
	client, err := a2aclient.New(agent.URL(), a2aclient.WithRetryPolicy(a2aclient.NoRetry()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.Card(context.Background())
	var cardErr *a2aclient.CardError
	if !errors.As(err, &cardErr) {
		t.Fatalf("error = %v (%T), want *CardError", err, err)
	}
	if cardErr.Stage != "status" || cardErr.StatusCode != http.StatusForbidden {
		t.Fatalf("card error = %+v, want stage=status status=403", cardErr)
	}
}

func TestCardSuppliedOutOfBand(t *testing.T) {
	agent := newAgent(t, agentConfig{cardStatus: http.StatusNotFound})
	card := a2a.NewAgentCard("offline", "distributed out of band", "1.0.0").
		WithInterface(a2a.BindingJSONRPC, agent.URL()+testJSONRPCPath).
		WithSkill(a2a.AgentSkill{ID: "chat", Name: "chat", Description: "chat", Tags: []string{"chat"}})

	client, err := a2aclient.New(agent.URL(), a2aclient.WithCard(&card))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := client.SendMessage(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "hello"),
	})
	if err != nil {
		t.Fatalf("SendMessage with an out-of-band card: %v", err)
	}
	if resp.Message == nil {
		t.Fatal("want a direct message reply")
	}
	// The well-known path was never fetched.
	for _, seen := range agent.seen() {
		if seen.Path == a2a.AgentCardPath {
			t.Fatal("card was fetched despite WithCard")
		}
	}
}

func TestEndpointMissingForBinding(t *testing.T) {
	agent := newAgent(t, agentConfig{cardInterfaces: []a2a.AgentInterface{
		{URL: "http://example.invalid/grpc", ProtocolBinding: a2a.BindingGRPC, ProtocolVersion: a2a.ProtocolVersion},
	}})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"})
	var bindErr *a2aclient.BindingError
	if !errors.As(err, &bindErr) {
		t.Fatalf("error = %v (%T), want *BindingError", err, err)
	}
	if !errors.Is(err, a2aclient.ErrNoEndpoint) {
		t.Fatalf("error %v does not wrap ErrNoEndpoint", err)
	}
}

func TestEndpointRejectsUnsupportedProtocolVersion(t *testing.T) {
	agent := newAgent(t, agentConfig{cardInterfaces: []a2a.AgentInterface{
		{URL: "http://example.invalid/a2a", ProtocolBinding: a2a.BindingJSONRPC, ProtocolVersion: "0.3"},
	}})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"})
	var bindErr *a2aclient.BindingError
	if !errors.As(err, &bindErr) {
		t.Fatalf("error = %v (%T), want *BindingError", err, err)
	}
	if !strings.Contains(bindErr.Detail, "0.3") {
		t.Fatalf("detail = %q, want it to name the offending version", bindErr.Detail)
	}
}

func TestEndpointPrefersFirstUsableInterface(t *testing.T) {
	agent := newAgent(t, agentConfig{})
	agent.cfg.cardInterfaces = []a2a.AgentInterface{
		{URL: "http://example.invalid/old", ProtocolBinding: a2a.BindingJSONRPC, ProtocolVersion: "0.3"},
		{URL: agent.URL() + testJSONRPCPath, ProtocolBinding: a2a.BindingJSONRPC, ProtocolVersion: a2a.ProtocolVersion},
	}
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The 0.3 interface comes first in preference order but is unusable, so the
	// 1.0 one behind it must be chosen rather than the whole binding refused.
	if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err != nil {
		t.Fatalf("GetTask: %v", err)
	}
}

func TestEndpointResolvesRelativeInterfaceURL(t *testing.T) {
	agent := newAgent(t, agentConfig{})
	agent.cfg.cardInterfaces = []a2a.AgentInterface{
		{URL: testJSONRPCPath, ProtocolBinding: a2a.BindingJSONRPC, ProtocolVersion: a2a.ProtocolVersion},
	}
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err != nil {
		t.Fatalf("GetTask against a relative interface url: %v", err)
	}
}

func TestNewRejectsBadConfiguration(t *testing.T) {
	if _, err := a2aclient.New(""); err == nil {
		t.Fatal("New with no base url and no pinned endpoint should fail")
	}
	if _, err := a2aclient.New("ftp://example.invalid"); err == nil {
		t.Fatal("New with a non-HTTP scheme should fail")
	}
	if _, err := a2aclient.New("http://example.invalid", a2aclient.WithBinding(a2a.BindingGRPC)); err == nil {
		t.Fatal("New with the gRPC binding should fail: this client speaks neither")
	}
	if _, err := a2aclient.New("", a2aclient.WithJSONRPCEndpoint("http://example.invalid/a2a")); err != nil {
		t.Fatalf("New with a pinned endpoint and no base url: %v", err)
	}
}

func TestCardURL(t *testing.T) {
	client, err := a2aclient.New("http://example.invalid/agents/one/")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := "http://example.invalid/agents/one" + a2a.AgentCardPath
	if got := client.CardURL(); got != want {
		t.Fatalf("CardURL = %q, want %q", got, want)
	}
	if got := client.BaseURL(); got != "http://example.invalid/agents/one" {
		t.Fatalf("BaseURL = %q", got)
	}
	if got := client.Binding(); got != a2a.BindingJSONRPC {
		t.Fatalf("Binding = %q, want the JSON-RPC default", got)
	}
}
