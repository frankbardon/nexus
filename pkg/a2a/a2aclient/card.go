package a2aclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// SchemeInfo is one security scheme a remote declares, flattened to the two
// facts a caller needs before it can choose a credential: the name the card
// keys it under (which is what a security requirement references) and which
// kind of scheme it is. The full declaration is carried along for a credential
// source that needs the flow parameters.
type SchemeInfo struct {
	// Name is the card's key for this scheme.
	Name string
	// Kind is the scheme arm that is set.
	Kind a2a.SecuritySchemeKind
	// Scheme is the full declaration.
	Scheme a2a.SecurityScheme
}

// Capabilities is a flat summary of what a remote's Agent Card promises. It
// exists so a caller can answer "can I stream to this agent, what must I
// authenticate with, and which bindings does it expose" without walking the
// card itself.
type Capabilities struct {
	// Name, Description and Version identify the agent.
	Name        string
	Description string
	Version     string

	// Streaming reports whether the agent accepts SendStreamingMessage and
	// SubscribeToTask.
	Streaming bool
	// PushNotifications reports whether the agent supports push-notification
	// configs. This client implements no push operation, so the flag is
	// informational.
	PushNotifications bool
	// ExtendedAgentCard reports whether the agent serves an authenticated
	// extended card.
	ExtendedAgentCard bool

	// Bindings are the protocol bindings the card exposes an interface for, in
	// card order, which is preference order.
	Bindings []a2a.ProtocolBinding
	// Interfaces are the raw declarations, for a caller that needs the URL or
	// the per-interface protocol version.
	Interfaces []a2a.AgentInterface

	// SecuritySchemes are the declared schemes, sorted by name for a stable
	// presentation.
	SecuritySchemes []SchemeInfo
	// SecurityRequirements are the card-level requirements: each entry is one
	// alternative, and satisfying any one of them is enough.
	SecurityRequirements []a2a.SecurityRequirement

	// Extensions are the extension URIs the card declares.
	Extensions []string
	// RequiredExtensions are the subset a client MUST support to interact at
	// all (specification section 3.5): calling this agent without them will be
	// refused.
	RequiredExtensions []string

	// SkillIDs are the ids of the skills the card advertises.
	SkillIDs []string
}

// SupportsBinding reports whether the card exposes an interface for a binding.
func (c Capabilities) SupportsBinding(b a2a.ProtocolBinding) bool {
	for _, have := range c.Bindings {
		if have == b {
			return true
		}
	}
	return false
}

// Scheme returns the declared security scheme with the given card key.
func (c Capabilities) Scheme(name string) (SchemeInfo, bool) {
	for _, s := range c.SecuritySchemes {
		if s.Name == name {
			return s, true
		}
	}
	return SchemeInfo{}, false
}

// RequiresAuth reports whether the card declares any security requirement. A
// card with schemes but no requirements is describing optional credentials.
func (c Capabilities) RequiresAuth() bool { return len(c.SecurityRequirements) > 0 }

// Card returns the remote's Agent Card, fetching it on first use and caching it
// for the life of the client. A card supplied with WithCard is returned as-is
// and never fetched.
//
// It is the discovery entry point: nothing else in this package needs to be
// called first, and every operation resolves the card implicitly if it has to.
func (c *Client) Card(ctx context.Context) (*a2a.AgentCard, error) {
	c.mu.Lock()
	cached := c.card
	c.mu.Unlock()
	if cached != nil {
		if err := c.offerCard(ctx, cached); err != nil {
			return nil, err
		}
		return cached, nil
	}

	card, err := c.fetchCard(ctx)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	// Another goroutine may have won the race; prefer the cached value so every
	// caller sees one card.
	if c.card == nil {
		c.card = card
	}
	card = c.card
	c.mu.Unlock()

	if err := c.offerCard(ctx, card); err != nil {
		return nil, err
	}
	return card, nil
}

// Capabilities resolves the card and summarizes it.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	card, err := c.Card(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	return SummarizeCard(card), nil
}

// SummarizeCard flattens an Agent Card into its capability summary. It is
// exported so a caller holding a card obtained elsewhere can inspect it the
// same way.
func SummarizeCard(card *a2a.AgentCard) Capabilities {
	if card == nil {
		return Capabilities{}
	}
	caps := Capabilities{
		Name:                 card.Name,
		Description:          card.Description,
		Version:              card.Version,
		Streaming:            card.Capabilities.Streaming,
		PushNotifications:    card.Capabilities.PushNotifications,
		ExtendedAgentCard:    card.Capabilities.ExtendedAgentCard,
		Interfaces:           card.SupportedInterfaces,
		SecurityRequirements: card.SecurityRequirements,
		RequiredExtensions:   card.RequiredExtensions(),
	}

	seen := map[a2a.ProtocolBinding]bool{}
	for _, iface := range card.SupportedInterfaces {
		if seen[iface.ProtocolBinding] {
			continue
		}
		seen[iface.ProtocolBinding] = true
		caps.Bindings = append(caps.Bindings, iface.ProtocolBinding)
	}

	for name, scheme := range card.SecuritySchemes {
		caps.SecuritySchemes = append(caps.SecuritySchemes, SchemeInfo{
			Name: name, Kind: scheme.Kind(), Scheme: scheme,
		})
	}
	sort.Slice(caps.SecuritySchemes, func(i, j int) bool {
		return caps.SecuritySchemes[i].Name < caps.SecuritySchemes[j].Name
	})

	for _, ext := range card.Capabilities.Extensions {
		caps.Extensions = append(caps.Extensions, ext.URI)
	}
	for _, skill := range card.Skills {
		caps.SkillIDs = append(caps.SkillIDs, skill.ID)
	}
	return caps
}

// CardURL returns the well-known Agent Card URL for the configured base.
func (c *Client) CardURL() string {
	if c.base == nil {
		return ""
	}
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + a2a.AgentCardPath
	return u.String()
}

// fetchCard performs the well-known GET and decodes the result. Every failure
// mode is reported as a *CardError naming the stage, because "the remote's card
// is wrong" is an operator problem and the operator needs to know which part.
func (c *Client) fetchCard(ctx context.Context) (*a2a.AgentCard, error) {
	cardURL := c.CardURL()
	if cardURL == "" {
		return nil, &CardError{
			Stage: "fetch",
			Err:   fmt.Errorf("no base url configured; supply one to New or pass a card with WithCard"),
		}
	}

	reqCtx, cancel := withTimeout(ctx, c.requestTimeout)
	defer cancel()

	body, _, err := c.doJSON(reqCtx, httpCall{
		operation:  "AgentCard",
		method:     http.MethodGet,
		url:        cardURL,
		accept:     a2a.ContentTypeAgentCard,
		idempotent: true,
		// The card is plain JSON fetched before any A2A negotiation.
	})
	if err != nil {
		ce := &CardError{URL: cardURL, Stage: "fetch", Err: err}
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			ce.Stage = "status"
			ce.StatusCode = httpErr.StatusCode
		}
		return nil, ce
	}

	card, decErr := a2a.DecodeAgentCard(body)
	if decErr != nil {
		return nil, &CardError{URL: cardURL, Stage: "decode", Err: decErr}
	}
	if c.validateCard {
		if verr := a2a.ValidateAgentCard(card); verr != nil {
			return nil, &CardError{URL: cardURL, Stage: "validate", Err: verr}
		}
	}
	return card, nil
}

// offerCard hands the resolved card to a card-aware credential source, once.
func (c *Client) offerCard(ctx context.Context, card *a2a.AgentCard) error {
	aware, ok := c.creds.(CardAwareCredentialSource)
	if !ok {
		return nil
	}

	c.mu.Lock()
	already := c.credsCarded
	c.credsCarded = true
	c.mu.Unlock()
	if already {
		return nil
	}

	if err := aware.UseCard(ctx, card); err != nil {
		c.mu.Lock()
		c.credsCarded = false
		c.mu.Unlock()
		return fmt.Errorf("a2aclient: configure credentials from agent card: %w", err)
	}
	return nil
}

// endpoint resolves the URL for the active binding, consulting the Agent Card
// unless an endpoint was pinned.
//
// The card's interfaces are in preference order, so the first match wins. An
// interface declaring a protocol version this client does not speak is skipped
// rather than used: 0.3 and 1.0 disagree about method names and the shape of a
// Part, so talking 1.0 at a 0.3 endpoint produces confusing failures rather
// than a clean refusal. An interface that declares no version at all is
// accepted, since a version is the only thing that could disqualify it.
func (c *Client) endpoint(ctx context.Context) (string, error) {
	if pinned := c.pinnedEndpoint(); pinned != "" {
		return pinned, nil
	}

	card, err := c.Card(ctx)
	if err != nil {
		return "", err
	}

	var versionMismatch string
	for _, iface := range card.SupportedInterfaces {
		if iface.ProtocolBinding != c.binding {
			continue
		}
		if declared := strings.TrimSpace(iface.ProtocolVersion); declared != "" {
			normalized, verr := a2a.NormalizeVersion(declared)
			if verr != nil || !a2a.IsVersionSupported(normalized) {
				versionMismatch = declared
				continue
			}
		}
		resolved, rerr := c.resolveEndpointURL(iface.URL)
		if rerr != nil {
			return "", rerr
		}
		return resolved, nil
	}

	if versionMismatch != "" {
		return "", &BindingError{
			Binding: string(c.binding),
			Detail: fmt.Sprintf("card exposes it at protocol version %s; this client speaks %s",
				versionMismatch, strings.Join(a2a.SupportedVersions(), ", ")),
			Err: ErrNoEndpoint,
		}
	}
	return "", &BindingError{
		Binding: string(c.binding),
		Detail:  fmt.Sprintf("agent card at %s exposes no interface for it", c.CardURL()),
		Err:     ErrNoEndpoint,
	}
}

// resolveEndpointURL makes a card-declared interface URL absolute. Cards are
// supposed to carry absolute URLs, but a relative one against the base is
// unambiguous and refusing it would be pedantry.
func (c *Client) resolveEndpointURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", &BindingError{
			Binding: string(c.binding),
			Detail:  "agent card declares an interface with an empty url",
			Err:     ErrNoEndpoint,
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", &BindingError{
			Binding: string(c.binding),
			Detail:  fmt.Sprintf("agent card declares an unparseable interface url %q", raw),
			Err:     err,
		}
	}
	if !u.IsAbs() {
		if c.base == nil {
			return "", &BindingError{
				Binding: string(c.binding),
				Detail:  fmt.Sprintf("agent card declares a relative interface url %q and no base url is configured", raw),
				Err:     ErrNoEndpoint,
			}
		}
		u = c.base.ResolveReference(u)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// requireStreaming refuses a streaming call to an agent whose card says it does
// not stream. The check applies only when a card is available: a client with a
// pinned endpoint and no card has nothing to check against and is trusted to
// know what it configured.
func (c *Client) requireStreaming(ctx context.Context, operation string) error {
	if c.pinnedEndpoint() != "" {
		c.mu.Lock()
		card := c.card
		c.mu.Unlock()
		if card == nil {
			return nil
		}
		if card.Capabilities.Streaming {
			return nil
		}
		return streamingUnsupported(c.binding, operation)
	}

	card, err := c.Card(ctx)
	if err != nil {
		return err
	}
	if card.Capabilities.Streaming {
		return nil
	}
	return streamingUnsupported(c.binding, operation)
}

func streamingUnsupported(binding a2a.ProtocolBinding, operation string) error {
	return &BindingError{
		Binding:   string(binding),
		Operation: operation,
		Detail:    "the agent card declares capabilities.streaming = false",
		Err:       a2a.ErrUnsupportedOperation(operation),
	}
}
