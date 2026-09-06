//go:build minio

// Kill and resume, against a real S3 implementation.
//
// pkg/engine's TestKillAndResumeRestoresIdenticalSessionState (E1-S5) proves
// the headline behaviour of this whole effort -- a session survives the death
// of the process that produced it and resumes elsewhere with the same state --
// against objectstoretest.Memory. The cycle with a real store in the middle
// lives in enginetest.RunResumeSuite, and this file is the twenty lines of S3
// that suite needs: how to register a factory, how to get an empty bucket, and
// how to read that bucket back with a client that is not the backend under
// test.
//
// # Why the choreography is not in this file
//
// It used to be. It is shared now because modules/objectstore-gcs runs the
// identical cycle against fake-gcs-server, and two hand-maintained copies of a
// 500-line kill-and-resume sequence diverge the first time either is fixed --
// with the divergence showing up as one backend quietly proving less than the
// other rather than as a failure. Everything in that sequence is written
// against objectstore.Backend and engine YAML, so there was nothing S3-shaped
// left in it once the four hooks below were named. See
// pkg/engine/objectstore/enginetest for what it asserts and why.
//
// # What a real store can break that the in-process fake cannot
//
//   - The WAL checkpoint. E1-S4 uploads a per-plugin store.db by running
//     wal_checkpoint(TRUNCATE) and then VACUUM INTO a staging copy. Against the
//     memory backend "upload" is a []byte copied inside the process, so a
//     checkpoint that produced a subtly wrong file would still round-trip
//     byte-for-byte and still open. Here the bytes are streamed out over HTTP
//     with an explicit ContentLength, stored by another process, and streamed
//     back -- and the suite pulls the object down through Get below and opens
//     it as a database, so "what MinIO is actually holding is a valid,
//     queryable SQLite file" is asserted rather than inferred.
//   - Durability outside this process. The memory backend's second engine reads
//     the same Go map the first one wrote to; a backend that never sent
//     anything would pass. Here the second engine is given a different
//     t.TempDir() *and* everything it reads has to have crossed a socket.
//   - Key escaping and latency. Session trees contain nested directories and
//     hundreds of small objects, and every one of them is a signed round trip.
//
// # What is deliberately NOT repeated from E1-S5
//
// The whole-tree byte comparison (TestHydratedTreeMatchesTheKilledTree) is not
// reproduced. It compares against objectStoreExcluded, an unexported engine
// predicate about which local paths the seam is allowed to sync -- a rule that
// is entirely store-independent, so re-deriving it out here would duplicate
// engine internals to test something MinIO cannot influence. The categories the
// story names (history, artifacts, blobs, per-plugin SQLite) are asserted
// individually by the shared suite instead.
package s3store_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/enginetest"

	s3store "github.com/frankbardon/nexus/modules/objectstore-s3"
)

// TestResumeSuiteAgainstMinIO is the story: the shared kill-and-resume cycle,
// with a real S3 implementation in another process underneath it.
func TestResumeSuiteAgainstMinIO(t *testing.T) {
	target := requireMinIO(t)
	enginetest.RunResumeSuite(t, &minioStore{target: target, raw: newRawClient(t, target)})
}

// minioStore is the S3 half of enginetest.Store.
type minioStore struct {
	target minioTarget
	raw    *s3.Client
}

func (s *minioStore) Name() string { return "MinIO" }

// Register puts the credentials in through the environment leg of the SDK's
// default chain -- the one leg an emulator can exercise at all -- with
// isolateAWSEnv first so a developer's populated ~/.aws cannot decide what this
// authenticates as. Done here rather than inside the factory because t.Setenv
// must run on the test goroutine and the factory runs during Boot.
func (s *minioStore) Register(t *testing.T, label string, wrap func(objectstore.Backend) objectstore.Backend) string {
	t.Helper()
	isolateAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", s.target.accessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", s.target.secretKey)

	name := "minio-" + label + "-" + strings.ReplaceAll(t.Name(), "/", "-")
	objectstore.Register(name, func(ctx context.Context, cfg objectstore.Config) (objectstore.Backend, error) {
		b, err := s3store.New(ctx, cfg)
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

func (s *minioStore) NewBucket(t *testing.T) string {
	t.Helper()
	return newBucket(t, s.raw)
}

// ObjectStoreYAML names a region because SigV4 signs over one; MinIO has no
// regions, but a request signed in a different scope than the server verifies
// in is a 403 that says nothing useful.
func (s *minioStore) ObjectStoreYAML(backendName, bucket string, policy objectstore.FailurePolicy) string {
	return fmt.Sprintf(
		"    backend: %s\n    bucket: %s\n    region: %s\n    endpoint: %s\n    failure_policy: %s\n",
		enginetest.YAMLString(backendName),
		enginetest.YAMLString(bucket),
		enginetest.YAMLString(minioRegion),
		enginetest.YAMLString(s.target.endpoint),
		enginetest.YAMLString(string(policy)),
	)
}

func (s *minioStore) Keys(t *testing.T, bucket string) []string {
	t.Helper()
	var out []string
	pager := s3.NewListObjectsV2Paginator(s.raw, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(context.Background())
		if err != nil {
			t.Fatalf("listing bucket %q: %v", bucket, err)
		}
		for _, obj := range page.Contents {
			out = append(out, aws.ToString(obj.Key))
		}
	}
	return out
}

func (s *minioStore) Get(t *testing.T, bucket, key string) []byte {
	t.Helper()
	out, err := s.raw.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("getting %q from bucket %q: %v", key, bucket, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("reading %q: %v", key, err)
	}
	return body
}

func (s *minioStore) Exists(t *testing.T, bucket, key string) bool {
	t.Helper()
	_, err := s.raw.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	return err == nil
}
