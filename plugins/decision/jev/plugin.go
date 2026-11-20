// Package jev implements a decision.provider capability backed by a Jev-style
// "System One" decision API: state plus typed questions in, typed answers with
// calibrated probabilities out.
//
// The endpoint is configurable because the same wire shape is served from
// several places: a local LM Studio instance hosting an open Jev-compatible
// model (the default — LM Studio exposes POST /v1/systemone for loaded
// decision models), TypeSafe AI's hosted API, or a gateway. The Vercel AI
// Gateway's /v1/evaluate names the yes/no type "boolean" and returns
// "probability" rather than "noul"; the `dialect` key selects that variant.
//
// Jev is not an LLM and does not ride llm.request: it answers decision.ask
// events, filling the pointer payload in place like the search providers do.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.decision.jev"
	pluginName = "Jev Decision Provider"
	version    = "0.1.0"

	// defaultEndpoint is LM Studio's default local server port plus its
	// Jev-compatible System One route.
	defaultEndpoint = "http://localhost:1234/v1/systemone"
	defaultKeyEnv   = "JEV_API_KEY"
	defaultTimeout  = 10 * time.Second

	dialectSystemOne = "systemone"
	dialectVercel    = "vercel"

	// maxErrorBody bounds how much of a non-2xx body lands in an error.
	maxErrorBody = 300
)

// Plugin advertises decision.provider and answers decision.ask events.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	endpoint string
	model    string
	apiKey   string
	dialect  string
	timeout  time.Duration
	client   *http.Client
	unsubs   []func()
}

// New creates a Jev decision provider.
func New() engine.Plugin {
	return &Plugin{endpoint: defaultEndpoint, dialect: dialectSystemOne, timeout: defaultTimeout}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "decision.provider",
		Description: "Typed decision questions answered by a Jev-compatible System One API.",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["endpoint"].(string); ok && v != "" {
		p.endpoint = v
	}
	if v, ok := ctx.Config["model"].(string); ok {
		p.model = v
	}
	if v, ok := ctx.Config["dialect"].(string); ok && v != "" {
		switch v {
		case dialectSystemOne, dialectVercel:
			p.dialect = v
		default:
			return fmt.Errorf("jev: unknown dialect %q (want %q or %q)", v, dialectSystemOne, dialectVercel)
		}
	}
	if ts, ok := ctx.Config["timeout"].(string); ok && ts != "" {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("jev: invalid timeout %q: %w", ts, err)
		}
		p.timeout = d
	}

	// A key is optional: a local LM Studio server needs none, the hosted
	// API does. An unset env var simply sends no Authorization header.
	if key, ok := ctx.Config["api_key"].(string); ok && key != "" {
		p.apiKey = key
	} else {
		envVar, _ := ctx.Config["api_key_env"].(string)
		if envVar == "" {
			envVar = defaultKeyEnv
		}
		p.apiKey = os.Getenv(envVar)
	}

	p.client = &http.Client{Timeout: p.timeout}
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("decision.ask", p.handleAsk,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	p.logger.Info("jev decision provider initialized",
		"endpoint", p.endpoint, "dialect", p.dialect, "model", p.model,
		"authenticated", p.apiKey != "")
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{{EventType: "decision.ask", Priority: 50}}
}

func (p *Plugin) Emissions() []string { return nil }

func (p *Plugin) handleAsk(event engine.Event[any]) {
	ask, ok := event.Payload.(*events.DecisionAsk)
	if !ok {
		return
	}
	// Another provider already answered — don't overwrite it.
	if ask.Provider != "" {
		return
	}
	ask.Provider = pluginID
	if err := p.ask(ask); err != nil {
		ask.Error = err.Error()
		p.logger.Warn("jev decision failed", "error", err)
	}
}

// --- wire types ---

type wireQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type wireRequest struct {
	Model     string                  `json:"model,omitempty"`
	State     any                     `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Probability   *float64           `json:"probability,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (p *Plugin) ask(ask *events.DecisionAsk) error {
	body, err := p.buildRequest(ask)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jev returned HTTP %d: %s", resp.StatusCode, truncate(string(raw), maxErrorBody))
	}

	var parsed wireResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	ask.ServedModel = parsed.Model
	ask.InputTokens = parsed.Usage.InputTokens
	ask.OutputTokens = parsed.Usage.OutputTokens
	ask.Answers = make(map[string]events.DecisionAnswer, len(parsed.Answers))
	for key, a := range parsed.Answers {
		ask.Answers[key] = toAnswer(a)
	}
	for key := range ask.Questions {
		if _, ok := ask.Answers[key]; !ok {
			return fmt.Errorf("response missing answer for question %q", key)
		}
	}
	return nil
}

func (p *Plugin) buildRequest(ask *events.DecisionAsk) ([]byte, error) {
	if len(ask.Questions) == 0 {
		return nil, fmt.Errorf("no questions")
	}
	if ask.State == nil {
		return nil, fmt.Errorf("no state")
	}
	req := wireRequest{
		Model:     p.model,
		State:     ask.State,
		Questions: make(map[string]wireQuestion, len(ask.Questions)),
	}
	if ask.Model != "" {
		req.Model = ask.Model
	}
	for key, q := range ask.Questions {
		wq, err := p.toWire(q)
		if err != nil {
			return nil, fmt.Errorf("question %q: %w", key, err)
		}
		req.Questions[key] = wq
	}
	return json.Marshal(req)
}

// toWire maps a Nexus question onto the System One vocabulary.
func (p *Plugin) toWire(q events.DecisionQuestion) (wireQuestion, error) {
	wq := wireQuestion{Instructions: q.Instructions}
	switch q.Type {
	case events.DecisionYesNo:
		wq.Type = "noul"
		if p.dialect == dialectVercel {
			wq.Type = "boolean"
		}
		if q.TrueMeans != "" || q.FalseMeans != "" {
			c := map[string]string{}
			if q.TrueMeans != "" {
				c["true"] = q.TrueMeans
			}
			if q.FalseMeans != "" {
				c["false"] = q.FalseMeans
			}
			wq.Criteria = c
		}
	case events.DecisionChoice:
		if len(q.Choices) == 0 {
			return wq, fmt.Errorf("choice question needs at least one choice")
		}
		wq.Type = "choice"
		wq.Criteria = q.Choices
	case events.DecisionScore:
		if len(q.Levels) < 2 || len(q.Levels) > 10 {
			return wq, fmt.Errorf("score question needs 2-10 levels, got %d", len(q.Levels))
		}
		wq.Type = "score"
		wq.Criteria = q.Levels
	default:
		return wq, fmt.Errorf("unsupported question type %q", q.Type)
	}
	return wq, nil
}

// toAnswer maps a wire answer back to Nexus types. The yes/no probability is
// read from "noul" or "probability", whichever the server sent, so one
// parser serves both dialects.
func toAnswer(a wireAnswer) events.DecisionAnswer {
	out := events.DecisionAnswer{
		Type:          a.Type,
		Choice:        a.Choice,
		Score:         a.Score,
		Confidence:    a.Confidence,
		Probabilities: a.Probabilities,
	}
	switch a.Type {
	case "noul", "boolean":
		out.Type = events.DecisionYesNo
	}
	switch {
	case a.Noul != nil:
		out.Probability = *a.Noul
	case a.Probability != nil:
		out.Probability = *a.Probability
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
