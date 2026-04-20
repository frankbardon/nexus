package codeexec

import (
	"testing"

	"github.com/traefik/yaegi/interp"
)

func TestFilteredStdlibSymbols_OnlyAllowed(t *testing.T) {
	got := filteredStdlibSymbols([]string{"fmt", "encoding/json"})

	// Must contain the allowed packages.
	if _, ok := got["fmt/fmt"]; !ok {
		t.Errorf("missing fmt/fmt")
	}
	if _, ok := got["encoding/json/json"]; !ok {
		t.Errorf("missing encoding/json/json")
	}

	// Must NOT contain blocked packages.
	blocked := []string{"os/os", "net/net", "os/exec/exec", "syscall/syscall", "unsafe/unsafe"}
	for _, key := range blocked {
		if _, ok := got[key]; ok {
			t.Errorf("unexpected %q in filtered stdlib", key)
		}
	}
}

func TestFilteredStdlibSymbols_EmptyWhenNoAllowed(t *testing.T) {
	got := filteredStdlibSymbols(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty Exports, got %d entries", len(got))
	}
}

func TestSplitYaegiKey(t *testing.T) {
	cases := map[string]string{
		"fmt/fmt":            "fmt",
		"encoding/json/json": "encoding/json",
		"net/http/http":      "net/http",
		"nopkg":              "nopkg",
	}
	for in, want := range cases {
		if got := splitYaegiKey(in); got != want {
			t.Errorf("splitYaegiKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// Confirm the default whitelist stays loadable — guards against typos.
func TestDefaultAllowedStdlibResolves(t *testing.T) {
	got := filteredStdlibSymbols(defaultAllowedStdlib)
	if len(got) == 0 {
		t.Fatal("default stdlib whitelist resolved to zero packages")
	}
	// Sanity: has at least fmt.
	if _, ok := got["fmt/fmt"]; !ok {
		t.Fatal("default whitelist missing fmt")
	}
	_ = interp.Exports(got) // type assertion: the value is assignable as exports
}
