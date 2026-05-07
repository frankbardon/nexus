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
// temp+rename, but Sweep is not coordinated across processes).
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

	mu sync.Mutex
}

// New opens or creates a blob store rooted at dir. dir is created if missing.
//
// byteBudget is the soft cap for total stored bytes. When zero, the store
// is unbounded and Sweep is a no-op. When positive, callers should call
// Sweep periodically (or after Put) to enforce the cap.
func New(dir string, byteBudget int64) (*Store, error) {
	if dir == "" {
		return nil, errors.New("blobs: empty root directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("blobs: create root: %w", err)
	}
	return &Store{root: dir, byteBudget: byteBudget}, nil
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

	s.mu.Lock()
	defer s.mu.Unlock()

	if info, err := os.Stat(binPath); err == nil {
		// Already present — touch mtime for LRU and read back the existing
		// media type so callers don't accidentally rewrite metadata.
		now := time.Now()
		_ = os.Chtimes(binPath, now, now)
		mt, _ := readMeta(metaPath)
		return Handle{SHA256: sha, MediaType: mt, Size: info.Size(), Path: binPath}, nil
	}

	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		return Handle{}, fmt.Errorf("blobs: mkdir: %w", err)
	}
	if err := writeAtomic(binPath, data); err != nil {
		return Handle{}, err
	}
	if err := writeAtomic(metaPath, []byte(formatMeta(mediaType, int64(len(data))))); err != nil {
		return Handle{}, err
	}
	return Handle{SHA256: sha, MediaType: mediaType, Size: int64(len(data)), Path: binPath}, nil
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

// Delete removes a blob. Missing blobs are not an error.
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
// No-op when byteBudget is zero (unbounded store).
func (s *Store) Sweep() (evicted int, freed int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byteBudget <= 0 {
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
