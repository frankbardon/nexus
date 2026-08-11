package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The registry fixture every test here serves. The values are deliberately
// distinctive strings that appear nowhere else in a response, so the
// "path/args/env never leak" assertion can look for them literally in the raw
// body rather than trusting the projection struct's field list.
const (
	// secretBinaryPath stands in for the kind of thing a path discloses: a build
	// location on the broker host.
	secretBinaryPath = "/opt/private-builds/nexus-vision-9000"

	// secretBinaryArg and secretBinaryEnvKey/Value are the other two host-only
	// fields, likewise chosen to be unmistakable in a body dump.
	secretBinaryArg      = "-profile=internal-vision"
	secretBinaryEnvKey   = "NEXUS_INTERNAL_FEATURE_FLAG"
	secretBinaryEnvValue = "on-for-acme-only"
)

// binariesFixture is a loaded-shaped registry with three entries: the reserved
// one carrying no presentation at all (the zero-config case a client must be
// able to fall back to `name` for), a fully-populated one carrying every field
// that must NOT be serialized, and a third whose name sorts BEFORE the reserved
// one so ordering is proved by more than "nexus happens to come first".
func binariesFixture() map[string]BinaryEntry {
	return map[string]BinaryEntry{
		reservedBinaryName: {
			Path:         "nexus",
			ResolvedPath: "/usr/local/bin/nexus",
		},
		"vision": {
			Path:         secretBinaryPath,
			ResolvedPath: secretBinaryPath,
			Label:        "Nexus (vision)",
			Description:  "Multimodal build with the image tools compiled in",
			Args:         []string{secretBinaryArg},
			Env:          map[string]string{secretBinaryEnvKey: secretBinaryEnvValue},
		},
		"archive": {
			Path:         "/usr/local/bin/nexus-0.9",
			ResolvedPath: "/usr/local/bin/nexus-0.9",
			Label:        "Nexus 0.9",
		},
	}
}

// newBinariesTestServer wires GET /binaries the way run() does — registered
// THROUGH the guard — over an httptest server. authYAML empty means no `auth:`
// block at all, which is the supported auth-disabled deployment, not a
// degraded one.
func newBinariesTestServer(t *testing.T, authYAML string) *httptest.Server {
	t.Helper()
	cfg := mustLoadConfig(t, authYAML)
	if authYAML != "" && !cfg.AuthChain.Enabled() {
		t.Fatalf("newBinariesTestServer: YAML produced a DISABLED chain; the test would prove nothing:\n%s", authYAML)
	}
	logger := testLogger()
	guard := newAuthGuard(logger, cfg.AuthChain)
	mux := http.NewServeMux()
	NewBinariesServer(logger, binariesFixture()).Register(guard.Guard(mux))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// getBinariesBody performs GET /binaries with an optional bearer token and
// returns the RAW response body, failing on any non-200 status. The raw bytes
// are what the leak assertion needs — a decode into []BinaryInfo would discard
// exactly the keys under test.
func getBinariesBody(t *testing.T, base, token string) []byte {
	t.Helper()
	resp := doAuthed(t, http.MethodGet, base+"/binaries", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /binaries status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read binaries body: %v", err)
	}
	return body
}

// decodeBinaries decodes a GET /binaries body into the listing.
func decodeBinaries(t *testing.T, body []byte) []BinaryInfo {
	t.Helper()
	var list []BinaryInfo
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode binaries %q: %v", body, err)
	}
	return list
}

// TestBinaries_AuthDisabledServesUnauthenticated pins the supported
// auth-disabled deployment: with no `auth:` block the route answers a caller
// presenting no credential at all, exactly as POST /claim does there.
func TestBinaries_AuthDisabledServesUnauthenticated(t *testing.T) {
	ts := newBinariesTestServer(t, "")

	list := decodeBinaries(t, getBinariesBody(t, ts.URL, ""))
	if len(list) != len(binariesFixture()) {
		t.Fatalf("listed %d entries, want %d: %+v", len(list), len(binariesFixture()), list)
	}
}

// TestBinaries_AuthEnabledRejectsMissingAndInvalidCredentials proves the route
// sits behind the SAME guard as POST /claim: with auth on, neither an absent nor
// a wrong credential gets the listing.
func TestBinaries_AuthEnabledRejectsMissingAndInvalidCredentials(t *testing.T) {
	ts := newBinariesTestServer(t, staticAuthYAML)

	t.Run("no credential", func(t *testing.T) {
		resp := doAuthed(t, http.MethodGet, ts.URL+"/binaries", "", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		assertErrorBody(t, resp, "authentication required")
	})

	t.Run("invalid credential", func(t *testing.T) {
		resp := doAuthed(t, http.MethodGet, ts.URL+"/binaries", "wrong-token", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		assertErrorBody(t, resp, "credential rejected")
	})
}

// TestBinaries_AuthEnabledServesValidCredential is the other half: a credential
// the chain accepts gets the full listing.
func TestBinaries_AuthEnabledServesValidCredential(t *testing.T) {
	ts := newBinariesTestServer(t, staticAuthYAML)

	list := decodeBinaries(t, getBinariesBody(t, ts.URL, "good-token"))
	if len(list) != len(binariesFixture()) {
		t.Fatalf("listed %d entries, want %d: %+v", len(list), len(binariesFixture()), list)
	}
}

// TestBinaries_ResponseShapeAndOrdering pins the whole contract a client codes
// against: one object per registry entry — the reserved `nexus` one included —
// sorted by name, carrying the presentational strings and nothing else.
//
// It asserts the EXACT slice rather than spot-checking fields, so an entry
// appearing, disappearing or losing its label fails here rather than in a
// client.
func TestBinaries_ResponseShapeAndOrdering(t *testing.T) {
	ts := newBinariesTestServer(t, "")

	got := decodeBinaries(t, getBinariesBody(t, ts.URL, ""))
	want := []BinaryInfo{
		{Name: "archive", Label: "Nexus 0.9"},
		{Name: reservedBinaryName},
		{Name: "vision", Label: "Nexus (vision)", Description: "Multimodal build with the image tools compiled in"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listing =\n %+v\nwant (sorted by name)\n %+v", got, want)
	}
}

// TestBinaries_OmitsUnsetLabelAndDescription pins the chosen convention on the
// WIRE, not just in the decoded struct: an entry with no presentation encodes as
// `{"name":"nexus"}` with the two optional keys ABSENT, so a client can tell "the
// operator wrote nothing" from "the operator wrote an empty string" and fall
// back to the name.
func TestBinaries_OmitsUnsetLabelAndDescription(t *testing.T) {
	ts := newBinariesTestServer(t, "")

	var raw []map[string]any
	body := getBinariesBody(t, ts.URL, "")
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode binaries %q: %v", body, err)
	}

	var reserved map[string]any
	for _, entry := range raw {
		if entry["name"] == reservedBinaryName {
			reserved = entry
		}
	}
	if reserved == nil {
		t.Fatalf("reserved %q entry missing from listing: %s", reservedBinaryName, body)
	}
	if len(reserved) != 1 {
		t.Errorf("reserved entry = %v, want the name key alone (label/description omitted when unset)", reserved)
	}
}

// TestBinaries_NeverSerializesHostDetail is the load-bearing one: `path`,
// `args` and `env` — and the derived absolute `resolved_path` — are broker-host
// detail that leaks deployment layout, so they must not reach a client.
//
// It searches the RAW body for the configured values themselves, not just for
// the key names. A projection that renamed a field, or a future BinaryEntry
// field carrying a path, would still be caught: the assertion is "these strings
// are not in the response", which is the actual disclosure property.
func TestBinaries_NeverSerializesHostDetail(t *testing.T) {
	ts := newBinariesTestServer(t, "")
	body := string(getBinariesBody(t, ts.URL, ""))

	for _, leak := range []string{
		secretBinaryPath,
		secretBinaryArg,
		secretBinaryEnvKey,
		secretBinaryEnvValue,
		"/usr/local/bin/nexus", // the reserved entry's ResolvedPath
		"/usr/local/bin/nexus-0.9",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaks broker-host detail %q:\n%s", leak, body)
		}
	}

	for _, key := range []string{`"path"`, `"resolved_path"`, `"args"`, `"env"`} {
		if strings.Contains(body, key) {
			t.Errorf("response carries key %s, which must never be serialized:\n%s", key, body)
		}
	}
}

// TestBinaries_ListingIsUnfiltered pins the explicit scope decision: every
// authenticated caller sees the SAME list. Per-principal filtering is out of
// scope for this effort, so two different valid principals getting different
// listings would be a behaviour nobody designed.
func TestBinaries_ListingIsUnfiltered(t *testing.T) {
	ts := newBinariesTestServer(t, twoPrincipalAuthYAML)

	owner := getBinariesBody(t, ts.URL, ownerToken)
	other := getBinariesBody(t, ts.URL, otherToken)
	if string(owner) != string(other) {
		t.Errorf("listings differ between principals — the listing must be unfiltered:\n %s\n %s", owner, other)
	}
}

// TestNewBinariesServer_EmptyRegistryEncodesAsArray guards the JSON zero value.
// A real load always yields at least the reserved entry, so this is about the
// projection never handing `null` to a client that does `for (const b of list)`.
func TestNewBinariesServer_EmptyRegistryEncodesAsArray(t *testing.T) {
	s := NewBinariesServer(testLogger(), nil)
	body, err := json.Marshal(s.entries)
	if err != nil {
		t.Fatalf("marshal entries: %v", err)
	}
	if string(body) != "[]" {
		t.Errorf("empty registry encodes as %s, want []", body)
	}
}
