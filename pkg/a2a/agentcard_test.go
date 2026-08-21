package a2a

import (
	"encoding/json"
	"strings"
	"testing"
)

// nexusCard builds the card a Nexus serve plugin would publish, exercising every
// required top-level field.
func nexusCard() AgentCard {
	card := NewAgentCard("nexus", "Nexus agent harness exposed over A2A.", "0.1.0")
	card.DocumentationURL = "https://example.test/docs"
	card.Provider = &AgentProvider{Organization: "Nexus", URL: "https://example.test"}
	card = card.
		WithInterface(BindingJSONRPC, "https://agent.example.test/a2a").
		WithInterface(BindingHTTPJSON, "https://agent.example.test/a2a/v1").
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
		"name", "description", "version", "capabilities", "securitySchemes",
		"supportedInterfaces", "skills", "defaultInputModes", "defaultOutputModes",
	} {
		if _, ok := doc[key]; !ok {
			t.Errorf("card is missing required key %q", key)
		}
	}

	// The card is flat: there is no nested identity object, and no card-level
	// protocol version. Both were plausible-looking inventions; neither exists.
	if _, ok := doc["identity"]; ok {
		t.Error("card serialized a nested identity object; the wire shape is flat")
	}
	if _, ok := doc["protocolVersion"]; ok {
		t.Error("card serialized a top-level protocolVersion; the version is per-interface")
	}
	if _, ok := doc["interfaces"]; ok {
		t.Error("card serialized interfaces; the field is supportedInterfaces")
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

	// Interfaces carry the open-form binding name and their own protocol version.
	ifaces, ok := doc["supportedInterfaces"].([]any)
	if !ok || len(ifaces) != 2 {
		t.Fatalf("supportedInterfaces = %v", doc["supportedInterfaces"])
	}
	first, _ := ifaces[0].(map[string]any)
	if first["protocolBinding"] != string(BindingJSONRPC) {
		t.Errorf("supportedInterfaces[0].protocolBinding = %v, want %q", first["protocolBinding"], BindingJSONRPC)
	}
	if first["protocolVersion"] != ProtocolVersion {
		t.Errorf("supportedInterfaces[0].protocolVersion = %v, want %q", first["protocolVersion"], ProtocolVersion)
	}

	// The security scheme is the flattened oneof, not an inline kind string.
	schemes, _ := doc["securitySchemes"].(map[string]any)
	bearer, _ := schemes["bearer"].(map[string]any)
	if _, ok := bearer["httpAuthSecurityScheme"]; !ok {
		t.Errorf("bearer scheme did not serialize as httpAuthSecurityScheme: %v", bearer)
	}

	// A security requirement wraps its scopes in the proto's StringList, because
	// a protobuf map value cannot be a bare repeated field.
	reqs, ok := doc["securityRequirements"].([]any)
	if !ok || len(reqs) != 1 {
		t.Fatalf("securityRequirements = %v", doc["securityRequirements"])
	}
	req, _ := reqs[0].(map[string]any)
	inner, ok := req["schemes"].(map[string]any)
	if !ok {
		t.Fatalf("securityRequirements[0].schemes = %v", req["schemes"])
	}
	if _, ok := inner["bearer"].(map[string]any); !ok {
		t.Errorf("securityRequirements[0].schemes.bearer is not a StringList object: %v", inner["bearer"])
	}

	// And it round-trips.
	back, err := DecodeAgentCard(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := ValidateAgentCard(back); err != nil {
		t.Fatalf("validate decoded: %v", err)
	}
	if back.Name != card.Name || len(back.Skills) != len(card.Skills) {
		t.Errorf("round trip lost data: %+v", back)
	}
	if url, ok := back.InterfaceFor(BindingHTTPJSON); !ok || url != "https://agent.example.test/a2a/v1" {
		t.Errorf("InterfaceFor(HTTP+JSON) = %q, %v", url, ok)
	}
	if _, ok := back.Skill("chat"); !ok {
		t.Error("decoded card lost the chat skill")
	}
}

// TestAgentCardEncodeNormalizesRequiredRepeatedFields pins section 8.4.1: a
// REQUIRED repeated field must be present even when empty, and [] is not null.
func TestAgentCardEncodeNormalizesRequiredRepeatedFields(t *testing.T) {
	card := AgentCard{Name: "n", Description: "d", Version: "1"}
	data, err := EncodeAgentCard(&card)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"supportedInterfaces", "defaultInputModes", "defaultOutputModes", "skills"} {
		v, ok := doc[key]
		if !ok {
			t.Errorf("%s absent; a required repeated field must be present", key)
			continue
		}
		if _, ok := v.([]any); !ok {
			t.Errorf("%s = %v, want an array (never null)", key, v)
		}
	}

	// Normalization must not mutate the caller's card.
	if card.Skills != nil || card.DefaultInputModes != nil {
		t.Error("EncodeAgentCard mutated the card it was given")
	}

	// A skill's tags are likewise required.
	card.Skills = []AgentSkill{{ID: "s", Name: "S", Description: "d"}}
	data, err = EncodeAgentCard(&card)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(data), `"tags": []`) {
		t.Errorf("skill tags were omitted rather than emitted as []:\n%s", data)
	}
	if card.Skills[0].Tags != nil {
		t.Error("EncodeAgentCard mutated the caller's skill")
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
	card := NewAgentCard("n", "d", "1")
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
		"no name":           func(c *AgentCard) { c.Name = "" },
		"no description":    func(c *AgentCard) { c.Description = "" },
		"no version":        func(c *AgentCard) { c.Version = "" },
		"no provider org":   func(c *AgentCard) { c.Provider = &AgentProvider{URL: "https://x.test"} },
		"no interfaces":     func(c *AgentCard) { c.SupportedInterfaces = nil },
		"no skills":         func(c *AgentCard) { c.Skills = nil },
		"no binding":        func(c *AgentCard) { c.SupportedInterfaces[0].ProtocolBinding = "" },
		"no interface url":  func(c *AgentCard) { c.SupportedInterfaces[0].URL = "" },
		"no iface version":  func(c *AgentCard) { c.SupportedInterfaces[0].ProtocolVersion = "" },
		"bad iface version": func(c *AgentCard) { c.SupportedInterfaces[0].ProtocolVersion = "one.oh" },
		"no skill id":       func(c *AgentCard) { c.Skills[0].ID = "" },
		"no skill desc":     func(c *AgentCard) { c.Skills[0].Description = "" },
		"no extension uri":  func(c *AgentCard) { c.Capabilities.Extensions[0].URI = "" },
		"empty requirement": func(c *AgentCard) { c.SecurityRequirements = []SecurityRequirement{{}} },
		"undeclared scheme": func(c *AgentCard) { c.SecurityRequirements = []SecurityRequirement{NewSecurityRequirement("nope")} },
		"undeclared on skill": func(c *AgentCard) {
			c.Skills[0].SecurityRequirements = []SecurityRequirement{NewSecurityRequirement("nope")}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			card := base
			card.SupportedInterfaces = append([]AgentInterface(nil), base.SupportedInterfaces...)
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

// TestValidateAgentCardAcceptsNonCoreBinding pins the open-vocabulary rule: a
// binding this codec does not implement is still a legal card.
func TestValidateAgentCardAcceptsNonCoreBinding(t *testing.T) {
	card := nexusCard()
	card.SupportedInterfaces = []AgentInterface{{
		URL:             "wss://agent.example.test/a2a",
		ProtocolBinding: "WEBSOCKET",
		ProtocolVersion: "1.0",
	}}
	if err := ValidateAgentCard(&card); err != nil {
		t.Fatalf("a non-core binding must not be rejected: %v", err)
	}
	if card.SupportedInterfaces[0].ProtocolBinding.Core() {
		t.Error("WEBSOCKET reported as a core binding")
	}
}

func TestValidateSecurityScheme(t *testing.T) {
	valid := map[string]SecurityScheme{
		"bearer":     BearerScheme("JWT", ""),
		"apiKey":     APIKeyScheme("X-Api-Key", APIKeyInHeader, ""),
		"oidc":       OpenIDConnectScheme("https://idp.example.test/.well-known/openid-configuration", ""),
		"mtls":       MutualTLSScheme(""),
		"oauth2":     {OAuth2: &OAuth2SecurityScheme{Flows: OAuthFlows{ClientCredentials: &ClientCredentialsOAuthFlow{TokenURL: "https://idp.example.test/token"}}}},
		"authzOAuth": {OAuth2: &OAuth2SecurityScheme{Flows: OAuthFlows{AuthorizationCode: &AuthorizationCodeOAuthFlow{AuthorizationURL: "a", TokenURL: "t", PKCERequired: true}}}},
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
		"two arms":       {APIKey: &APIKeySecurityScheme{Name: "k", Location: APIKeyInHeader}, MutualTLS: &MutualTlsSecurityScheme{}},
		"apiKey no name": {APIKey: &APIKeySecurityScheme{Location: APIKeyInHeader}},
		"apiKey bad loc": {APIKey: &APIKeySecurityScheme{Name: "k", Location: "body"}},
		"http no scheme": {HTTPAuth: &HTTPAuthSecurityScheme{}},
		"oauth no flows": {OAuth2: &OAuth2SecurityScheme{}},
		// flows is a oneof: two arms cannot be represented on the wire.
		"oauth two flows": {OAuth2: &OAuth2SecurityScheme{Flows: OAuthFlows{
			ClientCredentials: &ClientCredentialsOAuthFlow{TokenURL: "t"},
			DeviceCode:        &DeviceCodeOAuthFlow{DeviceAuthorizationURL: "d", TokenURL: "t"},
		}}},
		"oidc no url": {OpenIDConnect: &OpenIdConnectSecurityScheme{}},
	}
	for name, scheme := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSecurityScheme(scheme, "s"); err == nil {
				t.Fatal("invalid scheme accepted")
			}
		})
	}
}

// TestAPIKeySchemeSerializesLocation pins the field name: the proto calls the
// placement "location", not the OpenAPI-familiar "in".
func TestAPIKeySchemeSerializesLocation(t *testing.T) {
	data, err := json.Marshal(APIKeyScheme("X-Api-Key", APIKeyInHeader, ""))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"location":"header"`) {
		t.Errorf("api key scheme = %s, want a \"location\" key", data)
	}
	if strings.Contains(string(data), `"in":`) {
		t.Errorf("api key scheme serialized an \"in\" key: %s", data)
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

func TestProtocolBindingCore(t *testing.T) {
	for _, b := range []ProtocolBinding{BindingJSONRPC, BindingGRPC, BindingHTTPJSON} {
		if !b.Core() {
			t.Errorf("%q reported non-core", b)
		}
	}
	// The ProtoJSON enum spellings are NOT the wire values; they must not match.
	for _, b := range []ProtocolBinding{"", "jsonrpc", "TRANSPORT_PROTOCOL_JSONRPC", "HTTP_JSON"} {
		if b.Core() {
			t.Errorf("%q reported core", b)
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
	if ext.Params["version"] != NexusExtensionVersion {
		t.Errorf("extension params.version = %v, want %q", ext.Params["version"], NexusExtensionVersion)
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
	base := NewAgentCard("n", "d", "1").WithSecurityScheme("a", MutualTLSScheme(""))
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
	if !strings.Contains(string(data), "\n  \"name\"") {
		t.Errorf("card is not indented for human reading:\n%s", data)
	}
}
