package objectstore

import (
	"errors"
	"fmt"
	"strings"
)

// This file turns the key rules documented on Backend into code every
// implementation can share.
//
// The rules were written down when the interface was defined but left
// unenforced, on the theory that a well-behaved caller would simply obey them.
// That theory does not survive three independent implementations — the
// in-memory one, and the two out-of-tree cloud modules — each of which would
// otherwise have to re-derive "is this key legal" and "which keys are under
// this prefix" from prose. Two of them would get it subtly differently, and the
// difference would only surface as a mis-hydrated session.
//
// So the rules live here, exported, next to the interface that states them.
// Backends are not *required* to call these helpers — a backend that rejects
// the same keys and computes the same relative paths by other means is
// conformant — but the contract suite in objectstoretest holds every backend to
// exactly the behaviour below, so calling them is the cheap way to pass.
//
// The rejected alternative was to validate centrally in package engine, at the
// call sites, and leave backends free to assume well-formed input. That was
// rejected because it does not hold for out-of-tree backends: nothing stops an
// embedder from calling a Backend directly, and "..%2F" traversal into a
// hydration destination is precisely the input a backend must not trust from
// anywhere.

// ErrInvalidKey is the sentinel every key-syntax rejection wraps, so a caller
// can distinguish "this key is malformed" from "the remote store said no"
// without string matching.
var ErrInvalidKey = errors.New("invalid object key")

// ValidateKey checks a store-relative object key against the rules on Backend:
// non-empty, "/"-separated, no leading or trailing "/", no empty segment, no
// "." or ".." segment, no OS-specific separator, no NUL.
//
// The "." and ".." bans are the load-bearing ones. Hydrate joins a key's
// relative form onto a local destination directory, so a key containing ".."
// is a directory traversal out of the session tree; rejecting it at the
// backend boundary is the only place the check is guaranteed to run for every
// implementation.
//
// Errors wrap ErrInvalidKey.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: key is empty", ErrInvalidKey)
	}
	return validateKeyPath(key, "key")
}

// ValidateKeyPrefix checks a key prefix. Identical to ValidateKey except that
// the empty prefix is legal and means "everything the configured prefix
// covers" — the documented way for List to enumerate the whole store.
//
// Errors wrap ErrInvalidKey.
func ValidateKeyPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return validateKeyPath(prefix, "key prefix")
}

func validateKeyPath(s string, what string) error {
	if strings.HasPrefix(s, "/") {
		return fmt.Errorf("%w: %s %q begins with %q; keys are store-relative", ErrInvalidKey, what, s, "/")
	}
	if strings.HasSuffix(s, "/") {
		return fmt.Errorf("%w: %s %q ends with %q; keys name an object, not a directory", ErrInvalidKey, what, s, "/")
	}
	if strings.Contains(s, `\`) {
		// A backslash here almost always means a Windows path leaked into the
		// key space via filepath.Join instead of going through
		// filepath.ToSlash. Rejecting it keeps the same tree from being stored
		// under two different key layouts depending on the host OS.
		return fmt.Errorf(`%w: %s %q contains %q; keys use "/" on every OS`, ErrInvalidKey, what, s, `\`)
	}
	if strings.ContainsRune(s, 0) {
		return fmt.Errorf("%w: %s contains a NUL byte", ErrInvalidKey, what)
	}
	for _, seg := range strings.Split(s, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%w: %s %q has an empty segment", ErrInvalidKey, what, s)
		case ".", "..":
			return fmt.Errorf("%w: %s %q has a %q segment", ErrInvalidKey, what, s, seg)
		}
	}
	return nil
}

// TrimKeyPrefix reports whether key lies under keyPrefix and, if so, returns
// the remainder with the prefix and its separator removed.
//
// The prefix is matched on segment boundaries, not as a raw string: "a/b" is
// under "a" but "a/b" is NOT under "a/b" itself, and "ab/c" is not under "a".
// Raw string matching — what ListObjectsV2 and its GCS equivalent do natively —
// would make session "sess-1" hydrate the objects of session "sess-10", because
// the engine's key prefix for a session is literally "sessions/" + the ID. A
// backend that lists with a raw prefix must therefore post-filter, which is
// what this helper is for.
//
// An exact match returns ("", false) on purpose: an object *at* the prefix has
// no path beneath a hydration destination, so treating it as "under" would only
// create a naming problem with no legitimate producer. The engine never writes
// such an object.
func TrimKeyPrefix(key string, keyPrefix string) (string, bool) {
	if keyPrefix == "" {
		return key, key != ""
	}
	rest, ok := strings.CutPrefix(key, keyPrefix+"/")
	if !ok || rest == "" {
		return "", false
	}
	return rest, true
}
