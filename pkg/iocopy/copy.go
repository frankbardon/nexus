// Package iocopy provides byte-for-byte filesystem copy primitives shared by
// the eval promote pipeline and the online sampler plugin.
//
// Both callers need the same guarantees: a journal directory copied
// recursively, regular files preserved bit-for-bit, symlinks not followed,
// and special files (sockets, devices) skipped. Centralizing the helper
// here keeps the two callers from drifting and makes the contract auditable
// in one place.
package iocopy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CopyDir recursively copies src into dst (which is created as needed).
// Behavior contract:
//
//   - Regular files are copied byte-for-byte; mode bits are preserved.
//   - Directories are created with mode 0o755.
//   - Symlinks are not followed and not recreated; they are surfaced as an
//     error so callers explicitly decide whether to allow them. Journal
//     directories never contain symlinks, so this is the safe default.
//   - Special files (sockets, devices, named pipes) are skipped silently.
//   - dst may exist already; existing files at the same relative path are
//     overwritten. Pre-existing extra files at dst are left untouched —
//     callers wanting a clean target should remove dst first.
//
// On error, CopyDir returns immediately with a wrapped error; partial state
// at dst is the caller's responsibility to clean up.
func CopyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// Reject symlinks deliberately. journal/ never contains them; if a
		// caller surfaces one we want to know rather than silently ship a
		// dangling pointer (or worse, follow it into unbounded territory).
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("iocopy: refusing to copy symlink at %s", path)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return CopyFile(path, target)
	})
}

// CopyFile copies the contents of src to dst, preserving file mode. Parent
// directories are created if missing. Existing files at dst are overwritten
// in-place.
func CopyFile(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	info, err := sf.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer df.Close()
	if _, err := io.Copy(df, sf); err != nil {
		return err
	}
	return nil
}
