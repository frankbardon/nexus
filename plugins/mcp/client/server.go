package client

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

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
	client    *mcpclient.Client
	connected bool

	tools           map[string]mcp.Tool             // raw name -> Tool
	staticResources map[string]mcp.Resource         // catalog slug -> Resource
	templates       map[string]mcp.ResourceTemplate // catalog slug -> Template
	prompts         map[string]mcp.Prompt           // raw name -> Prompt
	resourceURIs    map[string]string               // catalog slug -> URI (for static resource invocation)
}

func newServer(cfg ServerConfig, parent *Plugin) *server {
	return &server{
		cfg:             cfg,
		parent:          parent,
		logger:          parent.logger.With("mcp_server", cfg.Name),
		tools:           map[string]mcp.Tool{},
		staticResources: map[string]mcp.Resource{},
		templates:       map[string]mcp.ResourceTemplate{},
		prompts:         map[string]mcp.Prompt{},
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

	var (
		c   *mcpclient.Client
		err error
	)

	switch s.cfg.Transport {
	case "stdio":
		c, err = mcpclient.NewStdioMCPClientWithOptions(s.cfg.Command, envSlice(s.cfg), s.cfg.Args)
		if err != nil {
			return fmt.Errorf("stdio transport: %w", err)
		}
	case "http":
		var opts []transport.StreamableHTTPCOption
		if len(s.cfg.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(s.cfg.Headers))
		}
		c, err = mcpclient.NewStreamableHttpClient(s.cfg.URL, opts...)
		if err != nil {
			return fmt.Errorf("http transport: %w", err)
		}
		if startErr := c.Start(ctx); startErr != nil {
			return fmt.Errorf("http start: %w", startErr)
		}
	default:
		return fmt.Errorf("unknown transport %q", s.cfg.Transport)
	}

	c.OnNotification(func(n mcp.JSONRPCNotification) {
		s.handleNotification(n)
	})

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.Capabilities = mcp.ClientCapabilities{}
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "nexus.mcp.client",
		Version: version,
	}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		_ = c.Close()
		return fmt.Errorf("initialize: %w", err)
	}

	s.mu.Lock()
	s.client = c
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

// disconnect closes the underlying client and clears all per-server
// projections so reconnects start from a clean slate.
func (s *server) disconnect() {
	s.mu.Lock()
	c := s.client
	s.client = nil
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
	s.tools = map[string]mcp.Tool{}
	s.staticResources = map[string]mcp.Resource{}
	s.templates = map[string]mcp.ResourceTemplate{}
	s.prompts = map[string]mcp.Prompt{}
	s.resourceURIs = map[string]string{}
	s.mu.Unlock()

	if c != nil {
		_ = c.Close()
	}
}

func (s *server) getClient() *mcpclient.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *server) getPrompt(name string) (mcp.Prompt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prompts[name]
	return p, ok
}

// handleNotification reacts to *_list_changed and resources/updated pushes.
// We refresh the affected projection rather than parsing the notification's
// own payload — it keeps the code paths the same as boot.
func (s *server) handleNotification(n mcp.JSONRPCNotification) {
	method := n.Method
	switch method {
	case mcp.MethodNotificationToolsListChanged:
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
		defer cancel()
		if err := s.refreshTools(ctx); err != nil {
			s.logger.Warn("mcp: tools refresh after notification failed", "error", err)
		}
	case mcp.MethodNotificationResourcesListChanged:
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
		defer cancel()
		if err := s.refreshResources(ctx); err != nil {
			s.logger.Warn("mcp: resources refresh after notification failed", "error", err)
		}
	case mcp.MethodNotificationPromptsListChanged:
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
		defer cancel()
		if err := s.refreshPrompts(ctx); err != nil {
			s.logger.Warn("mcp: prompts refresh after notification failed", "error", err)
		}
	case mcp.MethodNotificationResourceUpdated:
		uri, _ := n.Params.AdditionalFields["uri"].(string)
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
}
