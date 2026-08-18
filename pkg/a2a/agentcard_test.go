package a2a

import (
	"encoding/json"
	"strings"
	"testing"
)

// nexusCard builds the card a Nexus serve plugin would publish, exercising every
// required top-level field.
func nexusCard() AgentCard {
	card := NewAgentCard(AgentIdentity{
		Name:             "nexus",
		Description:      "Nexus agent harness exposed over A2A.",
		Version:          "0.1.0",
		DocumentationURL: "https://example.test/docs",
		Provider:         &AgentProvider{Organization: "Nexus", URL: "https://example.test"},
	})
	card = card.
		WithInterface(TransportJSONRPC, "https://agent.example.test/a2a").
		WithInterface(TransportHTTPJSON, "https://agent.example.test/a2a/v1").
		WithSecurityScheme("bearer", BearerScheme("JWT", "Bearer token verified by pkg/nexusauth.")).
		WithSecurityRequirement("bearer").
		WithSkill(AgentSkill{
			ID:          "chat",
			Name:        "Chat",
			Description: "Run a conversational turn against the configured agent loop.",
			Tags:        []string{"chat", "react"},
			Examples:    []string{"Summarize the repository README."},
		}).
		WithExtension(NexusExtension())
	card.DefaultInputModes = []string{"text/plain"}
	card.DefaultOutputModes = []string{"text/plain", ContentTypeJSON}
	return card
}

// TestAgentCardMarshalsToWellKnownDocument asserts the card serializes to valid
// /.well-known/agent-card.json content: every required key present, capability
// booleans explicit, and a clean round trip.
func TestAgentCardMarshalsToWellKnownDocument(t *testing.T) {
	card := nexusCard()
	if err := ValidateAgentCard(&card); err != nil {
		t.Fatalf("validate: %v", err)
	}

	data, err := EncodeAgentCard(&card)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("card is not valid JSON: %v", err)
	}
	for _, key := range []string{
		"protocolVersion", "identity", "capabilities", "securitySchemes", "interfaces", "skills",
	} {
		if _, ok := doc[key]; !ok {
			t.Errorf("card is missing required key %q", key)
		}
	}
	if doc["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %q", doc["protocolVersion"], ProtocolVersion)
	}

	// The three capability booleans are always stated, never inferred.
	caps, ok := doc["capabilities"].(map[string]any)
	if !ok {
		t.Fatal("capabilities is not an object")
	}
	for key, want := range map[string]bool{
		"streaming":         true,
		"pushNotifications": false,
		"extendedAgentCard": false,
	} {
		got, present := caps[key]
		if !present {
			t.Errorf("capabilities.%s is absent; it must be stated explicitly", key)
			continue
		}
		if got != want {
			t.Errorf("capabilities.%s = %v, want %v", key, got, want)
		}
	}

	// Interfaces carry ProtoJSON transport names.
	ifaces, ok := doc["interfaces"].([]any)
	if !ok || len(ifaces) != 2 {
		t.Fatalf("interfaces = %v", doc["interfaces"])
	}
	first, _ := ifaces[0].(map[string]any)
	if first["transport"] != string(TransportJSONRPC) {
		t.Errorf("interfaces[0].transport = %v, want %q", first["transport"], TransportJSONRPC)
	}

	// The security scheme is the flattened oneof, not an inline kind string.
	schemes, _ := doc["securitySchemes"].(map[string]any)
	bearer, _ := schemes["bearer"].(map[string]any)
	if _, ok := bearer["httpAuthSecurityScheme"]; !ok {
		t.Errorf("bearer scheme did not serialize as httpAuthSecurityScheme: %v", bearer)
	}

	// And it round-trips.
	back, err := DecodeAgentCard(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := ValidateAgentCard(back); err != nil {
		t.Fatalf("validate decoded: %v", err)
	}
	if back.Identity.Name != card.Identity.Name || len(back.Skills) != len(card.Skills) {
		t.Errorf("round trip lost data: %+v", back)
	}
	if url, ok := back.InterfaceFor(TransportHTTPJSON); !ok || url != "https://agent.example.test/a2a/v1" {
		t.Errorf("InterfaceFor(HTTP_JSON) = %q, %v", url, ok)
	}
	if _, ok := back.Skill("chat"); !ok {
		t.Error("decoded card lost the chat skill")
	}
}

// TestAgentCardServesAtWellKnownPath pins the discovery path and media type the
// serve plugin will use.
func TestAgentCardServesAtWellKnownPath(t *testing.T) {
	if AgentCardPath != "/.well-known/agent-card.json" {
		t.Errorf("AgentCardPath = %q", AgentCardPath)
	}
	if ContentTypeAgentCard != "application/json" {
		t.Errorf("ContentTypeAgentCard = %q", ContentTypeAgentCard)
	}
}

// TestAgentCapabilitiesZeroValueIsSafe pins the requirement that push
// notifications and the extended card default to false without any caller
// action.
func TestAgentCapabilitiesZeroValueIsSafe(t *testing.T) {
	var caps AgentCapabilities
	if caps.PushNotifications || caps.ExtendedAgentCard || caps.Streaming {
		t.Fatalf("zero value is not all-false: %+v", caps)
	}

	data, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"streaming":false,"pushNotifications":false,"extendedAgentCard":false}`
	if string(data) != want {
		t.Errorf("zero capabilities = %s, want %s", data, want)
	}

	// NewAgentCard keeps push and extended-card off for this effort.
	card := NewAgentCard(AgentIdentity{Name: "n", Version: "1"})
	if card.Capabilities.PushNotifications {
		t.Error("NewAgentCard advertised pushNotifications")
	}
	if card.Capabilities.ExtendedAgentCard {
		t.Error("NewAgentCard advertised extendedAgentCard")
	}
	if !card.Capabilities.Streaming {
		t.Error("NewAgentCard did not advertise streaming")
	}
}

func TestValidateAgentCardRequiredFields(t *testing.T) {
	base := nexusCard()

	cases := map[string]func(c *AgentCard){
		"no protocol version":   func(c *AgentCard) { c.ProtocolVersion = "" },
		"bad protocol version":  func(c *AgentCard) { c.ProtocolVersion = "one.oh" },
		"no name":               func(c *AgentCard) { c.Identity.Name = "" },
		"no version":            func(c *AgentCard) { c.Identity.Version = "" },
		"no provider org":       func(c *AgentCard) { c.Identity.Provider = &AgentProvider{} },
		"no interfaces":         func(c *AgentCard) { c.Interfaces = nil },
		"no skills":             func(c *AgentCard) { c.Skills = nil },
		"bad transport":         func(c *AgentCard) { c.Interfaces[0].Transport = "TRANSPORT_PROTOCOL_CARRIER_PIGEON" },
		"no interface url":      func(c *AgentCard) { c.Interfaces[0].URL = "" },
		"no skill id":           func(c *AgentCard) { c.Skills[0].ID = "" },
		"no skill description":  func(c *AgentCard) { c.Skills[0].Description = "" },
		"no extension uri":      func(c *AgentCard) { c.Capabilities.Extensions[0].URI = "" },
		"undeclared scheme ref": func(c *AgentCard) { c.Security = []map[string][]string{{"nope": {}}} },
		"undeclared skill scheme": func(c *AgentCard) {
			c.Skills[0].Security = []map[string][]string{{"nope": {}}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			card := base
			card.Interfaces = append([]AgentInterface(nil), base.Interfaces...)
			card.Skills = append([]AgentSkill(nil), base.Skills...)
			card.Capabilities.Extensions = append([]AgentExtension(nil), base.Capabilities.Extensions...)
			mutate(&card)
			if err := ValidateAgentCard(&card); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}

	if err := ValidateAgentCard(nil); err == nil {
		t.Fatal("nil card accepted")
	}
}

func TestValidateSecurityScheme(t *testing.T) {
	valid := map[string]SecurityScheme{
		"bearer":     BearerScheme("JWT", ""),
		"apiKey":     APIKeyScheme("X-Api-Key", APIKeyInHeader, ""),
		"oidc":       OpenIDConnectScheme("https://idp.example.test/.well-known/openid-configuration", ""),
		"mtls":       MutualTLSScheme(""),
		"oauth2":     {OAuth2: &OAuth2SecurityScheme{Flows: OAuthFlows{ClientCredentials: &ClientCredentialsOAuthFlow{TokenURL: "https://idp.example.test/token"}}}},
		"authzOAuth": {OAuth2: &OAuth2SecurityScheme{Flows: OAuthFlows{AuthorizationCode: &AuthorizationCodeOAuthFlow{AuthorizationURL: "a", TokenURL: "t"}}}},
		"deviceCode": {OAuth2: &OAuth2SecurityScheme{Flows: OAuthFlows{DeviceCode: &DeviceCodeOAuthFlow{DeviceAuthorizationURL: "d", TokenURL: "t"}}}},
	}
	for name, scheme := range valid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSecurityScheme(scheme, "s"); err != nil {
				t.Fatalf("valid scheme rejected: %v", err)
			}
		})
	}

	invalid := map[string]SecurityScheme{
		"empty":          {},
		"two arms":       {APIKey: &APIKeySecurityScheme{Name: "k", In: APIKeyInHeader}, MutualTLS: &MutualTLSSecurityScheme{}},
		"apiKey no name": {APIKey: &APIKeySecurityScheme{In: APIKeyInHeader}},
		"apiKey bad in":  {APIKey: &APIKeySecurityScheme{Name: "k", In: "body"}},
		"http no scheme": {HTTPAuth: &HTTPAuthSecurityScheme{}},
		"oauth no flows": {OAuth2: &OAuth2SecurityScheme{}},
		"oidc no url":    {OpenIDConnect: &OpenIDConnectSecurityScheme{}},
	}
	for name, scheme := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSecurityScheme(scheme, "s"); err == nil {
				t.Fatal("invalid scheme accepted")
			}
		})
	}
}

func TestSecuritySchemeKind(t *testing.T) {
	cases := map[SecuritySchemeKind]SecurityScheme{
		SecuritySchemeUnset:         {},
		SecuritySchemeAPIKey:        APIKeyScheme("k", APIKeyInHeader, ""),
		SecuritySchemeHTTPAuth:      BearerScheme("", ""),
		SecuritySchemeOpenIDConnect: OpenIDConnectScheme("u", ""),
		SecuritySchemeMutualTLS:     MutualTLSScheme(""),
		SecuritySchemeOAuth2:        {OAuth2: &OAuth2SecurityScheme{}},
	}
	for want, scheme := range cases {
		if got := scheme.Kind(); got != want {
			t.Errorf("Kind() = %q, want %q", got, want)
		}
	}
}

func TestTransportProtocolValid(t *testing.T) {
	for _, tp := range []TransportProtocol{TransportJSONRPC, TransportGRPC, TransportHTTPJSON} {
		if !tp.Valid() {
			t.Errorf("%q reported invalid", tp)
		}
	}
	for _, tp := range []TransportProtocol{TransportUnspecified, "", "grpc"} {
		if tp.Valid() {
			t.Errorf("%q reported valid", tp)
		}
	}
}

func TestAgentCardExtensionAccessors(t *testing.T) {
	card := nexusCard()
	if !card.DeclaresExtension(NexusExtensionURI) {
		t.Fatal("card does not declare the nexus extension")
	}
	ext, ok := card.Extension(NexusExtensionURI)
	if !ok {
		t.Fatal("Extension() missed the nexus extension")
	}
	if ext.Version != NexusExtensionVersion {
		t.Errorf("extension version = %q, want %q", ext.Version, NexusExtensionVersion)
	}
	if len(card.RequiredExtensions()) != 0 {
		t.Errorf("nexus extension must not be required: %v", card.RequiredExtensions())
	}
	if _, ok := card.Extension("https://example.test/other"); ok {
		t.Error("Extension() matched an undeclared uri")
	}

	card = card.WithExtension(AgentExtension{URI: "https://example.test/must", Required: true})
	required := card.RequiredExtensions()
	if len(required) != 1 || required[0] != "https://example.test/must" {
		t.Errorf("RequiredExtensions() = %v", required)
	}
}

func TestAgentCardWithSecuritySchemeDoesNotAliasMaps(t *testing.T) {
	base := NewAgentCard(AgentIdentity{Name: "n", Version: "1"}).
		WithSecurityScheme("a", MutualTLSScheme(""))
	derived := base.WithSecurityScheme("b", MutualTLSScheme(""))

	if _, ok := base.SecuritySchemes["b"]; ok {
		t.Error("WithSecurityScheme mutated the receiver's map")
	}
	if len(derived.SecuritySchemes) != 2 {
		t.Errorf("derived card has %d schemes, want 2", len(derived.SecuritySchemes))
	}
}

func TestAgentCardEncodeIsIndented(t *testing.T) {
	card := nexusCard()
	data, err := EncodeAgentCard(&card)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(data), "\n  \"identity\"") {
		t.Errorf("card is not indented for human reading:\n%s", data)
	}
}
