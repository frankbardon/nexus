package hitl

// registry.go implements the filesystem-backed registry that lets external
// tools (CLI, webhook handlers, Slack callbacks) answer pending HITL
// requests asynchronously.
//
// On hitl.requested the registry writes <dir>/<request-id>.request.yaml.
// An fsnotify watcher reacts to <dir>/<request-id>.response.yaml files
// appearing, parses them, emits the typed HITLResponse on the bus, and
// deletes both files. The existing IO-driven response path stays — IO
// plugins still emit hitl.responded directly. The fsnotify watcher is an
// additional source of responses; first response wins because the pending
// channel is single-shot.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// emitter is the narrow slice of engine.EventBus the registry actually
// needs. Declaring it locally lets tests pass a stand-in without
// implementing the full bus interface.
type emitter interface {
	Emit(eventType string, payload any) error
}

const (
	requestSuffix  = ".request.yaml"
	responseSuffix = ".response.yaml"
)

// registry persists HITL requests to disk and watches for response files.
// Wired by the hitl plugin when registry.enabled is set in config.
type registry struct {
	dir    string
	logger *slog.Logger

	bus    emitter // dispatch target for parsed responses
	fsw    *fsnotify.Watcher
	done   chan struct{}
	closed bool

	mu      sync.Mutex
	written map[string]string // requestID -> request file path (for cleanup on Shutdown)
}

// newRegistry expands dir, ensures it exists, and starts an fsnotify watch.
// Returns an error if the directory cannot be created or watched.
func newRegistry(dir string, logger *slog.Logger, bus emitter) (*registry, error) {
	expanded := engine.ExpandPath(dir)
	if expanded == "" {
		return nil, errors.New("registry.dir is required when registry.enabled is true")
	}
	if err := os.MkdirAll(expanded, 0o755); err != nil {
		return nil, fmt.Errorf("create hitl registry dir %q: %w", expanded, err)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("hitl registry: fsnotify: %w", err)
	}
	if err := fsw.Add(expanded); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("hitl registry: watch %q: %w", expanded, err)
	}
	r := &registry{
		dir:     expanded,
		logger:  logger,
		bus:     bus,
		fsw:     fsw,
		done:    make(chan struct{}),
		written: make(map[string]string),
	}
	go r.run()
	return r, nil
}

// Dir returns the resolved (tilde-expanded) registry directory.
func (r *registry) Dir() string { return r.dir }

// persistRequest writes the request as <dir>/<id>.request.yaml.
// Path traversal is prevented by validating the request ID.
func (r *registry) persistRequest(req events.HITLRequest) error {
	if err := validateRequestID(req.ID); err != nil {
		return err
	}
	path := r.requestPath(req.ID)
	rec := requestFile{
		RequestID:       req.ID,
		SessionID:       req.SessionID,
		TurnID:          req.TurnID,
		RequesterPlugin: req.RequesterPlugin,
		ActionKind:      req.ActionKind,
		ActionRef:       req.ActionRef,
		Mode:            string(req.Mode),
		Choices:         choiceFilesFromEvents(req.Choices),
		DefaultChoiceID: req.DefaultChoiceID,
		Prompt:          req.Prompt,
		Deadline:        req.Deadline,
		CreatedAt:       time.Now().UTC(),
	}
	data, err := yaml.Marshal(&rec)
	if err != nil {
		return fmt.Errorf("hitl registry: marshal request %s: %w", req.ID, err)
	}
	if err := writeFileAtomic(path, data); err != nil {
		return err
	}
	r.mu.Lock()
	r.written[req.ID] = path
	r.mu.Unlock()
	return nil
}

// removeRequest deletes the request file (and any orphaned response file)
// for the given request ID. No-ops if the files are already gone.
func (r *registry) removeRequest(requestID string) {
	r.mu.Lock()
	path, ok := r.written[requestID]
	if ok {
		delete(r.written, requestID)
	}
	r.mu.Unlock()
	if !ok {
		path = r.requestPath(requestID)
	}
	_ = os.Remove(path)
	_ = os.Remove(r.responsePath(requestID))
}

// Close stops the watcher and removes pending request files written by this
// registry. Safe to call multiple times.
func (r *registry) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	pending := make(map[string]string, len(r.written))
	for k, v := range r.written {
		pending[k] = v
	}
	r.written = nil
	r.mu.Unlock()

	close(r.done)
	_ = r.fsw.Close()

	// Remove any request files still on disk so a stale registry directory
	// doesn't stir up CLI/HTTP confusion on the next boot.
	for _, p := range pending {
		_ = os.Remove(p)
	}
}

func (r *registry) run() {
	for {
		select {
		case <-r.done:
			return
		case ev, ok := <-r.fsw.Events:
			if !ok {
				return
			}
			r.dispatch(ev)
		case err, ok := <-r.fsw.Errors:
			if !ok {
				return
			}
			r.logger.Warn("hitl registry: fsnotify error", "err", err)
		}
	}
}

// dispatch fires when a response file appears in the watched directory.
// It parses the file, emits hitl.responded, and deletes both the response
// and the matching request file.
func (r *registry) dispatch(ev fsnotify.Event) {
	if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
		return
	}
	name := filepath.Base(ev.Name)
	if !strings.HasSuffix(name, responseSuffix) {
		return
	}
	requestID := strings.TrimSuffix(name, responseSuffix)
	if requestID == "" {
		return
	}

	resp, err := readResponseFile(ev.Name)
	if err != nil {
		r.logger.Warn("hitl registry: read response", "path", ev.Name, "err", err)
		return
	}
	if resp.RequestID == "" {
		resp.RequestID = requestID
	}

	_ = r.bus.Emit("hitl.responded", resp)

	// Delete both files so the directory does not accumulate. Removal is
	// best-effort: if the file is already gone (e.g. a second writer racing)
	// the error is benign.
	_ = os.Remove(ev.Name)
	r.mu.Lock()
	delete(r.written, requestID)
	r.mu.Unlock()
	_ = os.Remove(r.requestPath(requestID))
}

func (r *registry) requestPath(requestID string) string {
	return filepath.Join(r.dir, requestID+requestSuffix)
}

func (r *registry) responsePath(requestID string) string {
	return filepath.Join(r.dir, requestID+responseSuffix)
}

// validateRequestID rejects IDs that would let a writer escape the registry
// directory or collide with the suffixes the registry uses internally.
func validateRequestID(id string) error {
	if id == "" {
		return errors.New("hitl request id is empty")
	}
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("hitl request id %q contains a path separator", id)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("hitl request id %q is invalid", id)
	}
	return nil
}

// writeFileAtomic writes data to path via a same-directory tempfile then
// renames into place so an fsnotify watcher cannot observe a partial file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("hitl registry: tempfile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("hitl registry: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("hitl registry: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("hitl registry: rename to %s: %w", path, err)
	}
	return nil
}

// readResponseFile parses a response YAML into a typed HITLResponse.
// Exported indirectly via the CLI (which reuses this helper).
func readResponseFile(path string) (events.HITLResponse, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return events.HITLResponse{SchemaVersion: events.HITLResponseVersion}, fmt.Errorf("read %s: %w", path, err)
	}
	var rec responseFile
	if err := yaml.Unmarshal(data, &rec); err != nil {
		return events.HITLResponse{SchemaVersion: events.HITLResponseVersion}, fmt.Errorf("parse %s: %w", path, err)
	}
	return events.HITLResponse{SchemaVersion: events.HITLResponseVersion, RequestID: rec.RequestID,
		ChoiceID:      rec.ChoiceID,
		FreeText:      rec.FreeText,
		EditedPayload: rec.EditedPayload,
		Cancelled:     rec.Cancelled,
		CancelReason:  rec.CancelReason,
	}, nil
}

// readRequestFile parses a request YAML into the on-disk record. Exported
// indirectly via the CLI to render `nexus hitl list`.
func readRequestFile(path string) (requestFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return requestFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	var rec requestFile
	if err := yaml.Unmarshal(data, &rec); err != nil {
		return requestFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return rec, nil
}

// writeResponseFile is the CLI's writer counterpart: it produces a response
// YAML in the registry directory the running engine is watching. Atomic
// rename keeps fsnotify from picking up half-written files.
func writeResponseFile(dir, requestID string, resp events.HITLResponse) (string, error) {
	if err := validateRequestID(requestID); err != nil {
		return "", err
	}
	rec := responseFile{
		RequestID:     requestID,
		ChoiceID:      resp.ChoiceID,
		FreeText:      resp.FreeText,
		EditedPayload: resp.EditedPayload,
		Cancelled:     resp.Cancelled,
		CancelReason:  resp.CancelReason,
	}
	data, err := yaml.Marshal(&rec)
	if err != nil {
		return "", fmt.Errorf("marshal response: %w", err)
	}
	path := filepath.Join(dir, requestID+responseSuffix)
	if err := writeFileAtomic(path, data); err != nil {
		return "", err
	}
	return path, nil
}

// listRequestFiles enumerates the registry directory, returning one parsed
// requestFile per *.request.yaml entry. Used by `nexus hitl list`.
func listRequestFiles(dir string) ([]requestFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	out := make([]requestFile, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, requestSuffix) {
			continue
		}
		rec, err := readRequestFile(filepath.Join(dir, name))
		if err != nil {
			// Skip unparsable entries but keep going so a single bad file
			// doesn't blank the operator's CLI listing.
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// requestFile is the on-disk YAML shape for a pending HITL request. Field
// names mirror the events.HITLRequest JSON tags for operator readability.
type requestFile struct {
	RequestID       string         `yaml:"request_id"`
	SessionID       string         `yaml:"session_id,omitempty"`
	TurnID          string         `yaml:"turn_id,omitempty"`
	RequesterPlugin string         `yaml:"requester_plugin,omitempty"`
	ActionKind      string         `yaml:"action_kind,omitempty"`
	ActionRef       map[string]any `yaml:"action_ref,omitempty"`
	Mode            string         `yaml:"mode,omitempty"`
	Choices         []choiceFile   `yaml:"choices,omitempty"`
	DefaultChoiceID string         `yaml:"default_choice_id,omitempty"`
	Prompt          string         `yaml:"prompt,omitempty"`
	Deadline        time.Time      `yaml:"deadline,omitempty"`
	CreatedAt       time.Time      `yaml:"created_at,omitempty"`
}

// choiceFile is the on-disk YAML shape for a single HITL choice.
type choiceFile struct {
	ID            string         `yaml:"id"`
	Label         string         `yaml:"label"`
	Kind          string         `yaml:"kind,omitempty"`
	EditedPayload map[string]any `yaml:"edited_payload,omitempty"`
}

// responseFile is the on-disk YAML shape for a response.
type responseFile struct {
	RequestID     string         `yaml:"request_id"`
	ChoiceID      string         `yaml:"choice_id,omitempty"`
	FreeText      string         `yaml:"free_text,omitempty"`
	EditedPayload map[string]any `yaml:"edited_payload,omitempty"`
	Cancelled     bool           `yaml:"cancelled,omitempty"`
	CancelReason  string         `yaml:"cancel_reason,omitempty"`
}

func choiceFilesFromEvents(in []events.HITLChoice) []choiceFile {
	if len(in) == 0 {
		return nil
	}
	out := make([]choiceFile, len(in))
	for i, c := range in {
		out[i] = choiceFile{
			ID:            c.ID,
			Label:         c.Label,
			Kind:          string(c.Kind),
			EditedPayload: c.EditedPayload,
		}
	}
	return out
}
