//go:build fakegcsserver

// Kill and resume, against a real Cloud Storage implementation.
//
// The choreography lives in pkg/engine/objectstore/enginetest, shared verbatim
// with modules/objectstore-s3, and this file is the handful of GCS the suite
// needs: how to register a factory, how to get an empty bucket, and how to read
// that bucket back with a client that is not the backend under test. See that
// package for what the cycle asserts and why.
//
// # Why it is worth running twice
//
// The suite's strongest assertion is that the store.db object *in the store* is
// a valid, queryable SQLite database holding every row -- pulled down with the
// independent client below, not read back through the backend that wrote it.
// That is what proves the session snapshot's wal_checkpoint(TRUNCATE) survived
// a real upload, and "a real upload" is a different thing in each backend: S3
// streams a single PutObject with an explicit ContentLength, while this one
// goes through storage.Writer, whose Close is where the SDK compares the CRC32C
// GCS computed against the one it computed itself and fails the write on a
// mismatch. A checkpoint that produced subtly wrong bytes fails differently in
// the two, and neither failure is reachable from the in-process fake.
//
// A session store.db in this suite is a few hundred kilobytes, comfortably
// under chunkSizeFor's threshold, so it is the single-request upload path that
// is exercised here; TestLargeObjectRoundTrip in fakegcsserver_test.go covers
// the resumable one.
package gcsstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/enginetest"
	"google.golang.org/api/iterator"

	gcsstore "github.com/frankbardon/nexus/modules/objectstore-gcs"
)

// TestResumeSuiteAgainstFakeGCSServer is the story: the shared kill-and-resume
// cycle, with a real Cloud Storage implementation in another process underneath
// it.
func TestResumeSuiteAgainstFakeGCSServer(t *testing.T) {
	target := requireFakeGCSServer(t)
	enginetest.RunResumeSuite(t, &fakeGCSStore{target: target, raw: newRawClient(t, target)})
}

// fakeGCSStore is the GCS half of enginetest.Store.
type fakeGCSStore struct {
	target fakeGCSTarget
	raw    *storage.Client
}

func (s *fakeGCSStore) Name() string { return "fake-gcs-server" }

// Register isolates the Google credential environment on the test goroutine
// before any engine boots, so a developer's `gcloud auth
// application-default login` cannot decide what these backends authenticate as
// -- and cannot send a real credential to an emulator. The backends themselves
// then take the unauthenticated path resolveCredentials selects when an
// endpoint is set and nothing else is available, which is the only path an
// emulator can exercise.
func (s *fakeGCSStore) Register(t *testing.T, label string, wrap func(objectstore.Backend) objectstore.Backend) string {
	t.Helper()
	isolateGoogleEnv(t)

	name := "fakegcs-" + label + "-" + strings.ReplaceAll(t.Name(), "/", "-")
	objectstore.Register(name, func(ctx context.Context, cfg objectstore.Config) (objectstore.Backend, error) {
		b, err := gcsstore.New(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if wrap != nil {
			return wrap(b), nil
		}
		return b, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })
	return name
}

func (s *fakeGCSStore) NewBucket(t *testing.T) string {
	t.Helper()
	return newBucket(t, s.raw)
}

// ObjectStoreYAML names no region, which is the difference from the S3 module's
// block and the reason enginetest asks the store for it rather than building it
// itself: a GCS bucket's location is a property of the bucket, so New accepts a
// region only to warn that it is being ignored.
func (s *fakeGCSStore) ObjectStoreYAML(backendName, bucket string, policy objectstore.FailurePolicy) string {
	return fmt.Sprintf(
		"    backend: %s\n    bucket: %s\n    endpoint: %s\n    failure_policy: %s\n",
		enginetest.YAMLString(backendName),
		enginetest.YAMLString(bucket),
		enginetest.YAMLString(s.target.endpoint),
		enginetest.YAMLString(string(policy)),
	)
}

func (s *fakeGCSStore) Keys(t *testing.T, bucket string) []string {
	t.Helper()
	var out []string
	it := s.raw.Bucket(bucket).Objects(context.Background(), nil)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("listing bucket %q: %v", bucket, err)
		}
		out = append(out, attrs.Name)
	}
}

func (s *fakeGCSStore) Get(t *testing.T, bucket, key string) []byte {
	t.Helper()
	r, err := s.raw.Bucket(bucket).Object(key).NewReader(context.Background())
	if err != nil {
		t.Fatalf("getting %q from bucket %q: %v", key, bucket, err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading %q: %v", key, err)
	}
	// Close reports the CRC32C mismatch the SDK checks for, which is the one
	// error that means these bytes are wrong rather than missing -- and this
	// reader is the suite's independent witness, so a silent corruption here
	// would be the worst possible thing to discard.
	if err := r.Close(); err != nil {
		t.Fatalf("verifying %q: %v", key, err)
	}
	return body
}

func (s *fakeGCSStore) Exists(t *testing.T, bucket, key string) bool {
	t.Helper()
	_, err := s.raw.Bucket(bucket).Object(key).Attrs(context.Background())
	return err == nil
}
