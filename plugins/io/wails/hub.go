package wails

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/ui"
)

// ErrFileDialogUnavailable is returned by Hub.OpenFileDialog when no
// runtime is attached or the attached runtime does not implement
// FileDialogRuntime. Consumers can errors.Is against this to detect
// the fallback path.
var ErrFileDialogUnavailable = errors.New("wails: file dialog runtime not attached")

// Runtime is the minimal surface the Wails embedder must provide.
//
// The Nexus repo does not import github.com/wailsapp/wails/v2/pkg/runtime
// directly — the downstream Wails app wraps the relevant runtime calls
// (with the Wails context baked in) and hands in an implementation
// before calling engine.Boot.
//
// Event pub/sub (EmitEvent / OnEvent) is always required. OS-integration
// methods like OpenFileDialog are additive: the plugin feature-gates
// them on an interface assertion at call time so a minimal embedder
// that only wants message passing does not have to implement them. Any
// embedder shipping a real desktop app should implement the full
// surface so wrapper features (file dialogs, etc.) work.
type Runtime interface {
	// EmitEvent publishes a Wails runtime event to the single attached webview.
	EmitEvent(name string, optionalData ...any)
	// OnEvent registers a callback for inbound events from the webview.
	OnEvent(name string, callback func(optionalData ...any))
}

// FileDialogRuntime is the optional OS-integration surface for file
// dialogs. An embedder implements this in addition to Runtime to enable
// the io.file.open.request handler; otherwise that handler responds
// with Error="no file dialog runtime attached" and callers get a clean
// cancellation path.
type FileDialogRuntime interface {
	// OpenFileDialog presents a native single-file open dialog and
	// returns the chosen absolute path, the empty string if the user
	// cancelled, or a non-nil error if the dialog itself failed.
	OpenFileDialog(opts FileDialogOptions) (string, error)
}

// FileDialogOptions mirrors the subset of Wails's OpenDialogOptions we
// currently expose on the event bus. Kept as a plain struct in the
// wails package (not pkg/events) so the Runtime interface never leaks
// event-bus types into embedder code.
type FileDialogOptions struct {
	Title            string
	DefaultDirectory string
	Filters          []FileDialogFilter
}

// FileDialogFilter mirrors Wails's frontend.FileFilter.
type FileDialogFilter struct {
	DisplayName string
	Pattern     string
}

// Hub is the single-client transport wrapper for the Wails plugin.
//
// Unlike the browser plugin's Hub, there is no fanout, no subscription
// tracking, and no client lifecycle — a Wails app has exactly one
// attached webview for its lifetime. Hub exists only to give the adapter
// a uniform surface to call into.
type Hub struct {
	mu      sync.RWMutex
	runtime Runtime
	logger  *slog.Logger

	connectedAt time.Time

	onMessage func(env ui.Envelope)
}

// NewHub creates a new Wails hub. The Runtime may be nil at construction
// time; the embedder installs it via SetRuntime before engine.Boot.
func NewHub(logger *slog.Logger) *Hub {
	return &Hub{
		logger:      logger,
		connectedAt: time.Now(),
	}
}

// SetRuntime attaches the Wails runtime implementation. Called by the
// embedder (Wails main.go) from OnStartup, before engine.Boot.
func (h *Hub) SetRuntime(rt Runtime) {
	h.mu.Lock()
	h.runtime = rt
	h.mu.Unlock()

	if rt == nil {
		return
	}

	// Register the single inbound channel. Every message the webview
	// sends comes in on "nexus.input" as a serialized ui.Envelope.
	rt.OnEvent("nexus.input", func(data ...any) {
		h.handleInbound(data...)
	})
}

// OnMessage registers a callback for inbound webview messages.
func (h *Hub) OnMessage(fn func(env ui.Envelope)) {
	h.mu.Lock()
	h.onMessage = fn
	h.mu.Unlock()
}

// BroadcastEnvelope marshals an envelope and emits it to the webview on
// the "nexus" channel.
func (h *Hub) BroadcastEnvelope(env ui.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	h.mu.RLock()
	rt := h.runtime
	h.mu.RUnlock()

	if rt == nil {
		// Runtime not yet attached — drop silently. In practice the
		// embedder installs the runtime in OnStartup before Boot, so
		// this only fires during early-boot ordering bugs.
		h.logger.Debug("wails runtime not attached, dropping event", "type", env.Type)
		return nil
	}

	rt.EmitEvent("nexus", string(data))
	return nil
}

// OpenFileDialog forwards to the attached runtime's FileDialogRuntime
// implementation if one exists. Returns a sentinel error when no
// runtime is attached or when the runtime does not implement the
// optional FileDialogRuntime interface. Callers should treat the
// error as "native file dialog unavailable" and surface it to the
// user or fall back.
func (h *Hub) OpenFileDialog(opts FileDialogOptions) (string, error) {
	h.mu.RLock()
	rt := h.runtime
	h.mu.RUnlock()

	if rt == nil {
		return "", ErrFileDialogUnavailable
	}
	fdr, ok := rt.(FileDialogRuntime)
	if !ok {
		return "", ErrFileDialogUnavailable
	}
	return fdr.OpenFileDialog(opts)
}

// Sessions returns a single synthetic session entry for the attached webview.
func (h *Hub) Sessions() []ui.SessionInfo {
	return []ui.SessionInfo{
		{
			ID:          "wails",
			Transport:   "wails",
			ConnectedAt: h.connectedAt,
			UserAgent:   "wails-webview",
		},
	}
}

// Close is a no-op on the Wails hub — the webview lifetime is owned by
// the Wails process, not the plugin.
func (h *Hub) Close() {}

func (h *Hub) handleInbound(data ...any) {
	if len(data) == 0 {
		return
	}

	// Wails delivers payloads as whatever the JS side passed to
	// EventsEmit. The browser plugin's protocol is a JSON-serialized
	// ui.Envelope, so we require the Wails UI to send the same shape:
	// a single string argument containing the envelope JSON.
	raw, ok := data[0].(string)
	if !ok {
		// Tolerate map payloads too — the JS runtime may auto-decode
		// objects. Re-marshal and let the envelope decode handle it.
		b, err := json.Marshal(data[0])
		if err != nil {
			h.logger.Debug("wails inbound: unrecognized payload", "type", "non-string")
			return
		}
		raw = string(b)
	}

	var env ui.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		h.logger.Debug("wails inbound: envelope decode failed", "error", err)
		return
	}

	h.mu.RLock()
	cb := h.onMessage
	h.mu.RUnlock()

	if cb != nil {
		cb(env)
	}
}
