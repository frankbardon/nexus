package jev

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("decision.ask")
	caps := h.Plugin().Capabilities()
	if len(caps) != 1 || caps[0].Name != "decision.provider" {
		t.Fatalf("Capabilities() = %v, want decision.provider", caps)
	}
}

func newPlugin(t *testing.T, cfg map[string]any) engine.EventBus {
	t.Helper()
	bus := engine.NewEventBus()
	p := New()
	if err := p.Init(engine.PluginContext{Bus: bus, Logger: slog.Default(), Config: cfg}); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return bus
}

func sampleAsk() *events.DecisionAsk {
	return &events.DecisionAsk{
		SchemaVersion: events.DecisionAskVersion,
		State:         "I was charged twice and need this fixed today.",
		Questions: map[string]events.DecisionQuestion{
			"is_urgent":  {Type: events.DecisionYesNo, Instructions: "Is this urgent?", TrueMeans: "needs action today"},
			"department": {Type: events.DecisionChoice, Instructions: "Route it.", Choices: map[string]string{"billing": "payments", "technical": "bugs"}},
			"frustration": {Type: events.DecisionScore, Instructions: "How frustrated?",
				Levels: []string{"calm", "annoyed", "angry"}},
		},
	}
}

func TestSystemOneRoundTrip(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{
			"model": "openjev-4b",
			"answers": {
				"is_urgent": {"type": "noul", "noul": 0.96},
				"department": {"type": "choice", "choice": "billing", "confidence": 0.94, "probabilities": {"billing": 0.94, "technical": 0.06}},
				"frustration": {"type": "score", "score": 1.21, "confidence": 0.71}
			},
			"usage": {"input_tokens": 120, "output_tokens": 3}
		}`)
	}))
	defer srv.Close()

	bus := newPlugin(t, map[string]any{"endpoint": srv.URL, "model": "openjev-4b", "api_key": "k"})
	ask := sampleAsk()
	if err := bus.Emit("decision.ask", ask); err != nil {
		t.Fatal(err)
	}
	if ask.Error != "" {
		t.Fatalf("unexpected error: %s", ask.Error)
	}
	if auth != "Bearer k" {
		t.Errorf("Authorization = %q", auth)
	}
	if got["model"] != "openjev-4b" {
		t.Errorf("request model = %v", got["model"])
	}
	qs := got["questions"].(map[string]any)
	if qs["is_urgent"].(map[string]any)["type"] != "noul" {
		t.Errorf("yes/no not mapped to noul: %v", qs["is_urgent"])
	}
	if lv := qs["frustration"].(map[string]any)["criteria"].([]any); len(lv) != 3 {
		t.Errorf("score criteria = %v", lv)
	}

	if ask.Provider != pluginID || ask.ServedModel != "openjev-4b" || ask.InputTokens != 120 {
		t.Errorf("metadata not filled: %+v", ask)
	}
	if a := ask.Answers["is_urgent"]; a.Type != events.DecisionYesNo || a.Probability != 0.96 {
		t.Errorf("is_urgent = %+v", a)
	}
	if a := ask.Answers["department"]; a.Choice != "billing" || a.Probabilities["technical"] != 0.06 {
		t.Errorf("department = %+v", a)
	}
	if a := ask.Answers["frustration"]; a.Score != 1.21 {
		t.Errorf("frustration = %+v", a)
	}
}

func TestVercelDialect(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{"answers": {"go": {"type": "boolean", "probability": 0.2}}}`)
	}))
	defer srv.Close()

	bus := newPlugin(t, map[string]any{"endpoint": srv.URL, "dialect": "vercel"})
	ask := &events.DecisionAsk{State: "x", Questions: map[string]events.DecisionQuestion{
		"go": {Type: events.DecisionYesNo, Instructions: "continue?"},
	}}
	_ = bus.Emit("decision.ask", ask)
	if ask.Error != "" {
		t.Fatal(ask.Error)
	}
	if _, present := got["model"]; present {
		t.Error("empty model should be omitted")
	}
	if got["questions"].(map[string]any)["go"].(map[string]any)["type"] != "boolean" {
		t.Errorf("vercel dialect should send boolean: %v", got)
	}
	if a := ask.Answers["go"]; a.Type != events.DecisionYesNo || a.Probability != 0.2 {
		t.Errorf("answer = %+v", a)
	}
}

func TestErrorsLandOnPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no model loaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	bus := newPlugin(t, map[string]any{"endpoint": srv.URL})

	ask := sampleAsk()
	_ = bus.Emit("decision.ask", ask)
	if !strings.Contains(ask.Error, "503") || ask.Provider != pluginID {
		t.Errorf("want HTTP 503 error, got provider=%q error=%q", ask.Provider, ask.Error)
	}

	bad := &events.DecisionAsk{State: "x", Questions: map[string]events.DecisionQuestion{
		"s": {Type: events.DecisionScore, Instructions: "?", Levels: []string{"only one"}},
	}}
	_ = bus.Emit("decision.ask", bad)
	if !strings.Contains(bad.Error, "2-10 levels") {
		t.Errorf("want level-count error, got %q", bad.Error)
	}
}

func TestMissingAnswerIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"answers": {}}`)
	}))
	defer srv.Close()
	bus := newPlugin(t, map[string]any{"endpoint": srv.URL})
	ask := sampleAsk()
	_ = bus.Emit("decision.ask", ask)
	if !strings.Contains(ask.Error, "missing answer") {
		t.Errorf("error = %q", ask.Error)
	}
}

func TestSkipsWhenAlreadyAnswered(t *testing.T) {
	bus := newPlugin(t, map[string]any{"endpoint": "http://127.0.0.1:1/unreachable"})
	ask := sampleAsk()
	ask.Provider = "someone.else"
	_ = bus.Emit("decision.ask", ask)
	if ask.Error != "" || ask.Provider != "someone.else" {
		t.Errorf("should not touch an answered ask: %+v", ask)
	}
}

func TestUnknownDialectFailsInit(t *testing.T) {
	err := New().Init(engine.PluginContext{Bus: engine.NewEventBus(), Logger: slog.Default(),
		Config: map[string]any{"dialect": "openai"}})
	if err == nil {
		t.Fatal("expected error")
	}
}
