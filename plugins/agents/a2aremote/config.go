package a2aremote

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
)

// Config keys. The transport keys are deliberately named after the a2aclient
// options they set, because those options ARE this plugin's transport surface
// and inventing a second vocabulary for them would only create drift.
const (
	cfgKeyAgents    = "agents"
	cfgKeyCache     = "cache"
	cfgKeyCacheSize = "cache_size"
	cfgKeyMaxDepth  = "max_depth"

	cfgKeyName            = "name"
	cfgKeyBaseURL         = "base_url"
	cfgKeyJSONRPCEndpoint = "jsonrpc_endpoint"
	cfgKeyRESTEndpoint    = "rest_endpoint"
	cfgKeyToolName        = "tool_name"
	cfgKeyDescription     = "description"
	cfgKeyPosture         = "posture"

	cfgKeyBinding           = "binding"
	cfgKeyValidateCard      = "validate_card"
	cfgKeyStream            = "stream"
	cfgKeyTimeout           = "timeout"
	cfgKeyRequestTimeout    = "request_timeout"
	cfgKeyMessageTimeout    = "message_timeout"
	cfgKeyStreamOpenTimeout = "stream_open_timeout"
	cfgKeyStreamIdleTimeout = "stream_idle_timeout"
	cfgKeyExtensions        = "extensions"
	cfgKeyProgress          = "progress"

	cfgKeyHITL             = "hitl"
	cfgKeyHITLEnabled      = "enabled"
	cfgKeyHITLInputTimeout = "input_timeout"
	cfgKeyHITLMaxRounds    = "max_rounds"

	cfgKeyRetry            = "retry"
	cfgKeyRetryMaxAttempts = "max_attempts"
	cfgKeyRetryBaseDelay   = "base_delay"
	cfgKeyRetryMaxDelay    = "max_delay"
)

// Defaults.
//
// Every transport default is a2aclient's own, restated here rather than
// re-derived: an operator reading the configuration reference and an operator
// reading the client's documentation must see the same numbers.
const (
	defaultCacheSize = 128
	defaultMaxDepth  = 3

	// defaultCallTimeout bounds one whole delegated call — discovery, the
	// message, and the stream that answers it. It exists because none of the
	// client's own timeouts bounds the total: a remote that keeps a stream
	// alive with keep-alive comments while making no progress trips no idle
	// timeout, and a delegating agent cannot wait forever for a tool result.
	//
	// Five minutes is chosen against the work, not the transport: a remote
	// agent asked to do something worth delegating runs a multi-step loop with
	// its own tool calls, which minutes covers and seconds does not. A posture
	// budget or explicit config narrows it; see resolveTimeout.
	defaultCallTimeout = 5 * time.Minute

	// defaultInputTimeout bounds how long ONE remote question may wait for a
	// human before this plugin gives up on it.
	//
	// It is the outbound twin of nexus.io.a2a's tasks.input_timeout, and the
	// number is deliberately the same: a question routed to a person has to
	// survive being paged, read, thought about and answered, which
	// minutes-not-seconds covers. "0s" removes this deadline specifically.
	//
	// It is NOT the only deadline. The whole-call budget keeps running while a
	// task sits at INPUT_REQUIRED — a remote parked on a question is still work
	// this session started and is still accountable to the budget that
	// authorized it — so the effective wait is whichever of the two expires
	// first. With the 5m default call timeout that is the CALL budget, not this
	// key; an operator who expects a remote to ask questions raises `timeout`
	// as well. Both expiries are reported distinctly so a transcript says which
	// one fired.
	defaultInputTimeout = 15 * time.Minute

	// defaultMaxHITLRounds bounds how many times one delegated call may bounce
	// a question back to the human. A remote that answers every answer with
	// another question would otherwise turn a single tool call into an
	// unbounded interrogation. Zero removes the cap, leaving the call budget as
	// the only bound.
	defaultMaxHITLRounds = 4
)

// bindingNames maps the config spelling of a protocol binding onto the wire
// constant. The config spelling is lowercase because YAML keys and values in
// this repo are, and "HTTP+JSON" in a config file invites a quoting mistake.
var bindingNames = map[string]a2a.ProtocolBinding{
	"jsonrpc":   a2a.BindingJSONRPC,
	"json-rpc":  a2a.BindingJSONRPC,
	"http+json": a2a.BindingHTTPJSON,
	"rest":      a2a.BindingHTTPJSON,
}

// transport is the set of knobs that exist at both the plugin level (as
// defaults) and per agent (as overrides). Every field is a pointer so "unset"
// is distinguishable from "set to the zero value" — `stream: false` and
// `stream_idle_timeout: 0s` are both meaningful, and a non-pointer field could
// not carry them through the inheritance step.
type transport struct {
	binding           *a2a.ProtocolBinding
	validateCard      *bool
	stream            *bool
	timeout           *time.Duration
	requestTimeout    *time.Duration
	messageTimeout    *time.Duration
	streamOpenTimeout *time.Duration
	streamIdleTimeout *time.Duration
	extensions        []string
	retry             *a2aclient.RetryPolicy

	// progress gates republishing a remote run's incremental progress onto the
	// local bus. It is not an a2aclient option — it governs what this plugin
	// does with the frames it reads — but it inherits the same way, because
	// "this remote is too chatty for my TUI" is a per-remote decision.
	progress *bool

	// hitl is the chained human-in-the-loop policy for this remote.
	hitl hitlPolicy
}

// hitlPolicy is the chained human-in-the-loop policy: what happens when a
// remote parks its task at TASK_STATE_INPUT_REQUIRED.
//
// Like transport, every field is a pointer so an agent-level block that sets
// one knob leaves the others inherited, and `enabled: false` is
// distinguishable from unset.
type hitlPolicy struct {
	enabled      *bool
	inputTimeout *time.Duration
	maxRounds    *int
}

// inherit fills the receiver's unset fields from base.
func (h hitlPolicy) inherit(base hitlPolicy) hitlPolicy {
	out := h
	if out.enabled == nil {
		out.enabled = base.enabled
	}
	if out.inputTimeout == nil {
		out.inputTimeout = base.inputTimeout
	}
	if out.maxRounds == nil {
		out.maxRounds = base.maxRounds
	}
	return out
}

// on reports whether a remote question should be routed to a human. It is on by
// default: the alternative is handing the question to the delegating MODEL,
// which invites it to invent an answer only a person can give.
func (h hitlPolicy) on() bool { return h.enabled == nil || *h.enabled }

// wait is the dedicated deadline for one question. See defaultInputTimeout.
func (h hitlPolicy) wait() time.Duration {
	if h.inputTimeout == nil {
		return defaultInputTimeout
	}
	return *h.inputTimeout
}

// rounds is the cap on questions per delegated call, zero meaning uncapped.
func (h hitlPolicy) rounds() int {
	if h.maxRounds == nil {
		return defaultMaxHITLRounds
	}
	return *h.maxRounds
}

// inherit returns t with every field the receiver leaves unset filled from
// base. Slices inherit wholesale rather than merging: an operator narrowing the
// extension set for one agent means "these", not "these as well as those".
func (t transport) inherit(base transport) transport {
	out := t
	if out.binding == nil {
		out.binding = base.binding
	}
	if out.validateCard == nil {
		out.validateCard = base.validateCard
	}
	if out.stream == nil {
		out.stream = base.stream
	}
	if out.timeout == nil {
		out.timeout = base.timeout
	}
	if out.requestTimeout == nil {
		out.requestTimeout = base.requestTimeout
	}
	if out.messageTimeout == nil {
		out.messageTimeout = base.messageTimeout
	}
	if out.streamOpenTimeout == nil {
		out.streamOpenTimeout = base.streamOpenTimeout
	}
	if out.streamIdleTimeout == nil {
		out.streamIdleTimeout = base.streamIdleTimeout
	}
	if out.extensions == nil {
		out.extensions = base.extensions
	}
	if out.retry == nil {
		out.retry = base.retry
	}
	if out.progress == nil {
		out.progress = base.progress
	}
	out.hitl = out.hitl.inherit(base.hitl)
	return out
}

// options renders the resolved transport as a2aclient options.
func (t transport) options() []a2aclient.Option {
	var opts []a2aclient.Option
	if t.binding != nil {
		opts = append(opts, a2aclient.WithBinding(*t.binding))
	}
	if t.validateCard != nil {
		opts = append(opts, a2aclient.WithCardValidation(*t.validateCard))
	}
	if t.requestTimeout != nil {
		opts = append(opts, a2aclient.WithRequestTimeout(*t.requestTimeout))
	}
	if t.messageTimeout != nil {
		opts = append(opts, a2aclient.WithMessageTimeout(*t.messageTimeout))
	}
	if t.streamOpenTimeout != nil {
		opts = append(opts, a2aclient.WithStreamOpenTimeout(*t.streamOpenTimeout))
	}
	if t.streamIdleTimeout != nil {
		opts = append(opts, a2aclient.WithStreamIdleTimeout(*t.streamIdleTimeout))
	}
	if t.retry != nil {
		opts = append(opts, a2aclient.WithRetryPolicy(*t.retry))
	}
	if len(t.extensions) > 0 {
		opts = append(opts, a2aclient.WithExtensions(t.extensions...))
	}
	return opts
}

// streaming reports whether a call should use the streaming operation. Absent
// configuration it does: streaming is the operation that reports progress and
// carries an INPUT_REQUIRED park, and a blocking SendMessage is the narrower
// choice an operator opts into.
func (t transport) streaming() bool { return t.stream == nil || *t.stream }

// republishProgress reports whether a remote run's incremental progress should
// be mirrored onto the local bus. Absent configuration it is: a delegation the
// local transports cannot see is a black box, and that is the failure this
// exists to prevent.
func (t transport) republishProgress() bool { return t.progress == nil || *t.progress }

// callTimeout is the configured whole-call deadline, zero when unset.
func (t transport) callTimeout() time.Duration {
	if t.timeout == nil {
		return 0
	}
	return *t.timeout
}

// agentConfig is one configured remote, resolved.
type agentConfig struct {
	name        string
	toolName    string
	description string
	posture     string

	// credentials is the resolved `credentials:` block. It is per agent and
	// never inherited from the plugin level; see credentials.go.
	credentials credentialConfig

	baseURL         string
	jsonrpcEndpoint string
	restEndpoint    string

	transport transport
}

// config is the resolved plugin configuration.
type config struct {
	agents []agentConfig

	cacheEnabled bool
	cacheSize    int
	maxDepth     int

	defaults transport
}

// parseConfig resolves the plugin's YAML configuration. It contacts no remote:
// every agent is validated for shape only, so an unreachable agent is a runtime
// tool error rather than a boot failure. The one thing it reaches outside the
// config map for is a credential named by an `_env` key, which is resolved here
// so an unset variable fails boot rather than the first delegation.
func parseConfig(raw map[string]any) (*config, error) {
	c := &config{
		cacheEnabled: true,
		cacheSize:    defaultCacheSize,
		maxDepth:     defaultMaxDepth,
	}

	if v, ok := raw[cfgKeyCache].(bool); ok {
		c.cacheEnabled = v
	}
	if v, present := raw[cfgKeyCacheSize]; present && v != nil {
		n, err := configInt(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", pluginID, cfgKeyCacheSize, err)
		}
		if n < 0 {
			return nil, fmt.Errorf("%s: %s: must not be negative; use 0 to disable eviction", pluginID, cfgKeyCacheSize)
		}
		c.cacheSize = n
	}
	if v, present := raw[cfgKeyMaxDepth]; present && v != nil {
		n, err := configInt(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", pluginID, cfgKeyMaxDepth, err)
		}
		if n < 0 {
			return nil, fmt.Errorf("%s: %s: must not be negative; use 0 to disable the cap", pluginID, cfgKeyMaxDepth)
		}
		c.maxDepth = n
	}

	defaults, err := parseTransport(raw, "")
	if err != nil {
		return nil, err
	}
	// The Nexus extension is requested by DEFAULT, because this plugin is now
	// its consumer: a remote Nexus instance publishes its tool calls and its own
	// subagent progress only through the extension, and a client that does not
	// ask for it sees a delegation that is a black box from the first frame to
	// the last. Asking costs one service-parameter header; specification section
	// 8.4 requires a server to activate only the extensions it recognizes, and
	// this one declares itself optional, so a remote that has never heard of it
	// answers exactly as it would have. An operator who wants the header gone
	// sets `extensions: []`.
	if defaults.extensions == nil {
		defaults.extensions = []string{a2a.NexusExtensionURI}
	}
	c.defaults = defaults

	rawAgents, ok := raw[cfgKeyAgents].([]any)
	if !ok || len(rawAgents) == 0 {
		return nil, fmt.Errorf("%s: %s must be a non-empty list of remote A2A agents", pluginID, cfgKeyAgents)
	}
	seen := map[string]string{}
	for i, entry := range rawAgents {
		m, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: %s[%d]: want a mapping, got %T", pluginID, cfgKeyAgents, i, entry)
		}
		ac, err := parseAgent(m, i, c.defaults)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[ac.toolName]; dup {
			return nil, fmt.Errorf("%s: %s[%d]: agent %q derives tool name %q, already taken by agent %q; set %s to disambiguate",
				pluginID, cfgKeyAgents, i, ac.name, ac.toolName, prev, cfgKeyToolName)
		}
		seen[ac.toolName] = ac.name
		c.agents = append(c.agents, ac)
	}
	return c, nil
}

// parseAgent resolves one `agents[]` entry against the plugin-level defaults.
func parseAgent(m map[string]any, index int, defaults transport) (agentConfig, error) {
	where := fmt.Sprintf("%s[%d]", cfgKeyAgents, index)

	name, _ := m[cfgKeyName].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return agentConfig{}, fmt.Errorf("%s: %s: %s is required", pluginID, where, cfgKeyName)
	}
	where = fmt.Sprintf("%s[%d] (%s)", cfgKeyAgents, index, name)

	ac := agentConfig{name: name}
	ac.baseURL = strings.TrimSpace(stringOf(m[cfgKeyBaseURL]))
	ac.jsonrpcEndpoint = strings.TrimSpace(stringOf(m[cfgKeyJSONRPCEndpoint]))
	ac.restEndpoint = strings.TrimSpace(stringOf(m[cfgKeyRESTEndpoint]))
	ac.description = strings.TrimSpace(stringOf(m[cfgKeyDescription]))
	ac.posture = strings.TrimSpace(stringOf(m[cfgKeyPosture]))

	if ac.baseURL == "" && ac.jsonrpcEndpoint == "" && ac.restEndpoint == "" {
		return agentConfig{}, fmt.Errorf("%s: %s: %s is required unless %s or %s pins an endpoint",
			pluginID, where, cfgKeyBaseURL, cfgKeyJSONRPCEndpoint, cfgKeyRESTEndpoint)
	}

	if tn := strings.TrimSpace(stringOf(m[cfgKeyToolName])); tn != "" {
		ac.toolName = tn
	} else {
		suffix := sanitizeToolSuffix(name)
		if suffix == "" {
			return agentConfig{}, fmt.Errorf("%s: %s: %s yields no usable tool-name suffix; set %s explicitly",
				pluginID, where, cfgKeyName, cfgKeyToolName)
		}
		ac.toolName = toolNamePrefix + suffix
	}

	creds, err := parseCredentials(m, where)
	if err != nil {
		return agentConfig{}, err
	}
	ac.credentials = creds

	t, err := parseTransport(m, where)
	if err != nil {
		return agentConfig{}, err
	}
	ac.transport = t.inherit(defaults)
	return ac, nil
}

// parseTransport reads the transport knobs out of a mapping. where is the
// error-message prefix identifying the block; empty means the plugin-level
// block.
func parseTransport(m map[string]any, where string) (transport, error) {
	var t transport
	key := func(k string) string {
		if where == "" {
			return k
		}
		return where + "." + k
	}

	if v, present := m[cfgKeyBinding]; present && v != nil {
		spelling := strings.ToLower(strings.TrimSpace(stringOf(v)))
		binding, ok := bindingNames[spelling]
		if !ok {
			return t, fmt.Errorf("%s: %s: unknown binding %q; want one of %s",
				pluginID, key(cfgKeyBinding), spelling, strings.Join(bindingSpellings(), ", "))
		}
		t.binding = &binding
	}
	if v, ok := m[cfgKeyValidateCard].(bool); ok {
		t.validateCard = &v
	}
	if v, ok := m[cfgKeyStream].(bool); ok {
		t.stream = &v
	}
	if v, ok := m[cfgKeyProgress].(bool); ok {
		t.progress = &v
	}

	durations := []struct {
		key string
		dst **time.Duration
	}{
		{cfgKeyTimeout, &t.timeout},
		{cfgKeyRequestTimeout, &t.requestTimeout},
		{cfgKeyMessageTimeout, &t.messageTimeout},
		{cfgKeyStreamOpenTimeout, &t.streamOpenTimeout},
		{cfgKeyStreamIdleTimeout, &t.streamIdleTimeout},
	}
	for _, d := range durations {
		v, present := m[d.key]
		if !present || v == nil {
			continue
		}
		parsed, err := configDuration(v, key(d.key))
		if err != nil {
			return t, err
		}
		*d.dst = &parsed
	}

	if v, present := m[cfgKeyExtensions]; present && v != nil {
		list, err := stringList(v, key(cfgKeyExtensions))
		if err != nil {
			return t, err
		}
		// A non-nil empty slice is how "declare no extensions" overrides an
		// inherited set; inherit() only fills a nil.
		if list == nil {
			list = []string{}
		}
		t.extensions = list
	}

	if v, present := m[cfgKeyRetry]; present && v != nil {
		policy, err := parseRetry(v, key(cfgKeyRetry))
		if err != nil {
			return t, err
		}
		t.retry = &policy
	}

	if v, present := m[cfgKeyHITL]; present && v != nil {
		policy, err := parseHITL(v, key(cfgKeyHITL))
		if err != nil {
			return t, err
		}
		t.hitl = policy
	}
	return t, nil
}

// parseHITL resolves a `hitl:` block. Every key is optional and an absent one
// stays nil so it can inherit; the defaults are applied at read time by
// hitlPolicy's accessors rather than here, so a plugin-level block that sets
// only max_rounds does not freeze the other two at their defaults for every
// agent.
func parseHITL(raw any, where string) (hitlPolicy, error) {
	var out hitlPolicy
	m, ok := raw.(map[string]any)
	if !ok {
		return out, fmt.Errorf("%s: %s: want a mapping, got %T", pluginID, where, raw)
	}
	if v, ok := m[cfgKeyHITLEnabled].(bool); ok {
		out.enabled = &v
	}
	if v, present := m[cfgKeyHITLInputTimeout]; present && v != nil {
		d, err := configDuration(v, where+"."+cfgKeyHITLInputTimeout)
		if err != nil {
			return out, err
		}
		out.inputTimeout = &d
	}
	if v, present := m[cfgKeyHITLMaxRounds]; present && v != nil {
		n, err := configInt(v)
		if err != nil {
			return out, fmt.Errorf("%s: %s.%s: %w", pluginID, where, cfgKeyHITLMaxRounds, err)
		}
		if n < 0 {
			return out, fmt.Errorf("%s: %s.%s: must not be negative; use 0 to remove the cap",
				pluginID, where, cfgKeyHITLMaxRounds)
		}
		out.maxRounds = &n
	}
	return out, nil
}

// parseRetry resolves a `retry:` block, starting from the client's default
// policy so a block that sets one knob leaves the others alone.
func parseRetry(raw any, where string) (a2aclient.RetryPolicy, error) {
	policy := a2aclient.DefaultRetryPolicy()
	m, ok := raw.(map[string]any)
	if !ok {
		return policy, fmt.Errorf("%s: %s: want a mapping, got %T", pluginID, where, raw)
	}
	if v, present := m[cfgKeyRetryMaxAttempts]; present && v != nil {
		n, err := configInt(v)
		if err != nil {
			return policy, fmt.Errorf("%s: %s.%s: %w", pluginID, where, cfgKeyRetryMaxAttempts, err)
		}
		if n < 1 {
			return policy, fmt.Errorf("%s: %s.%s: must be at least 1 (1 disables retrying)",
				pluginID, where, cfgKeyRetryMaxAttempts)
		}
		policy.MaxAttempts = n
	}
	if v, present := m[cfgKeyRetryBaseDelay]; present && v != nil {
		d, err := configDuration(v, where+"."+cfgKeyRetryBaseDelay)
		if err != nil {
			return policy, err
		}
		policy.BaseDelay = d
	}
	if v, present := m[cfgKeyRetryMaxDelay]; present && v != nil {
		d, err := configDuration(v, where+"."+cfgKeyRetryMaxDelay)
		if err != nil {
			return policy, err
		}
		policy.MaxDelay = d
	}
	return policy, nil
}

// ---- Scalar helpers ----

// configDuration reads a duration key. A duration is a duration STRING ("90s",
// "5m"), never a bare number: "timeout: 600" reads as ten minutes to an
// operator and six hundred nanoseconds to Go, and silently picking either would
// be worse than an error naming the key. This is the rule nexus.io.a2a and
// pkg/nexusauth already apply.
func configDuration(v any, where string) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("%s: %s: want a duration string such as \"90s\" or \"5m\", got %T",
			pluginID, where, v)
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%s: %s: %q is not a duration: %w", pluginID, where, s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: %s: must not be negative; use \"0s\" to disable the deadline", pluginID, where)
	}
	return d, nil
}

// configInt reads an integer key out of YAML, which may decode a whole number
// as int, int64 or float64 depending on how it was written.
func configInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("want a whole number, got %v", n)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("want an integer, got %T", v)
	}
}

// stringOf returns v as a string, empty when it is not one.
func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// stringList reads a list-of-strings key.
func stringList(v any, where string) ([]string, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s: want a list of strings, got %T", pluginID, where, v)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s: %s[%d]: want a string, got %T", pluginID, where, i, item)
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// bindingSpellings lists the accepted binding values, sorted, for error text.
func bindingSpellings() []string {
	out := make([]string, 0, len(bindingNames))
	for k := range bindingNames {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sanitizeToolSuffix lowercases and collapses non-alphanumeric runs to '_' so a
// human-friendly agent name yields a valid tool identifier.
func sanitizeToolSuffix(s string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}
