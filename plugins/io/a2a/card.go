package a2a

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// servedCard is a rendered Agent Card: the validated document plus the bytes
// and ETag the handler serves. It is built once in Init because nothing that
// feeds it changes at runtime, and because a card that fails validation should
// fail the boot rather than the first client request.
type servedCard struct {
	card a2a.AgentCard
	body []byte
	etag string
}

// buildCard assembles the served Agent Card from the hand-authored half and the
// derived half.
//
// THE SPLIT, and why it is not negotiable:
//
//   - Hand-authored (config): name, description, version, provider, icon,
//     documentation, default modes, and SKILLS. These are a public contract. The
//     skills in particular are deliberately NOT derived from the skills plugin or
//     the tool catalog: an internal catalog churns with every plugin an operator
//     enables, and a discovery document that churns with it would leak internal
//     structure and break clients that keyed off it.
//   - Derived (this function): supportedInterfaces, capabilities, and
//     securitySchemes/securityRequirements. These describe what the listener
//     actually does, and an operator cannot be allowed to state one that is
//     false. A card naming a URL nothing is bound to, a capability nothing
//     implements, or a scheme nothing enforces is worse than no card: it is a
//     confident wrong answer.
//
// The derived half therefore OVERWRITES whatever the card source carried,
// including a complete card_file document.
func buildCard(cfg *config) (*servedCard, error) {
	card := cfg.card

	card.SupportedInterfaces = []a2a.AgentInterface{
		// Ordered by preference (specification section 8.3.1). JSON-RPC leads
		// because it is the binding with the widest client support today.
		{
			URL:             cfg.publicURL + cfg.jsonrpcPath,
			ProtocolBinding: a2a.BindingJSONRPC,
			ProtocolVersion: a2a.ProtocolVersion,
		},
		{
			URL:             cfg.publicURL + cfg.restPrefix,
			ProtocolBinding: a2a.BindingHTTPJSON,
			ProtocolVersion: a2a.ProtocolVersion,
		},
	}

	// Capabilities are derived from what is actually wired, never configured.
	// implementedOperations is empty in this story, so the card honestly reports
	// streaming as false; it flips on its own when the operations land.
	card.Capabilities.Streaming = operationImplemented(a2a.MethodSendStreamingMessage) ||
		operationImplemented(a2a.MethodSubscribeToTask)
	card.Capabilities.PushNotifications = operationImplemented(a2a.MethodCreateTaskPushNotificationConfig)
	card.Capabilities.ExtendedAgentCard = operationImplemented(a2a.MethodGetExtendedAgentCard)

	schemes, requirements := deriveSecurity(cfg.validators)
	card.SecuritySchemes = schemes
	card.SecurityRequirements = requirements

	if err := a2a.ValidateAgentCard(&card); err != nil {
		return nil, fmt.Errorf("%s: agent card is not servable: %w", pluginID, err)
	}

	body, err := a2a.EncodeAgentCard(&card)
	if err != nil {
		return nil, fmt.Errorf("%s: encoding agent card: %w", pluginID, err)
	}

	// Section 8.6.1 asks for an ETag derived from the card version or a hash of
	// its content. The content hash is the stronger of the two: an operator who
	// edits a skill description without bumping card.version still gets a new
	// validator, so a cached client is not pinned to a stale document.
	sum := sha256.Sum256(body)
	return &servedCard{
		card: card,
		body: body,
		etag: `"` + hex.EncodeToString(sum[:16]) + `"`,
	}, nil
}

// deriveSecurity maps the configured validator chain onto the card's
// securitySchemes and securityRequirements.
//
// Each validator becomes one named scheme and one requirement entry. Separate
// entries — rather than one entry naming every scheme — is the accurate
// translation of a nexusauth.Chain: the chain is first-success, so satisfying
// ANY validator suffices, and A2A spells "any of these alternatives" as
// separate members of the securityRequirements array.
//
// Scheme names are the chain-order names nexusauth already assigned ("static",
// "jwks", "jwks#2"). Reusing them means a client's card and an operator's boot
// log name the same thing, and uniqueness comes for free.
func deriveSecurity(validators []validatorDescriptor) (map[string]a2a.SecurityScheme, []a2a.SecurityRequirement) {
	if len(validators) == 0 {
		// No validators means authentication is disabled. An empty
		// securitySchemes map is the honest card for that: there is nothing for
		// a client to present. It is also why the bind address defaults to
		// loopback — an unauthenticated A2A endpoint has no business being
		// reachable.
		return nil, nil
	}

	schemes := make(map[string]a2a.SecurityScheme, len(validators))
	var requirements []a2a.SecurityRequirement
	for _, v := range validators {
		scheme, ok := schemeFor(v)
		if !ok {
			continue
		}
		schemes[v.name] = scheme
		requirements = append(requirements, a2a.NewSecurityRequirement(v.name))
	}
	if len(schemes) == 0 {
		return nil, nil
	}
	return schemes, requirements
}

// schemeFor renders one validator as an A2A security scheme, reporting false
// when the validator accepts nothing a client can present.
func schemeFor(v validatorDescriptor) (a2a.SecurityScheme, bool) {
	switch v.typ {
	case nexusauth.ValidatorTypeStatic:
		return a2a.BearerScheme("", "Shared bearer token issued out-of-band by the operator of this agent."), true

	case nexusauth.ValidatorTypeJWKS:
		desc := "OIDC JWT access token, verified against the issuer's published key set."
		if issuer := stringOption(v.options, "issuer"); issuer != "" {
			desc += " Issuer: " + issuer + "."
		}
		if audience := joinOption(v.options, "audience"); audience != "" {
			desc += " Audience: " + audience + "."
		}
		return a2a.BearerScheme("JWT", desc), true

	case nexusauth.ValidatorTypeIntrospect:
		// No bearerFormat: an introspected token is opaque by construction, and
		// claiming a format would tell a client to inspect bytes it cannot read.
		desc := "Opaque bearer token, verified by RFC 7662 introspection at the issuer."
		return a2a.BearerScheme("", desc), true

	case nexusauth.ValidatorTypeProxyHeaders:
		// Deliberately unadvertised. This validator does not accept a client
		// credential at all: it honours an identity a trusted fronting proxy
		// already established, and it refuses those headers from anyone outside
		// the CIDR allowlist. Publishing a scheme for it would instruct clients
		// to send a header that is guaranteed to be ignored — or worse, invite
		// them to try asserting an identity directly.
		return a2a.SecurityScheme{}, false

	default:
		// An unknown validator type is a nexusauth addition this mapping has not
		// caught up with. Advertising a guess would be a lie; omitting it means
		// the card understates what is accepted, which is the safe direction.
		return a2a.SecurityScheme{}, false
	}
}

// stringOption reads a string validator option.
func stringOption(options map[string]any, key string) string {
	if v, ok := options[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// joinOption renders a string-or-list validator option for a description.
func joinOption(options map[string]any, key string) string {
	return strings.Join(optStringList(options[key]), ", ")
}
