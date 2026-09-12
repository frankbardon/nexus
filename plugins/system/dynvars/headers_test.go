package dynvars

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
)

func newHeaderTestPlugin(t *testing.T, cfg map[string]any) (*Plugin, *engine.SessionWorkspace) {
	t.Helper()
	bus := engine.NewEventBus()
	session, err := engine.NewSessionWorkspace(filepath.Join(t.TempDir(), "sessions"), bus)
	if err != nil {
		t.Fatalf("new session workspace: %v", err)
	}
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config:  cfg,
		Bus:     bus,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: session,
		Prompts: engine.NewPromptRegistry(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p, session
}

// The default has to be silence: these values are client-controlled, so a
// header must never reach a prompt merely because somebody sent it.
func TestRequestHeadersAreAbsentByDefault(t *testing.T) {
	p, session := newHeaderTestPlugin(t, map[string]any{"date": true})
	if err := session.SetRequestHeaders(map[string]string{"timezone": "Europe/Amsterdam"}); err != nil {
		t.Fatalf("SetRequestHeaders: %v", err)
	}
	if got := p.buildSection(); strings.Contains(got, "Europe/Amsterdam") {
		t.Errorf("an unnamed header reached the prompt:\n%s", got)
	}
}

// An operator naming a header gets exactly that one, and nothing else the
// caller happened to send.
func TestRequestHeadersAllowlistIsExact(t *testing.T) {
	p, session := newHeaderTestPlugin(t, map[string]any{
		"request_headers": []any{"timezone", "X-Nexus-Locale"},
	})
	if err := session.SetRequestHeaders(map[string]string{
		"timezone": "Europe/Amsterdam",
		"locale":   "nl-NL",
		"tenant":   "acme",
	}); err != nil {
		t.Fatalf("SetRequestHeaders: %v", err)
	}

	got := p.buildSection()
	if !strings.Contains(got, "Request header X-Nexus-timezone: Europe/Amsterdam") {
		t.Errorf("allowlisted header missing:\n%s", got)
	}
	if !strings.Contains(got, "Request header X-Nexus-locale: nl-NL") {
		t.Errorf("a prefixed allowlist entry was not normalized:\n%s", got)
	}
	if strings.Contains(got, "acme") {
		t.Errorf("an unnamed header reached the prompt:\n%s", got)
	}
}

// A header the caller did not send contributes no line at all, rather than an
// empty one the model would have to interpret.
func TestRequestHeaderAbsentContributesNoLine(t *testing.T) {
	p, _ := newHeaderTestPlugin(t, map[string]any{"request_headers": []any{"timezone"}})
	if got := p.buildSection(); got != "" {
		t.Errorf("buildSection = %q, want empty when no named header is bound", got)
	}
}

func TestRequestHeadersRejectsNonStringEntries(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	err := p.Init(engine.PluginContext{
		Config: map[string]any{"request_headers": []any{42}},
		Bus:    bus,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil {
		t.Fatal("Init accepted a non-string request_headers entry")
	}
}
