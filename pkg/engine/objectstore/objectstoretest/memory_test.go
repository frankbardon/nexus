package objectstoretest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
)

// TestMemoryPassesContractSuite is the load-bearing test in this package. The
// contract suite is only worth holding the S3 and GCS modules to if at least
// one implementation demonstrably passes it, and this is that implementation.
func TestMemoryPassesContractSuite(t *testing.T) {
	RunSuite(t, func(*testing.T) objectstore.Backend { return NewMemory() })
}

// The options exist for backends that cannot afford a default, so they have to
// work. A tiny probe count also keeps this second full pass cheap.
func TestRunSuiteHonoursOptions(t *testing.T) {
	RunSuite(t, func(*testing.T) objectstore.Backend { return NewMemory() },
		WithListProbeCount(3), WithoutConcurrency())
}

func TestMemorySeedAndGet(t *testing.T) {
	m := NewMemory()
	if err := m.Seed("files/a.txt", []byte("seeded")); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	got, ok := m.Get("files/a.txt")
	if !ok || string(got) != "seeded" {
		t.Fatalf("Get = %q, %v; want %q, true", got, ok, "seeded")
	}
	// Get must hand back a copy: a caller that mutates what it read has no
	// business changing the store.
	got[0] = 'X'
	again, _ := m.Get("files/a.txt")
	if string(again) != "seeded" {
		t.Errorf("mutating the result of Get changed the stored object: %q", again)
	}
	if _, ok := m.Get("files/missing.txt"); ok {
		t.Error("Get of a missing key reported ok")
	}
	if err := m.Seed("/bad", nil); !errors.Is(err, objectstore.ErrInvalidKey) {
		t.Errorf("Seed of an invalid key = %v, want ErrInvalidKey", err)
	}
}

func TestMemorySeedTree(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"metadata/session.json": "{}",
		"files/report.md":       "body",
		"files/nested/a.txt":    "nested",
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	m := NewMemory()
	if err := m.SeedTree("sessions/s1", dir); err != nil {
		t.Fatalf("SeedTree: %v", err)
	}
	want := []string{
		"sessions/s1/files/nested/a.txt",
		"sessions/s1/files/report.md",
		"sessions/s1/metadata/session.json",
	}
	if got := m.Keys(); !slices.Equal(got, want) {
		t.Errorf("Keys() = %v, want %v", got, want)
	}

	// SeedTree then Hydrate is the round trip a kill/resume test depends on, so
	// prove the two agree before anything is built on top of them.
	out := t.TempDir()
	if err := m.Hydrate(context.Background(), "sessions/s1", out); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	for rel, content := range files {
		data, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("reading %s: %v", rel, err)
			continue
		}
		if string(data) != content {
			t.Errorf("%s = %q, want %q", rel, data, content)
		}
	}
}

func TestMemoryListDoesNotReturnSortedOutput(t *testing.T) {
	// Deliberate: the interface says the order is unspecified, and a double that
	// returns sorted output lets callers grow a dependency on sorting that a
	// real store would break. Descending keeps it deterministic.
	m := NewMemory()
	for _, k := range []string{"a", "b", "c"} {
		if err := m.Seed(k, []byte(k)); err != nil {
			t.Fatalf("Seed: %v", err)
		}
	}
	objs, err := m.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	keys := make([]string, len(objs))
	for i, o := range objs {
		keys[i] = o.Key
	}
	if slices.IsSorted(keys) {
		t.Errorf("List returned ascending keys %v; the double must not hand callers a sorted order to rely on", keys)
	}
}

func TestMemoryFailureInjection(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	t.Run("flush", func(t *testing.T) {
		m := NewMemory()
		m.SetFlushError(boom)
		if err := m.Flush(ctx); !errors.Is(err, boom) {
			t.Errorf("Flush = %v, want %v", err, boom)
		}
		m.SetFlushError(nil)
		if err := m.Flush(ctx); err != nil {
			t.Errorf("Flush after clearing the injected error = %v, want nil", err)
		}
	})

	t.Run("put", func(t *testing.T) {
		m := NewMemory()
		m.SetPutError(boom)
		local := filepath.Join(t.TempDir(), "a.txt")
		if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := m.Put(ctx, "a.txt", local); !errors.Is(err, boom) {
			t.Errorf("Put = %v, want %v", err, boom)
		}
		if m.Len() != 0 {
			t.Errorf("a failed Put stored %d objects, want 0", m.Len())
		}
	})

	t.Run("delete", func(t *testing.T) {
		m := NewMemory()
		if err := m.Seed("a.txt", []byte("x")); err != nil {
			t.Fatalf("Seed: %v", err)
		}
		m.SetDeleteError(boom)
		if err := m.Delete(ctx, "a.txt"); !errors.Is(err, boom) {
			t.Errorf("Delete = %v, want %v", err, boom)
		}
		if m.Len() != 1 {
			t.Errorf("a failed Delete removed the object anyway")
		}
	})

	t.Run("hydrate", func(t *testing.T) {
		m := NewMemory()
		if err := m.Seed("a.txt", []byte("x")); err != nil {
			t.Fatalf("Seed: %v", err)
		}
		m.SetHydrateError(boom)
		dir := t.TempDir()
		if err := m.Hydrate(ctx, "", dir); !errors.Is(err, boom) {
			t.Errorf("Hydrate = %v, want %v", err, boom)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("a failed Hydrate wrote %d entries, want 0", len(entries))
		}
	})
}

func TestMemoryCounts(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := m.Put(ctx, "a.txt", local); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := m.List(ctx, ""); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := m.Hydrate(ctx, "", t.TempDir()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if err := m.Delete(ctx, "a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := m.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := Counts{Hydrates: 1, Puts: 1, Deletes: 1, Lists: 1, Flushes: 1, Closes: 1}
	if got := m.Counts(); got != want {
		t.Errorf("Counts() = %+v, want %+v", got, want)
	}
}

// The double implements io.Closer so tests using it exercise the engine's
// optional-release path rather than skipping it. Backend itself deliberately
// has no Close.
func TestMemoryIsAnOptionalCloser(t *testing.T) {
	var b objectstore.Backend = NewMemory()
	if _, ok := b.(interface{ Close() error }); !ok {
		t.Error("Memory no longer satisfies io.Closer; the engine's optional-close path would stop being exercised")
	}
}

func TestRegisterMemoryIsReachableByNameAndCleansUp(t *testing.T) {
	const name = "objectstoretest-memory"

	t.Run("registered", func(t *testing.T) {
		backend := RegisterMemory(t, name, nil)
		if !objectstore.Registered(name) {
			t.Fatalf("Registered(%q) = false after RegisterMemory", name)
		}
		cfg := objectstore.Config{BackendName: name, Bucket: "b"}
		if err := cfg.Validate("core.sessions.object_store"); err != nil {
			t.Fatalf("Validate against a registered memory backend = %v", err)
		}
		opened, err := objectstore.Open(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if opened != objectstore.Backend(backend) {
			t.Error("Open returned a different backend than RegisterMemory handed back")
		}
	})

	// Cleanup is the whole reason objectstore.Unregister is exported: without
	// it, the name above leaks and a second run in the same binary panics on the
	// duplicate registration.
	if objectstore.Registered(name) {
		t.Errorf("Registered(%q) = true after the subtest finished; RegisterMemory leaked the name", name)
	}
}
