package broker

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// TestContract asserts the declared Subscriptions/Emissions match the wired
// runtime surface. With no broker_addr in config the plugin stays dormant, so
// Init/Ready/Shutdown run cleanly inside the harness.
func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo(
		"io.output",
		"llm.stream.chunk",
		"llm.stream.end",
		"io.status",
		"io.approval.request",
		"hitl.requested",
		"cancel.complete",
	)
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"io.input",
		"before:io.input",
		"io.approval.response",
		"hitl.responded",
		"cancel.request",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}

// TestSpawnSecretConfigAndEnvResolution pins that `spawn_secret` resolves the
// same way `broker_addr` and `lease_id` do — config first, then the env var the
// broker injects at spawn.
//
// Precedence is asserted with BOTH sources present, which is the only case that
// distinguishes "config wins" from "whichever happens to be non-empty".
func TestSpawnSecretConfigAndEnvResolution(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		env        string
		want       string
	}{
		{"config wins over env", "from-config", "from-env", "from-config"},
		{"env is the fallback", "", "from-env", "from-env"},
		{"absent everywhere is empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(brokerframe.EnvSpawnSecret, tc.env)
			cfg := map[string]any{}
			if tc.configured != "" {
				cfg["spawn_secret"] = tc.configured
			}
			h := contract.NewContract(t, New, contract.WithPluginConfig(cfg))
			p, ok := h.Plugin().(*Plugin)
			if !ok {
				t.Fatalf("harness plugin is %T, want *Plugin", h.Plugin())
			}
			if p.spawnSecret != tc.want {
				t.Errorf("spawnSecret = %q, want %q", p.spawnSecret, tc.want)
			}
			// The value the client will actually put on the wire must be the same
			// one, or the resolution above is bookkeeping that never ships.
			if p.client.cfg.spawnSecret != tc.want {
				t.Errorf("client spawn secret = %q, want %q", p.client.cfg.spawnSecret, tc.want)
			}
		})
	}
}

// TestSpawnSecretIsNeverLogged pins the non-disclosure rule on the instance
// side. Init logs broker_addr and lease_id — both of which are already public —
// so the secret sitting beside them is one field away from joining them.
func TestSpawnSecretIsNeverLogged(t *testing.T) {
	const secret = "e4c8b21079a3f65d0b1c2d3e4f506172"

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	contract.NewContract(t, New,
		contract.WithLogger(logger),
		contract.WithPluginConfig(map[string]any{
			"lease_id":     "lease-logged",
			"spawn_secret": secret,
		}))

	out := buf.String()
	if !strings.Contains(out, "lease-logged") {
		t.Fatalf("no init record naming the lease; the assertion below is vacuous:\n%s", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("the spawn secret VALUE appears in the plugin's log output:\n%s", out)
	}
}
