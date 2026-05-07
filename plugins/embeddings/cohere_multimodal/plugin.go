// Package coheremultimodal implements an embeddings.provider capability
// backed by Cohere Embed v3 (multimodal): POST /v2/embed with either
// `text` or `image_url` content per input. Direct HTTP, no SDK — keeps
// deps minimal.
//
// Inputs are read from EmbeddingsRequest.Inputs first; when that field is
// empty the plugin falls back to EmbeddingsRequest.Texts so Cohere can
// also serve plain text-only callers if a deployer wires it as their
// embeddings.provider. Images are sent as data: URLs (base64-encoded) per
// Cohere's documented contract.
//
// Blob-scheme URIs ("nexus-blob:<sha>") are resolved via the per-session
// blob store when the plugin's PluginContext.Session is non-nil. When the
// session is unavailable (rare; mostly tests outside the harness) the
// plugin reports a clear error and asks the calling plugin to inline the
// bytes in EmbeddingsInput.Image instead. No silent fallback.
package coheremultimodal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID         = "nexus.embeddings.cohere_multimodal"
	pluginName       = "Cohere Multimodal Embeddings Provider"
	version          = "0.1.0"
	defaultBaseURL   = "https://api.cohere.com"
	embedPath        = "/v2/embed"
	defaultModel     = "embed-english-v3.0"
	defaultInputType = "search_document"
)

// Plugin advertises the embeddings.provider capability and answers
// embeddings.request events by calling Cohere's multimodal embeddings
// endpoint.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	apiKey       string
	baseURL      string
	defaultModel string
	inputType    string
	client       *http.Client
	timeout      time.Duration
	session      *engine.SessionWorkspace
	// blobStore is opened lazily on first need so plugins activated outside
	// a session (rare) still boot without error.
	blobStore *blobs.Store

	unsubs []func()
}

// New creates a new Cohere multimodal embeddings plugin.
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
		Description: "Multimodal text + image embeddings via Cohere Embed v3 (POST /v2/embed).",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	p.baseURL = defaultBaseURL
	if base, ok := ctx.Config["base_url"].(string); ok && base != "" {
		p.baseURL = strings.TrimRight(base, "/")
	}

	if key, ok := ctx.Config["api_key"].(string); ok && key != "" {
		p.apiKey = key
	} else {
		envVar, _ := ctx.Config["api_key_env"].(string)
		if envVar == "" {
			envVar = "COHERE_API_KEY"
		}
		p.apiKey = os.Getenv(envVar)
	}
	if p.apiKey == "" {
		return fmt.Errorf("embeddings/cohere_multimodal: no API key configured (set api_key in config or COHERE_API_KEY env var)")
	}

	p.defaultModel = defaultModel
	if m, ok := ctx.Config["model"].(string); ok && m != "" {
		p.defaultModel = m
	}

	p.inputType = defaultInputType
	if t, ok := ctx.Config["input_type"].(string); ok && t != "" {
		p.inputType = t
	}

	if ts, ok := ctx.Config["timeout"].(string); ok && ts != "" {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("embeddings/cohere_multimodal: invalid timeout %q: %w", ts, err)
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
	if req.Provider != "" {
		// Another adapter already answered.
		return
	}

	model := req.Model
	if model == "" {
		model = p.defaultModel
	}
	req.Provider = pluginID
	req.Model = model

	// Build the polymorphic input list. EmbeddingsInput is preferred; fall
	// back to legacy Texts when Inputs is empty.
	cohereInputs := make([]cohereInput, 0)
	if len(req.Inputs) > 0 {
		for i, in := range req.Inputs {
			ci, err := p.buildInput(in)
			if err != nil {
				req.Error = fmt.Sprintf("input %d: %v", i, err)
				return
			}
			cohereInputs = append(cohereInputs, ci)
		}
	} else {
		for _, t := range req.Texts {
			cohereInputs = append(cohereInputs, cohereInput{Content: []cohereContent{{Type: "text", Text: t}}})
		}
	}
	if len(cohereInputs) == 0 {
		// Empty request — provider claim already set, nothing more to do.
		return
	}

	vectors, usage, err := p.embed(cohereInputs, model)
	if err != nil {
		req.Error = err.Error()
		return
	}
	if len(vectors) != len(cohereInputs) {
		req.Error = fmt.Sprintf("expected %d vectors, got %d", len(cohereInputs), len(vectors))
		return
	}
	req.Vectors = vectors
	req.Usage = usage
}

// buildInput maps one EmbeddingsInput onto Cohere's polymorphic
// `inputs[].content[]` shape. Mutually-exclusive fields are validated;
// blob-scheme URIs are resolved via the engine blob store.
func (p *Plugin) buildInput(in events.EmbeddingsInput) (cohereInput, error) {
	switch {
	case in.Text != "" && (in.Image != nil || in.ImageURI != ""):
		return cohereInput{}, fmt.Errorf("EmbeddingsInput must populate exactly one of Text / Image / ImageURI")
	case in.Image != nil && in.ImageURI != "":
		return cohereInput{}, fmt.Errorf("EmbeddingsInput cannot set both Image and ImageURI")
	case in.Text != "":
		return cohereInput{Content: []cohereContent{{Type: "text", Text: in.Text}}}, nil
	case in.Image != nil:
		if in.MimeType == "" {
			return cohereInput{}, fmt.Errorf("EmbeddingsInput.Image requires MimeType")
		}
		dataURL := buildDataURL(in.MimeType, in.Image)
		return cohereInput{Content: []cohereContent{{Type: "image_url", ImageURL: &cohereImageURL{URL: dataURL}}}}, nil
	case in.ImageURI != "":
		// nexus-blob: scheme — resolve via per-session blob store.
		if sha := blobs.SHAFromURI(in.ImageURI); sha != "" {
			data, mime, err := p.resolveBlob(sha)
			if err != nil {
				return cohereInput{}, fmt.Errorf("resolve blob %q: %w", in.ImageURI, err)
			}
			if mime == "" {
				mime = in.MimeType
			}
			if mime == "" {
				return cohereInput{}, fmt.Errorf("blob %q has no recorded MimeType and EmbeddingsInput.MimeType is empty", in.ImageURI)
			}
			return cohereInput{Content: []cohereContent{{Type: "image_url", ImageURL: &cohereImageURL{URL: buildDataURL(mime, data)}}}}, nil
		}
		// External URL — Cohere can fetch it directly.
		return cohereInput{Content: []cohereContent{{Type: "image_url", ImageURL: &cohereImageURL{URL: in.ImageURI}}}}, nil
	default:
		return cohereInput{}, fmt.Errorf("EmbeddingsInput is empty (no Text / Image / ImageURI)")
	}
}

// resolveBlob looks up a "nexus-blob:<sha>" URI through the per-session
// blob store. Returns a clear error when the engine has no session
// (callers must inline bytes via EmbeddingsInput.Image instead).
func (p *Plugin) resolveBlob(sha string) ([]byte, string, error) {
	if p.session == nil {
		return nil, "", fmt.Errorf("blob URI cannot be resolved: plugin has no session workspace; inline bytes via EmbeddingsInput.Image instead")
	}
	if p.blobStore == nil {
		store, err := blobs.New(p.session.BlobsDir(), 0)
		if err != nil {
			return nil, "", fmt.Errorf("open blob store: %w", err)
		}
		p.blobStore = store
	}
	data, mime, err := p.blobStore.Get(sha)
	if err != nil {
		return nil, "", fmt.Errorf("blob get: %w", err)
	}
	return data, mime, nil
}

func buildDataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// --- Cohere wire types ---

type cohereInput struct {
	Content []cohereContent `json:"content"`
}

type cohereContent struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *cohereImageURL `json:"image_url,omitempty"`
}

type cohereImageURL struct {
	URL string `json:"url"`
}

type cohereEmbedRequest struct {
	Model          string        `json:"model"`
	InputType      string        `json:"input_type"`
	EmbeddingTypes []string      `json:"embedding_types"`
	Inputs         []cohereInput `json:"inputs"`
}

type cohereEmbedResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
	Meta struct {
		BilledUnits struct {
			InputTokens int `json:"input_tokens"`
			Images      int `json:"images"`
		} `json:"billed_units"`
	} `json:"meta"`
}

func (p *Plugin) embed(inputs []cohereInput, model string) ([][]float32, events.EmbeddingsUsage, error) {
	body := cohereEmbedRequest{
		Model:          model,
		InputType:      p.inputType,
		EmbeddingTypes: []string{"float"},
		Inputs:         inputs,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	endpoint := p.baseURL + embedPath
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("cohere returned HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var parsed cohereEmbedResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, events.EmbeddingsUsage{}, fmt.Errorf("decode: %w", err)
	}

	usage := events.EmbeddingsUsage{
		PromptTokens: parsed.Meta.BilledUnits.InputTokens,
		TotalTokens:  parsed.Meta.BilledUnits.InputTokens,
	}
	return parsed.Embeddings.Float, usage, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
