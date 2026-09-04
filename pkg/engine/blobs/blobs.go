// Package blobs implements a content-addressed blob store for multimodal
// payloads (images, audio, documents, video) referenced from event-payload
// MessageParts.
//
// The store keys blobs by sha256 of their bytes. Same content + media type
// always returns the same handle, so callers can re-emit MessageParts that
// reference a blob by URI without re-uploading.
//
// Layout:
//
//	{root}/
//	  ab/
//	    abcdef...01.bin   raw bytes (immutable once written)
//	    abcdef...01.meta  "media/type\nsize\n"
//
// LRU is recorded by file mtime on the .bin file. Get() touches mtime so
// recently-read blobs survive eviction. Sweep() walks the tree, sorts by
// mtime ascending, deletes oldest until total bytes <= byteBudget.
//
// The store is process-local and synchronized via an internal mutex. It is
// safe for concurrent Put/Get/Sweep across goroutines in one process.
// Cross-process access is not supported (blob writes are atomic via
// temp+rename, but Sweep is not coordinated across processes). A PutHook does
// not change that: the hook is a callback inside the same process, so two
// processes rooted at one directory still have two uncoordinated Sweeps and
// two independent hooks.
//
// # Object-store disposition: write-through
//
// When rooted at a session's blobs/ directory this package writes under the
// session tree with raw os.* calls and announces nothing on the bus. The bytes
// still reach a configured object store, and they reach it the moment they
// land rather than at the next turn boundary — see WithPutHook.
//
// What write-through buys is narrow, and worth stating precisely so nobody
// reads more into it. It is *not* a bandwidth saving: blobs are
// content-addressed, so a file at blobs/<xx>/<sha256>.bin either holds those
// bytes or does not exist, and engine.objectStoreImmutable already recognises
// that by identity and reduces the whole subtree to a once-ever upload per
// blob. The repeated per-turn cost was never being paid. What is left is
// latency and durability *within* one turn: a turn that produces several large
// blobs otherwise holds all of them on local disk until agent.turn.end, so a
// process killed mid-turn loses them, and the turn boundary pays for all of
// them at once. Pushing on write overlaps the upload with the rest of the turn
// and shrinks the loss window from "one turn" to "one queue drain".
//
// It is a pure optimisation, deliberately. A push that fails, is dropped under
// load, or never happens because no hook was installed costs nothing but the
// delay it was trying to remove: the turn-boundary snapshot still walks the
// whole tree, and it re-uploads any immutable file the store does not already
// hold at the right size. Correctness stays with the snapshot.
//
// # No bus dependency
//
// This package still imports nothing outside the standard library and knows
// nothing about an event bus, an object store or a session. WithPutHook takes
// a plain func, which is what lets pkg/engine wire the push without this
// package acquiring a dependency it would then carry into every standalone
// use. Recorded in engine.SessionTreeWriters.
//
// # Eviction is local only
//
// Sweep and Delete remove local files and nothing else. There is deliberately
// no delete hook: the LRU sweep exists to bound *disk*, which is exactly the
// constraint a bucket does not have, so mirroring a local eviction remotely
// would destroy data the operator is paying to keep — and would do it to
// content-addressed objects a later session may still be referencing by URI.
// A swept blob stays in the store; a Get for it after eviction is a local miss
// that hydration can repair, not a lost object.
package blobs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Handle is a content-addressed reference to a stored blob.
type Handle struct {
	// SHA256 is the lowercase hex sha256 of the blob bytes. 64 chars.
	SHA256 string

	// MediaType is the IANA media type the caller supplied at Put time
	// (e.g. "image/png", "application/pdf"). Empty when the caller didn't
	// pass one.
	MediaType string

	// Size is the number of bytes stored.
	Size int64

	// Path is the absolute path to the .bin file. Useful for tools that
	// want to stream the file rather than load into memory.
	Path string
}

// URI returns a stable reference scheme that consumers (provider plugins,
// MessagePart.URI) can resolve back to a blob. Form: "nexus-blob:<sha256>".
func (h Handle) URI() string {
	return "nexus-blob:" + h.SHA256
}

// SHAFromURI extracts the sha256 from a "nexus-blob:<sha>" URI. Returns the
// empty string if the URI is not in that scheme.
func SHAFromURI(uri string) string {
	const prefix = "nexus-blob:"
	if !strings.HasPrefix(uri, prefix) {
		return ""
	}
	return uri[len(prefix):]
}

// Store is a content-addressed blob store rooted at a directory.
type Store struct {
	root       string
	byteBudget int64

	// onPut is set once at construction and never mutated, so Put reads it
	// without the mutex. Nil unless WithPutHook was passed.
	onPut PutHook

	mu sync.Mutex
}

// PutHook is notified after Put has durably written a *new* blob. It receives
// the handle (whose Path is the .bin file) and the path of the .meta sidecar
// written beside it, because a blob is two files and a consumer that copies
// only one of them stores a blob with no media type.
//
// It is a plain func rather than an interface or an event bus on purpose: this
// package is a standalone content store with no dependency outside the
// standard library, and a func is the narrowest thing that lets pkg/engine
// wire an object-store push into it without that dependency leaking back here.
//
// # What the hook may assume, and what it must not
//
// It runs exactly once per blob per Store, on the goroutine that called Put,
// *after* the internal mutex has been released. Outside the lock is the whole
// point: the hook is expected to do I/O (an upload), holding the store lock
// across a network round trip would serialise every other Put behind it, and a
// hook that called back into Put, Delete or Sweep would deadlock.
//
// The consequence of being outside the lock is that a concurrent Sweep may
// evict the blob before the hook opens it. A hook must therefore treat a
// missing file as ordinary, not as an error worth escalating.
//
// A Put that finds the content already present fires nothing: the bytes are
// unchanged and by content-addressing they can only ever be those bytes, so a
// second notification would describe a write that did not happen.
//
// Panics are not recovered. A hook is engine-supplied glue, not user code, and
// swallowing a panic here would hide it behind a successful Put.
type PutHook func(h Handle, metaPath string)

// Option configures a Store at construction. Variadic on New so the existing
// two-argument call shape keeps compiling — an out-of-tree caller that only
// wants a local blob store never has to learn this exists.
type Option func(*Store)

// WithPutHook installs fn as the store's PutHook. A nil fn is ignored, so a
// caller can pass a hook it may or may not have without branching.
func WithPutHook(fn PutHook) Option {
	return func(s *Store) {
		if fn != nil {
			s.onPut = fn
		}
	}
}

// New opens a blob store rooted at dir.
//
// The root directory is NOT created here: Put creates it (and the two-char
// shard beneath it) on the first blob, so a session whose tools never produce
// a blob never grows an empty blobs/ directory. That is deliberate, and it is
// what SessionWorkspace.BlobsDir documents. The cost is that a root that
// cannot be created is reported by the first Put rather than by New; the
// alternative — an eager MkdirAll for early validation — put an empty
// directory into every session tree, including every one that gets synced to
// an object store, to catch a misconfiguration that Put reports anyway.
//
// byteBudget is the soft cap for total stored bytes. When zero, the store
// is unbounded and Sweep is a no-op. When positive, callers should call
// Sweep periodically (or after Put) to enforce the cap.
func New(dir string, byteBudget int64, opts ...Option) (*Store, error) {
	if dir == "" {
		return nil, errors.New("blobs: empty root directory")
	}
	s := &Store{root: dir, byteBudget: byteBudget}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Root returns the absolute root directory the store writes into.
func (s *Store) Root() string { return s.root }

// ByteBudget returns the configured soft cap (0 = unbounded).
func (s *Store) ByteBudget() int64 { return s.byteBudget }

// SetByteBudget updates the soft cap. Doesn't trigger an immediate sweep —
// call Sweep separately if needed.
func (s *Store) SetByteBudget(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byteBudget = n
}

// Put writes data under its sha256 and returns a Handle. Idempotent — if a
// blob with the same sha already exists, Put updates the mtime (touching
// LRU) and returns its handle without rewriting.
//
// MediaType is recorded in a sidecar .meta file. When a Put hits an
// existing blob, the recorded MediaType is the original — callers that
// pass a different MediaType for the same content will read back the
// original. (Same bytes + different media type would imply a content-type
// confusion bug at the caller.)
func (s *Store) Put(data []byte, mediaType string) (Handle, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	binPath, metaPath := s.paths(sha)

	h, created, err := s.put(sha, binPath, metaPath, data, mediaType)
	if err != nil || !created {
		return h, err
	}

	// Outside s.put, therefore outside the mutex. See PutHook for why that is
	// a requirement rather than a tidiness preference: the hook uploads, and a
	// network round trip under the store lock would block every other Put.
	if s.onPut != nil {
		s.onPut(h, metaPath)
	}
	return h, nil
}

// put is Put's critical section. created reports whether this call is the one
// that wrote the blob, which is what makes the hook fire exactly once per blob
// however many callers race on the same content.
func (s *Store) put(sha, binPath, metaPath string, data []byte, mediaType string) (h Handle, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if info, statErr := os.Stat(binPath); statErr == nil {
		// Already present — touch mtime for LRU and read back the existing
		// media type so callers don't accidentally rewrite metadata.
		now := time.Now()
		_ = os.Chtimes(binPath, now, now)
		mt, _ := readMeta(metaPath)
		return Handle{SHA256: sha, MediaType: mt, Size: info.Size(), Path: binPath}, false, nil
	}

	// Creates the store root as well as the two-char shard, which is why New
	// does not have to: MkdirAll builds every missing parent.
	if mkErr := os.MkdirAll(filepath.Dir(binPath), 0o755); mkErr != nil {
		return Handle{}, false, fmt.Errorf("blobs: mkdir: %w", mkErr)
	}
	if wErr := writeAtomic(binPath, data); wErr != nil {
		return Handle{}, false, wErr
	}
	if wErr := writeAtomic(metaPath, []byte(formatMeta(mediaType, int64(len(data))))); wErr != nil {
		return Handle{}, false, wErr
	}
	return Handle{SHA256: sha, MediaType: mediaType, Size: int64(len(data)), Path: binPath}, true, nil
}

// Get returns the bytes and media type for a blob, or os.ErrNotExist if
// missing. Touches mtime so subsequent Sweep treats this as recently used.
func (s *Store) Get(sha string) ([]byte, string, error) {
	binPath, metaPath := s.paths(sha)
	data, err := os.ReadFile(binPath)
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	_ = os.Chtimes(binPath, now, now)
	mt, _ := readMeta(metaPath)
	return data, mt, nil
}

// Stat returns the handle for a blob without loading its bytes. Updates
// mtime like Get so Stat-based hot paths don't get evicted.
func (s *Store) Stat(sha string) (Handle, error) {
	binPath, metaPath := s.paths(sha)
	info, err := os.Stat(binPath)
	if err != nil {
		return Handle{}, err
	}
	now := time.Now()
	_ = os.Chtimes(binPath, now, now)
	mt, _ := readMeta(metaPath)
	return Handle{SHA256: sha, MediaType: mt, Size: info.Size(), Path: binPath}, nil
}

// Delete removes a blob from the local store. Missing blobs are not an error.
//
// Local only, and there is deliberately no delete counterpart to PutHook — see
// the package doc's "Eviction is local only" section. Removing the local copy
// says the disk is full, not that the content is unwanted.
func (s *Store) Delete(sha string) error {
	binPath, metaPath := s.paths(sha)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(binPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blobs: delete bin: %w", err)
	}
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blobs: delete meta: %w", err)
	}
	return nil
}

// Sweep evicts blobs in LRU order until total stored bytes <= byteBudget.
// Returns the count of evicted blobs and the total bytes freed.
//
// No-op when byteBudget is zero (unbounded store), and no-op when the root
// does not exist yet — a store nobody has Put to has nothing to evict, and
// since New no longer creates the root that is an ordinary state rather than
// a broken one.
//
// Eviction is local. Nothing here touches a remote copy of a blob, and nothing
// should: the byte budget bounds disk, a bucket has no such bound, and
// deleting remotely to match would throw away content a later hydration is
// expected to bring back. See the package doc.
func (s *Store) Sweep() (evicted int, freed int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byteBudget <= 0 {
		return 0, 0, nil
	}
	if _, statErr := os.Stat(s.root); os.IsNotExist(statErr) {
		return 0, 0, nil
	}

	type entry struct {
		path  string
		sha   string
		size  int64
		mtime time.Time
	}
	var entries []entry
	var total int64

	walkErr := filepath.Walk(s.root, func(path string, info os.FileInfo, ferr error) error {
		if ferr != nil {
			return ferr
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".bin") {
			return nil
		}
		sha := strings.TrimSuffix(filepath.Base(path), ".bin")
		entries = append(entries, entry{path: path, sha: sha, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
		return nil
	})
	if walkErr != nil {
		return 0, 0, fmt.Errorf("blobs: walk: %w", walkErr)
	}

	if total <= s.byteBudget {
		return 0, 0, nil
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].mtime.Before(entries[j].mtime)
	})

	for _, e := range entries {
		if total <= s.byteBudget {
			break
		}
		_, metaPath := s.paths(e.sha)
		if rerr := os.Remove(e.path); rerr != nil && !os.IsNotExist(rerr) {
			return evicted, freed, fmt.Errorf("blobs: evict bin: %w", rerr)
		}
		if rerr := os.Remove(metaPath); rerr != nil && !os.IsNotExist(rerr) {
			return evicted, freed, fmt.Errorf("blobs: evict meta: %w", rerr)
		}
		total -= e.size
		freed += e.size
		evicted++
	}
	return evicted, freed, nil
}

// TotalBytes walks the store and returns the current total of stored blob
// bytes. Useful for tests and for callers that want to decide whether to
// trigger Sweep.
func (s *Store) TotalBytes() (int64, error) {
	var total int64
	// A root that was never created holds no bytes. Reported as 0 rather than
	// as a walk error so callers do not have to special-case a store whose
	// first Put has not happened yet.
	if _, err := os.Stat(s.root); os.IsNotExist(err) {
		return 0, nil
	}
	err := filepath.Walk(s.root, func(path string, info os.FileInfo, ferr error) error {
		if ferr != nil {
			return ferr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".bin") {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// paths returns the (.bin, .meta) absolute paths for a sha. The blob is
// nested into a two-char prefix directory to avoid one giant flat dir.
func (s *Store) paths(sha string) (binPath, metaPath string) {
	prefix := "00"
	if len(sha) >= 2 {
		prefix = sha[:2]
	}
	bin := filepath.Join(s.root, prefix, sha+".bin")
	meta := filepath.Join(s.root, prefix, sha+".meta")
	return bin, meta
}

// writeAtomic writes data to path via a same-directory temp file + rename
// so concurrent readers never see a partial blob.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("blobs: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("blobs: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("blobs: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("blobs: rename: %w", err)
	}
	return nil
}

// formatMeta serializes the sidecar contents: "<media-type>\n<size>\n".
func formatMeta(mediaType string, size int64) string {
	return mediaType + "\n" + strconv.FormatInt(size, 10) + "\n"
}

// readMeta loads "<media-type>\n<size>\n" from a sidecar. Errors and
// missing files yield ("", err) so the caller can decide whether to surface
// or treat as empty (Get/Stat treat as empty).
func readMeta(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(string(buf), "\n", 2)
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], nil
}
