package gcsstore

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
)

// The path <-> key mapping, in one place.
//
// The rule is the same one the S3 backend uses, and deliberately so: an object
// key mirrors the local tree exactly -- the store-relative path of a file, with
// the OS separator turned into "/", under the configured bucket prefix.
// Nothing is escaped, hashed, flattened or otherwise encoded.
//
//	local  <session root>/plugins/nexus.scene/scene.jsonl
//	key    plugins/nexus.scene/scene.jsonl
//	object gs://<bucket>/<prefix>/plugins/nexus.scene/scene.jsonl
//
// Keeping the two backends' layouts identical is a decision, not a
// coincidence. A deployment migrating between clouds, or replicating one
// bucket into the other, can do it with the vendors' own copy tools and no
// translation step. The rejected alternative -- letting each backend pick a
// layout that suits its API -- would have bought nothing here, because both
// APIs have the same flat key space.
//
// The mapping is total, and the awkward cases are the point:
//
//   - Nested plugin directories ("plugins/nexus.scene/...") are ordinary
//     segments. GCS has no directories; the console renders "/" as one, but
//     the key is a single flat string and depth costs nothing.
//   - Dotfiles ("files/.hidden") are ordinary segments. Only whole "." and
//     ".." segments are illegal, and objectstore.ValidateKey rejects those
//     before anything is sent.
//   - Empty files become zero-byte objects, not absent ones. Tools that write
//     a zero-byte object as a folder placeholder exist, so it matters that
//     this backend never writes such a marker: every zero-byte object it
//     stores is a real empty file and is hydrated back as one.
//   - Spaces and non-ASCII segments survive because the SDK escapes them on
//     the wire and unescapes them on the way back; the key this package
//     handles is always the decoded form.
//
// Windows is handled by refusing to guess: keys are built with
// filepath.ToSlash on the way out and filepath.FromSlash on the way in, and
// objectstore.ValidateKey rejects a key containing a backslash outright, so a
// tree cannot end up stored under two different layouts depending on the host.

// joinKey applies the configured bucket prefix to a store-relative key or
// prefix. An empty configured prefix is the identity, which keeps the
// no-prefix deployment's keys byte-identical to the local tree.
func joinKey(configPrefix, key string) string {
	switch {
	case configPrefix == "":
		return key
	case key == "":
		return configPrefix
	default:
		return configPrefix + "/" + key
	}
}

// storeKey is joinKey's inverse: it strips the configured bucket prefix from a
// key as the store reported it, reporting false for anything outside.
//
// It goes through objectstore.TrimKeyPrefix rather than strings.TrimPrefix
// because the match must be on segment boundaries. With prefix "prod/nexus" a
// raw match would claim every object of a neighbouring "prod/nexus-staging"
// deployment sharing the bucket, and hydrate them into this one's session tree.
func storeKey(configPrefix, objectKey string) (string, bool) {
	if configPrefix == "" {
		return objectKey, objectKey != ""
	}
	return objectstore.TrimKeyPrefix(objectKey, configPrefix)
}

// listPrefix renders the raw string prefix to hand to the object list call for
// a store-relative key prefix.
//
// The trailing "/" is what makes the server-side filter agree with
// objectstore.TrimKeyPrefix instead of merely resembling it: "sessions/sess-1/"
// cannot match "sessions/sess-10/...", and it also excludes an object sitting
// at exactly the prefix, which TrimKeyPrefix reports as not-under. The empty
// result means "everything in the bucket" and is only produced when neither a
// configured prefix nor a key prefix was given.
//
// The result is an optimisation, not the correctness boundary. List and
// Hydrate still post-filter every key through TrimKeyPrefix. GCS documents
// Query.Prefix as a plain string match with no notion of a path separator
// unless a delimiter is set -- this backend sets none, because a delimiter
// would make the listing hierarchical and hide every object below the first
// level -- so the segment rule has to be applied on this side either way.
func listPrefix(configPrefix, keyPrefix string) string {
	full := joinKey(configPrefix, keyPrefix)
	if full == "" {
		return ""
	}
	return full + "/"
}

// localPathForKey resolves the store-relative remainder of a key to an absolute
// local path under destDir.
//
// The containment check is deliberate belt-and-braces. objectstore.ValidateKey
// already rejects "." and ".." segments, and it runs on the caller's prefix
// before any request is made -- but the keys this function sees come back from
// the *store*, which is a remote party that can return whatever it likes. A
// bucket shared with another writer is enough to make "the store said so" an
// untrustworthy source for a path that is about to be joined onto a directory
// and written to. Failing loudly beats hydrating a session over the top of
// something outside it.
func localPathForKey(destDir, rel string) (string, error) {
	if err := objectstore.ValidateKey(rel); err != nil {
		return "", fmt.Errorf("object key from the store is not usable as a path: %w", err)
	}
	path := filepath.Join(destDir, filepath.FromSlash(rel))
	// filepath.Join has already cleaned the result, so a traversal would show
	// up here as a path that no longer starts with destDir.
	if path != destDir && !strings.HasPrefix(path, destDir+string(filepath.Separator)) {
		return "", fmt.Errorf("object key %q escapes the hydration destination", rel)
	}
	return path, nil
}
