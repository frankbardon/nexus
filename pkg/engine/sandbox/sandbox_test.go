package sandbox_test

import (
	"slices"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	_ "github.com/frankbardon/nexus/pkg/engine/sandbox/host"
)

func TestRegistryHasHost(t *testing.T) {
	got := sandbox.Registered()
	if !slices.Contains(got, sandbox.BackendHost) {
		t.Fatalf("expected %q registered, got %v", sandbox.BackendHost, got)
	}
}

func TestNewUnknownBackendErrors(t *testing.T) {
	_, err := sandbox.New("does-not-exist", nil)
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestFromPluginConfigFallback(t *testing.T) {
	tests := []struct {
		name   string
		cfg    map[string]any
		want   sandbox.Backend
		hasMap bool
	}{
		{"nil cfg", nil, sandbox.BackendHost, false},
		{"no sandbox key", map[string]any{"foo": 1}, sandbox.BackendHost, false},
		{"non-map sandbox", map[string]any{"sandbox": true}, sandbox.BackendHost, false},
		{"map without backend", map[string]any{"sandbox": map[string]any{"timeout": "10s"}}, sandbox.BackendHost, true},
		{"explicit backend", map[string]any{"sandbox": map[string]any{"backend": "wasm"}}, sandbox.BackendWasm, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, block := sandbox.FromPluginConfig(tc.cfg, sandbox.BackendHost)
			if name != tc.want {
				t.Fatalf("backend: got %q, want %q", name, tc.want)
			}
			if (block != nil) != tc.hasMap {
				t.Fatalf("block presence mismatch: got %v, want hasMap=%v", block, tc.hasMap)
			}
		})
	}
}
