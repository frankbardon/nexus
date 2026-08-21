package a2a

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

// TestParseServiceParamsVersion covers the A2A-Version service parameter from
// specification sections 3.2.6 and 3.6.
func TestParseServiceParamsVersion(t *testing.T) {
	tests := []struct {
		name        string
		header      string
		query       string
		wantVersion string
		wantErrType ErrorType
	}{
		{name: "exact 1.0", header: "1.0", wantVersion: "1.0"},
		{name: "patch component is dropped", header: "1.0.1", wantVersion: "1.0"},
		{name: "whitespace is trimmed", header: "  1.0  ", wantVersion: "1.0"},
		{
			name: "absent header means 0.3, which this codec does not speak",
			// Specification section 3.6.2: an empty version must be interpreted
			// as 0.3. Since only 1.0 is implemented, that is a hard rejection
			// rather than a silent mis-parse.
			header: "", wantErrType: ErrorTypeVersionNotSupported,
		},
		{name: "explicit 0.3", header: "0.3", wantErrType: ErrorTypeVersionNotSupported},
		{name: "future version", header: "2.0", wantErrType: ErrorTypeVersionNotSupported},
		{name: "unsupported minor", header: "1.5", wantErrType: ErrorTypeVersionNotSupported},
		{name: "major only", header: "1", wantErrType: ErrorTypeInvalidRequest},
		{name: "four components", header: "1.0.0.0", wantErrType: ErrorTypeInvalidRequest},
		{name: "non-numeric", header: "one.zero", wantErrType: ErrorTypeInvalidRequest},
		{name: "query fallback", query: "1.0", wantVersion: "1.0"},
		{name: "header wins over query", header: "1.0", query: "0.3", wantVersion: "1.0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set(HeaderVersion, tc.header)
			}
			var q url.Values
			if tc.query != "" {
				q = url.Values{HeaderVersion: []string{tc.query}}
			}

			params, err := ParseServiceParams(h, q)
			if tc.wantErrType != "" {
				if err == nil {
					t.Fatalf("expected a %s, got version %q", tc.wantErrType, params.Version)
				}
				if err.Type != tc.wantErrType {
					t.Fatalf("error type = %q, want %q", err.Type, tc.wantErrType)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseServiceParams: %v", err)
			}
			if params.Version != tc.wantVersion {
				t.Fatalf("version = %q, want %q", params.Version, tc.wantVersion)
			}
		})
	}
}

// TestVersionNotSupportedErrorCarriesContext checks the error a client gets is
// actionable: it names both the rejected and the supported versions.
func TestVersionNotSupportedErrorCarriesContext(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderVersion, "0.3")
	_, err := ParseServiceParams(h, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Code() != CodeVersionNotSupported {
		t.Fatalf("code = %d, want %d", err.Code(), CodeVersionNotSupported)
	}
	if err.Metadata["requestedVersion"] != "0.3" {
		t.Errorf("requestedVersion = %q", err.Metadata["requestedVersion"])
	}
	if err.Metadata["supportedVersions"] != ProtocolVersion {
		t.Errorf("supportedVersions = %q, want %q", err.Metadata["supportedVersions"], ProtocolVersion)
	}
}

// TestParseExtensions covers the comma-separated A2A-Extensions parameter.
func TestParseExtensions(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   []string
	}{
		{name: "empty", values: nil, want: nil},
		{name: "empty string", values: []string{""}, want: nil},
		{
			name:   "single",
			values: []string{"https://example.com/extensions/geolocation/v1"},
			want:   []string{"https://example.com/extensions/geolocation/v1"},
		},
		{
			name:   "comma separated",
			values: []string{"https://example.com/extensions/geolocation/v1,https://standards.org/extensions/citations/v1"},
			want: []string{
				"https://example.com/extensions/geolocation/v1",
				"https://standards.org/extensions/citations/v1",
			},
		},
		{
			name:   "whitespace around entries is trimmed",
			values: []string{" https://a.example/v1 , https://b.example/v1 "},
			want:   []string{"https://a.example/v1", "https://b.example/v1"},
		},
		{
			name:   "repeated headers are merged",
			values: []string{"https://a.example/v1", "https://b.example/v1"},
			want:   []string{"https://a.example/v1", "https://b.example/v1"},
		},
		{
			name:   "duplicates are dropped, order preserved",
			values: []string{"https://b.example/v1,https://a.example/v1", "https://b.example/v1"},
			want:   []string{"https://b.example/v1", "https://a.example/v1"},
		},
		{
			name:   "empty entries are dropped",
			values: []string{"https://a.example/v1,,https://b.example/v1,"},
			want:   []string{"https://a.example/v1", "https://b.example/v1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseExtensions(tc.values...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("= %v, want %v", got, tc.want)
			}
		})
	}
}

// TestServiceParamsRoundTrip writes params onto a header and reads them back.
func TestServiceParamsRoundTrip(t *testing.T) {
	original := ServiceParams{
		Version: ProtocolVersion,
		Extensions: []string{
			"https://example.com/extensions/geolocation/v1",
			"https://standards.org/extensions/citations/v1",
		},
	}

	h := http.Header{}
	original.Apply(h)

	if got := h.Get(HeaderVersion); got != ProtocolVersion {
		t.Fatalf("%s = %q, want %q", HeaderVersion, got, ProtocolVersion)
	}
	const wantExtensions = "https://example.com/extensions/geolocation/v1,https://standards.org/extensions/citations/v1"
	if got := h.Get(HeaderExtensions); got != wantExtensions {
		t.Fatalf("%s = %q, want %q", HeaderExtensions, got, wantExtensions)
	}

	parsed, err := ParseServiceParams(h, nil)
	if err != nil {
		t.Fatalf("ParseServiceParams: %v", err)
	}
	if !reflect.DeepEqual(parsed, original) {
		t.Fatalf("round trip drifted:\nwant %+v\n got %+v", original, parsed)
	}
}

// TestServiceParamsApplyDefaultsVersion checks that a client always sends the
// version header, as specification section 3.6.1 requires.
func TestServiceParamsApplyDefaultsVersion(t *testing.T) {
	h := http.Header{}
	ServiceParams{}.Apply(h)
	if got := h.Get(HeaderVersion); got != ProtocolVersion {
		t.Fatalf("%s = %q, want %q", HeaderVersion, got, ProtocolVersion)
	}
	if _, ok := h[http.CanonicalHeaderKey(HeaderExtensions)]; ok {
		t.Error("an empty extension list must not emit the header")
	}
}

// TestServiceParamHeaderNamesAreCaseInsensitive relies on http.Header
// canonicalization, matching the RFC 9110 rule the spec cites.
func TestServiceParamHeaderNamesAreCaseInsensitive(t *testing.T) {
	h := http.Header{}
	h.Set("a2a-version", ProtocolVersion)
	h.Set("A2A-EXTENSIONS", "https://a.example/v1")

	params, err := ParseServiceParams(h, nil)
	if err != nil {
		t.Fatalf("ParseServiceParams: %v", err)
	}
	if params.Version != ProtocolVersion {
		t.Fatalf("version = %q", params.Version)
	}
	if !params.SupportsExtension("https://a.example/v1") {
		t.Fatalf("extensions = %v", params.Extensions)
	}
}

// TestSupportsExtension covers the client-declaration lookup that gates
// required extensions.
func TestSupportsExtension(t *testing.T) {
	p := ServiceParams{Extensions: []string{"https://a.example/v1"}}
	if !p.SupportsExtension("https://a.example/v1") {
		t.Error("declared extension reported unsupported")
	}
	if p.SupportsExtension("https://b.example/v1") {
		t.Error("undeclared extension reported supported")
	}
	if (ServiceParams{}).SupportsExtension("https://a.example/v1") {
		t.Error("empty params reported an extension as supported")
	}
}

// TestNormalizeVersion covers the Major.Minor parser directly.
func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "1.0", want: "1.0"},
		{in: "0.3", want: "0.3"},
		{in: "1.0.1", want: "1.0"},
		{in: "12.34", want: "12.34"},
		{in: "", wantErr: true},
		{in: "1", wantErr: true},
		{in: "1.", wantErr: true},
		{in: "v1.0", wantErr: true},
		{in: "1.0.0.1", wantErr: true},
		{in: "-1.0", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := NormalizeVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeVersion: %v", err)
			}
			if got != tc.want {
				t.Fatalf("= %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSupportedVersions pins what this codec advertises.
func TestSupportedVersions(t *testing.T) {
	got := SupportedVersions()
	if !reflect.DeepEqual(got, []string{"1.0"}) {
		t.Fatalf("SupportedVersions() = %v, want [1.0]", got)
	}
	if !IsVersionSupported("1.0") {
		t.Error("1.0 must be supported")
	}
	if IsVersionSupported(DefaultVersion) {
		t.Errorf("this codec must not claim to speak %s", DefaultVersion)
	}

	// The returned slice must be a copy: mutating it must not corrupt the
	// package-level list.
	got[0] = "9.9"
	if SupportedVersions()[0] != "1.0" {
		t.Error("SupportedVersions() returned a slice aliasing package state")
	}
}
