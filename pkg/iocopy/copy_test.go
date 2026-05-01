package iocopy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestCopyDir_ByteEquality copies a journal-shaped tree (header + active
// segment + rotated segments) and asserts every file is identical bit-for-bit.
func TestCopyDir_ByteEquality(t *testing.T) {
	src := t.TempDir()
	// Synthesize a journal-ish layout.
	files := map[string][]byte{
		"header.json":           []byte(`{"schema_version":"1","created_at":"2026-04-01T00:00:00Z"}`),
		"events.jsonl":          []byte("{\"seq\":1,\"type\":\"io.session.start\"}\n{\"seq\":2,\"type\":\"io.input\"}\n"),
		"events-001.jsonl.zst":  bytes.Repeat([]byte{0x28, 0xb5, 0x2f, 0xfd}, 64),
		"events-002.jsonl.zst":  bytes.Repeat([]byte{0xff, 0x00}, 128),
		"cache/foo/abcdef.json": []byte(`{"args":"x"}`),
	}
	for rel, content := range files {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	dst := filepath.Join(t.TempDir(), "out")
	if err := CopyDir(src, dst); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: bytes differ\n  want=%q\n   got=%q", rel, want, got)
		}
	}
}

// TestCopyDir_SymlinkRejected confirms the helper refuses to recreate or
// follow symlinks. Sampling a journal that somehow contains a symlink should
// surface as a loud error, not a silently broken sample.
func TestCopyDir_SymlinkRejected(t *testing.T) {
	src := t.TempDir()
	// real file
	if err := os.WriteFile(filepath.Join(src, "real.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// symlink pointing at it
	if err := os.Symlink(filepath.Join(src, "real.txt"), filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("symlink unsupported on this fs: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "out")
	err := CopyDir(src, dst)
	if err == nil {
		t.Fatal("CopyDir on a tree containing a symlink should error, got nil")
	}
}

// TestCopyFile_OverwriteExisting verifies the writer path truncates an
// existing file rather than appending.
func TestCopyFile_OverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("old-and-longer"), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}
	if err := CopyFile(src, dst); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("dst = %q, want %q", got, "new")
	}
}
