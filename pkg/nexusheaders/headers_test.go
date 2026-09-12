package nexusheaders

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	cases := []struct {
		field string
		want  string
		ok    bool
	}{
		{"X-Nexus-Timezone", "timezone", true},
		{"x-nexus-timezone", "timezone", true},
		{"X-NEXUS-Tenant-ID", "tenant-id", true},
		{"X-Nexus-trace.id", "trace.id", true},
		{"X-Nexus-a_b", "a_b", true},
		{"X-Nexus-", "", false},
		{"X-Nexus", "", false},
		{"Authorization", "", false},
		{"X-Forwarded-For", "", false},
		{"X-Nexus-bad key", "", false},
		{"X-Nexus-" + strings.Repeat("a", MaxNameBytes+1), "", false},
	}
	for _, tc := range cases {
		got, ok := Name(tc.field)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Name(%q) = (%q, %v), want (%q, %v)", tc.field, got, ok, tc.want, tc.ok)
		}
	}
}

func TestExtract_OnlyPrefixedHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("Cookie", "session=abc")
	h.Set("X-Forwarded-For", "10.0.0.1")
	h.Set("X-Nexus-Timezone", "Europe/Amsterdam")
	h.Set("X-Nexus-Tenant-ID", "acme")

	got := Extract(h)
	want := map[string]string{"timezone": "Europe/Amsterdam", "tenant-id": "acme"}
	if len(got) != len(want) {
		t.Fatalf("Extract() = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Extract()[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestExtract_NoneReturnsNil(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	if got := Extract(h); got != nil {
		t.Errorf("Extract() = %v, want nil", got)
	}
	if got := Extract(nil); got != nil {
		t.Errorf("Extract(nil) = %v, want nil", got)
	}
}

func TestExtract_RepeatedFieldLinesJoin(t *testing.T) {
	h := http.Header{}
	h.Add("X-Nexus-Scope", "read")
	h.Add("X-Nexus-Scope", "write")

	if got := Extract(h)["scope"]; got != "read, write" {
		t.Errorf("Extract()[scope] = %q, want %q", got, "read, write")
	}
}

// A value that reaches a prompt must not be able to forge a line break in a
// block the agent reads as structure.
func TestExtract_StripsControlCharacters(t *testing.T) {
	h := http.Header{}
	// Set() would reject CR/LF; the map is written directly to simulate a
	// value that somehow reached the handler unfiltered.
	h["X-Nexus-Note"] = []string{"line\r\none\x00two\tkept"}

	if got := Extract(h)["note"]; got != "lineonetwo\tkept" {
		t.Errorf("Extract()[note] = %q, want %q", got, "lineonetwo\tkept")
	}
}

func TestExtract_TrimsSurroundingSpace(t *testing.T) {
	h := http.Header{}
	h.Set("X-Nexus-Locale", "  nl-NL  ")
	if got := Extract(h)["locale"]; got != "nl-NL" {
		t.Errorf("Extract()[locale] = %q, want %q", got, "nl-NL")
	}
}

func TestExtract_BoundsCount(t *testing.T) {
	h := http.Header{}
	for i := 0; i < MaxCount+10; i++ {
		h.Set(fmt.Sprintf("X-Nexus-H%03d", i), "v")
	}
	got := Extract(h)
	if len(got) != MaxCount {
		t.Fatalf("Extract() kept %d headers, want %d", len(got), MaxCount)
	}
	// Deterministic drop: the lowest names by sort order survive.
	if _, ok := got["h000"]; !ok {
		t.Error("Extract() dropped h000; drop order is not name-sorted")
	}
	if _, ok := got[fmt.Sprintf("h%03d", MaxCount+9)]; ok {
		t.Error("Extract() kept the highest name; drop order is not name-sorted")
	}
}

func TestExtract_BoundsValueLength(t *testing.T) {
	h := http.Header{}
	h.Set("X-Nexus-Big", strings.Repeat("x", MaxValueBytes+500))
	if got := len(Extract(h)["big"]); got != MaxValueBytes {
		t.Errorf("Extract()[big] length = %d, want %d", got, MaxValueBytes)
	}
}

func TestExtract_BoundsTotalBytes(t *testing.T) {
	h := http.Header{}
	// Six values of MaxValueBytes each exceeds MaxTotalBytes (16KiB).
	for i := 0; i < 6; i++ {
		h.Set(fmt.Sprintf("X-Nexus-B%d", i), strings.Repeat("x", MaxValueBytes))
	}
	total := 0
	for k, v := range Extract(h) {
		total += len(k) + len(v)
	}
	if total > MaxTotalBytes {
		t.Errorf("Extract() kept %d bytes, want <= %d", total, MaxTotalBytes)
	}
	if total == 0 {
		t.Error("Extract() kept nothing; the total bound should truncate, not empty")
	}
}

// The returned map must share nothing with the request's header map.
func TestExtract_ResultIsIndependent(t *testing.T) {
	h := http.Header{}
	h.Set("X-Nexus-Tenant", "acme")
	got := Extract(h)
	h.Set("X-Nexus-Tenant", "other")
	if got["tenant"] != "acme" {
		t.Errorf("Extract() result changed with the request header: %q", got["tenant"])
	}
}
