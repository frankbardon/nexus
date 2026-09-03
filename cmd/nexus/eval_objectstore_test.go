package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
)

// An eval run is the seam's fourth root: written once, never mutated, and with
// no engine lifecycle to hang a snapshot off. These cover the two things that
// can go wrong in a CLI-side publish — reading the block out of a file that is
// not required to be a valid engine config, and excluding the per-case session
// trees, which belong to the *other* root and would otherwise be stored twice.

func TestLoadEvalObjectStoreReadsTheEngineBlock(t *testing.T) {
	backend := objectstoretest.NewMemory()
	name := "memory-eval-" + t.Name()
	objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
		return backend, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "eval:\n  reports_dir: /tmp/reports\ncore:\n  sessions:\n    object_store:\n      backend: " +
		name + "\n      bucket: eval-bucket\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadEvalObjectStore(path)
	if err != nil {
		t.Fatalf("loadEvalObjectStore: %v", err)
	}
	if !cfg.Enabled() || cfg.Bucket != "eval-bucket" {
		t.Fatalf("cfg = %+v, want the named backend and bucket", cfg)
	}
	// Validate normalises the policy at load, the same as the engine does, so
	// a downstream reader never re-derives the default.
	if cfg.FailurePolicy != objectstore.FailurePolicyDegrade {
		t.Errorf("failure policy = %q, want %q", cfg.FailurePolicy, objectstore.FailurePolicyDegrade)
	}

	// No path and no block are both "nothing to publish to", not errors: the
	// overwhelmingly common case is an eval config that has never heard of
	// object storage.
	if cfg, err := loadEvalObjectStore(""); err != nil || cfg.Enabled() {
		t.Errorf("loadEvalObjectStore(\"\") = %+v/%v, want a disabled config", cfg, err)
	}
}

func TestPublishEvalRunExcludesPerCaseSessionTrees(t *testing.T) {
	backend := objectstoretest.NewMemory()
	name := "memory-eval-" + t.Name()
	objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
		return backend, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(
		"core:\n  sessions:\n    object_store:\n      backend: "+name+"\n      bucket: eval-bucket\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(dir, "20260903T101500Z")
	if err := os.MkdirAll(filepath.Join(runDir, evalSessionsDirName, "sess-1", "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "report.json"), []byte(`{"run":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "summary.txt"), []byte("1 passed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, evalSessionsDirName, "sess-1", "files", "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	publishEvalRun(context.Background(), configPath, "20260903T101500Z", runDir)

	keys := backend.Keys()
	want := map[string]bool{
		"eval/20260903T101500Z/report.json": false,
		"eval/20260903T101500Z/summary.txt": false,
	}
	for _, k := range keys {
		if _, ok := want[k]; ok {
			want[k] = true
		}
		if strings.Contains(k, evalSessionsDirName) {
			t.Errorf("per-case session tree published at %q; sessions are the seam's other root", k)
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("%q not published; got %v", k, keys)
		}
	}
}
