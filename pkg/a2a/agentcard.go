package a2a

import (
	"encoding/json"
	"fmt"
)

// Agent Card discovery types (specification section 8, data model section 4.4).
// The card is the self-describing manifest an agent publishes at AgentCardPath.
// It tells a client who the agent is, which bindings it exposes and at what
// URLs, how to authenticate, what it can do, and which protocol extensions it
// speaks.
//
// The card is served as plain JSON, not as ContentTypeJSON: it is fetched by
// ordinary HTTP clients before any A2A negotiation has happened.
//
// # Shape provenance
//
// Specification section 4.4 delegates every card type to the canonical Protocol
// Buffer definition ("proto_to_table"), so a2a.proto — not the prose — is the
// authority for field names, presence and nesting. Three consequences are worth
// naming because they read as surprising next to other agent-manifest formats:
//
//   - The card is FLAT. There is no nested identity object: name, description,
//     version, provider, documentationUrl and iconUrl sit at the top level.
//   - There is no card-level protocol version. The A2A version is declared PER
//     INTERFACE (AgentInterface.ProtocolVersion), which is what lets one agent
//     serve 0.3 and 1.0 from different URLs — section 3.6.2's "agents CAN expose
//     multiple interfaces for the same transport with different versions".
//   - The transport is an OPEN-FORM STRING ("JSONRPC", "GRPC", "HTTP+JSON"), not
//     a ProtoJSON enum. See ProtocolBinding.

// ContentTypeAgentCard is the media type of a served Agent Card.
const ContentTypeAgentCard = "application/json"

// ProtocolBinding names the protocol binding spoken at an AgentInterface URL.
//
// This is deliberately a string alias over an open vocabulary rather than a
// closed enum: a2a.proto types AgentInterface.protocol_binding as a plain
// string and documents it as "an open form string, to be easily extended for
// other protocol bindings", naming JSONRPC, GRPC and HTTP+JSON as the three
// officially supported values. A card MAY therefore advertise a binding this
// codec has never heard of, and rejecting it would make this package the reason
// a conforming card fails to parse.
type ProtocolBinding string

// The three officially supported protocol bindings. Note the values are the
// bare names from the proto comment, NOT ProtoJSON enum spellings: there is no
// TransportProtocol enum in the specification.
const (
	BindingJSONRPC  ProtocolBinding = "JSONRPC"
	BindingGRPC     ProtocolBinding = "GRPC"
	BindingHTTPJSON ProtocolBinding = "HTTP+JSON"
)

// Core reports whether the binding is one of the three the specification
// officially defines. A binding outside this set is legal on the wire; Core
// exists so a client can tell "I do not speak this" from "this is malformed".
func (b ProtocolBinding) Core() bool {
	switch b {
	case BindingJSONRPC, BindingGRPC, BindingHTTPJSON:
		return true
	default:
		return false
	}
}

// AgentCard is the agent's public manifest, served at AgentCardPath.
//
// Field order follows a2a.proto field numbers so a reader can diff the two side
// by side.
type AgentCard struct {
	// Name is the human-readable agent name. Required.
	Name string `json:"name"`
	// Description is a human-readable summary of what the agent does. Required
	// by the proto, so it is serialized even when empty.
	Description string `json:"description"`
	// SupportedInterfaces are the protocol endpoints the agent exposes, in
	// preference order: the first entry is the preferred one (section 8.3.1).
	// Required, and must be non-empty.
	SupportedInterfaces []AgentInterface `json:"supportedInterfaces"`
	// Provider identifies the organization operating the agent.
	Provider *AgentProvider `json:"provider,omitempty"`
	// Version is the agent's own version, independent of any protocol version.
	// Required.
	Version string `json:"version"`
	// DocumentationURL points at human-readable documentation.
	DocumentationURL string `json:"documentationUrl,omitempty"`
	// Capabilities are the optional protocol features the agent supports.
	// Required, and always serialized in full so a client never has to infer a
	// capability from an absent field.
	Capabilities AgentCapabilities `json:"capabilities"`
	// SecuritySchemes declares, by name, every authentication scheme the agent
	// accepts. Names are referenced from SecurityRequirements and from
	// AgentSkill.SecurityRequirements.
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	// SecurityRequirements lists the scheme requirement sets that apply
	// agent-wide. Each entry is one alternative: satisfying any entry suffices,
	// and within an entry every named scheme must be satisfied.
	SecurityRequirements []SecurityRequirement `json:"securityRequirements,omitempty"`
	// DefaultInputModes are the media types the agent accepts in message parts
	// when a skill does not narrow them. Required.
	DefaultInputModes []string `json:"defaultInputModes"`
	// DefaultOutputModes are the media types the agent produces when a skill
	// does not narrow them. Required.
	DefaultOutputModes []string `json:"defaultOutputModes"`
	// Skills describe what the agent can do. Required, and must be non-empty.
	Skills []AgentSkill `json:"skills"`
	// Signatures carry JWS detached signatures over the card, for clients that
	// verify card provenance.
	Signatures []AgentCardSignature `json:"signatures,omitempty"`
	// IconURL points at an icon representing the agent.
	IconURL string `json:"iconUrl,omitempty"`
}

// AgentProvider identifies the organization behind an agent.
type AgentProvider struct {
	// URL is the provider's website or relevant documentation. Required when a
	// provider is given.
	URL string `json:"url"`
	// Organization is the provider's name. Required when a provider is given.
	Organization string `json:"organization"`
}

// AgentCapabilities declares the optional protocol features an agent supports.
//
// The three booleans are serialized unconditionally rather than with omitempty.
// The proto marks them `optional bool`, and section 8.4.1's canonicalization
// rules say an optional field that was explicitly set must be present in the
// JSON even when its value equals the default — which is exactly the position
// this codec takes: a discovery document should state a capability as false
// rather than leave a client to infer it from an absent key. The zero value is
// therefore the correct, honest card for an agent supporting none of them.
type AgentCapabilities struct {
	// Streaming reports support for SendStreamingMessage and SubscribeToTask.
	Streaming bool `json:"streaming"`
	// PushNotifications reports support for the push-notification config
	// operations and webhook delivery.
	PushNotifications bool `json:"pushNotifications"`
	// ExtendedAgentCard reports support for GetExtendedAgentCard.
	ExtendedAgentCard bool `json:"extendedAgentCard"`
	// Extensions declares the protocol extensions the agent speaks.
	Extensions []AgentExtension `json:"extensions,omitempty"`
}

// AgentExtension declares one protocol extension in an agent card
// (specification section 4.6.1). Clients opt in per request via the
// A2A-Extensions service parameter.
//
// There is deliberately no version field: section 4.6.3 requires extensions to
// carry their version inside the URI and requires a NEW URI for any breaking
// change, so a separate version field would be a second, disagreeable source of
// truth. See NexusExtensionURI for how this codebase applies that rule.
type AgentExtension struct {
	// URI uniquely identifies the extension, version included. Required.
	URI string `json:"uri"`
	// Description is a human-readable summary of how this agent uses the
	// extension.
	Description string `json:"description,omitempty"`
	// Required reports whether a client must support the extension to interact
	// with this agent at all. Optional extensions are the norm; a required one
	// lets an agent refuse clients that would misread its output.
	Required bool `json:"required,omitempty"`
	// Params is extension-defined configuration data, whose shape the extension
	// itself specifies (google.protobuf.Struct in the proto).
	Params map[string]any `json:"params,omitempty"`
}

// AgentInterface binds one protocol binding, at one A2A protocol version, to
// one endpoint URL. An agent exposing both the JSON-RPC and REST bindings
// declares two interfaces; an agent serving 0.3 and 1.0 declares one per
// version.
type AgentInterface struct {
	// URL is the endpoint address. For HTTP bindings it is the base URL that
	// operation paths are appended to (REST) or POSTed to directly (JSON-RPC);
	// for gRPC it is "hostname:port". Required.
	URL string `json:"url"`
	// ProtocolBinding is the binding spoken at this URL. Required.
	ProtocolBinding ProtocolBinding `json:"protocolBinding"`
	// Tenant is an opaque routing identifier for deployments serving several
	// agents or tenants behind one A2A endpoint. When set, a client MUST echo
	// it in the tenant field of every request to this interface (section
	// 8.3.2). The protocol does not define its format or semantics.
	Tenant string `json:"tenant,omitempty"`
	// ProtocolVersion is the Major.Minor A2A version this interface exposes.
	// Required.
	ProtocolVersion string `json:"protocolVersion"`
}

// AgentSkill describes one capability the agent advertises.
type AgentSkill struct {
	// ID uniquely identifies the skill within the card. Required.
	ID string `json:"id"`
	// Name is the human-readable skill name. Required.
	Name string `json:"name"`
	// Description explains what the skill does. Required.
	Description string `json:"description"`
	// Tags are keywords describing the skill's capabilities. Required by the
	// proto, so the key is always present.
	Tags []string `json:"tags"`
	// Examples are sample prompts that exercise the skill.
	Examples []string `json:"examples,omitempty"`
	// InputModes narrows the card's default input media types for this skill.
	InputModes []string `json:"inputModes,omitempty"`
	// OutputModes narrows the card's default output media types for this skill.
	OutputModes []string `json:"outputModes,omitempty"`
	// SecurityRequirements narrows the card's agent-wide security requirements
	// for this skill, for skills needing stronger credentials than the agent as
	// a whole.
	SecurityRequirements []SecurityRequirement `json:"securityRequirements,omitempty"`
}

// AgentCardSignature is a JWS detached signature over the card.
type AgentCardSignature struct {
	// Protected is the base64url-encoded JWS protected header. Required.
	Protected string `json:"protected"`
	// Signature is the base64url-encoded signature. Required.
	Signature string `json:"signature"`
	// Header is the unprotected JWS header.
	Header map[string]any `json:"header,omitempty"`
}

// ---- Security requirements ----

// StringList is the proto's wrapper for a repeated string inside a map value,
// which protobuf maps cannot hold directly. It is why a security requirement's
// scopes serialize as {"list": [...]} rather than as a bare array.
type StringList struct {
	List []string `json:"list,omitempty"`
}

// SecurityRequirement is one alternative set of credentials that satisfies
// access: every scheme named in Schemes must be satisfied, and its StringList
// carries the scopes that scheme requires (empty for schemes without scopes).
type SecurityRequirement struct {
	Schemes map[string]StringList `json:"schemes,omitempty"`
}

// NewSecurityRequirement builds a single-scheme requirement.
func NewSecurityRequirement(scheme string, scopes ...string) SecurityRequirement {
	if scopes == nil {
		scopes = []string{}
	}
	return SecurityRequirement{Schemes: map[string]StringList{scheme: {List: scopes}}}
}

// ---- Security schemes ----

// SecuritySchemeKind names which arm of a SecurityScheme oneof is set.
type SecuritySchemeKind string

// Security scheme kinds.
const (
	SecuritySchemeUnset         SecuritySchemeKind = ""
	SecuritySchemeAPIKey        SecuritySchemeKind = "apiKey"
	SecuritySchemeHTTPAuth      SecuritySchemeKind = "httpAuth"
	SecuritySchemeOAuth2        SecuritySchemeKind = "oauth2"
	SecuritySchemeOpenIDConnect SecuritySchemeKind = "openIdConnect"
	SecuritySchemeMutualTLS     SecuritySchemeKind = "mutualTls"
)

// SecurityScheme is one authentication scheme an agent accepts. Like Part it is
// a flattened oneof: exactly one arm is set.
type SecurityScheme struct {
	// APIKey carries a credential in a named header, query parameter or cookie.
	APIKey *APIKeySecurityScheme `json:"apiKeySecurityScheme,omitempty"`
	// HTTPAuth is an HTTP Authorization scheme such as Bearer or Basic.
	HTTPAuth *HTTPAuthSecurityScheme `json:"httpAuthSecurityScheme,omitempty"`
	// OAuth2 is an OAuth 2.0 scheme with exactly one supported flow.
	OAuth2 *OAuth2SecurityScheme `json:"oauth2SecurityScheme,omitempty"`
	// OpenIDConnect is OpenID Connect Discovery.
	OpenIDConnect *OpenIdConnectSecurityScheme `json:"openIdConnectSecurityScheme,omitempty"`
	// MutualTLS is client-certificate authentication.
	MutualTLS *MutualTlsSecurityScheme `json:"mtlsSecurityScheme,omitempty"`
}

// Kind reports which arm is set, or SecuritySchemeUnset when none is. It
// reports only the first set arm when several are; use ValidateSecurityScheme
// to detect that.
func (s SecurityScheme) Kind() SecuritySchemeKind {
	switch {
	case s.APIKey != nil:
		return SecuritySchemeAPIKey
	case s.HTTPAuth != nil:
		return SecuritySchemeHTTPAuth
	case s.OAuth2 != nil:
		return SecuritySchemeOAuth2
	case s.OpenIDConnect != nil:
		return SecuritySchemeOpenIDConnect
	case s.MutualTLS != nil:
		return SecuritySchemeMutualTLS
	default:
		return SecuritySchemeUnset
	}
}

// APIKeyLocation names where an API key credential travels.
type APIKeyLocation string

// API key locations.
const (
	APIKeyInQuery  APIKeyLocation = "query"
	APIKeyInHeader APIKeyLocation = "header"
	APIKeyInCookie APIKeyLocation = "cookie"
)

// Valid reports whether the location is one of the three defined placements.
func (l APIKeyLocation) Valid() bool {
	switch l {
	case APIKeyInQuery, APIKeyInHeader, APIKeyInCookie:
		return true
	default:
		return false
	}
}

// APIKeySecurityScheme carries a credential in a named header, query parameter
// or cookie.
//
// The placement field is named "location", not the OpenAPI-familiar "in": the
// proto calls it location and section 5.5 derives the JSON names from the proto.
type APIKeySecurityScheme struct {
	// Description is a human-readable note about the scheme.
	Description string `json:"description,omitempty"`
	// Location is where the credential travels. Required.
	Location APIKeyLocation `json:"location"`
	// Name is the header, parameter or cookie name. Required.
	Name string `json:"name"`
}

// HTTPAuthSecurityScheme is an HTTP Authorization scheme.
type HTTPAuthSecurityScheme struct {
	// Description is a human-readable note about the scheme.
	Description string `json:"description,omitempty"`
	// Scheme is the HTTP auth scheme name as registered with IANA, e.g.
	// "Bearer" or "Basic". Required.
	Scheme string `json:"scheme"`
	// BearerFormat hints at the bearer token's format, e.g. "JWT".
	BearerFormat string `json:"bearerFormat,omitempty"`
}

// OAuth2SecurityScheme is an OAuth 2.0 scheme.
type OAuth2SecurityScheme struct {
	// Description is a human-readable note about the scheme.
	Description string `json:"description,omitempty"`
	// Flows is the supported grant flow. Required, and exactly one arm is set.
	Flows OAuthFlows `json:"flows"`
	// OAuth2MetadataURL points at the RFC 8414 authorization server metadata
	// document, letting a client discover endpoints rather than hardcode them.
	OAuth2MetadataURL string `json:"oauth2MetadataUrl,omitempty"`
}

// OAuthFlows selects the OAuth 2.0 grant flow an agent supports. It is a proto
// oneof, so exactly one arm is set — not a set of independently-toggled flows.
//
// The implicit and resource-owner-password grants exist in the proto but are
// marked deprecated ("use Authorization Code + PKCE instead"). They are
// deliberately absent here: this codec will not help anyone advertise a grant
// the specification tells them not to use.
type OAuthFlows struct {
	// AuthorizationCode is the authorization code grant, with PKCE.
	AuthorizationCode *AuthorizationCodeOAuthFlow `json:"authorizationCode,omitempty"`
	// ClientCredentials is the machine-to-machine grant.
	ClientCredentials *ClientCredentialsOAuthFlow `json:"clientCredentials,omitempty"`
	// DeviceCode is the device authorization grant.
	DeviceCode *DeviceCodeOAuthFlow `json:"deviceCode,omitempty"`
}

// Empty reports whether no flow is declared.
func (f OAuthFlows) Empty() bool {
	return f.AuthorizationCode == nil && f.ClientCredentials == nil && f.DeviceCode == nil
}

// set counts how many flow arms are populated, for the oneof check.
func (f OAuthFlows) set() int {
	n := 0
	for _, present := range []bool{
		f.AuthorizationCode != nil,
		f.ClientCredentials != nil,
		f.DeviceCode != nil,
	} {
		if present {
			n++
		}
	}
	return n
}

// AuthorizationCodeOAuthFlow is the authorization code grant.
type AuthorizationCodeOAuthFlow struct {
	// AuthorizationURL is the authorization endpoint. Required.
	AuthorizationURL string `json:"authorizationUrl"`
	// TokenURL is the token endpoint. Required.
	TokenURL string `json:"tokenUrl"`
	// RefreshURL is the refresh endpoint, when it differs from TokenURL.
	RefreshURL string `json:"refreshUrl,omitempty"`
	// Scopes maps scope name to human-readable description. Required.
	Scopes map[string]string `json:"scopes,omitempty"`
	// PKCERequired reports whether RFC 7636 PKCE is required for this flow. It
	// should always be set for public clients and is recommended for all.
	PKCERequired bool `json:"pkceRequired,omitempty"`
}

// ClientCredentialsOAuthFlow is the machine-to-machine grant.
type ClientCredentialsOAuthFlow struct {
	// TokenURL is the token endpoint. Required.
	TokenURL string `json:"tokenUrl"`
	// RefreshURL is the refresh endpoint, when it differs from TokenURL.
	RefreshURL string `json:"refreshUrl,omitempty"`
	// Scopes maps scope name to human-readable description. Required.
	Scopes map[string]string `json:"scopes,omitempty"`
}

// DeviceCodeOAuthFlow is the device authorization grant, for input-constrained
// clients.
type DeviceCodeOAuthFlow struct {
	// DeviceAuthorizationURL is the device authorization endpoint. Required.
	DeviceAuthorizationURL string `json:"deviceAuthorizationUrl"`
	// TokenURL is the token endpoint. Required.
	TokenURL string `json:"tokenUrl"`
	// RefreshURL is the refresh endpoint, when it differs from TokenURL.
	RefreshURL string `json:"refreshUrl,omitempty"`
	// Scopes maps scope name to human-readable description. Required.
	Scopes map[string]string `json:"scopes,omitempty"`
}

// OpenIdConnectSecurityScheme is OpenID Connect Discovery. The spelling matches
// the proto message name, which is also what the JSON key derives from.
type OpenIdConnectSecurityScheme struct {
	// Description is a human-readable note about the scheme.
	Description string `json:"description,omitempty"`
	// OpenIdConnectURL is the OIDC discovery document URL. Required.
	OpenIdConnectURL string `json:"openIdConnectUrl"`
}

// MutualTlsSecurityScheme is client-certificate authentication.
type MutualTlsSecurityScheme struct {
	// Description is a human-readable note about the scheme.
	Description string `json:"description,omitempty"`
}

// ---- Constructors ----

// NewAgentCard builds a card with the capability booleans explicitly at their
// conservative defaults: streaming on, push notifications and extended card
// off. The three required identity fields are taken as arguments because a card
// without them cannot be served at all.
func NewAgentCard(name, description, version string) AgentCard {
	return AgentCard{
		Name:        name,
		Description: description,
		Version:     version,
		Capabilities: AgentCapabilities{
			Streaming:         true,
			PushNotifications: false,
			ExtendedAgentCard: false,
		},
	}
}

// WithInterface appends a protocol endpoint speaking this codec's protocol
// version and returns the card, for chaining.
func (c AgentCard) WithInterface(binding ProtocolBinding, url string) AgentCard {
	c.SupportedInterfaces = append(c.SupportedInterfaces, AgentInterface{
		URL:             url,
		ProtocolBinding: binding,
		ProtocolVersion: ProtocolVersion,
	})
	return c
}

// WithSkill appends a skill and returns the card, for chaining.
func (c AgentCard) WithSkill(s AgentSkill) AgentCard {
	c.Skills = append(c.Skills, s)
	return c
}

// WithSecurityScheme registers a named security scheme and returns the card,
// for chaining.
func (c AgentCard) WithSecurityScheme(name string, scheme SecurityScheme) AgentCard {
	schemes := make(map[string]SecurityScheme, len(c.SecuritySchemes)+1)
	for k, v := range c.SecuritySchemes {
		schemes[k] = v
	}
	schemes[name] = scheme
	c.SecuritySchemes = schemes
	return c
}

// WithSecurityRequirement appends an agent-wide security requirement naming one
// scheme and its scopes, and returns the card for chaining.
func (c AgentCard) WithSecurityRequirement(name string, scopes ...string) AgentCard {
	c.SecurityRequirements = append(c.SecurityRequirements, NewSecurityRequirement(name, scopes...))
	return c
}

// WithExtension appends an extension declaration and returns the card, for
// chaining.
func (c AgentCard) WithExtension(e AgentExtension) AgentCard {
	c.Capabilities.Extensions = append(c.Capabilities.Extensions, e)
	return c
}

// BearerScheme builds an HTTP bearer-token security scheme.
func BearerScheme(bearerFormat, description string) SecurityScheme {
	return SecurityScheme{HTTPAuth: &HTTPAuthSecurityScheme{
		Scheme:       "Bearer",
		BearerFormat: bearerFormat,
		Description:  description,
	}}
}

// APIKeyScheme builds an API-key security scheme.
func APIKeyScheme(name string, location APIKeyLocation, description string) SecurityScheme {
	return SecurityScheme{APIKey: &APIKeySecurityScheme{
		Name:        name,
		Location:    location,
		Description: description,
	}}
}

// OpenIDConnectScheme builds an OpenID Connect Discovery security scheme.
func OpenIDConnectScheme(discoveryURL, description string) SecurityScheme {
	return SecurityScheme{OpenIDConnect: &OpenIdConnectSecurityScheme{
		OpenIdConnectURL: discoveryURL,
		Description:      description,
	}}
}

// MutualTLSScheme builds a client-certificate security scheme.
func MutualTLSScheme(description string) SecurityScheme {
	return SecurityScheme{MutualTLS: &MutualTlsSecurityScheme{Description: description}}
}

// ---- Accessors ----

// Extension returns the declared extension with the given URI.
func (c AgentCard) Extension(uri string) (AgentExtension, bool) {
	for _, e := range c.Capabilities.Extensions {
		if e.URI == uri {
			return e, true
		}
	}
	return AgentExtension{}, false
}

// DeclaresExtension reports whether the card declares the given extension URI.
func (c AgentCard) DeclaresExtension(uri string) bool {
	_, ok := c.Extension(uri)
	return ok
}

// RequiredExtensions returns the URIs of every extension a client must support
// to interact with this agent.
func (c AgentCard) RequiredExtensions() []string {
	var out []string
	for _, e := range c.Capabilities.Extensions {
		if e.Required {
			out = append(out, e.URI)
		}
	}
	return out
}

// InterfaceFor returns the first endpoint URL the agent exposes for a binding.
// Interfaces are ordered by preference, so "first" is "preferred".
func (c AgentCard) InterfaceFor(binding ProtocolBinding) (string, bool) {
	for _, i := range c.SupportedInterfaces {
		if i.ProtocolBinding == binding {
			return i.URL, true
		}
	}
	return "", false
}

// Skill returns the declared skill with the given id.
func (c AgentCard) Skill(id string) (AgentSkill, bool) {
	for _, s := range c.Skills {
		if s.ID == id {
			return s, true
		}
	}
	return AgentSkill{}, false
}

// ---- Codec ----

// EncodeAgentCard serializes a card for serving at AgentCardPath, using indented
// JSON since cards are read by humans as often as by clients.
//
// It normalizes the required repeated fields from nil to an empty array first.
// Section 8.4.1 requires a REQUIRED field to be present even at its default
// value, and a nil Go slice would marshal to null — which is a different JSON
// value from [], and would change the canonical form a signature is computed
// over. The card argument is not mutated.
func EncodeAgentCard(c *AgentCard) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("a2a: encode agent card: card is nil")
	}
	normalized := *c
	if normalized.SupportedInterfaces == nil {
		normalized.SupportedInterfaces = []AgentInterface{}
	}
	if normalized.DefaultInputModes == nil {
		normalized.DefaultInputModes = []string{}
	}
	if normalized.DefaultOutputModes == nil {
		normalized.DefaultOutputModes = []string{}
	}
	if normalized.Skills == nil {
		normalized.Skills = []AgentSkill{}
	}
	skills := make([]AgentSkill, len(normalized.Skills))
	copy(skills, normalized.Skills)
	for i := range skills {
		if skills[i].Tags == nil {
			skills[i].Tags = []string{}
		}
	}
	normalized.Skills = skills

	data, err := json.MarshalIndent(&normalized, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("a2a: encode agent card: %w", err)
	}
	return data, nil
}

// DecodeAgentCard parses an Agent Card.
func DecodeAgentCard(data []byte) (*AgentCard, error) {
	return decode[AgentCard](data, "agent card")
}

// ---- Validation ----

// ValidateAgentCard checks a card's required fields and internal consistency:
// declared interfaces and skills are non-empty and well-formed, every security
// scheme sets exactly one arm, and every security requirement names a declared
// scheme.
func ValidateAgentCard(c *AgentCard) *Error {
	if c == nil {
		return ErrInvalidParams(FieldViolation{Field: "agentCard", Description: "agent card is required"})
	}

	var violations []FieldViolation
	if c.Name == "" {
		violations = append(violations, FieldViolation{Field: "name", Description: "agent name is required"})
	}
	if c.Description == "" {
		violations = append(violations, FieldViolation{
			Field:       "description",
			Description: "agent description is required",
		})
	}
	if c.Version == "" {
		violations = append(violations, FieldViolation{Field: "version", Description: "agent version is required"})
	}
	if c.Provider != nil && c.Provider.Organization == "" {
		violations = append(violations, FieldViolation{
			Field:       "provider.organization",
			Description: "provider organization is required",
		})
	}
	if len(c.SupportedInterfaces) == 0 {
		violations = append(violations, FieldViolation{
			Field:       "supportedInterfaces",
			Description: "at least one protocol interface is required",
		})
	}
	if len(c.Skills) == 0 {
		violations = append(violations, FieldViolation{
			Field:       "skills",
			Description: "at least one skill is required",
		})
	}
	if len(violations) > 0 {
		return ErrInvalidParams(violations...)
	}

	for i, iface := range c.SupportedInterfaces {
		field := fmt.Sprintf("supportedInterfaces[%d]", i)
		if iface.URL == "" {
			return ErrInvalidParams(FieldViolation{Field: field + ".url", Description: "interface url is required"})
		}
		// The binding vocabulary is open (see ProtocolBinding), so the only
		// check that can be made without rejecting a legal card is presence.
		if iface.ProtocolBinding == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".protocolBinding",
				Description: "protocol binding is required",
			})
		}
		if iface.ProtocolVersion == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".protocolVersion",
				Description: "protocol version is required",
			})
		}
		if _, err := NormalizeVersion(iface.ProtocolVersion); err != nil {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".protocolVersion",
				Description: err.Error(),
			})
		}
	}

	for i, s := range c.Skills {
		if err := validateSkill(s, fmt.Sprintf("skills[%d]", i), c.SecuritySchemes); err != nil {
			return err
		}
	}

	for name, scheme := range c.SecuritySchemes {
		if err := ValidateSecurityScheme(scheme, fmt.Sprintf("securitySchemes[%q]", name)); err != nil {
			return err
		}
	}

	if err := validateSecurityRequirements(c.SecurityRequirements, "securityRequirements", c.SecuritySchemes); err != nil {
		return err
	}

	for i, e := range c.Capabilities.Extensions {
		if e.URI == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       fmt.Sprintf("capabilities.extensions[%d].uri", i),
				Description: "extension uri is required",
			})
		}
	}
	return nil
}

func validateSkill(s AgentSkill, field string, schemes map[string]SecurityScheme) *Error {
	var violations []FieldViolation
	if s.ID == "" {
		violations = append(violations, FieldViolation{Field: field + ".id", Description: "skill id is required"})
	}
	if s.Name == "" {
		violations = append(violations, FieldViolation{Field: field + ".name", Description: "skill name is required"})
	}
	if s.Description == "" {
		violations = append(violations, FieldViolation{
			Field:       field + ".description",
			Description: "skill description is required",
		})
	}
	if len(violations) > 0 {
		return ErrInvalidParams(violations...)
	}
	return validateSecurityRequirements(s.SecurityRequirements, field+".securityRequirements", schemes)
}

func validateSecurityRequirements(reqs []SecurityRequirement, field string, schemes map[string]SecurityScheme) *Error {
	for i, req := range reqs {
		if len(req.Schemes) == 0 {
			return ErrInvalidParams(FieldViolation{
				Field:       fmt.Sprintf("%s[%d].schemes", field, i),
				Description: "security requirement must name at least one scheme",
			})
		}
		for name := range req.Schemes {
			if _, ok := schemes[name]; !ok {
				return ErrInvalidParams(FieldViolation{
					Field:       fmt.Sprintf("%s[%d]", field, i),
					Description: fmt.Sprintf("security requirement names undeclared scheme %q", name),
				})
			}
		}
	}
	return nil
}

// ValidateSecurityScheme checks that a scheme sets exactly one arm and that the
// arm's required fields are present.
func ValidateSecurityScheme(s SecurityScheme, field string) *Error {
	set := 0
	for _, present := range []bool{
		s.APIKey != nil,
		s.HTTPAuth != nil,
		s.OAuth2 != nil,
		s.OpenIDConnect != nil,
		s.MutualTLS != nil,
	} {
		if present {
			set++
		}
	}
	if set != 1 {
		return ErrInvalidParams(FieldViolation{
			Field:       field,
			Description: fmt.Sprintf("security scheme must set exactly one scheme kind, got %d", set),
		})
	}

	switch s.Kind() {
	case SecuritySchemeAPIKey:
		if s.APIKey.Name == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".apiKeySecurityScheme.name",
				Description: "api key name is required",
			})
		}
		if !s.APIKey.Location.Valid() {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".apiKeySecurityScheme.location",
				Description: fmt.Sprintf("must be one of %q, %q or %q, got %q", APIKeyInQuery, APIKeyInHeader, APIKeyInCookie, string(s.APIKey.Location)),
			})
		}
	case SecuritySchemeHTTPAuth:
		if s.HTTPAuth.Scheme == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".httpAuthSecurityScheme.scheme",
				Description: "http auth scheme is required",
			})
		}
	case SecuritySchemeOAuth2:
		// flows is a proto oneof: zero arms is unconfigured, two or more is a
		// document that cannot be represented on the wire at all.
		if n := s.OAuth2.Flows.set(); n != 1 {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".oauth2SecurityScheme.flows",
				Description: fmt.Sprintf("exactly one oauth2 flow is required, got %d", n),
			})
		}
	case SecuritySchemeOpenIDConnect:
		if s.OpenIDConnect.OpenIdConnectURL == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".openIdConnectSecurityScheme.openIdConnectUrl",
				Description: "openid connect discovery url is required",
			})
		}
	}
	return nil
}
