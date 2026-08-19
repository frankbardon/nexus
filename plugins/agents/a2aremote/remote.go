package a2aremote

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
)

// toolNamePrefix is the fixed prefix of every tool this plugin registers. It
// names the transport on purpose: a model choosing between delegate_a2a_legal
// and delegate_agui_legal is choosing between two different remotes, and a
// prefix that hid the difference would make that ambiguous.
const toolNamePrefix = "delegate_a2a_"

// maxDescribedSkills bounds how many of a card's skills reach the tool
// description. A tool description is spent from the same context budget as the
// conversation, and a remote publishing thirty skills is describing a catalog,
// not a delegate target — the first few plus a count communicates as much for a
// fraction of the tokens.
const maxDescribedSkills = 8

// remote is one configured remote A2A agent: its resolved configuration, the
// transport client bound to it, and the Agent Card once discovery has
// succeeded.
//
// # Why the card is not fetched at boot
//
// A remote agent is somebody else's process. Fetching its card during Ready()
// would put a network round trip on the engine's startup path and would make a
// remote that is down — restarting, not deployed yet, behind a VPN the operator
// has not connected to — into a boot failure for the whole Nexus instance. That
// trade is backwards: the cost of a missing card is a slightly less specific
// tool description, and the cost of a failed boot is everything else the
// instance was going to do.
//
// So the card is resolved on FIRST USE. Until then the tool carries the
// operator's configured description (or a generic one derived from the agent's
// name), and the first successful call replaces it with a description built
// from the card's own skills. a2aclient caches a successful fetch for the life
// of the client and does not cache a failure, so a remote that comes up later
// resolves on the next call without any retry logic here.
type remote struct {
	cfg    agentConfig
	client *a2aclient.Client

	mu sync.Mutex
	// card is the resolved Agent Card, nil until discovery first succeeds.
	card *a2a.AgentCard
	// published is the description currently in the tool catalog. It exists so
	// the card-derived description is re-registered exactly once rather than on
	// every call.
	published string
}

// newRemote builds the client for one configured agent. It performs no I/O:
// a2aclient.New validates the URL and the binding and nothing else, so a
// misconfigured remote fails at boot and an unreachable one does not.
func newRemote(cfg agentConfig, creds a2aclient.CredentialSource) (*remote, error) {
	opts := cfg.transport.options()
	if cfg.jsonrpcEndpoint != "" {
		opts = append(opts, a2aclient.WithJSONRPCEndpoint(cfg.jsonrpcEndpoint))
	}
	if cfg.restEndpoint != "" {
		opts = append(opts, a2aclient.WithRESTEndpoint(cfg.restEndpoint))
	}
	if creds != nil {
		opts = append(opts, a2aclient.WithCredentials(creds))
	}

	client, err := a2aclient.New(cfg.baseURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("%s: agent %q: %w", pluginID, cfg.name, err)
	}
	return &remote{cfg: cfg, client: client}, nil
}

// discoverable reports whether this remote publishes a card this plugin can
// fetch. A remote configured with a pinned endpoint and no base URL does not:
// there is no well-known URL to fetch it from, and the operator who pinned the
// endpoint has already supplied what discovery would have told us.
func (a *remote) discoverable() bool { return a.client.BaseURL() != "" }

// resolveCard fetches the Agent Card, or returns the one already resolved.
// It returns (nil, nil) for a remote that publishes no fetchable card.
func (a *remote) resolveCard(ctx context.Context) (*a2a.AgentCard, error) {
	a.mu.Lock()
	cached := a.card
	a.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	if !a.discoverable() {
		return nil, nil
	}

	card, err := a.client.Card(ctx)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	if a.card == nil {
		a.card = card
	}
	card = a.card
	a.mu.Unlock()
	return card, nil
}

// cachedCard returns the resolved card without fetching one.
func (a *remote) cachedCard() *a2a.AgentCard {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.card
}

// streamingSupported reports whether a streaming call should be attempted:
// the operator asked for one AND the card, if we have one, does not say the
// remote refuses them. Without a card the configuration is trusted, which is
// what a2aclient does for a pinned endpoint too.
func (a *remote) streamingSupported() bool {
	if !a.cfg.transport.streaming() {
		return false
	}
	if card := a.cachedCard(); card != nil {
		return card.Capabilities.Streaming
	}
	return true
}

// description returns the tool description to publish, and whether it differs
// from the one already published. The second return value is what keeps the
// post-discovery refresh to a single re-registration.
func (a *remote) description() (string, bool) {
	desc := buildDescription(a.cfg, a.cachedCard())
	a.mu.Lock()
	defer a.mu.Unlock()
	if desc == a.published {
		return desc, false
	}
	a.published = desc
	return desc, true
}

// buildDescription renders the LLM-facing tool description.
//
// Precedence is card, then operator, then generic. The card wins because it is
// the remote's own account of what it does and it stays current as the remote
// changes; the operator's `description` is the fallback that carries the tool
// through the window before discovery has succeeded, and the generic form
// exists so a tool is never published with an empty description.
func buildDescription(cfg agentConfig, card *a2a.AgentCard) string {
	if card == nil {
		if cfg.description != "" {
			return cfg.description
		}
		return fmt.Sprintf(
			"Delegate a task to the remote A2A agent %q. Runs the remote agent to completion "+
				"and returns its final response and any artifacts it produced. The remote agent "+
				"has not been contacted yet, so its published capabilities are not known.",
			cfg.name)
	}

	var b strings.Builder
	label := strings.TrimSpace(card.Name)
	if label == "" {
		label = cfg.name
	}
	fmt.Fprintf(&b, "Delegate a task to the remote A2A agent %q", label)
	if v := strings.TrimSpace(card.Version); v != "" {
		fmt.Fprintf(&b, " (version %s)", v)
	}
	b.WriteString(". ")

	if summary := strings.TrimSpace(card.Description); summary != "" {
		b.WriteString(summary)
		if !strings.HasSuffix(summary, ".") {
			b.WriteByte('.')
		}
		b.WriteByte(' ')
	} else if cfg.description != "" {
		b.WriteString(cfg.description)
		b.WriteByte(' ')
	}

	if skills := describeSkills(card.Skills); skills != "" {
		b.WriteString("It advertises these skills:\n")
		b.WriteString(skills)
	}
	b.WriteString("\nRuns the remote agent to completion and returns its final response and any artifacts it produced.")
	return b.String()
}

// describeSkills renders a card's skills as a bounded bullet list.
func describeSkills(skills []a2a.AgentSkill) string {
	var b strings.Builder
	shown := 0
	for _, skill := range skills {
		name := strings.TrimSpace(skill.Name)
		if name == "" {
			name = strings.TrimSpace(skill.ID)
		}
		if name == "" {
			continue
		}
		if shown == maxDescribedSkills {
			fmt.Fprintf(&b, "- ... and %d more\n", len(skills)-shown)
			break
		}
		b.WriteString("- ")
		b.WriteString(name)
		if desc := strings.TrimSpace(skill.Description); desc != "" {
			b.WriteString(": ")
			b.WriteString(desc)
		}
		b.WriteByte('\n')
		shown++
	}
	return b.String()
}
