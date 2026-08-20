package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// servedAgentCard is one profile's rendered Agent Card: the validated document
// plus the bytes and ETag the handler serves.
//
// It is built once PER CONFIGURATION — at boot, and again on each accepted
// SIGHUP reload — and published inside the liveConfig snapshot alongside the
// configuration it was rendered from, so the whole card map is replaced in one
// atomic store rather than card by card.
//
// A card that fails validation fails the step that builds it: the BOOT, rather
// than the first client that fetches it, and on a reload the whole reload,
// rather than leaving the broker serving a half-replaced map.
type servedAgentCard struct {
	// profile is the name the card's routes are namespaced under.
	profile string

	// spec is the resolved `agents:` entry this card was rendered from — the
	// config path and binary a turn addressed to this profile will boot.
	//
	// It rides ON THE CARD rather than being looked up by name when a turn
	// starts, and that is what makes profiles reloadable safely: a request
	// resolves its card with one atomic load, and everything the turn needs
	// comes from that same resolved value. A SIGHUP that removes or repoints the
	// profile between the lookup and the spawn cannot leave the turn running the
	// wrong agent — or, worse, an empty AgentProfile a map miss would have
	// yielded.
	spec AgentProfile

	card a2a.AgentCard
	body []byte
	etag string
}

// buildAgentCard assembles one profile's Agent Card from the hand-authored half
// (the `card:` block) and the derived half (what this broker actually serves).
//
// THE SPLIT is `nexus.io.a2a`'s, deliberately unchanged: an operator authors
// identity and skills, and the server derives supportedInterfaces, capabilities
// and securitySchemes. The derived half OVERWRITES anything the config carried,
// because a card naming a URL nothing is bound to, a capability nothing
// implements, or a scheme nothing enforces is worse than no card at all — it is
// a confident wrong answer, and the client has no way to tell.
func buildAgentCard(profile string, spec AgentProfile, baseURL string, validators []nexusauth.ValidatorConfig) (*servedAgentCard, error) {
	card := cardFromSpec(spec.Card)

	// Ordered by preference (specification section 8.3.1), matching
	// nexus.io.a2a: JSON-RPC leads because it has the widest client support.
	//
	// Tenant is deliberately LEFT EMPTY on both interfaces, and this broker is
	// exactly the deployment shape that field exists for — several agents on one
	// origin — so the omission is a decision, not an oversight.
	// AgentInterface.Tenant disambiguates agents that share ONE endpoint URL,
	// telling a client which logical agent to address at it. Here they do not
	// share one: every profile publishes its own card naming its own URLs, and
	// the path segment already carries the routing. Populating tenant as well
	// would give a request two sources of truth for which agent it is for — the
	// URL it was sent to and a value echoed in its body — which the broker would
	// then have to reconcile, and would have to refuse when they disagreed.
	// One router, one answer.
	card.SupportedInterfaces = []a2a.AgentInterface{
		{
			URL:             baseURL + agentJSONRPCPath(profile),
			ProtocolBinding: a2a.BindingJSONRPC,
			ProtocolVersion: a2a.ProtocolVersion,
		},
		{
			URL:             baseURL + agentRESTPrefix(profile),
			ProtocolBinding: a2a.BindingHTTPJSON,
			ProtocolVersion: a2a.ProtocolVersion,
		},
	}

	// Capabilities are derived from what is actually wired, never configured, and
	// they track brokerImplementedOperations so the card cannot drift from the
	// dispatch.
	//
	// `streaming` is TRUE, and it is now true in the full sense rather than the
	// narrow one. SendStreamingMessage is dispatched (which is what this
	// expression reads) AND the ingress has a lifecycle behind it that produces a
	// real instance to stream from, so a client that acts on the advertisement
	// gets a live SSE stream of an agent turn rather than a refusal about missing
	// machinery. That second half is not expressible in this expression — the
	// provider is installed after the cards are rendered, and a card that flipped
	// a capability based on runtime wiring would change meaning between two
	// fetches — so it is guarded instead by the loud boot WARN in
	// A2AServer.logStartupState when no provider is wired, and by the end-to-end
	// streaming tests.
	//
	// `pushNotifications` and `extendedAgentCard` remain false: neither operation
	// is dispatched, and the matching refusal (UnsupportedOperationError) is what
	// a client is told if it tries.
	card.Capabilities.Streaming = brokerOperationImplemented(a2a.MethodSendStreamingMessage) ||
		brokerOperationImplemented(a2a.MethodSubscribeToTask)
	card.Capabilities.PushNotifications = brokerOperationImplemented(a2a.MethodCreateTaskPushNotificationConfig)
	card.Capabilities.ExtendedAgentCard = brokerOperationImplemented(a2a.MethodGetExtendedAgentCard)

	schemes, requirements := deriveCardSecurity(validators)
	card.SecuritySchemes = schemes
	card.SecurityRequirements = requirements

	if err := a2a.ValidateAgentCard(&card); err != nil {
		return nil, fmt.Errorf("agents: %s: agent card is not servable: %w", profile, err)
	}

	body, err := a2a.EncodeAgentCard(&card)
	if err != nil {
		return nil, fmt.Errorf("agents: %s: encoding agent card: %w", profile, err)
	}

	// Section 8.6.1 asks for an ETag derived from the card version or a hash of
	// its content. The content hash is the stronger of the two: an operator who
	// edits a skill description without bumping card.version still gets a new
	// validator, so a cached client is not pinned to a stale document.
	sum := sha256.Sum256(body)
	return &servedAgentCard{
		profile: profile,
		spec:    spec,
		card:    card,
		body:    body,
		etag:    `"` + hex.EncodeToString(sum[:16]) + `"`,
	}, nil
}

// cardFromSpec projects the hand-authored YAML block onto the card type. It
// carries no interfaces, capabilities or security: those have no keys, so an
// operator cannot state one that is false.
func cardFromSpec(spec AgentCardSpec) a2a.AgentCard {
	card := a2a.AgentCard{
		Name:               strings.TrimSpace(spec.Name),
		Description:        strings.TrimSpace(spec.Description),
		Version:            strings.TrimSpace(spec.Version),
		DocumentationURL:   strings.TrimSpace(spec.DocumentationURL),
		IconURL:            strings.TrimSpace(spec.IconURL),
		DefaultInputModes:  trimmedList(spec.DefaultInputModes),
		DefaultOutputModes: trimmedList(spec.DefaultOutputModes),
	}
	if spec.Provider != nil {
		card.Provider = &a2a.AgentProvider{
			Organization: strings.TrimSpace(spec.Provider.Organization),
			URL:          strings.TrimSpace(spec.Provider.URL),
		}
	}
	for _, skill := range spec.Skills {
		card.Skills = append(card.Skills, a2a.AgentSkill{
			ID:          strings.TrimSpace(skill.ID),
			Name:        strings.TrimSpace(skill.Name),
			Description: strings.TrimSpace(skill.Description),
			Tags:        trimmedList(skill.Tags),
			Examples:    trimmedList(skill.Examples),
			InputModes:  trimmedList(skill.InputModes),
			OutputModes: trimmedList(skill.OutputModes),
		})
	}
	return card
}

// trimmedList trims each entry of a YAML string list and drops the empties, so
// a stray blank list item cannot reach a served document.
func trimmedList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// deriveCardSecurity maps the broker's configured validator chain onto a card's
// securitySchemes and securityRequirements.
//
// This is `nexus.io.a2a`'s derivation, reused rather than reinvented: one named
// scheme and one requirement entry per validator. Separate entries — rather than
// one entry naming every scheme — is the accurate translation of a
// nexusauth.Chain, which is FIRST-SUCCESS: satisfying ANY validator suffices,
// and A2A spells "any of these alternatives" as separate members of the
// securityRequirements array.
//
// Scheme names are the chain-order names nexusauth already assigned ("static",
// "jwks", "jwks#2"), so a client's card and the operator's boot log name the
// same thing and uniqueness comes for free.
func deriveCardSecurity(validators []nexusauth.ValidatorConfig) (map[string]a2a.SecurityScheme, []a2a.SecurityRequirement) {
	if len(validators) == 0 {
		// No validators means the broker runs with no `auth:` block. An absent
		// securitySchemes map is the honest card for that: there is nothing for
		// a client to present, and the broker already logs one loud WARN at boot
		// saying anyone who can reach it can drive it.
		return nil, nil
	}

	schemes := make(map[string]a2a.SecurityScheme, len(validators))
	var requirements []a2a.SecurityRequirement
	for _, v := range validators {
		scheme, ok := cardSchemeFor(v)
		if !ok {
			continue
		}
		schemes[v.Name] = scheme
		requirements = append(requirements, a2a.NewSecurityRequirement(v.Name))
	}
	if len(schemes) == 0 {
		return nil, nil
	}
	return schemes, requirements
}

// cardSchemeFor renders one validator as an A2A security scheme, reporting false
// when the validator accepts nothing a client can present.
func cardSchemeFor(v nexusauth.ValidatorConfig) (a2a.SecurityScheme, bool) {
	switch v.Type {
	case nexusauth.ValidatorTypeStatic:
		return a2a.BearerScheme("", "Shared bearer token issued out-of-band by the operator of this broker."), true

	case nexusauth.ValidatorTypeJWKS:
		desc := "OIDC JWT access token, verified against the issuer's published key set."
		if issuer := cardStringOption(v.Options, "issuer"); issuer != "" {
			desc += " Issuer: " + issuer + "."
		}
		if audience := cardJoinOption(v.Options, "audience"); audience != "" {
			desc += " Audience: " + audience + "."
		}
		return a2a.BearerScheme("JWT", desc), true

	case nexusauth.ValidatorTypeIntrospect:
		// No bearerFormat: an introspected token is opaque by construction, and
		// claiming a format would tell a client to inspect bytes it cannot read.
		return a2a.BearerScheme("", "Opaque bearer token, verified by RFC 7662 introspection at the issuer."), true

	case nexusauth.ValidatorTypeProxyHeaders:
		// Deliberately unadvertised. This validator accepts no client credential
		// at all: it honours an identity a trusted fronting proxy already
		// established, and refuses those headers from anyone outside the CIDR
		// allowlist. Publishing a scheme for it would instruct clients to send a
		// header guaranteed to be ignored — or worse, invite them to try
		// asserting an identity directly.
		return a2a.SecurityScheme{}, false

	default:
		// An unknown validator type is a nexusauth addition this mapping has not
		// caught up with. Advertising a guess would be a lie; omitting it makes
		// the card understate what is accepted, which is the safe direction.
		return a2a.SecurityScheme{}, false
	}
}

// cardStringOption reads a string validator option.
func cardStringOption(options map[string]any, key string) string {
	if v, ok := options[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// cardJoinOption renders a string-or-list validator option for a scheme
// description.
func cardJoinOption(options map[string]any, key string) string {
	switch v := options[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return strings.Join(out, ", ")
	case []string:
		return strings.Join(trimmedList(v), ", ")
	default:
		return ""
	}
}
