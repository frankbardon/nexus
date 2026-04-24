// Package openai implements an embeddings.provider capability backed by the
// OpenAI embeddings API (https://api.openai.com/v1/embeddings). Compatible
// with Azure OpenAI and OpenAI-compatible proxies via base_url override.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID     = "nexus.embeddings.openai"
	pluginName   = "OpenAI Embeddings Provider"
	version      = "0.1.0"
	apiURL       = "https://api.openai.com/v1/embeddings"
	defaultModel = "text-embedding-3-small"
)

// Plugin advertises the embeddings.provider capability and answers
// embeddings.request events by calling the OpenAI embeddings REST API.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	apiKey       string
	baseURL      string
	defaultModel string
	client       *http.Client
	timeout      time.Duration
	unsubs       []func()
}

// New creates a new OpenAI embeddings plugin.
func New() engine.Plugin {
	return &Plugin{timeout: 30 * time.Second}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "embeddings.provider",
		Description: "Text embeddings via the OpenAI embeddings API.",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	p.baseURL = apiURL
	if base, ok := ctx.Config["base_url"].(string); ok && base != "" {
		p.baseURL = strings.TrimRight(base, "/")
	}

	if key, ok := ctx.Config["api_key"].(string); ok && key != "" {
		p.apiKey = key
	} else {
		envVar, _ := ctx.Config["api_key_env"].(string)
		if envVar == "" {
			envVar = "OPENAI_API_KEY"
		}
		p.apiKey = os.Getenv(envVar)
	}
	if p.apiKey == "" {
		return fmt.Errorf("embeddings/openai: no API key configured (set api_key in config or OPENAI_API_KEY env var)")
	}

	p.defaultModel = defaultModel
	if m, ok := ctx.Config["model"].(string); ok && m != "" {
		p.defaultModel = m
	}

	if ts, ok := ctx.Config["timeout"].(string); ok {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("embeddings/openai: invalid timeout %q: %w", ts, err)
		}
		p.timeout = d
	}

	p.client = &http.Client{Timeout: p.timeout}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("embeddings.request", p.handleEmbeddings,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)
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
	return []engine.EventSubscription{
		{EventType: "embeddings.request", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string { return nil }

func (p *Plugin) handleEmbeddings(event engine.Event[any]) {
	req, ok := event.Payload.(*events.EmbeddingsRequest)
	if !ok {
		return
	}
	// Another provider already answered — don't overwrite.
	if req.Provider != "" {
		return
	}
	if len(req.Texts) == 0 {
		req.Provider = pluginID
		return
	}

	model := req.Model
	if model == "" {
		model = p.defaultModel
	}

	vectors, usage, err := p.embed(req.Texts, model, req.Dimensions)
	req.Provider = pluginID
	req.Model = model
	if err != nil {
		req.Error = err.Error()
		return
	}
	req.Vectors = vectors
	req.Usage = usage
}

type openaiEmbeddingsRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	Dimensions     int      `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

type openaiEmbeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func (p *Plugin) embed(texts []string, model string, dims int) ([][]float32, events.EmbeddingsUsage, error) {
	body := openaiEmbeddingsRequest{
		Input:          texts,
		Model:          model,
		EncodingFormat: "float",
	}
	if dims > 0 {
		body.Dimensions = dims
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL, bytes.NewReader(buf))
	if err != nil {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("openai returned HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var parsed openaiEmbeddingsResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("decode: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(parsed.Data))
	}

	// OpenAI returns data in request order, but spec says consumers should
	// sort by Index to be safe. Build output slice keyed on Index.
	vectors := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(vectors) {
			return nil, events.EmbeddingsUsage{}, fmt.Errorf("response index %d out of range", d.Index)
		}
		vectors[d.Index] = d.Embedding
	}
	return vectors, events.EmbeddingsUsage{
		PromptTokens: parsed.Usage.PromptTokens,
		TotalTokens:  parsed.Usage.TotalTokens,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
