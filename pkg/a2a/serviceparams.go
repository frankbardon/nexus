package a2a

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// A2A service parameter names (specification section 3.2.6). Over HTTP bindings
// these travel as request headers; header names are case-insensitive, and
// http.Header canonicalization handles that for us.
const (
	// HeaderVersion carries the Major.Minor protocol version the client speaks.
	HeaderVersion = "A2A-Version"
	// HeaderExtensions carries a comma-separated list of extension URIs the
	// client wants to use.
	HeaderExtensions = "A2A-Extensions"
)

// DefaultVersion is the version an agent must assume when the A2A-Version
// service parameter is absent or empty. The specification pins this to 0.3 for
// backwards compatibility with pre-1.0 clients (section 3.6.2).
const DefaultVersion = "0.3"

// supportedVersions are the Major.Minor protocol versions this codec accepts.
// Only 1.0 is implemented; 0.3 differs on method names and Part shape and is
// deliberately rejected rather than silently mishandled.
var supportedVersions = []string{ProtocolVersion}

// SupportedVersions returns the Major.Minor versions this codec accepts. The
// returned slice is a fresh copy.
func SupportedVersions() []string {
	out := make([]string, len(supportedVersions))
	copy(out, supportedVersions)
	return out
}

// ServiceParams are the A2A service parameters carried alongside a request.
type ServiceParams struct {
	// Version is the normalized Major.Minor protocol version. It is never empty
	// after parsing: an absent parameter yields DefaultVersion.
	Version string
	// Extensions are the extension URIs the client declared support for, in the
	// order given, deduplicated.
	Extensions []string
}

// SupportsExtension reports whether the client declared support for uri.
func (p ServiceParams) SupportsExtension(uri string) bool {
	for _, e := range p.Extensions {
		if e == uri {
			return true
		}
	}
	return false
}

// Apply writes the service parameters onto an outgoing request's headers. An
// empty Version is written as ProtocolVersion, since clients must always send
// the header (specification section 3.6.1).
func (p ServiceParams) Apply(h http.Header) {
	version := p.Version
	if version == "" {
		version = ProtocolVersion
	}
	h.Set(HeaderVersion, version)
	if len(p.Extensions) > 0 {
		h.Set(HeaderExtensions, strings.Join(p.Extensions, ","))
	}
}

// ParseServiceParams reads the A2A service parameters from request headers,
// falling back to query parameters for the version, which the specification
// permits clients to supply either way (section 3.6.1). Pass a nil query when
// there is none.
//
// It returns a VersionNotSupportedError when the requested version is not one
// this codec speaks, and an InvalidRequestError when the version is malformed.
func ParseServiceParams(h http.Header, q url.Values) (ServiceParams, *Error) {
	var p ServiceParams

	raw := ""
	if h != nil {
		raw = strings.TrimSpace(h.Get(HeaderVersion))
	}
	if raw == "" && q != nil {
		raw = strings.TrimSpace(q.Get(HeaderVersion))
	}
	if raw == "" {
		raw = DefaultVersion
	}

	version, err := NormalizeVersion(raw)
	if err != nil {
		return p, ErrInvalidRequest(HeaderVersion, err.Error())
	}
	if !IsVersionSupported(version) {
		return p, Errorf(ErrorTypeVersionNotSupported,
			"protocol version %s is not supported; this agent speaks %s",
			version, strings.Join(supportedVersions, ", ")).
			WithMetadata("requestedVersion", version).
			WithMetadata("supportedVersions", strings.Join(supportedVersions, ","))
	}
	p.Version = version

	if h != nil {
		// A client may fold the extension list into one header or repeat the
		// header; both are accepted, with comma-separated values preferred.
		p.Extensions = ParseExtensions(h.Values(HeaderExtensions)...)
	}
	return p, nil
}

// ParseExtensions splits one or more A2A-Extensions header values into a
// deduplicated, order-preserving list of extension URIs. Empty entries are
// dropped.
func ParseExtensions(values ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		for _, uri := range strings.Split(value, ",") {
			uri = strings.TrimSpace(uri)
			if uri == "" || seen[uri] {
				continue
			}
			seen[uri] = true
			out = append(out, uri)
		}
	}
	return out
}

// NormalizeVersion parses a protocol version and returns its Major.Minor form.
// A patch component is accepted and dropped: the specification says patch
// versions must not be considered when negotiating (section 3.6).
func NormalizeVersion(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("version is empty")
	}
	parts := strings.Split(raw, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return "", fmt.Errorf("version %q must be Major.Minor", raw)
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return "", fmt.Errorf("version %q has a non-numeric component %q", raw, parts[i])
		}
	}
	return parts[0] + "." + parts[1], nil
}

// IsVersionSupported reports whether a normalized Major.Minor version is one
// this codec speaks.
func IsVersionSupported(version string) bool {
	for _, v := range supportedVersions {
		if v == version {
			return true
		}
	}
	return false
}
