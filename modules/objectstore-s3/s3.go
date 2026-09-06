package s3store

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
)

// Backend is the S3 implementation of objectstore.Backend.
//
// It holds no mutable state beyond the SDK client, which is itself safe for
// concurrent use, so the concurrency requirement on the interface is met by
// construction rather than by a mutex. That is a direct consequence of the
// synchronous design recorded in doc.go: there is no queue to guard.
type Backend struct {
	api    *s3.Client
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
var _ objectstore.Backend = (*Backend)(nil)

// Hydrate downloads every object under keyPrefix into destDir, stripping the
// prefix.
//
// Objects are fetched one at a time rather than in parallel. Concurrency was
// left out because hydration happens once, before a session opens, over a tree
// of small files where the cost is dominated by per-request latency that a
// worker pool would only partly hide -- and because a partly-parallel hydration
// that fails halfway leaves a harder-to-reason-about tree. If a large-tree
// deployment makes this the bottleneck, a bounded worker pool is the change,
// and List already gives the full work set up front to size it with.
func (b *Backend) Hydrate(ctx context.Context, keyPrefix string, destDir string) error {
	if err := objectstore.ValidateKeyPrefix(keyPrefix); err != nil {
		return fmt.Errorf("s3 object store: hydrate: %w", err)
	}
	// Absolute, so the containment check in localPathForKey compares like with
	// like. A relative destDir would make the prefix test meaningless.
	dest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("s3 object store: hydrate destination %q: %w", destDir, err)
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
		if err := b.download(ctx, joinKey(b.prefix, key), path); err != nil {
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
	b.log.Debug("s3 object store hydrated",
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
func (b *Backend) download(ctx context.Context, objectKey, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("s3 object store: creating %q: %w", filepath.Dir(path), err)
	}

	out, err := b.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return fmt.Errorf("s3 object store: getting %q from bucket %q: %w", objectKey, b.bucket, err)
	}
	defer out.Body.Close()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".nexus-hydrate-*")
	if err != nil {
		return fmt.Errorf("s3 object store: staging %q: %w", path, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on every failure path below. Harmless after a
	// successful rename, where the name no longer exists.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, out.Body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("s3 object store: writing %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("s3 object store: closing %q: %w", tmpName, err)
	}
	// CreateTemp makes 0600 files; the session tree is 0644/0755 throughout, so
	// match it or a hydrated tree reads differently from a locally created one.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("s3 object store: setting mode on %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("s3 object store: placing %q: %w", path, err)
	}
	return nil
}

// Put uploads localPath to key, replacing whatever was there.
//
// The file is opened and handed to the SDK as the request body. It is never
// modified or moved: this is the session's live working copy, still being read
// by the run that produced it.
//
// The S3 transfer manager (feature/s3/manager) was considered for its automatic
// multipart upload and rejected. Nexus session artifacts are transcripts,
// notes and JSONL journals -- kilobytes to low megabytes -- so the multipart
// path would never execute in any test this repository can run, neither the
// in-process fake nor MinIO. An untested code path inside the component whose
// entire job is durability is a worse trade than the documented ceiling: a
// single PutObject caps one object at 5 GiB. If an artifact that large ever
// becomes real, the manager is the change to make, and it is a change confined
// to this method.
func (b *Backend) Put(ctx context.Context, key string, localPath string) error {
	if err := objectstore.ValidateKey(key); err != nil {
		return fmt.Errorf("s3 object store: put: %w", err)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("s3 object store: reading %q for key %q: %w", localPath, key, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("s3 object store: stating %q: %w", localPath, err)
	}
	if info.IsDir() {
		// Otherwise the read below fails with a platform-dependent errno and a
		// message that does not say what went wrong.
		return fmt.Errorf("s3 object store: %q is a directory, not a file", localPath)
	}

	// ContentLength is set explicitly from the stat rather than left for the
	// SDK to infer. An *os.File is seekable, so the SDK can size and re-read it
	// for a retry -- but only if it is told the length up front; without it the
	// request falls back to chunked encoding, which several S3-compatible
	// stores reject.
	objectKey := joinKey(b.prefix, key)
	if _, err := b.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(objectKey),
		Body:          f,
		ContentLength: aws.Int64(info.Size()),
	}); err != nil {
		return fmt.Errorf("s3 object store: putting %q into bucket %q: %w", objectKey, b.bucket, err)
	}
	return nil
}

// Delete removes key. A key that was never there is a success, per the
// interface and per S3 itself, which is what lets the engine retry a delete
// without special-casing the second attempt.
func (b *Backend) Delete(ctx context.Context, key string) error {
	if err := objectstore.ValidateKey(key); err != nil {
		return fmt.Errorf("s3 object store: delete: %w", err)
	}
	objectKey := joinKey(b.prefix, key)
	if _, err := b.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(objectKey),
	}); err != nil {
		return fmt.Errorf("s3 object store: deleting %q from bucket %q: %w", objectKey, b.bucket, err)
	}
	return nil
}

// List returns every object under keyPrefix, following every page.
func (b *Backend) List(ctx context.Context, keyPrefix string) ([]objectstore.Object, error) {
	if err := objectstore.ValidateKeyPrefix(keyPrefix); err != nil {
		return nil, fmt.Errorf("s3 object store: list: %w", err)
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
// The single place pagination is handled. Returning only the first page is the
// canonical way to be quietly wrong against S3 -- the default page size is 1000
// and most test trees are smaller -- so both List and Hydrate go through here
// rather than each driving the paginator themselves.
//
// Every key is put through the segment-aware filter even though listPrefix has
// already asked the server for a "/"-terminated prefix. The server-side filter
// is an optimisation over an S3 API that is only specified to match raw bytes;
// this is the correctness boundary, and it is what stops session "sess-1" from
// hydrating the objects of "sess-10".
func (b *Backend) eachObject(ctx context.Context, keyPrefix string, fn func(key string, size int64, mod time.Time) error) error {
	raw := listPrefix(b.prefix, keyPrefix)
	input := &s3.ListObjectsV2Input{Bucket: aws.String(b.bucket)}
	if raw != "" {
		input.Prefix = aws.String(raw)
	}

	pager := s3.NewListObjectsV2Paginator(b.api, input)
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("s3 object store: listing %q in bucket %q: %w", raw, b.bucket, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			key, ok := storeKey(b.prefix, *obj.Key)
			if !ok {
				// Another deployment's object sharing the bucket. Skipped
				// silently: it is not this backend's, and logging it would be
				// noise on every list.
				continue
			}
			if _, under := objectstore.TrimKeyPrefix(key, keyPrefix); !under {
				continue
			}
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			var mod time.Time
			if obj.LastModified != nil {
				mod = *obj.LastModified
			}
			if err := fn(key, size, mod); err != nil {
				return err
			}
		}
	}
	return nil
}

// Flush is a no-op that reports the context's state, and that is the whole
// implementation.
//
// The interface makes Flush the only method that promises durability precisely
// because a backend is allowed to queue. This one does not: Put and Delete
// return only once S3 has acknowledged the write, so by the time any caller can
// reach Flush there is nothing outstanding to wait for and the promise is
// already kept. Flush therefore cannot fail for a reason of its own, and the
// property is provable from outside the process -- flush, throw the client
// away, build a fresh one, read the objects back -- which is what the
// against-MinIO suite does.
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
		return fmt.Errorf("s3 object store: flush: %w", err)
	}
	return nil
}
