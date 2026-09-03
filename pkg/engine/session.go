package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/blobs"
)

// SessionConfigSnapshotPath returns the path to a session's config snapshot.
// It uses the sessions root from the provided config file (or the default) to locate
// the session directory. The configPath is needed to determine the sessions root dir.
func SessionConfigSnapshotPath(configPath string, sessionID string) (string, error) {
	var root string
	if configPath == "" {
		root = DefaultConfig().Core.Sessions.Root
	} else {
		cfg, err := LoadConfig(configPath)
		if err != nil {
			// Fall back to default root if config can't be loaded.
			root = DefaultConfig().Core.Sessions.Root
		} else {
			root = cfg.Core.Sessions.Root
		}
	}

	root = ExpandPath(root)
	snapshotPath := filepath.Join(root, sessionID, "metadata", "config-snapshot.yaml")

	if _, err := os.Stat(snapshotPath); err != nil {
		return "", fmt.Errorf("config snapshot not found for session %q: %w", sessionID, err)
	}

	return snapshotPath, nil
}

// SessionWorkspace manages a session's file-based workspace.
type SessionWorkspace struct {
	ID        string
	RootDir   string
	StartedAt time.Time
	bus       EventBus

	// blobPush is the object-store write-through sink for content-addressed
	// blobs, installed by Boot and nil in every build or configuration with no
	// backend. It is not on the bus: blobs are the one subtree whose push
	// needs no event, because sha256 addressing makes a duplicate upload a
	// no-op rather than a race, and an announcement per blob would put traffic
	// on the hottest tool paths in a session to say something the key already
	// says. See session_objectstore_blobs.go.
	//
	// Atomic because the write and the reads genuinely are on different
	// goroutines: Boot installs it, and the reads come from whichever tool
	// goroutine happens to Put a blob — including nexus.embeddings.cohere,
	// which opens its store lazily inside a request handler rather than at
	// Init.
	blobPush atomic.Pointer[blobPushFunc]
}

// blobPushFunc receives the two files one blob is made of. Named rather than
// used inline because atomic.Pointer needs a type to point at.
type blobPushFunc func(binPath, metaPath string)

// SessionMeta holds metadata about a session.
type SessionMeta struct {
	ID                   string            `json:"id"`
	StartedAt            time.Time         `json:"started_at"`
	EndedAt              *time.Time        `json:"ended_at,omitempty"`
	Profile              string            `json:"profile"`
	Plugins              []string          `json:"plugins"`
	Labels               map[string]string `json:"labels"`
	TurnCount            int               `json:"turn_count"`
	TokensUsed           int               `json:"tokens_used"`
	PromptTokensUsed     int               `json:"prompt_tokens_used"`
	CompletionTokensUsed int               `json:"completion_tokens_used"`
	CostUSD              float64           `json:"cost_usd"`
	Status               string            `json:"status"`
}

// NewSessionWorkspace creates a new session workspace with the standard directory structure.
func NewSessionWorkspace(rootDir string, bus EventBus) (*SessionWorkspace, error) {
	return newSessionWorkspaceAt(rootDir, GenerateID(), bus)
}

// newSessionWorkspaceAt creates a session workspace under a caller-supplied ID.
//
// Split out of NewSessionWorkspace for the object-store hydrate path: resuming
// a session ID the store has never seen must yield a tree indistinguishable
// from a brand-new local session, and the only way to guarantee
// "indistinguishable" is to run the same code rather than a second
// almost-identical directory-creation routine that will drift.
//
// Unexported because minting a session under a caller-chosen ID is an
// engine-internal concern; every external caller wants a fresh ID.
func newSessionWorkspaceAt(rootDir string, id string, bus EventBus) (*SessionWorkspace, error) {
	now := time.Now()
	sessionDir := filepath.Join(rootDir, id)

	// The bus is attached only after the bootstrap metadata write below, so
	// that write stays silent. Engine.prepareSession documents that creating
	// the workspace emits no bus events, and that is a hard invariant rather
	// than a tidiness preference: it runs before startJournal, and the bus
	// assigns a dispatch seq to every event whether or not the journal's
	// wildcard is subscribed yet. An event here would consume seq 1 and never
	// reach the writer, whose drain writes only in contiguous seq order — so
	// the missing seq would stall every later envelope forever and the journal
	// would be empty. StartSession re-saves the metadata once the journal is
	// running, so the file is still announced, just not from in here.
	s := &SessionWorkspace{
		ID:        id,
		RootDir:   sessionDir,
		StartedAt: now,
	}

	// Create directory structure.
	dirs := []string{
		s.ContextDir(),
		s.FilesDir(),
		filepath.Join(sessionDir, "plugins"),
		s.MetadataDir(),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating session directory %s: %w", dir, err)
		}
	}

	// Write initial metadata.
	meta := &SessionMeta{
		ID:        id,
		StartedAt: now,
		Labels:    make(map[string]string),
		Status:    "active",
	}
	if err := s.SaveMeta(meta); err != nil {
		return nil, fmt.Errorf("saving initial metadata: %w", err)
	}

	s.bus = bus
	return s, nil
}

// LoadSessionWorkspace opens an existing session workspace by ID.
// It reads the session metadata and returns a workspace pointing at the existing directory.
func LoadSessionWorkspace(rootDir string, sessionID string, bus EventBus) (*SessionWorkspace, error) {
	sessionDir := filepath.Join(rootDir, sessionID)

	info, err := os.Stat(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("session %q not found: %w", sessionID, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("session path %q is not a directory", sessionDir)
	}

	// Bus attached after the reactivation write below, for the same reason as
	// newSessionWorkspaceAt: this runs before startJournal.
	s := &SessionWorkspace{
		ID:      sessionID,
		RootDir: sessionDir,
	}

	// Read existing metadata to restore StartedAt.
	meta, err := s.SessionMetadata()
	if err != nil {
		return nil, fmt.Errorf("reading session metadata: %w", err)
	}
	s.StartedAt = meta.StartedAt

	// Mark session as active again.
	meta.EndedAt = nil
	meta.Status = "active"
	if err := s.SaveMeta(meta); err != nil {
		return nil, fmt.Errorf("updating session metadata: %w", err)
	}

	s.bus = bus
	return s, nil
}

// ContextDir returns the path to the context subdirectory.
func (s *SessionWorkspace) ContextDir() string {
	return filepath.Join(s.RootDir, "context")
}

// FilesDir returns the path to the files subdirectory.
func (s *SessionWorkspace) FilesDir() string {
	return filepath.Join(s.RootDir, "files")
}

// BlobsDir returns the path to the per-session blobs subdirectory used by
// pkg/engine/blobs. Lazily created by the blob store on first Put — no
// directory is forced at session boot, and none by opening a store either, so
// a session whose tools never produce a blob has no blobs/ directory at all.
func (s *SessionWorkspace) BlobsDir() string {
	return filepath.Join(s.RootDir, "blobs")
}

// BlobStore opens the session's content-addressed blob store, wired for
// object-store write-through.
//
// This is the door every in-tree caller should use instead of calling
// blobs.New(session.BlobsDir(), ...) directly. The difference is not the path —
// it is the PutHook: a store opened here pushes each new blob to a configured
// object store the moment it lands, rather than waiting for the whole-tree
// snapshot at the next agent.turn.end. A store opened by hand still works and
// still ends up in the bucket at the turn boundary; it just does not get the
// write-through.
//
// The hook is installed unconditionally and costs one atomic load per *new*
// blob when no backend is configured, which is what keeps the default path
// indistinguishable from the one that existed before write-through: no
// goroutine, no event, no upload, no branch anywhere else.
//
// byteBudget is the local LRU soft cap, passed straight through. It bounds
// local disk only: evicting a blob here never deletes the object the store
// already holds. See the blobs package doc.
func (s *SessionWorkspace) BlobStore(byteBudget int64, opts ...blobs.Option) (*blobs.Store, error) {
	if s == nil {
		return nil, fmt.Errorf("opening blob store: no session workspace")
	}
	return blobs.New(s.BlobsDir(), byteBudget, append([]blobs.Option{blobs.WithPutHook(s.onBlobPut)}, opts...)...)
}

// onBlobPut is the blobs.PutHook every store from BlobStore carries. It runs
// on whichever goroutine wrote the blob, outside the store's mutex, so it must
// stay cheap: the installed sink hands the two paths to a background worker
// and returns.
func (s *SessionWorkspace) onBlobPut(h blobs.Handle, metaPath string) {
	if s == nil {
		return
	}
	push := s.blobPush.Load()
	if push == nil {
		return
	}
	(*push)(h.Path, metaPath)
}

// setBlobPush installs (or with a nil fn, removes) the write-through sink.
// Engine-only: Boot installs it once a backend and a session both exist, and
// Stop removes it before the backend handle is released so a late blob cannot
// reach a closed backend.
func (s *SessionWorkspace) setBlobPush(fn blobPushFunc) {
	if s == nil {
		return
	}
	if fn == nil {
		s.blobPush.Store(nil)
		return
	}
	s.blobPush.Store(&fn)
}

// MetadataDir returns the path to the metadata subdirectory.
func (s *SessionWorkspace) MetadataDir() string {
	return filepath.Join(s.RootDir, "metadata")
}

// PluginDir returns the path to a plugin-specific directory, creating it lazily.
func (s *SessionWorkspace) PluginDir(pluginID string) string {
	dir := filepath.Join(s.RootDir, "plugins", pluginID)
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// WriteFile writes data to a file within the session workspace.
// It emits session.file.created or session.file.updated events.
//
// The emitted payload carries an append-aware delta — "offset" and
// "bytes_added" — alongside "size"; see the comment on AppendFile for why the
// delta lives on this payload rather than in a third event type, and for what
// a sync backend may and may not conclude from it. A whole-file write reports
// offset 0 and bytes_added == size, which is the honest reading of "every byte
// of this object is new".
func (s *SessionWorkspace) WriteFile(subpath string, data []byte) error {
	fullPath := filepath.Join(s.RootDir, subpath)

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", subpath, err)
	}

	existed := s.FileExists(subpath)

	if err := os.WriteFile(fullPath, data, 0o644); err != nil {
		return fmt.Errorf("writing file %s: %w", subpath, err)
	}

	if s.bus != nil {
		eventType := "session.file.created"
		if existed {
			eventType = "session.file.updated"
		}
		_ = s.bus.Emit(eventType, map[string]any{
			"session_id": s.ID,
			"path":       subpath,
			"size":       len(data),
			// offset 0 with bytes_added == size is not padding to make the
			// two emitters look alike: it is what actually happened. The
			// previous contents are gone, so the changed region really does
			// start at byte 0 and really does span the whole object, and a
			// backend that coalesces on "offset > 0" correctly refuses to
			// treat this as an append.
			"offset":      0,
			"bytes_added": len(data),
		})
	}

	return nil
}

// ReadFile reads a file from the session workspace.
func (s *SessionWorkspace) ReadFile(subpath string) ([]byte, error) {
	fullPath := filepath.Join(s.RootDir, subpath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("reading file %s: %w", subpath, err)
	}
	return data, nil
}

// AppendFile appends data to a file in the session workspace.
// It emits session.file.created or session.file.updated events.
//
// It reuses WriteFile's two event types rather than introducing a third
// "appended" type. Every existing subscriber — nexus.io.tui, nexus.io.browser,
// nexus.io.wails, nexus.tool.fileio — treats these as "this path changed,
// re-read it", which is exactly true of an append; a new type would have left
// all of them silently blind to appends until each was updated, which is the
// same invisibility this emission exists to remove. An append-aware delta
// (how many bytes landed, and at what offset) is additional information about
// the same change and belongs on the payload, not in a separate event type.
//
// # The append-aware delta, and what it does not promise
//
// The payload carries "offset" (where the change begins) and "bytes_added"
// (how many bytes landed there) beside "size" (the file size afterwards). The
// invariant across both emitters is that the region [offset, offset+bytes_added)
// is the only part of the object that changed, so:
//
//	offset == 0 && bytes_added == size   the whole object is new (WriteFile)
//	offset >  0 && offset+bytes_added == size   a pure append; every byte
//	                                            before offset is byte-identical
//	                                            to what the last event described
//
// A distinct session.file.appended event was the rejected alternative. It
// would have carried exactly the same three numbers, cost every existing
// subscriber a code change to keep seeing appends at all, and — because
// conversation.jsonl is written by both WriteFile and AppendFile — forced each
// of them to merge two event streams to reconstruct one file's history. The
// delta is more information about a change subscribers already receive, not a
// different kind of change; putting it in a separate type would have split one
// fact across two topics. Adding map keys is also forward-compatible with the
// untyped payload: a subscriber that does not read them is unaffected.
//
// Object stores have no append primitive. Nothing here lets a backend write
// the appended bytes into an existing object — S3, GCS and every
// S3-compatible store replace whole objects, and a "multipart append" is a
// different object with a different lifecycle, not an edit. What the delta
// buys is the ability to *coalesce and defer*: a backend that knows the last
// 200 events on context/conversation.jsonl only added bytes to the tail knows
// it can collapse them into one upload of the current file at the next
// boundary, and knows it has not missed a rewrite in between. Anyone reading
// "offset" as "seek here and write bytes_added bytes into the bucket" has
// misread it.
//
// Until that emission existed the highest-churn writers in a session were
// invisible to the bus: conversation history (context/conversation.jsonl),
// turn timing, compaction output, shell history and the HITL cache all append
// here. conversation.jsonl was the sharpest case — WriteFile covered its
// rewrites and nothing covered its appends, so it looked correct in a smoke
// test and dropped data in a real run.
func (s *SessionWorkspace) AppendFile(subpath string, data []byte) error {
	fullPath := filepath.Join(s.RootDir, subpath)

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", subpath, err)
	}

	// Sampled before the open: O_CREATE makes the file exist as a side effect,
	// so checking afterwards would report every first append as an update and
	// no subscriber would ever see the file appear.
	existed := s.FileExists(subpath)

	f, err := os.OpenFile(fullPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening file %s for append: %w", subpath, err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("appending to file %s: %w", subpath, err)
	}

	if s.bus != nil {
		// size is the size of the file after the write, not the length of the
		// appended chunk, so the field means the same thing here as it does in
		// WriteFile and a subscriber never has to know which helper produced
		// the event. Stat goes through the open descriptor rather than the
		// path so a concurrent append cannot make the two disagree. A failed
		// Stat reports 0 rather than dropping the event: a subscriber that
		// re-reads the path is still correct with a wrong size, and is
		// unrecoverably wrong with no event at all.
		var size int
		if fi, statErr := f.Stat(); statErr == nil {
			size = int(fi.Size())
		}

		// offset comes from the descriptor's own position after the write, not
		// from size-len(data). Under O_APPEND the kernel seeks to end-of-file
		// and writes atomically, leaving the position immediately after *our*
		// bytes — so this stays exact even when another writer appends to the
		// same path between our write and our Stat, where size-len(data) would
		// silently point into someone else's chunk.
		//
		// Failure falls back to offset 0, which reads as "the whole object
		// changed". That is the conservative direction: a backend re-uploads a
		// file it could have coalesced, rather than coalescing a change it
		// should have treated as a rewrite.
		offset := 0
		if end, seekErr := f.Seek(0, io.SeekCurrent); seekErr == nil && end >= int64(len(data)) {
			offset = int(end) - len(data)
		}

		eventType := "session.file.created"
		if existed {
			eventType = "session.file.updated"
		}
		_ = s.bus.Emit(eventType, map[string]any{
			"session_id":  s.ID,
			"path":        subpath,
			"size":        size,
			"offset":      offset,
			"bytes_added": len(data),
		})
	}

	return nil
}

// ListFiles lists files under a subdirectory in the session workspace.
func (s *SessionWorkspace) ListFiles(subpath string) ([]string, error) {
	fullPath := filepath.Join(s.RootDir, subpath)

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return nil, fmt.Errorf("listing files in %s: %w", subpath, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// FileExists returns true if a file exists in the session workspace.
func (s *SessionWorkspace) FileExists(subpath string) bool {
	fullPath := filepath.Join(s.RootDir, subpath)
	_, err := os.Stat(fullPath)
	return err == nil
}

// SessionMeta reads and returns the session metadata.
func (s *SessionWorkspace) SessionMetadata() (*SessionMeta, error) {
	data, err := os.ReadFile(filepath.Join(s.MetadataDir(), "session.json"))
	if err != nil {
		return nil, fmt.Errorf("reading session metadata: %w", err)
	}

	var meta SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parsing session metadata: %w", err)
	}
	return &meta, nil
}

// SaveMeta writes session metadata to disk.
// It emits session.file.created or session.file.updated for
// metadata/session.json.
//
// The write is routed through WriteFile rather than calling os.WriteFile and
// emitting alongside it. metadata/session.json is rewritten on every
// llm.response (token and cost totals) and every agent.turn.end (turn count),
// so it is one of the most frequently changed files in a session and was
// previously changing without a single event. Delegating means there is one
// implementation of "write a session file and announce it" — a second copy
// here would be free to drift in event type, payload shape or permissions,
// and the two would then disagree about the most-written file in the tree.
func (s *SessionWorkspace) SaveMeta(meta *SessionMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling session metadata: %w", err)
	}

	// Slash-separated literal, not filepath.Join: subpath is both a path
	// fragment and the session-relative "path" carried on the emitted event,
	// and every other caller of WriteFile spells it with forward slashes.
	// filepath.Join under the session root handles the separator on the way to
	// disk; joining here would put a backslash on the wire on Windows.
	if err := s.WriteFile("metadata/session.json", data); err != nil {
		return fmt.Errorf("writing session metadata: %w", err)
	}
	return nil
}

// AnnounceWrite announces a whole-file write that a caller made under the
// session tree with its own os.* call instead of routing through WriteFile.
//
// # Why a second door exists at all
//
// WriteFile is the door, and everything that can use it should. But a handful
// of writers genuinely cannot: they hold a long-lived *os.File (the scene
// patch journal), they write through temp-file + rename for atomicity (the ICM
// artifact writer), or they own a directory tree whose layout is theirs rather
// than the workspace's. Forcing those through WriteFile would mean either
// giving up atomicity or re-implementing it inside the workspace for one
// caller. Announcing after the fact is the honest alternative: the bytes
// landed the way that writer needs them to, and the bus still learns about it.
//
// The rejected alternative was an fsnotify watcher over the session root.
// It buys completeness without any call-site changes, and costs a watch
// descriptor per directory, a rename storm to debounce on every atomic write,
// and — fatally — no way to distinguish "the writer finished" from "the writer
// is halfway through", which is exactly the distinction a sync backend needs.
//
// # Contract
//
// existed selects created vs updated and must be sampled *before* the write;
// after it, every path exists. The emitted payload is byte-for-byte the shape
// WriteFile emits — see AppendFile for what "offset" and "bytes_added" do and
// do not promise. A whole-file write reports offset 0 and bytes_added == size.
//
// Announcing never fails the write that preceded it, so there is no error to
// return: a dropped announcement costs a backend one deferred upload, whereas
// a returned error would tempt a caller into treating a successful write as
// failed. Silently does nothing when the workspace has no bus, when fullPath
// is not under the session root, or when the path is one objectStoreExcluded
// rejects — so it is impossible to announce store.db or session.lock by
// mistake, however a future caller wires it up.
func (s *SessionWorkspace) AnnounceWrite(fullPath string, existed bool) {
	rel, ok := s.announceRel(fullPath)
	if !ok {
		return
	}
	size := statSize(fullPath)
	eventType := "session.file.created"
	if existed {
		eventType = "session.file.updated"
	}
	_ = s.bus.Emit(eventType, map[string]any{
		"session_id":  s.ID,
		"path":        rel,
		"size":        size,
		"offset":      0,
		"bytes_added": size,
	})
}

// AnnounceAppend announces bytesAdded bytes appended to the end of fullPath by
// a caller holding its own append-mode descriptor.
//
// Unlike AnnounceWrite it takes no "existed" flag, because for an append the
// answer is derivable and the derived answer is better than a sampled one: a
// file whose post-append offset is 0 held no bytes before the append, so this
// is the write that made it observable and "created" is the useful signal.
// Sampling existed instead would report "updated" for the first real line
// written to a file some earlier O_CREATE had left empty — which is how the
// scene patch journal, opened at plugin Init and appended to on the first tool
// call, would otherwise never announce a creation at all.
//
// offset is derived as size-bytesAdded rather than read back from the caller's
// descriptor, because the caller still owns that descriptor and this helper
// must not seek it. That is exact for a single-writer append (the case every
// caller here is) and clamps to 0 if a concurrent truncation makes the
// arithmetic go negative — the conservative direction, since offset 0 reads as
// "the whole object changed".
func (s *SessionWorkspace) AnnounceAppend(fullPath string, bytesAdded int) {
	rel, ok := s.announceRel(fullPath)
	if !ok {
		return
	}
	size := statSize(fullPath)
	offset := size - bytesAdded
	if offset < 0 {
		offset = 0
	}
	eventType := "session.file.updated"
	if offset == 0 {
		eventType = "session.file.created"
	}
	_ = s.bus.Emit(eventType, map[string]any{
		"session_id":  s.ID,
		"path":        rel,
		"size":        size,
		"offset":      offset,
		"bytes_added": bytesAdded,
	})
}

// announceRel resolves an absolute (or relative-to-cwd) path to the
// slash-separated session-relative form the session.file.* payload carries,
// reporting ok=false when there is nothing to announce.
//
// The escape check is why callers may pass a path they are not sure about:
// plugins whose output directory is configurable (the HITL registry, the
// sampler, long-term memory) point outside every session tree by default, and
// this returning false is the correct, silent outcome for them rather than
// something each call site has to guard.
func (s *SessionWorkspace) announceRel(fullPath string) (string, bool) {
	if s == nil || s.bus == nil || fullPath == "" || s.RootDir == "" {
		return "", false
	}
	root, err := filepath.Abs(s.RootDir)
	if err != nil {
		return "", false
	}
	abs, err := filepath.Abs(fullPath)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	slash := filepath.ToSlash(rel)
	if slash == "." || slash == ".." || strings.HasPrefix(slash, "../") {
		return "", false
	}
	if objectStoreExcluded(slash) {
		return "", false
	}
	return slash, true
}

// statSize reports a file's current size, or 0 when it cannot be read.
//
// 0 rather than dropping the announcement, for the same reason AppendFile
// tolerates a failed Stat: a subscriber that re-reads the path is still
// correct with a wrong size, and is unrecoverably wrong with no event at all.
func statSize(fullPath string) int {
	fi, err := os.Stat(fullPath)
	if err != nil {
		return 0
	}
	return int(fi.Size())
}
