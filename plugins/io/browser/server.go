package browser

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

//go:embed static
var staticFS embed.FS

// Server handles HTTP requests and WebSocket upgrades for the browser UI.
type Server struct {
	hub          *Hub
	session      *engine.SessionWorkspace
	logger       *slog.Logger
	server       *http.Server
	addr         string
	capabilities map[string][]string
	bus          engine.EventBus

	// reloadMu serializes admin reload requests so two simultaneous POSTs
	// don't race on the result subscription. The engine's own ReloadConfig
	// is already mutex-protected; this guard is purely about pairing the
	// request and result events on the bus without crosstalk.
	reloadMu sync.Mutex
}

// NewServer creates a new HTTP server for the browser UI. capabilities is the
// boot-time capability → provider-IDs map, used to answer feature probes
// without string-matching specific plugin IDs. bus is the engine event bus,
// used to drive the admin reload-config endpoint.
func NewServer(hub *Hub, session *engine.SessionWorkspace, logger *slog.Logger, host string, port int, capabilities map[string][]string, bus engine.EventBus) *Server {
	addr := fmt.Sprintf("%s:%d", host, port)
	return &Server{
		hub:          hub,
		session:      session,
		logger:       logger,
		addr:         addr,
		capabilities: capabilities,
		bus:          bus,
	}
}

// Start begins listening for HTTP connections.
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Static files from embedded FS.
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("creating static sub-fs: %w", err)
	}
	fileServer := http.FileServer(http.FS(staticSub))

	mux.HandleFunc("GET /ws", s.handleWebSocket)
	mux.HandleFunc("GET /api/plugins", s.handlePlugins)
	mux.HandleFunc("GET /api/files", s.handleFileList)
	mux.HandleFunc("GET /api/files/", s.handleFileDownload)
	// Admin reload endpoint. No auth layer yet; alpha-only — operators
	// running the browser UI on a public network must front it with a
	// reverse proxy that handles auth.
	mux.HandleFunc("POST /admin/reload-config", s.handleReloadConfig)
	mux.Handle("GET /static/", http.StripPrefix("/static/", fileServer))
	mux.HandleFunc("GET /", s.handleIndex)

	s.server = &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.addr, err)
	}

	s.logger.Info("browser UI server started", "addr", "http://"+s.addr)

	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("server error", "error", err)
		}
	}()

	return nil
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// Addr returns the server's listen address.
func (s *Server) Addr() string {
	return s.addr
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		s.logger.Error("websocket accept failed", "error", err)
		return
	}

	clientID := fmt.Sprintf("browser-%d", time.Now().UnixNano())
	client := &Client{
		id:          clientID,
		conn:        conn,
		send:        make(chan []byte, 256),
		hub:         s.hub,
		done:        make(chan struct{}),
		userAgent:   r.UserAgent(),
		connectedAt: time.Now(),
	}

	s.hub.Register(client)
	s.hub.ServeClient(r.Context(), client)
}

func (s *Server) handlePlugins(w http.ResponseWriter, _ *http.Request) {
	type pluginResponse struct {
		Active   []string        `json:"active"`
		Features map[string]bool `json:"features"`
	}

	resp := pluginResponse{
		Active:   []string{},
		Features: make(map[string]bool),
	}

	if s.session != nil {
		data, err := s.session.ReadFile("metadata/plugins.json")
		if err == nil {
			var manifest struct {
				Active []string `json:"active"`
			}
			if json.Unmarshal(data, &manifest) == nil {
				resp.Active = manifest.Active
				for _, id := range manifest.Active {
					if strings.HasPrefix(id, "nexus.planner.") {
						resp.Features["planner"] = true
					}
					if id == "nexus.observe.thinking" {
						resp.Features["thinking"] = true
					}
					if id == "nexus.skills" {
						resp.Features["skills"] = true
					}
					if strings.HasPrefix(id, "nexus.memory.") {
						resp.Features["memory"] = true
					}
				}
				if len(s.capabilities["control.cancel"]) > 0 {
					resp.Features["cancel"] = true
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleFileList(w http.ResponseWriter, _ *http.Request) {
	type fileEntry struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}

	files := []fileEntry{}

	if s.session != nil {
		filesDir := s.session.FilesDir()
		_ = filepath.WalkDir(filesDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(filesDir, path)
			if err != nil || rel == "." {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			files = append(files, fileEntry{
				Name:  d.Name(),
				Path:  rel,
				IsDir: d.IsDir(),
				Size:  info.Size(),
			})
			return nil
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	relPath := strings.TrimPrefix(r.URL.Path, "/api/files/")
	if relPath == "" {
		http.Error(w, "file path required", http.StatusBadRequest)
		return
	}

	// Prevent directory traversal.
	clean := filepath.Clean(relPath)
	if strings.Contains(clean, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if s.session == nil {
		http.Error(w, "no active session", http.StatusServiceUnavailable)
		return
	}

	data, err := s.session.ReadFile(filepath.Join("files", clean))
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Write(data)
}

// handleReloadConfig accepts an empty POST body (re-read the original config
// path) or {"path": "/abs/path/to/new.yaml"}. The endpoint waits for the
// matching core.config.reload.result event before responding so the operator
// gets a synchronous OK / error rather than a fire-and-forget. Subscribing
// before emitting closes the race window where a fast engine path could fire
// the result before the subscription registers.
func (s *Server) handleReloadConfig(w http.ResponseWriter, r *http.Request) {
	if s.bus == nil {
		http.Error(w, "engine bus unavailable", http.StatusServiceUnavailable)
		return
	}

	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	type reloadBody struct {
		Path string `json:"path"`
	}
	var body reloadBody
	if r.Body != nil {
		raw, err := io.ReadAll(r.Body)
		if err == nil && len(raw) > 0 {
			if jerr := json.Unmarshal(raw, &body); jerr != nil {
				http.Error(w, fmt.Sprintf("invalid JSON body: %v", jerr), http.StatusBadRequest)
				return
			}
		}
	}

	resultCh := make(chan events.ConfigReloadResult, 1)
	unsub := s.bus.Subscribe("core.config.reload.result", func(ev engine.Event[any]) {
		res, ok := ev.Payload.(events.ConfigReloadResult)
		if !ok {
			return
		}
		if res.Source != "browser-admin" {
			return
		}
		select {
		case resultCh <- res:
		default:
		}
	})
	defer unsub()

	if err := s.bus.Emit("core.config.reload.request", events.ConfigReloadRequest{
		SchemaVersion: events.ConfigReloadRequestVersion,
		Path:          body.Path,
		Source:        "browser-admin",
	}); err != nil {
		http.Error(w, fmt.Sprintf("emit failed: %v", err), http.StatusInternalServerError)
		return
	}

	// 30s aligns with the engine's default drain timeout. A reload that
	// takes longer almost certainly indicates a stuck plugin Init; the
	// admin caller gets a 504 and operators investigate via journal/logs.
	select {
	case res := <-resultCh:
		w.Header().Set("Content-Type", "application/json")
		if !res.OK {
			w.WriteHeader(http.StatusBadRequest)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    res.OK,
			"error": res.ErrorMessage,
		})
	case <-time.After(30 * time.Second):
		http.Error(w, "reload timed out", http.StatusGatewayTimeout)
	case <-r.Context().Done():
		return
	}
}
