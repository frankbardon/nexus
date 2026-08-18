package a2a

import (
	"encoding/json"
	"fmt"
)

// Agent Card discovery types (specification section 8). The card is the
// self-describing manifest an agent publishes at AgentCardPath. It tells a
// client who the agent is, which bindings it exposes and at what URLs, how to
// authenticate, what it can do, and which protocol extensions it speaks.
//
// The card is served as plain JSON, not as ContentTypeJSON: it is fetched by
// ordinary HTTP clients before any A2A negotiation has happened.

// ContentTypeAgentCard is the media type of a served Agent Card.
const ContentTypeAgentCard = "application/json"

// TransportProtocol names one of the three normative A2A bindings, using the
// ProtoJSON enum spelling.
type TransportProtocol string

// Transport protocols an AgentInterface may advertise.
const (
	TransportUnspecified TransportProtocol = "TRANSPORT_PROTOCOL_UNSPECIFIED"
	TransportJSONRPC     TransportProtocol = "TRANSPORT_PROTOCOL_JSONRPC"
	TransportGRPC        TransportProtocol = "TRANSPORT_PROTOCOL_GRPC"
	TransportHTTPJSON    TransportProtocol = "TRANSPORT_PROTOCOL_HTTP_JSON"
)

// Valid reports whether the transport is one of the three addressable bindings.
func (t TransportProtocol) Valid() bool {
	switch t {
	case TransportJSONRPC, TransportGRPC, TransportHTTPJSON:
		return true
	default:
		return false
	}
}

// AgentCard is the agent's public manifest, served at AgentCardPath.
type AgentCard struct {
	// ProtocolVersion is the Major.Minor A2A version the agent speaks. Required.
	ProtocolVersion string `json:"protocolVersion"`
	// Identity is who the agent is. Required.
	Identity AgentIdentity `json:"identity"`
	// Capabilities are the optional protocol features the agent supports.
	// Required, and always serialized in full so a client never has to infer a
	// capability from an absent field.
	Capabilities AgentCapabilities `json:"capabilities"`
	// SecuritySchemes declares, by name, every authentication scheme the agent
	// accepts. Names are referenced from Security and from AgentSkill.Security.
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	// Security lists the scheme requirement sets that apply agent-wide. Each
	// entry is one alternative: satisfying any entry suffices, and within an
	// entry every named scheme must be satisfied. The value is the scope list
	// the scheme requires, empty for schemes without scopes.
	Security []map[string][]string `json:"security,omitempty"`
	// Interfaces are the protocol endpoints the agent exposes, one per binding
	// and URL. Required, and must be non-empty.
	Interfaces []AgentInterface `json:"interfaces"`
	// Skills describe what the agent can do. Required, and must be non-empty.
	Skills []AgentSkill `json:"skills"`
	// DefaultInputModes are the media types the agent accepts in message parts
	// when a skill does not narrow them.
	DefaultInputModes []string `json:"defaultInputModes,omitempty"`
	// DefaultOutputModes are the media types the agent produces when a skill
	// does not narrow them.
	DefaultOutputModes []string `json:"defaultOutputModes,omitempty"`
	// Signatures carry JWS detached signatures over the card, for clients that
	// verify card provenance.
	Signatures []AgentCardSignature `json:"signatures,omitempty"`
	// Metadata is free-form custom data attached to the card.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// AgentIdentity is the who of an agent card.
type AgentIdentity struct {
	// Name is the human-readable agent name. Required.
	Name string `json:"name"`
	// Description is a human-readable summary of what the agent does.
	Description string `json:"description,omitempty"`
	// Version is the agent's own version, independent of the protocol version.
	// Required.
	Version string `json:"version"`
	// DocumentationURL points at human-readable documentation.
	DocumentationURL string `json:"documentationUrl,omitempty"`
	// IconURL points at an icon representing the agent.
	IconURL string `json:"iconUrl,omitempty"`
	// Provider identifies the organization operating the agent.
	Provider *AgentProvider `json:"provider,omitempty"`
}

// AgentProvider identifies the organization behind an agent.
type AgentProvider struct {
	// Organization is the provider's name. Required when a provider is given.
	Organization string `json:"organization"`
	// URL is the provider's website.
	URL string `json:"url,omitempty"`
}

// AgentCapabilities declares the optional protocol features an agent supports.
//
// The three booleans are serialized unconditionally rather than with omitempty:
// a discovery document should state a capability as false rather than leave a
// client to infer it from an absent key. The zero value is therefore the
// correct, honest card for an agent that supports none of them.
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
// (specification section 8.4). Clients opt in per request via the
// A2A-Extensions service parameter.
type AgentExtension struct {
	// URI uniquely identifies the extension. Required.
	URI string `json:"uri"`
	// Version is the extension's own version.
	Version string `json:"version,omitempty"`
	// Description is a human-readable summary of what the extension adds.
	Description string `json:"description,omitempty"`
	// Required reports whether a client must support the extension to interact
	// with this agent at all. Optional extensions are the norm; a required one
	// lets an agent refuse clients that would misread its output.
	Required bool `json:"required,omitempty"`
	// Metadata is extension-defined configuration data, whose shape the
	// extension itself specifies.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// AgentInterface binds one transport protocol to one endpoint URL. An agent
// exposing both the JSON-RPC and REST bindings declares two interfaces.
type AgentInterface struct {
	// Transport is the binding spoken at this URL. Required.
	Transport TransportProtocol `json:"transport"`
	// URL is the base URL of the endpoint. Operation paths are appended to it
	// for the REST binding; the JSON-RPC binding POSTs to it directly. Required.
	URL string `json:"url"`
}

// AgentSkill describes one capability the agent advertises.
type AgentSkill struct {
	// ID uniquely identifies the skill within the card. Required.
	ID string `json:"id"`
	// Name is the human-readable skill name. Required.
	Name string `json:"name"`
	// Description explains what the skill does. Required.
	Description string `json:"description"`
	// Tags are free-form labels for discovery and filtering.
	Tags []string `json:"tags,omitempty"`
	// Examples are sample prompts that exercise the skill.
	Examples []string `json:"examples,omitempty"`
	// InputModes narrows the card's default input media types for this skill.
	InputModes []string `json:"inputModes,omitempty"`
	// OutputModes narrows the card's default output media types for this skill.
	OutputModes []string `json:"outputModes,omitempty"`
	// Security narrows the card's agent-wide security requirements for this
	// skill, for skills that need stronger credentials than the agent as a
	// whole.
	Security []map[string][]string `json:"security,omitempty"`
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

// ---- Security schemes ----

// SecuritySchemeKind names which arm of a SecurityScheme oneof is set.
type SecuritySchemeKind string

// Security scheme kinds. The 1.0 revision removed the OAuth2 implicit and
// password grants; they have no representation here on purpose.
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
	// OAuth2 is an OAuth 2.0 scheme with one or more supported flows.
	OAuth2 *OAuth2SecurityScheme `json:"oauth2SecurityScheme,omitempty"`
	// OpenIDConnect is OpenID Connect Discovery.
	OpenIDConnect *OpenIDConnectSecurityScheme `json:"openIdConnectSecurityScheme,omitempty"`
	// MutualTLS is client-certificate authentication.
	MutualTLS *MutualTLSSecurityScheme `json:"mtlsSecurityScheme,omitempty"`
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
type APIKeySecurityScheme struct {
	// Name is the header, parameter or cookie name. Required.
	Name string `json:"name"`
	// In is where the credential travels. Required.
	In APIKeyLocation `json:"in"`
	// Description is a human-readable note about the scheme.
	Description string `json:"description,omitempty"`
}

// HTTPAuthSecurityScheme is an HTTP Authorization scheme.
type HTTPAuthSecurityScheme struct {
	// Scheme is the HTTP auth scheme name, lowercase per RFC 7235: "bearer",
	// "basic". Required.
	Scheme string `json:"scheme"`
	// BearerFormat hints at the bearer token's format, e.g. "JWT".
	BearerFormat string `json:"bearerFormat,omitempty"`
	// Description is a human-readable note about the scheme.
	Description string `json:"description,omitempty"`
}

// OAuth2SecurityScheme is an OAuth 2.0 scheme.
type OAuth2SecurityScheme struct {
	// Flows are the supported grant flows. At least one is required.
	Flows OAuthFlows `json:"flows"`
	// OAuth2MetadataURL points at the RFC 8414 authorization server metadata
	// document, letting a client discover endpoints rather than hardcode them.
	OAuth2MetadataURL string `json:"oauth2MetadataUrl,omitempty"`
	// Description is a human-readable note about the scheme.
	Description string `json:"description,omitempty"`
}

// OAuthFlows enumerates the OAuth 2.0 grant flows an agent supports. The
// implicit and resource-owner-password grants were removed in A2A 1.0 and are
// deliberately absent.
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

// AuthorizationCodeOAuthFlow is the authorization code grant.
type AuthorizationCodeOAuthFlow struct {
	// AuthorizationURL is the authorization endpoint. Required.
	AuthorizationURL string `json:"authorizationUrl"`
	// TokenURL is the token endpoint. Required.
	TokenURL string `json:"tokenUrl"`
	// RefreshURL is the refresh endpoint, when it differs from TokenURL.
	RefreshURL string `json:"refreshUrl,omitempty"`
	// Scopes maps scope name to human-readable description.
	Scopes map[string]string `json:"scopes,omitempty"`
}

// ClientCredentialsOAuthFlow is the machine-to-machine grant.
type ClientCredentialsOAuthFlow struct {
	// TokenURL is the token endpoint. Required.
	TokenURL string `json:"tokenUrl"`
	// RefreshURL is the refresh endpoint, when it differs from TokenURL.
	RefreshURL string `json:"refreshUrl,omitempty"`
	// Scopes maps scope name to human-readable description.
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
	// Scopes maps scope name to human-readable description.
	Scopes map[string]string `json:"scopes,omitempty"`
}

// OpenIDConnectSecurityScheme is OpenID Connect Discovery.
type OpenIDConnectSecurityScheme struct {
	// OpenIDConnectURL is the OIDC discovery document URL. Required.
	OpenIDConnectURL string `json:"openIdConnectUrl"`
	// Description is a human-readable note about the scheme.
	Description string `json:"description,omitempty"`
}

// MutualTLSSecurityScheme is client-certificate authentication.
type MutualTLSSecurityScheme struct {
	// Description is a human-readable note about the scheme.
	Description string `json:"description,omitempty"`
}

// ---- Constructors ----

// NewAgentCard builds a card stamped with this codec's protocol version and
// with the capability booleans explicitly at their conservative defaults:
// streaming on, push notifications and extended card off.
func NewAgentCard(identity AgentIdentity) AgentCard {
	return AgentCard{
		ProtocolVersion: ProtocolVersion,
		Identity:        identity,
		Capabilities: AgentCapabilities{
			Streaming:         true,
			PushNotifications: false,
			ExtendedAgentCard: false,
		},
	}
}

// WithInterface appends a protocol endpoint and returns the card, for chaining.
func (c AgentCard) WithInterface(transport TransportProtocol, url string) AgentCard {
	c.Interfaces = append(c.Interfaces, AgentInterface{Transport: transport, URL: url})
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
	if scopes == nil {
		scopes = []string{}
	}
	c.Security = append(c.Security, map[string][]string{name: scopes})
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
		Scheme:       "bearer",
		BearerFormat: bearerFormat,
		Description:  description,
	}}
}

// APIKeyScheme builds an API-key security scheme.
func APIKeyScheme(name string, in APIKeyLocation, description string) SecurityScheme {
	return SecurityScheme{APIKey: &APIKeySecurityScheme{Name: name, In: in, Description: description}}
}

// OpenIDConnectScheme builds an OpenID Connect Discovery security scheme.
func OpenIDConnectScheme(discoveryURL, description string) SecurityScheme {
	return SecurityScheme{OpenIDConnect: &OpenIDConnectSecurityScheme{
		OpenIDConnectURL: discoveryURL,
		Description:      description,
	}}
}

// MutualTLSScheme builds a client-certificate security scheme.
func MutualTLSScheme(description string) SecurityScheme {
	return SecurityScheme{MutualTLS: &MutualTLSSecurityScheme{Description: description}}
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

// InterfaceFor returns the endpoint URL the agent exposes for a transport.
func (c AgentCard) InterfaceFor(transport TransportProtocol) (string, bool) {
	for _, i := range c.Interfaces {
		if i.Transport == transport {
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
func EncodeAgentCard(c *AgentCard) ([]byte, error) {
	data, err := json.MarshalIndent(c, "", "  ")
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
	if c.ProtocolVersion == "" {
		violations = append(violations, FieldViolation{
			Field:       "protocolVersion",
			Description: "protocol version is required",
		})
	} else if _, err := NormalizeVersion(c.ProtocolVersion); err != nil {
		violations = append(violations, FieldViolation{Field: "protocolVersion", Description: err.Error()})
	}
	if c.Identity.Name == "" {
		violations = append(violations, FieldViolation{Field: "identity.name", Description: "agent name is required"})
	}
	if c.Identity.Version == "" {
		violations = append(violations, FieldViolation{Field: "identity.version", Description: "agent version is required"})
	}
	if c.Identity.Provider != nil && c.Identity.Provider.Organization == "" {
		violations = append(violations, FieldViolation{
			Field:       "identity.provider.organization",
			Description: "provider organization is required",
		})
	}
	if len(c.Interfaces) == 0 {
		violations = append(violations, FieldViolation{
			Field:       "interfaces",
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

	for i, iface := range c.Interfaces {
		if !iface.Transport.Valid() {
			return ErrInvalidParams(FieldViolation{
				Field:       fmt.Sprintf("interfaces[%d].transport", i),
				Description: fmt.Sprintf("unknown transport protocol %q", string(iface.Transport)),
			})
		}
		if iface.URL == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       fmt.Sprintf("interfaces[%d].url", i),
				Description: "interface url is required",
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

	if err := validateSecurityRequirements(c.Security, "security", c.SecuritySchemes); err != nil {
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
	return validateSecurityRequirements(s.Security, field+".security", schemes)
}

func validateSecurityRequirements(reqs []map[string][]string, field string, schemes map[string]SecurityScheme) *Error {
	for i, req := range reqs {
		for name := range req {
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
		if !s.APIKey.In.Valid() {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".apiKeySecurityScheme.in",
				Description: fmt.Sprintf("must be one of %q, %q or %q, got %q", APIKeyInQuery, APIKeyInHeader, APIKeyInCookie, string(s.APIKey.In)),
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
		if s.OAuth2.Flows.Empty() {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".oauth2SecurityScheme.flows",
				Description: "at least one oauth2 flow is required",
			})
		}
	case SecuritySchemeOpenIDConnect:
		if s.OpenIDConnect.OpenIDConnectURL == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       field + ".openIdConnectSecurityScheme.openIdConnectUrl",
				Description: "openid connect discovery url is required",
			})
		}
	}
	return nil
}
