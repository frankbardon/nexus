// Package nexusheaders is the single definition of the X-Nexus-* request
// header convention: the one way a client of a web-facing Nexus IO transport
// passes per-request out-of-band context — tenant, locale, timezone, an
// upstream authorization subject, a trace correlator — to the plugins running
// inside the engine.
//
// # Why a convention and not a config key per header
//
// The alternative is every transport growing its own allowlist of header
// names, and every deployment editing four YAML blocks to add one field. A
// reserved prefix costs nothing to extend: a caller that wants to pass
// `X-Nexus-Timezone` just sends it, and every transport already carries it.
// The prefix is what keeps that open-endedness safe — it is a namespace no
// standard or proxy-injected header occupies, so "forward everything in it"
// cannot accidentally forward an Authorization, a Cookie or an
// X-Forwarded-For.
//
// # These values are client-controlled
//
// Extract normalizes and bounds them; it does NOT authenticate them. A header
// says only what the caller typed. Identity verification is pkg/nexusauth's
// job, and a header is never a substitute for it: `X-Nexus-Tenant: acme` is a
// hint about which tenant the caller CLAIMS, useful once the credential the
// request also carried has been validated, and worthless on its own. For the
// same reason the engine binds them into the reserved (host-only, prompt-
// invisible) session-label namespace rather than the general one — see
// engine.SessionWorkspace.SetRequestHeaders — so reaching an LLM prompt is a
// deliberate opt-in (nexus.system.dynvars' request_headers allowlist) rather
// than the default.
package nexusheaders

import (
	"net/http"
	"sort"
	"strings"
)

// Prefix is the header-name prefix that marks a request header as Nexus
// out-of-band context. Matching is case-insensitive, as HTTP field names are.
const Prefix = "X-Nexus-"

// Bounds on what Extract will carry out of one request. They exist because
// every extracted header is persisted into session metadata and may be
// rendered into a prompt: an unbounded map would let a caller inflate the
// session file and the context window at will.
//
// Exceeding a bound drops entries rather than failing the request. A header
// is supplementary context by construction — refusing an otherwise valid turn
// because the caller sent a 33rd one would trade a real request for a
// cosmetic rule. Dropping is deterministic (see Extract) so the same request
// always yields the same map.
const (
	// MaxCount is the greatest number of X-Nexus-* headers carried from one
	// request.
	MaxCount = 32
	// MaxNameBytes bounds one normalized name.
	MaxNameBytes = 128
	// MaxValueBytes bounds one value.
	MaxValueBytes = 4096
	// MaxTotalBytes bounds the sum of all names and values kept.
	MaxTotalBytes = 16384
)

// Name normalizes one HTTP field name to the key plugins see, reporting
// whether the field belongs to the X-Nexus-* namespace at all.
//
// `X-Nexus-Tenant-ID` becomes `tenant-id`: the prefix is stripped and the
// remainder lowercased, so a caller's capitalization never changes the key a
// plugin reads. A bare `X-Nexus-` with nothing after it is not a name and is
// rejected, as is any remainder carrying a character outside the set a
// session-label key and a prompt line can both hold verbatim.
func Name(field string) (string, bool) {
	if len(field) <= len(Prefix) || !strings.EqualFold(field[:len(Prefix)], Prefix) {
		return "", false
	}
	name := strings.ToLower(field[len(Prefix):])
	if name == "" || len(name) > MaxNameBytes {
		return "", false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return "", false
		}
	}
	return name, true
}

// Extract returns every X-Nexus-* header of h keyed by its normalized name,
// or nil when the request carries none. The result is safe to retain: it
// shares nothing with h.
//
// Repeated field lines are joined with ", ", the combination RFC 9110 §5.3
// already defines for a list-valued field, so a caller that sends one header
// twice and a caller that sends one comma-joined header are indistinguishable
// to a plugin — which is what the RFC promises them.
//
// Over-bound input is dropped in a defined order: names are sorted, then kept
// while MaxCount and MaxTotalBytes allow. Sorting first is what makes the
// dropping deterministic — the same request yields the same map on every run
// and in a replay, instead of whichever entries Go's map iteration happened to
// reach first.
func Extract(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}

	type entry struct {
		name  string
		value string
	}
	var entries []entry
	for field, values := range h {
		name, ok := Name(field)
		if !ok {
			continue
		}
		value := sanitize(strings.Join(values, ", "))
		if len(value) > MaxValueBytes {
			value = value[:MaxValueBytes]
		}
		entries = append(entries, entry{name: name, value: value})
	}
	if len(entries) == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	out := make(map[string]string, len(entries))
	total := 0
	for _, e := range entries {
		if len(out) >= MaxCount {
			break
		}
		size := len(e.name) + len(e.value)
		if total+size > MaxTotalBytes {
			break
		}
		total += size
		out[e.name] = e.value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sanitize strips the control characters a header value should never carry.
//
// Go's HTTP server already rejects a CR or LF inside a field value, so this is
// not the response-splitting guard it would be in a language without that
// protection. It matters for where these values GO: a session label rendered
// into a system prompt, where an embedded newline would let a caller forge a
// line break in a block the agent reads as structure. Tab survives because it
// is ordinary whitespace; everything below 0x20, plus DEL, does not.
func sanitize(v string) string {
	if strings.IndexFunc(v, isControl) < 0 {
		return strings.TrimSpace(v)
	}
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		if isControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func isControl(r rune) bool {
	return (r < 0x20 && r != '\t') || r == 0x7f
}
