package gcsstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"cloud.google.com/go/storage"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"google.golang.org/api/iterator"
)

// Backend is the Google Cloud Storage implementation of objectstore.Backend.
//
// It holds no mutable state beyond the SDK client, which is itself safe for
// concurrent use, so the concurrency requirement on the interface is met by
// construction rather than by a mutex. That is a direct consequence of the
// synchronous design recorded in doc.go: there is no queue to guard.
type Backend struct {
	client *storage.Client
	bucket string
	// prefix is the configured bucket prefix, store-relative form: no leading
	// or trailing "/". Applied on the way out by joinKey and stripped on the
	// way back by storeKey, so nothing above this type ever does prefix
	// arithmetic.
	prefix string
	log    *slog.Logger
}

// compile-time proof this type still satisfies the seam. Cheap, and the failure
// it catches -- core changing the interface -- is one this module exists to
// surface early.
var (
	_ objectstore.Backend = (*Backend)(nil)
	_ io.Closer           = (*Backend)(nil)
)

// object returns the handle for a store-relative key, with the retry policy
// this backend wants.
//
// RetryAlways is set because the SDK's default (RetryIdempotent) does not retry
// an object insert or an unconditional delete: without a generation or
// does-not-exist precondition it cannot know the caller is safe to repeat. This
// caller is. Every Put writes a whole object and takes last-write-wins, and
// Delete already treats a missing object as success, so a repeated request
// converges on the same state. Without this, a transient 503 would fail a push
// that the AWS SDK would have retried silently -- an avoidable difference in
// behaviour between the two backends for the same outage.
//
// The rejected alternative was to add generation preconditions and let the
// default policy apply. That would be a stronger guarantee against a concurrent
// writer, but the engine's model has one writer per key and a precondition
// failure would surface as an error the engine would then retry anyway, which
// is the same outcome by a longer route.
func (b *Backend) object(key string) *storage.ObjectHandle {
	return b.client.
		Bucket(b.bucket).
		Object(joinKey(b.prefix, key)).
		Retryer(storage.WithPolicy(storage.RetryAlways))
}

// Hydrate downloads every object under keyPrefix into destDir, stripping the
// prefix.
//
// Objects are fetched one at a time rather than in parallel, matching the S3
// backend. Concurrency was left out because hydration happens once, before a
// session opens, over a tree of small files where the cost is dominated by
// per-request latency that a worker pool would only partly hide -- and because
// a partly-parallel hydration that fails halfway leaves a harder-to-reason-about
// tree. If a large-tree deployment makes this the bottleneck, a bounded worker
// pool is the change, and List already gives the full work set up front to size
// it with.
func (b *Backend) Hydrate(ctx context.Context, keyPrefix string, destDir string) error {
	if err := objectstore.ValidateKeyPrefix(keyPrefix); err != nil {
		return fmt.Errorf("gcs object store: hydrate: %w", err)
	}
	// Absolute, so the containment check in localPathForKey compares like with
	// like. A relative destDir would make the prefix test meaningless.
	dest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("gcs object store: hydrate destination %q: %w", destDir, err)
	}

	n := 0
	var bytes int64
	err = b.eachObject(ctx, keyPrefix, func(key string, size int64, _ time.Time) error {
		rel, ok := objectstore.TrimKeyPrefix(key, keyPrefix)
		if !ok {
			return nil
		}
		path, err := localPathForKey(dest, rel)
		if err != nil {
			return err
		}
		if err := b.download(ctx, key, path); err != nil {
			return err
		}
		n++
		bytes += size
		return nil
	})
	if err != nil {
		return err
	}

	// An empty prefix is a brand-new session, not a failure -- log it rather
	// than treating it as one, because it is the first-run path.
	b.log.Debug("gcs object store hydrated",
		"prefix", keyPrefix, "dest", dest, "objects", n, "bytes", bytes)
	return nil
}

// download fetches one object to path, creating parent directories.
//
// The body lands in a temporary file next to the destination and is renamed
// into place. Streaming straight into the final path would leave a truncated
// file behind if the transfer failed mid-way -- and because Hydrate's whole
// promise is "every subsequent os.* read behaves as it would on a host that
// never left", a half-written file is worse than an error: nothing downstream
// would know to distrust it.
//
// The reader's Close is checked, not deferred-and-discarded. It is where the
// SDK reports a CRC32C mismatch between what the service sent and what arrived,
// which is the one error in this function that means the bytes on disk are
// wrong rather than absent.
func (b *Backend) download(ctx context.Context, key, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("gcs object store: creating %q: %w", filepath.Dir(path), err)
	}

	r, err := b.object(key).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("gcs object store: reading %q from bucket %q: %w", joinKey(b.prefix, key), b.bucket, err)
	}
	defer r.Close()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".nexus-hydrate-*")
	if err != nil {
		return fmt.Errorf("gcs object store: staging %q: %w", path, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on every failure path below. Harmless after a
	// successful rename, where the name no longer exists.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gcs object store: writing %q: %w", path, err)
	}
	if err := r.Close(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gcs object store: verifying %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("gcs object store: closing %q: %w", tmpName, err)
	}
	// CreateTemp makes 0600 files; the session tree is 0644/0755 throughout, so
	// match it or a hydrated tree reads differently from a locally created one.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("gcs object store: setting mode on %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("gcs object store: placing %q: %w", path, err)
	}
	return nil
}

// Put uploads localPath to key, replacing whatever was there.
//
// The file is opened and streamed to the object writer. It is never modified or
// moved: this is the session's live working copy, still being read by the run
// that produced it.
//
// The upload is committed by Close, and Close is where GCS reports the CRC32C
// it computed over what it received. The SDK compares that against what it
// sent and fails the write on a mismatch, so a successful Put means the bytes
// in the bucket are the bytes on disk -- not merely that a request returned
// 200.
func (b *Backend) Put(ctx context.Context, key string, localPath string) error {
	if err := objectstore.ValidateKey(key); err != nil {
		return fmt.Errorf("gcs object store: put: %w", err)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("gcs object store: reading %q for key %q: %w", localPath, key, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("gcs object store: stating %q: %w", localPath, err)
	}
	if info.IsDir() {
		// Otherwise the copy below fails with a platform-dependent errno and a
		// message that does not say what went wrong.
		return fmt.Errorf("gcs object store: %q is a directory, not a file", localPath)
	}

	// A cancellable child context is the SDK's documented way to abandon an
	// upload without committing it: storage.Writer has no Abort, and
	// CloseWithError is deprecated precisely because it did not reliably stop
	// an in-flight resumable upload. Cancelling is what guarantees a failed
	// io.Copy cannot leave a truncated object behind.
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	objectKey := joinKey(b.prefix, key)
	w := b.object(key).NewWriter(uploadCtx)
	w.ChunkSize = chunkSizeFor(info.Size())
	if _, err := io.Copy(w, f); err != nil {
		cancel()
		return fmt.Errorf("gcs object store: uploading %q to bucket %q: %w", objectKey, b.bucket, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs object store: putting %q into bucket %q: %w", objectKey, b.bucket, err)
	}
	return nil
}

// defaultChunkSize mirrors the SDK's own default upload chunk size (16 MiB).
// Named here rather than referenced from the SDK because the value is a
// threshold this backend chooses, not a constant it inherits.
const defaultChunkSize = 16 << 20

// chunkSizeFor picks storage.Writer.ChunkSize for an object of the given size.
//
// The SDK's default allocates a ChunkSize-capacity buffer per writer -- 16 MiB
// -- before it knows how big the object is. A session snapshot is hundreds of
// files of a few kilobytes each, so that is 16 MiB of garbage per artifact for
// no benefit whatsoever.
//
// Zero means "send it in one request with no buffering", which is what the SDK
// does for anything that fits in a single chunk anyway. The cost is that a
// single-request upload cannot be replayed by the SDK's retry, because the body
// has already been consumed; the engine's own retry queue re-issues the Put
// from the local file, which is the same work one layer up and the layer that
// owns failure_policy. Objects above the threshold keep the default, where
// chunking buys a resumable upload that can survive a mid-transfer failure.
func chunkSizeFor(size int64) int {
	if size < defaultChunkSize {
		return 0
	}
	return defaultChunkSize
}

// Delete removes key.
//
// A key that was never there is a success, per objectstore.Backend, which is
// what lets the engine retry a delete without special-casing the second
// attempt. This is the one place GCS disagrees with S3 outright: GCS returns
// 404 and the SDK surfaces storage.ErrObjectNotExist where S3 returns 204. The
// translation is a single errors.Is, and it belongs here -- the interface
// specifies the S3 behaviour because it is the more useful of the two for a
// retrying caller, and absorbing the difference is exactly what a backend is
// for.
func (b *Backend) Delete(ctx context.Context, key string) error {
	if err := objectstore.ValidateKey(key); err != nil {
		return fmt.Errorf("gcs object store: delete: %w", err)
	}
	objectKey := joinKey(b.prefix, key)
	if err := b.object(key).Delete(ctx); err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil
		}
		return fmt.Errorf("gcs object store: deleting %q from bucket %q: %w", objectKey, b.bucket, err)
	}
	return nil
}

// List returns every object under keyPrefix, following every page.
func (b *Backend) List(ctx context.Context, keyPrefix string) ([]objectstore.Object, error) {
	if err := objectstore.ValidateKeyPrefix(keyPrefix); err != nil {
		return nil, fmt.Errorf("gcs object store: list: %w", err)
	}
	var out []objectstore.Object
	err := b.eachObject(ctx, keyPrefix, func(key string, size int64, mod time.Time) error {
		out = append(out, objectstore.Object{Key: key, Size: size, ModTime: mod})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// eachObject walks every object under a store-relative key prefix, calling fn
// with the store-relative key.
//
// The single place pagination is handled. The SDK's iterator pages internally,
// so "follow every page" here means "loop until iterator.Done" rather than
// driving a paginator by hand -- but the failure it prevents is the same one
// the contract suite probes for with more than a thousand objects, and both
// List and Hydrate go through here so neither can get it half right.
//
// Query.Delimiter is deliberately left empty. Setting it would make the listing
// hierarchical and return synthetic prefixes instead of the objects below them,
// which is the opposite of what a whole-tree hydration needs.
//
// Every key is put through the segment-aware filter even though listPrefix has
// already asked the server for a "/"-terminated prefix. The server-side filter
// is an optimisation over an API that matches Query.Prefix as a plain string;
// this is the correctness boundary, and it is what stops session "sess-1" from
// hydrating the objects of "sess-10".
//
// A bucket that does not exist is reported as such rather than as an empty
// listing: Hydrate of an unknown *prefix* must succeed (it is a brand-new
// session), but an unknown *bucket* is a misconfiguration, and silently
// hydrating nothing would turn it into a session that starts empty every time.
func (b *Backend) eachObject(ctx context.Context, keyPrefix string, fn func(key string, size int64, mod time.Time) error) error {
	raw := listPrefix(b.prefix, keyPrefix)
	it := b.client.Bucket(b.bucket).Objects(ctx, &storage.Query{Prefix: raw})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("gcs object store: listing %q in bucket %q: %w", raw, b.bucket, err)
		}
		if attrs.Name == "" {
			// A synthetic prefix entry. Impossible without a delimiter, but
			// skipping it costs nothing and a nameless entry would otherwise
			// become an invalid key later.
			continue
		}
		key, ok := storeKey(b.prefix, attrs.Name)
		if !ok {
			// Another deployment's object sharing the bucket. Skipped
			// silently: it is not this backend's, and logging it would be
			// noise on every list.
			continue
		}
		if _, under := objectstore.TrimKeyPrefix(key, keyPrefix); !under {
			continue
		}
		if err := fn(key, attrs.Size, attrs.Updated); err != nil {
			return err
		}
	}
}

// Flush is a no-op that reports the context's state, and that is the whole
// implementation.
//
// The interface makes Flush the only method that promises durability precisely
// because a backend is allowed to queue. This one does not: Put and Delete
// return only once GCS has acknowledged the write, so by the time any caller
// can reach Flush there is nothing outstanding to wait for and the promise is
// already kept. Flush therefore cannot fail for a reason of its own, and the
// property is provable from outside the process -- flush, throw the client
// away, build a fresh one, read the objects back.
//
// The alternative shape, a queue drained here, is what doc.go rejects: it would
// have put a second retry regime underneath the engine's own, and would have
// made this the one method whose correctness could not be demonstrated by a
// test that does not know the queue's internals.
func (b *Backend) Flush(ctx context.Context) error {
	// Still honour cancellation. A caller flushing with a dead context is
	// shutting down, and returning nil would tell it the state was persisted
	// during a window in which nothing was even attempted.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("gcs object store: flush: %w", err)
	}
	return nil
}

// Close releases the client's connections.
//
// objectstore.Backend has no Close method, deliberately: requiring one would
// force every stateless backend to write a no-op, and widening a published
// interface later is a breaking change for exactly the third-party modules the
// registry exists to support. The engine type-asserts io.Closer instead, so a
// backend that holds something implements it and gets a real release point.
// This is that case -- storage.Client owns an HTTP client with a connection
// pool and, when OpenTelemetry metrics are enabled, a background exporter --
// and it is worth recording that the accommodation was already in core: GCS
// needed no seam change to be closeable.
func (b *Backend) Close() error {
	if err := b.client.Close(); err != nil {
		return fmt.Errorf("gcs object store: closing the Cloud Storage client: %w", err)
	}
	return nil
}
