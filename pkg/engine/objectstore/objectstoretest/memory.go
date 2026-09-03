package objectstoretest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
)

// Memory is a complete objectstore.Backend held entirely in process memory.
//
// It exists for two jobs. First, it is the reference implementation: it passes
// the contract suite in this package, which is what makes the suite meaningful
// to hold the S3 and GCS modules to — a suite no implementation has ever
// passed proves nothing about the suite. Second, it is the substituted seam for
// ordinary untagged unit tests, in the spirit of the fake commandRunner in
// cmd/nexus-broker: a test that needs the engine's object-store path to do
// something real, without a network, a credential or a build tag, hands the
// engine one of these.
//
// It is deliberately NOT a driver registered at init. A "memory" backend
// silently selectable in production config is a footgun — an operator who
// typo'd their real backend name would get a store that discards everything on
// exit while reporting success. Tests that need it reachable by name call
// RegisterMemory, which removes it again on cleanup.
//
// Safe for concurrent use, because the interface requires it and because
// make test-race runs over this package like any other.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]memObject
	counts  Counts

	hydrateErr error
	putErr     error
	deleteErr  error
	listErr    error
	flushErr   error
}

type memObject struct {
	data    []byte
	modTime time.Time
}

// Counts records how often each method was called. Exposed so a test can
// assert on the *shape* of the engine's traffic — "exactly one flush on a
// clean shutdown", "no hydrate when the local tree was already present" —
// which is the kind of thing the seam gets wrong in ways the resulting file
// tree cannot show.
type Counts struct {
	Hydrates int
	Puts     int
	Deletes  int
	Lists    int
	Flushes  int
	Closes   int
}

// NewMemory returns an empty in-memory backend.
func NewMemory() *Memory {
	return &Memory{objects: make(map[string]memObject)}
}

// Compile-time proof the double really satisfies the seam. A test double that
// has drifted from the interface it stands in for is worse than no double.
var _ objectstore.Backend = (*Memory)(nil)

// Hydrate writes every object under keyPrefix beneath destDir, with keyPrefix
// stripped from each key. Directories are created as needed; entries at
// destDir with no corresponding object are left alone.
func (m *Memory) Hydrate(ctx context.Context, keyPrefix string, destDir string) error {
	if err := objectstore.ValidateKeyPrefix(keyPrefix); err != nil {
		return fmt.Errorf("memory hydrate: %w", err)
	}

	m.mu.Lock()
	m.counts.Hydrates++
	injected := m.hydrateErr
	// Snapshot under the lock and write outside it: holding a write lock
	// across filesystem I/O would serialise every other caller behind the
	// slowest thing this type does, which is exactly the shape of contention
	// a concurrent-use bug hides behind.
	type pending struct {
		rel  string
		data []byte
	}
	var work []pending
	if injected == nil {
		for key, obj := range m.objects {
			rel, ok := objectstore.TrimKeyPrefix(key, keyPrefix)
			if !ok {
				continue
			}
			work = append(work, pending{rel: rel, data: obj.data})
		}
	}
	m.mu.Unlock()

	if injected != nil {
		return injected
	}

	for _, p := range work {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("memory hydrate: %w", err)
		}
		// TrimKeyPrefix already guaranteed the key had no "." or ".." segment,
		// so this Join cannot escape destDir.
		full := filepath.Join(destDir, filepath.FromSlash(p.rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("memory hydrate: creating %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, p.data, 0o644); err != nil {
			return fmt.Errorf("memory hydrate: writing %s: %w", full, err)
		}
	}
	return nil
}

// Put copies the contents of localPath to key, replacing whatever was there.
func (m *Memory) Put(ctx context.Context, key string, localPath string) error {
	if err := objectstore.ValidateKey(key); err != nil {
		return fmt.Errorf("memory put: %w", err)
	}
	m.mu.Lock()
	m.counts.Puts++
	injected := m.putErr
	m.mu.Unlock()
	if injected != nil {
		return injected
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("memory put %s: %w", key, err)
	}

	// Read before taking the lock, and only commit on success: a failed read
	// must not leave a truncated or empty object behind, which is the failure
	// mode a real multipart upload has to work hardest to avoid.
	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("memory put %s: reading %s: %w", key, localPath, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{data: data, modTime: time.Now().UTC()}
	return nil
}

// Delete removes key. A missing key is not an error.
func (m *Memory) Delete(_ context.Context, key string) error {
	if err := objectstore.ValidateKey(key); err != nil {
		return fmt.Errorf("memory delete: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts.Deletes++
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.objects, key)
	return nil
}

// List returns every object under keyPrefix.
//
// The result is returned in *descending* key order on purpose. The interface
// says the order is unspecified, and a double that happens to return sorted
// output lets a caller — or the contract suite itself — grow an accidental
// dependency on sorting that a real store would break. Descending is
// deterministic, so nothing is flaky, and it is wrong for anyone who assumed.
func (m *Memory) List(_ context.Context, keyPrefix string) ([]objectstore.Object, error) {
	if err := objectstore.ValidateKeyPrefix(keyPrefix); err != nil {
		return nil, fmt.Errorf("memory list: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts.Lists++
	if m.listErr != nil {
		return nil, m.listErr
	}

	var out []objectstore.Object
	for key, obj := range m.objects {
		if _, ok := objectstore.TrimKeyPrefix(key, keyPrefix); !ok {
			continue
		}
		out = append(out, objectstore.Object{
			Key:     key,
			Size:    int64(len(obj.data)),
			ModTime: obj.modTime,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key > out[j].Key })
	return out, nil
}

// Flush is a no-op: an in-memory object is durable the instant it is stored,
// which is the whole reason this type is a useful baseline — it can never fail
// the durability barrier for reasons of its own.
func (m *Memory) Flush(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts.Flushes++
	return m.flushErr
}

// Close satisfies io.Closer, which the engine type-asserts rather than
// requiring on Backend. Implemented here so a test using this double exercises
// that optional-release path instead of silently skipping it.
func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts.Closes++
	return nil
}

// Seed stores an object directly, bypassing the local-file round trip. It is
// how a test arranges "the store already contains a session" without first
// having to build that session on disk.
func (m *Memory) Seed(key string, data []byte) error {
	if err := objectstore.ValidateKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{data: append([]byte(nil), data...), modTime: time.Now().UTC()}
	return nil
}

// SeedTree stores every file under dir, keyed by keyPrefix plus the path
// relative to dir. The inverse of Hydrate, for tests that build a tree on disk
// and then want to resume it somewhere else.
func (m *Memory) SeedTree(keyPrefix string, dir string) error {
	if err := objectstore.ValidateKeyPrefix(keyPrefix); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return fmt.Errorf("relativising %s: %w", path, relErr)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", path, readErr)
		}
		key := filepath.ToSlash(rel)
		if keyPrefix != "" {
			key = keyPrefix + "/" + key
		}
		return m.Seed(key, data)
	})
}

// Get returns a copy of the object stored at key.
func (m *Memory) Get(key string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	obj, ok := m.objects[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), obj.data...), true
}

// Keys returns every stored key in ascending order. Sorted here, unlike List,
// because this is an assertion helper rather than part of the seam.
func (m *Memory) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Len returns the number of stored objects.
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.objects)
}

// Counts returns a snapshot of the per-method call counters.
func (m *Memory) Counts() Counts {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.counts
}

// SetHydrateError makes every subsequent Hydrate fail with err. Pass nil to
// stop failing.
func (m *Memory) SetHydrateError(err error) { m.setErr(&m.hydrateErr, err) }

// SetPutError makes every subsequent Put fail with err.
func (m *Memory) SetPutError(err error) { m.setErr(&m.putErr, err) }

// SetDeleteError makes every subsequent Delete fail with err.
func (m *Memory) SetDeleteError(err error) { m.setErr(&m.deleteErr, err) }

// SetListError makes every subsequent List fail with err. The engine's
// immutable-skip path is the main consumer: a backend that cannot say what it
// holds must make the engine upload everything, never assume presence.
func (m *Memory) SetListError(err error) { m.setErr(&m.listErr, err) }

// SetFlushError makes every subsequent Flush fail with err. The engine's
// failure_policy branch is the main consumer: degrade must swallow this and
// strict must surface it.
func (m *Memory) SetFlushError(err error) { m.setErr(&m.flushErr, err) }

func (m *Memory) setErr(field *error, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	*field = err
}

// RegisterMemory registers backend under name for the duration of one test and
// returns it, so a test can drive the engine through real config
// (core.sessions.object_store.backend: <name>) rather than reaching past it.
//
// The registry is process-global and Register panics on a duplicate, so the
// cleanup is not optional — leaving the name behind would make a second run of
// the same test in the same binary panic. Passing a distinct name per test is
// still wise if tests within a package might run in parallel.
func RegisterMemory(t *testing.T, name string, backend *Memory) *Memory {
	t.Helper()
	if backend == nil {
		backend = NewMemory()
	}
	objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
		return backend, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })
	return backend
}
