package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// cardTestConfig loads a broker config carrying one profile, with whatever extra
// document head a test needs (an `auth:` block, an advertise_addr).
func cardTestConfig(t *testing.T, head string) Config {
	t.Helper()
	yaml := head + "listen_addr: \"127.0.0.1:8080\"\n" +
		agentsBlock(oneValidProfile("support", "/tmp/agent.yaml"))
	return mustLoadConfig(t, yaml)
}

// mustBuildCard renders one profile's card through the same path the boot does.
func mustBuildCard(t *testing.T, cfg Config, profile string) *servedAgentCard {
	t.Helper()
	card, err := buildAgentCard(profile, cfg.Agents[profile], cfg.A2ABaseURL, cfg.AuthValidators)
	if err != nil {
		t.Fatalf("buildAgentCard: %v", err)
	}
	return card
}

// TestAgentCardAdvertisesThisProfilesEndpoints is the discovery contract: the
// card a client fetches must name the URLs that client should then post to,
// absolute and namespaced to the profile it asked about.
func TestAgentCardAdvertisesThisProfilesEndpoints(t *testing.T) {
	cfg := cardTestConfig(t, "")
	card := mustBuildCard(t, cfg, "support").card

	jsonrpc, ok := card.InterfaceFor(a2a.BindingJSONRPC)
	if !ok {
		t.Fatal("card declares no JSON-RPC interface")
	}
	if want := "http://127.0.0.1:8080/agents/support/a2a"; jsonrpc != want {
		t.Errorf("JSON-RPC interface = %q, want %q", jsonrpc, want)
	}
	rest, ok := card.InterfaceFor(a2a.BindingHTTPJSON)
	if !ok {
		t.Fatal("card declares no HTTP+JSON interface")
	}
	if want := "http://127.0.0.1:8080/agents/support/a2a/v1"; rest != want {
		t.Errorf("REST interface = %q, want %q", rest, want)
	}

	// Preference order matters: section 8.3.1 reads the list as ordered, and
	// JSON-RPC leads because it has the widest client support today.
	if card.SupportedInterfaces[0].ProtocolBinding != a2a.BindingJSONRPC {
		t.Errorf("first interface is %q, want JSON-RPC to lead", card.SupportedInterfaces[0].ProtocolBinding)
	}
	for i, iface := range card.SupportedInterfaces {
		if iface.ProtocolVersion != a2a.ProtocolVersion {
			t.Errorf("supportedInterfaces[%d].protocolVersion = %q, want %q", i, iface.ProtocolVersion, a2a.ProtocolVersion)
		}
	}
}

// TestAgentCardLeavesTenantUnset pins the decision recorded on buildAgentCard.
//
// AgentInterface.Tenant disambiguates several agents sharing ONE endpoint URL.
// Profiles do not share one — each publishes its own card naming its own paths —
// so populating tenant would give a request two sources of truth for which agent
// it is for, and the broker would have to reconcile them. The URL routes; tenant
// stays empty.
func TestAgentCardLeavesTenantUnset(t *testing.T) {
	cfg := cardTestConfig(t, "")
	card := mustBuildCard(t, cfg, "support").card
	for i, iface := range card.SupportedInterfaces {
		if iface.Tenant != "" {
			t.Errorf("supportedInterfaces[%d].tenant = %q, want empty: the path segment routes, not the tenant", i, iface.Tenant)
		}
	}
}

// TestAgentCardCapabilitiesAreDerivedNotConfigured asserts every optional
// capability tracks what the ingress actually dispatches, never what an
// operator wrote.
//
// streaming is TRUE now that SendStreamingMessage is dispatched: the ingress
// opens a real SSE stream and writes the turn onto it. The other two stay false
// because nothing implements them, and each assertion is tied back to the
// operation map rather than to a literal, so the card and the dispatch cannot
// disagree — which is the property this test exists for.
func TestAgentCardCapabilitiesAreDerivedNotConfigured(t *testing.T) {
	cfg := cardTestConfig(t, "")
	card := mustBuildCard(t, cfg, "support").card

	wantStreaming := brokerOperationImplemented(a2a.MethodSendStreamingMessage) ||
		brokerOperationImplemented(a2a.MethodSubscribeToTask)
	if card.Capabilities.Streaming != wantStreaming {
		t.Errorf("capabilities.streaming = %v, want %v: it must track the dispatched operations",
			card.Capabilities.Streaming, wantStreaming)
	}
	if !wantStreaming {
		t.Error("no streaming operation is dispatched; SendStreamingMessage was expected to be")
	}
	if card.Capabilities.PushNotifications {
		t.Error("capabilities.pushNotifications = true, but push notifications are not implemented")
	}
	if card.Capabilities.ExtendedAgentCard {
		t.Error("capabilities.extendedAgentCard = true, but no extended card is served")
	}
}

// TestAgentCardSecuritySchemesComeFromTheAuthChain is the second acceptance
// criterion for cards: what a client is told to present is derived from the
// broker's own `auth:` block, so the published card cannot claim a credential
// the broker does not actually accept.
func TestAgentCardSecuritySchemesComeFromTheAuthChain(t *testing.T) {
	head := `auth:
  validators:
    - type: static
      tokens:
        - token: "good-token"
          principal: "ci-runner"
    - type: jwks
      issuer: "https://idp.example.test"
      audience: "nexus-broker"
      jwks_url: "https://idp.example.test/.well-known/jwks.json"
      principal_claim: "sub"
`
	cfg := cardTestConfig(t, head)
	card := mustBuildCard(t, cfg, "support").card

	// The scheme NAMES are nexusauth's chain-order names, so the card and the
	// broker's boot log ("validators=[static jwks]") name the same things.
	var names []string
	for name := range card.SecuritySchemes {
		names = append(names, name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"jwks", "static"}) {
		t.Fatalf("securitySchemes = %v, want the chain-order names [jwks static]", names)
	}

	if kind := card.SecuritySchemes["static"].Kind(); kind != a2a.SecuritySchemeHTTPAuth {
		t.Errorf("static scheme kind = %q, want httpAuth", kind)
	}
	jwks := card.SecuritySchemes["jwks"]
	if jwks.HTTPAuth == nil || jwks.HTTPAuth.BearerFormat != "JWT" {
		t.Fatalf("jwks scheme = %+v, want a JWT bearer scheme", jwks)
	}
	// The description carries the issuer and audience so a client knows which
	// token to go and get, not merely that a token is wanted.
	if desc := jwks.HTTPAuth.Description; desc == "" ||
		!strings.Contains(desc, "https://idp.example.test") || !strings.Contains(desc, "nexus-broker") {
		t.Errorf("jwks description = %q, want it to name the issuer and audience", desc)
	}

	// A chain is FIRST-SUCCESS, so satisfying either validator suffices. A2A
	// spells that as separate members of securityRequirements, not one member
	// naming both schemes.
	if len(card.SecurityRequirements) != 2 {
		t.Fatalf("securityRequirements = %d entries, want 2 alternatives: %+v", len(card.SecurityRequirements), card.SecurityRequirements)
	}
	for i, req := range card.SecurityRequirements {
		if len(req.Schemes) != 1 {
			t.Errorf("securityRequirements[%d] names %d schemes, want 1 (alternatives, not a conjunction)", i, len(req.Schemes))
		}
	}
}

// TestAgentCardOmitsSecurityWhenAuthIsDisabled: a broker with no `auth:` block
// accepts anything, and the honest card for that says nothing about credentials
// rather than inventing one.
func TestAgentCardOmitsSecurityWhenAuthIsDisabled(t *testing.T) {
	cfg := cardTestConfig(t, "")
	card := mustBuildCard(t, cfg, "support").card
	if len(card.SecuritySchemes) != 0 || len(card.SecurityRequirements) != 0 {
		t.Errorf("card advertises security with no auth block: schemes=%v requirements=%v",
			card.SecuritySchemes, card.SecurityRequirements)
	}
}

// TestAgentCardOmitsProxyHeadersValidator: proxy_headers accepts no client
// credential at all — it honours an identity a trusted proxy already established
// and refuses those headers from anyone else. Publishing a scheme for it would
// instruct clients to send a header guaranteed to be ignored.
func TestAgentCardOmitsProxyHeadersValidator(t *testing.T) {
	head := `auth:
  validators:
    - type: proxy_headers
      trusted_proxy_cidrs: ["127.0.0.1/32"]
      principal_header: "X-Auth-Subject"
`
	cfg := cardTestConfig(t, head)
	card := mustBuildCard(t, cfg, "support").card
	if len(card.SecuritySchemes) != 0 {
		t.Errorf("card advertises %v for a proxy_headers-only chain; it accepts no client credential", card.SecuritySchemes)
	}
	if !cfg.AuthChain.Enabled() {
		t.Fatal("fixture produced a disabled chain; the test would prove nothing")
	}
}

// TestAgentCardEncodesTheAuthoredHalf checks the hand-authored fields survive to
// the served document, and that the document itself is what a client expects at
// the well-known path.
func TestAgentCardEncodesTheAuthoredHalf(t *testing.T) {
	cfg := cardTestConfig(t, "")
	served := mustBuildCard(t, cfg, "support")

	var doc map[string]any
	if err := json.Unmarshal(served.body, &doc); err != nil {
		t.Fatalf("served card is not valid JSON: %v", err)
	}
	if doc["name"] != "Support Agent" {
		t.Errorf("name = %v, want %q", doc["name"], "Support Agent")
	}
	if doc["version"] != "1.2.0" {
		t.Errorf("version = %v, want %q", doc["version"], "1.2.0")
	}
	skills, ok := doc["skills"].([]any)
	if !ok || len(skills) != 1 {
		t.Fatalf("skills = %v, want the one authored skill", doc["skills"])
	}
	if served.etag == "" {
		t.Error("served card has no ETag")
	}
}

// TestAgentCardRejectsAnUnservableCard: validation failures are BOOT failures.
// The config parser already rejects the omissions an operator can make in YAML,
// so this exercises the belt-and-braces gate on the assembled document — a card
// with no interfaces cannot be served, and finding that out at boot beats
// finding it out from a client.
func TestAgentCardRejectsAnUnservableCard(t *testing.T) {
	profile := AgentProfile{
		Config: "/tmp/agent.yaml",
		Card: AgentCardSpec{
			Name:        "Support",
			Description: "Answers.",
			Version:     "1.0.0",
			// No skills: valid Go, rejected by the specification.
		},
	}
	if _, err := buildAgentCard("support", profile, "http://127.0.0.1:8080", nil); err == nil {
		t.Fatal("buildAgentCard accepted a card with no skills")
	}
}
