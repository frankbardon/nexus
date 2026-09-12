package engine

import (
	"fmt"
	"strings"
)

// requestHeaderLabelPrefix namespaces the session labels that hold a turn's
// X-Nexus-* request headers. It sits inside the reserved ("_"-prefixed)
// namespace, which decides three things at once:
//
//  1. No plugin, and no agent holding the nexus.tool.session_tags tools, can
//     write or overwrite one through the general before:session.tag.set path
//     — installSessionTagHandlers rejects every reserved key unconditionally.
//     A header a transport bound is the transport's statement about the
//     request, not something anything downstream can forge.
//  2. They are invisible to the prompt by default. Every <session_context>
//     builder (react, orchestrator, subagent, ICM) filters reserved keys out,
//     so a client-controlled value never reaches an LLM just because it was
//     sent. Surfacing one is a deliberate opt-in — see nexus.system.dynvars'
//     request_headers allowlist.
//  3. They cannot collide with an agent's own session tags, whatever the
//     caller names a header.
//
// The trailing dot is what makes the namespace enumerable: RequestHeaders
// reads back exactly the labels under it and nothing else.
const requestHeaderLabelPrefix = reservedLabelPrefix + "header."

// RequestHeaderLabelKey returns the session-label key an X-Nexus-* header is
// bound under. name is the normalized header name nexusheaders.Name produces
// (`X-Nexus-Tenant-ID` -> `tenant-id` -> `_header.tenant-id`).
func RequestHeaderLabelKey(name string) string {
	return requestHeaderLabelPrefix + name
}

// RequestHeaderName reports whether key is a request-header session label and,
// when it is, the header name it carries. It is the inverse of
// RequestHeaderLabelKey.
func RequestHeaderName(key string) (string, bool) {
	if !strings.HasPrefix(key, requestHeaderLabelPrefix) {
		return "", false
	}
	name := key[len(requestHeaderLabelPrefix):]
	if name == "" {
		return "", false
	}
	return name, true
}

// SetRequestHeaders binds the X-Nexus-* headers of the request that opened the
// current turn as reserved session labels, REPLACING whatever the previous
// turn bound.
//
// # Why replace rather than merge
//
// These describe one request, not the session. A second turn arriving without
// `X-Nexus-Tenant` must not be read by a plugin as still carrying the first
// turn's tenant: a stale bind that looks exactly like a fresh one is the one
// failure mode worth designing against here, and it is why the previous set is
// cleared in the same write that lays down the new one. nexus.io.agui already
// applies the same reasoning to `_principal_id` — every run re-binds fresh,
// with no "unchanged, skip the write" case — and this follows it.
//
// Passing an empty map clears the namespace, which is the correct handling of
// a turn whose request carried no X-Nexus-* header at all.
//
// # Ordering
//
// Call this BEFORE emitting the turn's io.input, for the reason
// bindSessionContext documents in nexus.io.agui: a plugin that reads the
// headers while handling io.input must not be able to observe the turn before
// the context it belongs to. Both the clear and the bind land in one
// read-modify-write, so no consumer ever sees a half-replaced set on disk.
//
// Header names are expected to be normalized already (nexusheaders.Extract
// does this); a name carrying a "/" or a "=" is rejected rather than written,
// since a session-label key is also a manifest key and a prompt line.
func (s *SessionWorkspace) SetRequestHeaders(headers map[string]string) error {
	meta, err := s.SessionMetadata()
	if err != nil {
		return fmt.Errorf("reading session metadata: %w", err)
	}

	var stale []string
	for key := range meta.Labels {
		name, ok := RequestHeaderName(key)
		if !ok {
			continue
		}
		if _, kept := headers[name]; !kept {
			stale = append(stale, key)
		}
	}

	var set map[string]string
	if len(headers) > 0 {
		set = make(map[string]string, len(headers))
		for name, value := range headers {
			if name == "" || strings.ContainsAny(name, "/\\= \t\n") {
				return fmt.Errorf("session request headers: header name %q is not a usable label key", name)
			}
			set[RequestHeaderLabelKey(name)] = value
		}
	}

	return s.applyLabels(set, stale)
}

// RequestHeaders returns the X-Nexus-* headers bound for the current turn,
// keyed by normalized header name, or an empty map when none are bound.
//
// This is the read seam for a plugin that never sees io.input — a tool, a
// memory provider, an LLM provider — and it is why the headers are bound to
// the session at all rather than only carried on the event. Plugins that DO
// handle io.input can read events.UserInput.Headers instead and skip the
// metadata read.
func (s *SessionWorkspace) RequestHeaders() (map[string]string, error) {
	meta, err := s.SessionMetadata()
	if err != nil {
		return nil, fmt.Errorf("reading session metadata: %w", err)
	}
	out := make(map[string]string)
	for key, value := range meta.Labels {
		if name, ok := RequestHeaderName(key); ok {
			out[name] = value
		}
	}
	return out, nil
}

// RequestHeader returns one bound request header by normalized name, and
// whether it was present. It is the convenience form of RequestHeaders for the
// common single-lookup case (`session.RequestHeader("timezone")`).
func (s *SessionWorkspace) RequestHeader(name string) (string, bool) {
	meta, err := s.SessionMetadata()
	if err != nil {
		return "", false
	}
	value, ok := meta.Labels[RequestHeaderLabelKey(name)]
	return value, ok
}
