package a2a

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// Config keys. The auth keys are the same two spellings nexus.io.agui accepts,
// deliberately: an operator who has already secured one Nexus listener should
// not have to learn a second vocabulary to secure this one.
const (
	cfgKeyBind                = "bind"
	cfgKeyPublicURL           = "public_url"
	cfgKeyJSONRPCPath         = "jsonrpc_path"
	cfgKeyRESTPrefix          = "rest_prefix"
	cfgKeyStrictVersionHeader = "strict_version_header"
	cfgKeyCardRequiresAuth    = "card_requires_auth"
	cfgKeyCORSOrigins         = "cors_origins"
	cfgKeyCard                = "card"
	cfgKeyCardFile            = "card_file"

	cfgKeyBearerToken    = "bearer_token"
	cfgKeyBearerTokenEnv = "bearer_token_env"
	cfgKeyAuth           = "auth"

	cfgKeyTasks              = "tasks"
	cfgKeyTasksTTL           = "ttl"
	cfgKeyTasksMaxPerContext = "max_per_context"
	cfgKeyTasksInputTimeout  = "input_timeout"

	cfgKeyArtifacts            = "artifacts"
	cfgKeyArtifactsMaxFile     = "max_file_bytes"
	cfgKeyArtifactsMaxToolOut  = "max_tool_output_bytes"
	cfgKeyArtifactsMaxTask     = "max_task_bytes"
	cfgKeyArtifactsFileBaseDir = "file_base_dir"
	cfgKeyArtifactsFileSources = "file_sources"
)

// Defaults. The bind address is loopback for the same reason nexus.io.agui's
// is: an A2A endpoint speaks for a live agent session, and nothing that
// consequential should reach the network because an operator forgot to narrow
// a default.
const (
	defaultBindAddr    = "127.0.0.1:8091"
	defaultJSONRPCPath = "/a2a"
	defaultRESTPrefix  = "/a2a/v1"
)

// Task-retention defaults.
//
// Retention is load-bearing rather than housekeeping: every task carries its
// status history and its artifacts, and the artifact side grows without bound
// once tool results and inline file parts are recorded, so an unbounded store
// would grow with traffic rather than with the conversation.
//
// defaultTaskTTL is 24 hours. A task is only useful to a client that still holds
// its id, and an A2A client that has been away for a day has restarted, retried
// or given up — GetTask on a day-old task answers a question nobody is asking.
// A day is also comfortably longer than any plausible reconnect window, so the
// TTL never expires a task a client is actually still following.
//
// defaultTasksPerContext is 200. A standalone listener serves one context and
// runs one task per turn, so this is 200 turns of history: far more than a
// client polls back over, small enough that the worst case — every task carrying
// artifacts — stays in the low megabytes, and large enough that an ordinary
// working session never reaches it. Both knobs are configurable; zero disables
// either one.
const (
	defaultTaskTTL         = 24 * time.Hour
	defaultTasksPerContext = 200
)

// defaultInputTimeout bounds how long a task may sit at
// TASK_STATE_INPUT_REQUIRED before it is abandoned.
//
// THE POLICY, and why it has a default at all:
//
// A parked task is not idle — it holds this listener's single active-task slot
// and the process's one agent loop, because the turn that asked the question is
// blocked inside ask_user waiting for an answer. A question nobody answers
// would therefore pin the whole instance, and pkg/a2a's parked-stream contract
// explicitly hands the deadline decision to the serving layer rather than
// letting one be inherited by accident. So there is always a deadline, and it
// is stated rather than implied.
//
// 15 minutes is the number, and it is chosen against a human, not a machine: a
// question routed to a person has to survive being paged, read, thought about
// and answered, which minutes-not-seconds covers and seconds does not. It is
// also short enough that an abandoned question frees the instance inside one
// coffee break rather than at the next restart.
//
// On expiry the task is driven to FAILED — a real terminal transition, written
// through to the store and closing every attached stream — and hitl.cancel is
// emitted so the waiting agent loop unblocks. "0s" disables the deadline for an
// operator who genuinely wants a task parked until the process exits; the cost
// of that choice is documented, not hidden.
const defaultInputTimeout = 15 * time.Minute

// config is the resolved plugin configuration.
type config struct {
	bindAddr    string
	publicURL   string
	jsonrpcPath string
	restPrefix  string

	// strictVersionHeader selects the absent-A2A-Version policy. See
	// assumedVersion for the decision and its reasoning.
	strictVersionHeader bool

	// cardRequiresAuth gates the discovery document behind the same validator
	// chain as the operations. See the comment on the constant default below.
	cardRequiresAuth bool

	corsOrigins []string

	// card is the hand-authored half of the Agent Card: identity, skills,
	// modes, provider, extensions. Interfaces, capabilities and security are
	// derived and overwrite whatever this carries — see buildCard.
	card a2a.AgentCard

	chain *nexusauth.Chain
	// validators describes the configured chain in card terms, so the served
	// securitySchemes cannot drift from what is actually enforced.
	validators []validatorDescriptor

	// retention bounds the durable task store. See defaultTaskTTL.
	retention retention

	// inputTimeout bounds a task parked at INPUT_REQUIRED. Zero disables the
	// deadline. See defaultInputTimeout.
	inputTimeout time.Duration

	// artifacts bounds what a turn is allowed to publish. See artifacts.go.
	artifacts artifactPolicy
}

// validatorDescriptor is the minimum a security-scheme derivation needs to know
// about one configured validator: what it is called and what it verifies.
type validatorDescriptor struct {
	// name is the chain-order name nexusauth assigned ("static", "jwks#2").
	name string
	// typ is the nexusauth validator type.
	typ string
	// options are the remaining type-specific config keys, used only to enrich
	// a scheme description (issuer, audience). Never a secret: the values that
	// could be one (client_secret) are not read here.
	options map[string]any
}

// assumedVersion returns the protocol version this listener reads an absent
// A2A-Version service parameter as.
//
// THE POLICY, and why it is not the specification's literal default:
//
// Section 3.6.2 says an agent MUST interpret an empty A2A-Version as 0.3. That
// rule exists to protect clients that predate the parameter: an agent that used
// to serve 0.3 and later added 1.0 must not silently reinterpret an old
// client's requests under new semantics. pkg/a2a implements it literally, and
// since this codec does not speak 0.3, the literal reading turns every
// header-less request into a VersionNotSupportedError.
//
// This listener has no such client to protect. It has never served 0.3, its
// Agent Card advertises protocolVersion 1.0 on every interface, and 1.0 is the
// only version any of its URLs has ever answered. So a request arriving without
// the header is not a 0.3 client — there are none — it is a 1.0 client whose
// HTTP layer omitted a header. Refusing it buys no compatibility and costs
// interop, so the default assumption is 1.0, and every response echoes the
// A2A-Version it was actually processed under so the client can see what it
// got rather than infer it.
//
// Operators who need the letter of 3.6.2 — a conformance harness, or a
// deployment that will later front a 0.3 interface from the same origin — set
// strict_version_header: true and get the literal behaviour back.
func (c *config) assumedVersion() string {
	if c.strictVersionHeader {
		return a2a.DefaultVersion
	}
	return a2a.ProtocolVersion
}

// parseConfig resolves the plugin's YAML configuration.
func parseConfig(raw map[string]any) (*config, error) {
	c := &config{
		bindAddr:    defaultBindAddr,
		jsonrpcPath: defaultJSONRPCPath,
		restPrefix:  defaultRESTPrefix,
		retention: retention{
			ttl:           defaultTaskTTL,
			maxPerContext: defaultTasksPerContext,
		},
		inputTimeout: defaultInputTimeout,
		artifacts:    defaultArtifactPolicy(),
	}

	if v, ok := raw[cfgKeyBind].(string); ok && strings.TrimSpace(v) != "" {
		c.bindAddr = strings.TrimSpace(v)
	}
	if v, ok := raw[cfgKeyPublicURL].(string); ok && strings.TrimSpace(v) != "" {
		c.publicURL = strings.TrimRight(strings.TrimSpace(v), "/")
	} else {
		// The card must carry absolute URLs, so an unset public_url is filled
		// from the bind address. That is right for the loopback default and
		// wrong the moment a reverse proxy is involved, which is exactly when
		// an operator sets it.
		c.publicURL = "http://" + c.bindAddr
	}
	if v, ok := raw[cfgKeyJSONRPCPath].(string); ok && strings.TrimSpace(v) != "" {
		c.jsonrpcPath = strings.TrimSpace(v)
	}
	if v, ok := raw[cfgKeyRESTPrefix].(string); ok && strings.TrimSpace(v) != "" {
		c.restPrefix = strings.TrimSpace(v)
	}
	c.jsonrpcPath = strings.TrimRight(c.jsonrpcPath, "/")
	c.restPrefix = strings.TrimRight(c.restPrefix, "/")
	if !strings.HasPrefix(c.jsonrpcPath, "/") || c.jsonrpcPath == "" {
		return nil, fmt.Errorf("%s: %s must be an absolute path such as %q", pluginID, cfgKeyJSONRPCPath, defaultJSONRPCPath)
	}
	if !strings.HasPrefix(c.restPrefix, "/") || c.restPrefix == "" {
		return nil, fmt.Errorf("%s: %s must be an absolute path such as %q", pluginID, cfgKeyRESTPrefix, defaultRESTPrefix)
	}
	if c.jsonrpcPath == c.restPrefix {
		return nil, fmt.Errorf("%s: %s and %s must differ; both are mounted on the same listener",
			pluginID, cfgKeyJSONRPCPath, cfgKeyRESTPrefix)
	}

	if v, ok := raw[cfgKeyStrictVersionHeader].(bool); ok {
		c.strictVersionHeader = v
	}
	if v, ok := raw[cfgKeyCardRequiresAuth].(bool); ok {
		c.cardRequiresAuth = v
	}
	c.corsOrigins = parseCORSOrigins(raw[cfgKeyCORSOrigins])

	policy, inputTimeout, err := parseTasks(raw[cfgKeyTasks], c.retention, c.inputTimeout)
	if err != nil {
		return nil, err
	}
	c.retention = policy
	c.inputTimeout = inputTimeout

	artifacts, err := parseArtifacts(raw[cfgKeyArtifacts], c.artifacts)
	if err != nil {
		return nil, err
	}
	c.artifacts = artifacts

	chain, descriptors, err := resolveAuth(raw)
	if err != nil {
		return nil, err
	}
	c.chain = chain
	c.validators = descriptors

	card, err := resolveCard(raw)
	if err != nil {
		return nil, err
	}
	c.card = card

	return c, nil
}

// parseTasks resolves the `tasks:` block into a retention policy and the input
// deadline, starting from the supplied defaults so an absent block, or a block
// that sets only one knob, leaves the others at their defaults.
//
// A duration is a duration STRING ("24h", "90m"), never a bare number: "ttl: 600"
// reads as ten minutes to an operator and six hundred nanoseconds to Go, and
// silently picking either would be worse than an error naming the key. This is
// the same rule pkg/nexusauth applies to its own duration keys.
func parseTasks(raw any, defaults retention, defaultInput time.Duration) (retention, time.Duration, error) {
	out := defaults
	input := defaultInput
	if raw == nil {
		return out, input, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return out, input, fmt.Errorf("%s: %s: want a mapping, got %T", pluginID, cfgKeyTasks, raw)
	}

	if v, present := m[cfgKeyTasksTTL]; present && v != nil {
		d, err := taskDuration(v, cfgKeyTasksTTL,
			"use \"0s\" to keep tasks for the life of the session")
		if err != nil {
			return out, input, err
		}
		out.ttl = d
	}

	if v, present := m[cfgKeyTasksInputTimeout]; present && v != nil {
		d, err := taskDuration(v, cfgKeyTasksInputTimeout,
			"use \"0s\" to let a task stay parked until the process exits")
		if err != nil {
			return out, input, err
		}
		input = d
	}

	if v, present := m[cfgKeyTasksMaxPerContext]; present && v != nil {
		n, err := configInt(v)
		if err != nil {
			return out, input, fmt.Errorf("%s: %s.%s: %w", pluginID, cfgKeyTasks, cfgKeyTasksMaxPerContext, err)
		}
		if n < 0 {
			return out, input, fmt.Errorf("%s: %s.%s: must not be negative; use 0 to keep every task",
				pluginID, cfgKeyTasks, cfgKeyTasksMaxPerContext)
		}
		out.maxPerContext = n
	}
	return out, input, nil
}

// taskDuration reads one non-negative duration key from the `tasks:` block.
func taskDuration(v any, key, zeroHint string) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("%s: %s.%s: want a duration string such as \"24h\", got %T",
			pluginID, cfgKeyTasks, key, v)
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%s: %s.%s: %w", pluginID, cfgKeyTasks, key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: %s.%s: must not be negative; %s", pluginID, cfgKeyTasks, key, zeroHint)
	}
	return d, nil
}

// configInt reads an integer that YAML may have decoded as an int or a float.
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
		return 0, fmt.Errorf("want a whole number, got %T", v)
	}
}

// resolveCard reads the hand-authored half of the Agent Card, from either the
// inline `card:` block or the `card_file:` document.
//
// The two are MUTUALLY EXCLUSIVE rather than merged. A card is a public
// contract; a field-level merge between a file and a config block means the
// document an operator reads in one place is not the document that gets served,
// and the divergence only shows up in someone else's client.
func resolveCard(raw map[string]any) (a2a.AgentCard, error) {
	inline, hasInline := raw[cfgKeyCard]
	if inline == nil {
		hasInline = false
	}
	filePath, _ := raw[cfgKeyCardFile].(string)
	filePath = strings.TrimSpace(filePath)

	switch {
	case hasInline && filePath != "":
		return a2a.AgentCard{}, fmt.Errorf("%s: %s and %s are mutually exclusive: the agent card has exactly one source",
			pluginID, cfgKeyCard, cfgKeyCardFile)
	case filePath != "":
		return loadCardFile(filePath)
	case hasInline:
		m, ok := inline.(map[string]any)
		if !ok {
			return a2a.AgentCard{}, fmt.Errorf("%s: %s: want a mapping, got %T", pluginID, cfgKeyCard, inline)
		}
		return cardFromMap(m)
	default:
		return a2a.AgentCard{}, fmt.Errorf("%s: one of %s or %s is required: an A2A server MUST publish an agent card, "+
			"and this plugin will not invent a name, description or skill list on an operator's behalf",
			pluginID, cfgKeyCard, cfgKeyCardFile)
	}
}

// loadCardFile reads a complete Agent Card JSON document from disk.
func loadCardFile(path string) (a2a.AgentCard, error) {
	expanded := engine.ExpandPath(path)
	data, err := os.ReadFile(expanded)
	if err != nil {
		return a2a.AgentCard{}, fmt.Errorf("%s: %s %q: %w", pluginID, cfgKeyCardFile, expanded, err)
	}
	card, err := a2a.DecodeAgentCard(data)
	if err != nil {
		return a2a.AgentCard{}, fmt.Errorf("%s: %s %q: %w", pluginID, cfgKeyCardFile, expanded, err)
	}
	return *card, nil
}

// cardFromMap builds the hand-authored half of a card from the inline `card:`
// block. Interfaces, capabilities and security are absent by design: they are
// derived from the listener's own configuration, so the block has no keys for
// them and an operator cannot state one that is false.
func cardFromMap(m map[string]any) (a2a.AgentCard, error) {
	var card a2a.AgentCard
	card.Name = optString(m, "name")
	card.Description = optString(m, "description")
	card.Version = optString(m, "version")
	card.DocumentationURL = optString(m, "documentation_url")
	card.IconURL = optString(m, "icon_url")
	card.DefaultInputModes = optStringList(m["default_input_modes"])
	card.DefaultOutputModes = optStringList(m["default_output_modes"])

	if rawProvider, ok := m["provider"]; ok && rawProvider != nil {
		pm, ok := rawProvider.(map[string]any)
		if !ok {
			return card, fmt.Errorf("%s: %s.provider: want a mapping, got %T", pluginID, cfgKeyCard, rawProvider)
		}
		card.Provider = &a2a.AgentProvider{
			Organization: optString(pm, "organization"),
			URL:          optString(pm, "url"),
		}
	}

	rawSkills, ok := m["skills"].([]any)
	if !ok {
		return card, fmt.Errorf("%s: %s.skills: want a non-empty list of skills, got %T", pluginID, cfgKeyCard, m["skills"])
	}
	for i, entry := range rawSkills {
		sm, ok := entry.(map[string]any)
		if !ok {
			return card, fmt.Errorf("%s: %s.skills[%d]: want a mapping, got %T", pluginID, cfgKeyCard, i, entry)
		}
		card.Skills = append(card.Skills, a2a.AgentSkill{
			ID:          optString(sm, "id"),
			Name:        optString(sm, "name"),
			Description: optString(sm, "description"),
			Tags:        optStringList(sm["tags"]),
			Examples:    optStringList(sm["examples"]),
			InputModes:  optStringList(sm["input_modes"]),
			OutputModes: optStringList(sm["output_modes"]),
		})
	}
	return card, nil
}

// resolveAuth turns the plugin's authentication config into a validator chain
// plus the descriptors the card derivation needs.
//
// Two spellings are accepted and they are MUTUALLY EXCLUSIVE, matching
// nexus.io.agui exactly:
//
//   - `bearer_token` / `bearer_token_env` — one shared secret, desugared into a
//     single `static` validator. Inline wins; the env var is consulted only when
//     there is no inline token.
//   - `auth:` — the full validator-chain block, parsed by the same
//     nexusauth.ParseConfig the session broker uses.
//
// Setting both is a boot error rather than a precedence rule: two sources for
// one security decision means one of them is stale, and quietly preferring
// either is how an operator comes to believe a credential was tightened when it
// was not.
func resolveAuth(raw map[string]any) (*nexusauth.Chain, []validatorDescriptor, error) {
	var inline, envVar string
	if v, ok := raw[cfgKeyBearerToken].(string); ok {
		inline = strings.TrimSpace(v)
	}
	if v, ok := raw[cfgKeyBearerTokenEnv].(string); ok {
		envVar = strings.TrimSpace(v)
	}

	rawAuth, hasAuth := raw[cfgKeyAuth]
	if rawAuth == nil {
		hasAuth = false
	}

	if hasAuth {
		// Presence of the legacy keys is what conflicts, not the value they
		// resolve to: bearer_token_env naming an unset variable is still an
		// operator saying "authenticate with a shared token".
		if inline != "" || envVar != "" {
			return nil, nil, fmt.Errorf("%s: %s and %s/%s are mutually exclusive: express the shared token as an %s validator with type: %s, or drop the %s block",
				pluginID, cfgKeyAuth, cfgKeyBearerToken, cfgKeyBearerTokenEnv,
				cfgKeyAuth, nexusauth.ValidatorTypeStatic, cfgKeyAuth)
		}
		m, ok := rawAuth.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s: %s: want a mapping, got %T", pluginID, cfgKeyAuth, rawAuth)
		}
		cfg, err := nexusauth.ParseConfig(m)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %s: %w", pluginID, cfgKeyAuth, err)
		}
		chain, err := nexusauth.BuildChain(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %s: %w", pluginID, cfgKeyAuth, err)
		}
		descriptors := make([]validatorDescriptor, 0, len(cfg.Validators))
		for _, vc := range cfg.Validators {
			descriptors = append(descriptors, validatorDescriptor{name: vc.Name, typ: vc.Type, options: vc.Options})
		}
		return chain, descriptors, nil
	}

	token := inline
	if token == "" && envVar != "" {
		token = strings.TrimSpace(os.Getenv(envVar))
	}
	if token == "" {
		return nexusauth.NewChain(), nil, nil
	}
	chain, err := staticChainFromToken(token)
	if err != nil {
		return nil, nil, err
	}
	// The desugared chain is one static validator, so the card advertises the
	// same bearer scheme it would have for an explicit `auth:` block.
	return chain, []validatorDescriptor{{
		name: nexusauth.ValidatorTypeStatic,
		typ:  nexusauth.ValidatorTypeStatic,
	}}, nil
}

// staticChainFromToken desugars the legacy bearer_token configuration into a
// one-entry static validator chain. Routing it through StaticValidator upgrades
// the comparison to constant time, which a hand-rolled == would not be.
func staticChainFromToken(token string) (*nexusauth.Chain, error) {
	v, err := nexusauth.NewStaticValidator([]nexusauth.StaticToken{{
		Token:     token,
		Principal: nexusauth.Principal{ID: legacyBearerPrincipal},
	}})
	if err != nil {
		return nil, fmt.Errorf("%s: desugaring %s into a static validator: %w", pluginID, cfgKeyBearerToken, err)
	}
	return nexusauth.NewChain(nexusauth.NamedValidator{
		Name:      nexusauth.ValidatorTypeStatic,
		Validator: v,
	}), nil
}

// optString reads an optional trimmed string key.
func optString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// optStringList accepts a YAML list of strings or a single string.
func optStringList(raw any) []string {
	var out []string
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case string:
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseCORSOrigins accepts a YAML list or a single comma-separated string,
// matching nexus.io.agui.
func parseCORSOrigins(raw any) []string {
	if s, ok := raw.(string); ok {
		var out []string
		for _, part := range strings.Split(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out
	}
	return optStringList(raw)
}
