package client

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/frankbardon/nexus/pkg/events"
)

// server owns one MCP connection plus the projections (tools, resources,
// prompts) registered into Nexus on its behalf. Each server lives in its
// own goroutine for notification dispatch; mutation goes through the mutex.
type server struct {
	cfg    ServerConfig
	parent *Plugin
	logger *slog.Logger

	mu        sync.Mutex
	session   *mcp.ClientSession
	connected bool

	tools           map[string]*mcp.Tool             // raw name -> Tool
	staticResources map[string]*mcp.Resource         // catalog slug -> Resource
	templates       map[string]*mcp.ResourceTemplate // catalog slug -> Template
	prompts         map[string]*mcp.Prompt           // raw name -> Prompt
	resourceURIs    map[string]string                // catalog slug -> URI (for static resource invocation)
}

func newServer(cfg ServerConfig, parent *Plugin) *server {
	return &server{
		cfg:             cfg,
		parent:          parent,
		logger:          parent.logger.With("mcp_server", cfg.Name),
		tools:           map[string]*mcp.Tool{},
		staticResources: map[string]*mcp.Resource{},
		templates:       map[string]*mcp.ResourceTemplate{},
		prompts:         map[string]*mcp.Prompt{},
		resourceURIs:    map[string]string{},
	}
}

// connect spins up the transport, performs the initialize handshake, and
// kicks off the first tool/resource/prompt refresh. A failure leaves the
// server marked disconnected so subsequent dispatches return clean errors
// instead of nil-panicking on a half-built client.
func (s *server) connect(ctx context.Context) error {
	s.mu.Lock()
	if s.connected {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	transport, err := s.newTransport()
	if err != nil {
		return err
	}

	// Notification handlers are wired into the client up front; the SDK
	// dispatches each typed push to the matching callback. We refresh the
	// affected projection rather than parsing the notification's own
	// payload — it keeps the code paths the same as boot.
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "nexus.mcp.client",
		Version: version,
	}, &mcp.ClientOptions{
		ToolListChangedHandler:     s.onToolListChanged,
		ResourceListChangedHandler: s.onResourceListChanged,
		PromptListChangedHandler:   s.onPromptListChanged,
		ResourceUpdatedHandler:     s.onResourceUpdated,
	})

	// Connect performs the initialize handshake and protocol-version
	// negotiation internally; there is no manual Initialize step.
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	s.mu.Lock()
	s.session = session
	s.connected = true
	s.mu.Unlock()

	if err := s.refreshTools(ctx); err != nil {
		s.logger.Warn("mcp: tools/list failed", "error", err)
	}
	if s.cfg.Resources.Enabled {
		if err := s.refreshResources(ctx); err != nil {
			s.logger.Warn("mcp: resources/list failed", "error", err)
		}
	}
	if s.cfg.Prompts.Enabled {
		if err := s.refreshPrompts(ctx); err != nil {
			s.logger.Warn("mcp: prompts/list failed", "error", err)
		}
	}

	s.logger.Info("mcp server connected", "transport", s.cfg.Transport)
	return nil
}

// newTransport builds the SDK client transport for the configured mode.
// stdio launches a subprocess; http dials a streamable HTTP endpoint and
// injects any configured headers via a wrapping RoundTripper.
func (s *server) newTransport() (mcp.Transport, error) {
	switch s.cfg.Transport {
	case "stdio":
		cmd := exec.Command(s.cfg.Command, s.cfg.Args...)
		cmd.Env = append(os.Environ(), envSlice(s.cfg)...)
		return &mcp.CommandTransport{Command: cmd}, nil
	case "http":
		t := &mcp.StreamableClientTransport{Endpoint: s.cfg.URL}
		if len(s.cfg.Headers) > 0 {
			t.HTTPClient = &http.Client{
				Transport: &headerRoundTripper{
					headers: s.cfg.Headers,
					base:    http.DefaultTransport,
				},
			}
		}
		return t, nil
	default:
		return nil, fmt.Errorf("unknown transport %q", s.cfg.Transport)
	}
}

// headerRoundTripper sets a fixed set of headers on every outbound request.
// The official SDK's StreamableClientTransport exposes no header option, so
// header injection rides on the transport's *http.Client.
type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone so we never mutate a caller-owned Request.
	r := req.Clone(req.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// disconnect closes the underlying session and clears all per-server
// projections so reconnects start from a clean slate.
func (s *server) disconnect() {
	s.mu.Lock()
	sess := s.session
	s.session = nil
	s.connected = false
	for catalog := range s.tools {
		s.parent.unregisterToolRoute(toolName(s.cfg.Name, catalog))
	}
	for slug := range s.staticResources {
		s.parent.unregisterToolRoute(staticResourceToolName(s.cfg.Name, slug))
	}
	for slug := range s.templates {
		s.parent.unregisterToolRoute(templateResourceToolName(s.cfg.Name, slug))
	}
	for raw := range s.prompts {
		s.parent.unregisterPrompt("/" + s.parent.cfg.Defaults.CommandPrefix + "." + s.cfg.Name + "." + promptSlug(raw))
	}
	s.tools = map[string]*mcp.Tool{}
	s.staticResources = map[string]*mcp.Resource{}
	s.templates = map[string]*mcp.ResourceTemplate{}
	s.prompts = map[string]*mcp.Prompt{}
	s.resourceURIs = map[string]string{}
	s.mu.Unlock()

	if sess != nil {
		_ = sess.Close()
	}
}

func (s *server) getSession() *mcp.ClientSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

func (s *server) getPrompt(name string) (*mcp.Prompt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prompts[name]
	return p, ok
}

// onToolListChanged / onResourceListChanged / onPromptListChanged refresh
// the affected projection in response to the server's *_list_changed push.
func (s *server) onToolListChanged(_ context.Context, _ *mcp.ToolListChangedRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()
	if err := s.refreshTools(ctx); err != nil {
		s.logger.Warn("mcp: tools refresh after notification failed", "error", err)
	}
}

func (s *server) onResourceListChanged(_ context.Context, _ *mcp.ResourceListChangedRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()
	if err := s.refreshResources(ctx); err != nil {
		s.logger.Warn("mcp: resources refresh after notification failed", "error", err)
	}
}

func (s *server) onPromptListChanged(_ context.Context, _ *mcp.PromptListChangedRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()
	if err := s.refreshPrompts(ctx); err != nil {
		s.logger.Warn("mcp: prompts refresh after notification failed", "error", err)
	}
}

// onResourceUpdated emits mcp.resource.updated for a subscribed resource the
// server reports as changed. The URI arrives as a typed param.
func (s *server) onResourceUpdated(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
	if req == nil || req.Params == nil {
		return
	}
	uri := req.Params.URI
	if uri == "" {
		return
	}
	var title string
	s.mu.Lock()
	for _, r := range s.staticResources {
		if r.URI == uri {
			title = firstNonEmpty(r.Title, r.Name)
			break
		}
	}
	s.mu.Unlock()
	_ = s.parent.bus.Emit("mcp.resource.updated", events.MCPResourceUpdated{
		SchemaVersion: events.MCPResourceUpdatedVersion,
		Server:        s.cfg.Name,
		URI:           uri,
		Title:         title,
	})
}
