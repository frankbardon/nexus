//go:build minio

// The against-MinIO suite: the same backend, the same exported conformance
// suite, but pointed at a real S3 implementation in another process instead of
// the loopback fake in fakes3_test.go.
//
// # Why this exists when the fake already runs the same suite
//
// The fake agrees with S3 exactly as far as its author understood S3, and it is
// the same author. Everything the fake asserts about the wire -- that a
// signature was presented, that the path began with the bucket, that
// aws-chunked was not used, that the paginator was followed -- is asserted
// against an implementation written from the same reading of the same
// documentation. A misreading is invisible to it by construction.
//
// MinIO is not: it is an independent implementation of the S3 API that rejects
// what real S3 rejects. What that buys, concretely:
//
//   - SigV4 over the *canonical* request. The fake accepts any Authorization
//     header, so a key that is escaped wrongly in the canonical URI -- a space,
//     a CJK segment -- signs and stores happily there and 403s against anything
//     that verifies. TestKeyLayoutMirrorsTheLocalTree here is the same test as
//     its fake counterpart and a materially stronger one.
//   - The real 1000-key ListObjectsV2 page. The fake's page size is 50, chosen
//     so `make test` does not pay 1200 signed round trips; that proves the
//     paginator is followed but not that it is followed at the boundary a real
//     store actually has. This suite runs the conformance suite at its default
//     1200 probe count, and TestListCrossesTheRealPageBoundary first proves the
//     boundary is where it is claimed to be.
//   - Durability outside this process. E1-S3 recorded that an in-process
//     backend cannot prove Flush: the fake's "fresh client" reads back through
//     the same Go map the first one wrote to, so a backend that never sent
//     anything would pass. Here the bytes have to have reached another process.
//   - Remote error mapping. The fake produces the errors it was told to; MinIO
//     produces the ones S3 produces, including a 403 for a bad signature, which
//     the fake structurally cannot generate because it does not verify one.
//
// # What this suite deliberately does not claim
//
// Not IAM. The IRSA / EKS Pod Identity, ECS task-role and IMDSv2 legs of the
// credential chain -- the reason doc.go takes the AWS SDK at all -- are
// reproduced by no emulator, MinIO included, and nothing here pretends
// otherwise. The PRD carries that as the emulator-fidelity risk and it stays
// open. "Denied access" below is a signature/identity denial, not a policy
// denial, and is named accordingly.
//
// Not multipart upload. Put is single-part by design (see its doc comment), so
// there is no multipart code path to exercise; TestLargeObjectRoundTrip pins
// what there is instead -- that a multi-megabyte body survives a real store,
// which is where an incorrect ContentLength or an unwanted aws-chunked framing
// actually shows up.
package s3store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"

	s3store "github.com/frankbardon/nexus/modules/objectstore-s3"
)

// The knobs, all env-supplied so the same test binary serves three callers:
// `make test-objectstore-minio` (which starts a container and sets all four),
// CI (the same target), and a developer with their own MinIO already running
// who exports NEXUS_TEST_MINIO_ENDPOINT and nothing else.
const (
	envEndpoint  = "NEXUS_TEST_MINIO_ENDPOINT"
	envAccessKey = "NEXUS_TEST_MINIO_ACCESS_KEY"
	envSecretKey = "NEXUS_TEST_MINIO_SECRET_KEY"

	// envRequired turns the absent-MinIO skip into a failure.
	//
	// This is the whole answer to "a suite that silently skips in CI is green
	// forever". The skip has to exist -- a laptop without Docker must still be
	// able to run `go test -tags minio ./...` without a spurious red -- but a
	// skip is indistinguishable from a pass in a CI summary, so the caller that
	// *provisioned* MinIO sets this and gets a hard failure if the suite would
	// have skipped anyway. scripts/with-minio.sh sets it unconditionally,
	// because by the time it runs the command it has already waited for the
	// health endpoint; a skip after that is a bug in this file, not a missing
	// dependency.
	envRequired = "NEXUS_TEST_MINIO_REQUIRED"
)

// Defaults for a hand-started MinIO. scripts/with-minio.sh sets all three
// explicitly -- it publishes on a Docker-chosen port precisely so it cannot
// collide with a MinIO somebody already has on 9000 -- so these are what a
// developer running `go test -tags minio ./...` directly gets, against the
// conventional port and the credentials the script would have used.
const (
	defaultEndpoint  = "http://127.0.0.1:9000"
	defaultAccessKey = "nexusminio"
	defaultSecretKey = "nexusminio-secret"

	// MinIO has no regions, but SigV4 signs over one. This is the value
	// fallbackRegion would supply anyway; naming it explicitly here keeps the
	// raw admin client below signing in the same scope as the backend under
	// test, so a mismatch shows up as a test bug rather than as a 403.
	minioRegion = "us-east-1"
)

// minioTarget is a reachable MinIO.
type minioTarget struct {
	endpoint  string
	accessKey string
	secretKey string
}

// requireMinIO resolves the target and proves something is listening, skipping
// the test when nothing is -- or failing it when the caller promised there
// would be.
//
// The probe is a bare HTTP request to the endpoint root rather than to MinIO's
// /minio/health/live. Any S3 endpoint answers the root with *something* (real
// S3 and MinIO both answer an unsigned request with 403), so "the TCP connect
// and the HTTP exchange completed" is the reachability question, and asking it
// this way keeps the file usable against a non-MinIO S3-compatible store
// without pretending the health path is standard. Status codes are ignored on
// purpose: a 403 here means a server answered, which is all this needs to know.
// Whether the credentials are any good is the tests' business, and they say so
// loudly.
func requireMinIO(t *testing.T) minioTarget {
	t.Helper()

	target := minioTarget{
		endpoint:  envOr(envEndpoint, defaultEndpoint),
		accessKey: envOr(envAccessKey, defaultAccessKey),
		secretKey: envOr(envSecretKey, defaultSecretKey),
	}

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target.endpoint, nil)
	if err != nil {
		t.Fatalf("%s = %q is not a usable URL: %v", envEndpoint, target.endpoint, err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return target
	}

	msg := fmt.Sprintf("no MinIO at %s (%v); start one with `make test-objectstore-minio`, "+
		"or point %s at your own", target.endpoint, err, envEndpoint)
	if os.Getenv(envRequired) != "" {
		// The caller said it provisioned MinIO. A skip here would report green
		// while testing nothing, which is exactly the failure this suite exists
		// to make impossible.
		t.Fatalf("%s is set, so this suite must not skip: %s", envRequired, msg)
	}
	t.Skip(msg)
	return minioTarget{}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// TestMinIOIsReachable is the guard on the guard.
//
// It asserts nothing about the backend. It exists so that "was MinIO actually
// there?" is a named line in the test output rather than something a reader has
// to infer from the absence of skips, and so that the required/skip decision is
// exercised once by itself instead of only as a side effect of a bigger test.
func TestMinIOIsReachable(t *testing.T) {
	target := requireMinIO(t)
	t.Logf("MinIO at %s, required=%t", target.endpoint, os.Getenv(envRequired) != "")
}

// newRawClient builds an S3 client that is NOT the backend under test.
//
// Used for the things the backend has no API for -- creating and emptying a
// bucket, reading the physical keys, asking what one page of a list looks like.
// Credentials are supplied inline rather than through the environment so this
// client is unaffected by whatever isolateAWSEnv has done to the process for
// the backend's benefit: if the backend's credential resolution breaks, the
// assertions made through this client stay trustworthy.
func newRawClient(t *testing.T, target minioTarget) *s3.Client {
	t.Helper()
	return s3.New(s3.Options{
		Region:       minioRegion,
		BaseEndpoint: aws.String(target.endpoint),
		UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     target.accessKey,
				SecretAccessKey: target.secretKey,
				Source:          "nexus minio test",
			}, nil
		}),
		// Match clientOptions in config.go. Not for correctness here -- MinIO
		// copes with either -- but so that a checksum-related failure in the
		// backend cannot be masked or mimicked by this client behaving
		// differently on the same wire.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}

// newBucket creates a fresh bucket and registers its teardown.
//
// A bucket per case, not a prefix per case. The conformance suite calls its
// factory once per case and documents that per-case isolation is the
// implementer's to arrange; a unique *prefix* would arrange it, but it would
// also mean every case in the "no configured prefix" run was secretly running
// with one, and the whole point of running the suite twice is that the
// prefixed and unprefixed key paths are different code. A bucket is the only
// isolation that leaves Config.Prefix free to be empty.
func newBucket(t *testing.T, raw *s3.Client) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generating a bucket name: %v", err)
	}
	// Bucket names are DNS labels: lowercase alphanumerics and hyphens,
	// 3-63 characters, starting and ending alphanumeric.
	bucket := "nexus-e4s3-" + hex.EncodeToString(b[:])

	ctx := context.Background()
	if _, err := raw.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("creating bucket %q: %v", bucket, err)
	}
	t.Cleanup(func() { emptyAndRemoveBucket(t, raw, bucket) })
	return bucket
}

// emptyAndRemoveBucket deletes every object and then the bucket.
//
// Failures are reported, not swallowed. A developer running this against their
// own long-lived MinIO would otherwise accumulate a bucket per case per run
// forever, and a leak here is also a signal in its own right -- the most likely
// reason a delete fails is that the store is not behaving the way the rest of
// this file assumes it does.
func emptyAndRemoveBucket(t *testing.T, raw *s3.Client, bucket string) {
	t.Helper()
	ctx := context.Background()

	pager := s3.NewListObjectsV2Paginator(raw, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			t.Errorf("emptying bucket %q: listing: %v", bucket, err)
			return
		}
		ids := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: obj.Key})
		}
		if len(ids) == 0 {
			continue
		}
		// Batched: the pagination case leaves 1200 objects behind, and 1200
		// individual DeleteObject round trips would cost more than the case
		// that created them.
		out, err := raw.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			t.Errorf("emptying bucket %q: deleting %d objects: %v", bucket, len(ids), err)
			return
		}
		for _, e := range out.Errors {
			t.Errorf("emptying bucket %q: %s: %s", bucket, aws.ToString(e.Key), aws.ToString(e.Message))
		}
	}

	if _, err := raw.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Errorf("removing bucket %q: %v", bucket, err)
	}
}

// newMinIOBackend builds the backend under test against a fresh bucket.
//
// isolateAWSEnv (shared with the fake suite) clears every AWS_* variable and
// points the shared config and credentials files at paths that do not exist, so
// a developer's populated ~/.aws cannot decide what this run authenticates as.
// The MinIO credentials then go in through the environment leg of the SDK's
// default chain, which is the one leg an emulator can exercise at all.
func newMinIOBackend(t *testing.T, target minioTarget, bucket string, mutate ...func(*objectstore.Config)) objectstore.Backend {
	t.Helper()
	isolateAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", target.accessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", target.secretKey)

	cfg := objectstore.Config{
		BackendName: s3store.BackendName,
		Bucket:      bucket,
		Region:      minioRegion,
		Endpoint:    target.endpoint,
		Logger:      quietLogger(),
	}
	for _, m := range mutate {
		m(&cfg)
	}

	b, err := s3store.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("s3store.New against MinIO: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// The conformance suite, against a real store
// ---------------------------------------------------------------------------

// TestMinIOContractSuite is the acceptance criterion: the shared suite, at its
// full default probe count, against an implementation this repository did not
// write.
//
// No WithListProbeCount here, unlike the fake run in s3_test.go. The default is
// 1200 precisely because ListObjectsV2's default page size is 1000, and paying
// that once in a tagged suite is the trade E4-S2 recorded when it capped the
// untagged run at 200 against a 50-key fake page.
// WithoutObjectAtPrefix is the one accommodation made for the emulator, and it
// is a divergence in MinIO rather than a gap in this backend --
// TestMinIOCannotRepresentAnObjectAtAPrefix below measures it and fails if
// MinIO ever stops diverging, at which point this option should come off. The
// semantic itself stays covered: the fake models S3's flat key space correctly
// and asserts it on every `make test`.
func TestMinIOContractSuite(t *testing.T) {
	target := requireMinIO(t)
	raw := newRawClient(t, target)
	objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
		return newMinIOBackend(t, target, newBucket(t, raw))
	}, objectstoretest.WithoutObjectAtPrefix())
}

// TestMinIOContractSuiteUnderAConfiguredPrefix runs it again with a bucket
// prefix, for the reason s3_test.go gives: the suite only ever speaks
// store-relative keys, so a prefix applied on Put but not on List, or stripped
// with a raw string trim, passes it cleanly. Against MinIO this also covers the
// prefix's effect on the *server-side* list filter, which is where a trailing
// slash that is present locally but missing on the wire would show up.
func TestMinIOContractSuiteUnderAConfiguredPrefix(t *testing.T) {
	target := requireMinIO(t)
	raw := newRawClient(t, target)
	objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
		return newMinIOBackend(t, target, newBucket(t, raw), func(c *objectstore.Config) {
			c.Prefix = "prod/nexus"
		})
	}, objectstoretest.WithoutObjectAtPrefix())
}

// TestMinIOCannotRepresentAnObjectAtAPrefix is the receipt for the one option
// the two RunSuite calls above pass.
//
// An accommodation that is merely passed as an option is indistinguishable, a
// year later, from an accommodation nobody remembers the reason for -- and the
// reason matters here, because "the emulator cannot do this" and "the backend
// gets this wrong" look identical in a suite that simply does not run the case.
// So the divergence is measured, with the raw client, going nowhere near the
// backend under test.
//
// What S3 does: the key space is flat, "/" is an ordinary byte, and an object
// named "sessions/sess-1" coexists with "sessions/sess-1/files/a.txt" without
// either affecting the other.
//
// What MinIO does, measured on RELEASE.2025-09-07T16-13-09Z in both
// single-drive and 4-drive erasure modes: the PUT succeeds with 200 and the
// child object stops appearing in any list. It is not merely hidden -- the
// bucket then cannot be emptied by listing and deleting what the list returned,
// which is how this was found in the first place.
//
// If this test starts failing, MinIO has gained a flat key space and
// WithoutObjectAtPrefix should be removed from both suite calls rather than the
// assertion below being relaxed.
func TestMinIOCannotRepresentAnObjectAtAPrefix(t *testing.T) {
	target := requireMinIO(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)
	ctx := context.Background()

	const child = "sessions/sess-1/files/a.txt"
	const atPrefix = "sessions/sess-1"

	put := func(key, body string) {
		t.Helper()
		if _, err := raw.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(key),
			Body:          strings.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		}); err != nil {
			t.Fatalf("raw PutObject(%q) = %v", key, err)
		}
	}
	listAll := func() []string {
		t.Helper()
		page, err := raw.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		if err != nil {
			t.Fatalf("raw ListObjectsV2 = %v", err)
		}
		keys := make([]string, 0, len(page.Contents))
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		slices.Sort(keys)
		return keys
	}

	put(child, "a")
	if got := listAll(); !slices.Equal(got, []string{child}) {
		t.Fatalf("after the first put the bucket holds %v, want [%s]", got, child)
	}

	put(atPrefix, "an object at the prefix itself")

	got := listAll()
	if slices.Equal(got, []string{atPrefix, child}) {
		t.Fatalf("MinIO now holds both %q and %q; it has a flat key space, so "+
			"objectstoretest.WithoutObjectAtPrefix is no longer needed in this file "+
			"and should be removed from both RunSuite calls", atPrefix, child)
	}
	if !slices.Equal(got, []string{atPrefix}) {
		t.Fatalf("bucket holds %v; expected MinIO's measured behaviour, which is "+
			"that the object at the prefix replaces everything under it", got)
	}

	// The other half of the damage, and the reason emptyAndRemoveBucket cannot
	// clean up after this case: the shadowed object is not listable, so a
	// list-then-delete leaves the bucket non-empty. Deleting the object at the
	// prefix first is what makes the child reachable again.
	if _, err := raw.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(atPrefix),
	}); err != nil {
		t.Fatalf("raw DeleteObject(%q) = %v", atPrefix, err)
	}
	if got := listAll(); !slices.Equal(got, []string{child}) {
		t.Errorf("after removing the object at the prefix the bucket holds %v, want [%s]", got, child)
	}
}

// ---------------------------------------------------------------------------
// The things only a real store can prove
// ---------------------------------------------------------------------------

// TestFlushDurabilityThroughAFreshClientAgainstMinIO is the story's central
// assertion and the one E1-S3 explicitly flagged as unprovable in process.
//
// TestFlushIsProvableThroughAFreshClient in s3_test.go performs the same
// choreography against the fake and proves less than it looks like it does: the
// "fresh" client reads back through the very Go map the first client wrote
// into, so a backend that buffered everything in the fake's memory -- or, for
// that matter, a fake that lied -- would satisfy it. Nothing about that run
// distinguishes "durable" from "still in this process".
//
// Here the two clients are separated by a process boundary. The first backend
// writes, Flush returns, and then a *second* backend -- its own aws.Config, its
// own credential resolution, its own *s3.Client, its own connection pool --
// reads the bytes back out of MinIO. Nothing in the first backend's memory can
// satisfy the second one. That is what makes the synchronous-Put decision in
// doc.go checkable rather than merely asserted: if Put ever started queueing,
// this test is where it would be caught.
//
// The read-back deliberately goes through both of Backend's read paths. List
// proves the object is enumerable by a client that never saw the write, and
// Hydrate proves the bytes themselves survived, which List's metadata alone
// would not.
func TestFlushDurabilityThroughAFreshClientAgainstMinIO(t *testing.T) {
	target := requireMinIO(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)
	ctx := context.Background()

	// Two payloads, so the assertion is about content and not just presence,
	// and one of them is a delete so Flush's promise covers removals too --
	// a queued delete is as durable a lie as a queued write.
	const key = "metadata/session.json"
	const want = `{"id":"sess-e4s3","turns":7}`

	first := newMinIOBackend(t, target, bucket)
	local := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(local, []byte(want), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	doomed := filepath.Join(t.TempDir(), "doomed.txt")
	if err := os.WriteFile(doomed, []byte("removed before the flush"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := first.Put(ctx, key, local); err != nil {
		t.Fatalf("Put = %v", err)
	}
	if err := first.Put(ctx, "files/doomed.txt", doomed); err != nil {
		t.Fatalf("Put = %v", err)
	}
	if err := first.Delete(ctx, "files/doomed.txt"); err != nil {
		t.Fatalf("Delete = %v", err)
	}
	if err := first.Flush(ctx); err != nil {
		t.Fatalf("Flush = %v", err)
	}

	// From here on the first backend is not touched again. Everything below is
	// a client that has never issued a write.
	second := newMinIOBackend(t, target, bucket)

	objs, err := second.List(ctx, "")
	if err != nil {
		t.Fatalf("List through a fresh client = %v", err)
	}
	keys := make([]string, 0, len(objs))
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{key}) {
		t.Fatalf("List through a fresh client = %v, want exactly [%s]; "+
			"either the Put or the Delete did not survive Flush", keys, key)
	}

	dest := t.TempDir()
	if err := second.Hydrate(ctx, "", dest); err != nil {
		t.Fatalf("Hydrate through a fresh client = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("reading hydrated %q: %v", key, err)
	}
	if string(got) != want {
		t.Errorf("hydrated through a fresh client = %q, want %q", got, want)
	}
}

// TestListCrossesTheRealPageBoundary does two things the fake cannot.
//
// First it *proves the boundary is where the code assumes it is*, instead of
// taking 1000 on trust from the documentation: a raw single ListObjectsV2 with
// no MaxKeys comes back truncated at exactly 1000 keys. If MinIO's default ever
// changed, or if 1000 had been the wrong number all along, this line fails and
// says so -- and it is the premise the conformance suite's 1200-object probe
// rests on.
//
// Then it crosses that boundary with the segment-aware prefix rule engaged,
// which is the combination that actually bites: "sessions/sess-1" and
// "sessions/sess-10" collide under raw byte matching, the paginator has to be
// followed to see all of the first one, and a backend that post-filtered only
// the first page would return a plausible-looking 1000 objects. The counts here
// are chosen so a first-page-only bug returns 1000 rather than the correct
// 1005 -- close enough to look right in a log, which is why the assertion is on
// the exact number.
func TestListCrossesTheRealPageBoundary(t *testing.T) {
	target := requireMinIO(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)
	b := newMinIOBackend(t, target, bucket)
	ctx := context.Background()

	// Just past the page boundary, and a sibling whose key is a raw-byte prefix
	// match for the one under test.
	const mine = 1005
	const neighbours = 5

	src := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	want := make([]string, 0, mine)
	for i := range mine {
		key := fmt.Sprintf("sessions/sess-1/files/%06d.bin", i)
		if err := b.Put(ctx, key, src); err != nil {
			t.Fatalf("Put(%q) = %v", key, err)
		}
		want = append(want, key)
	}
	for i := range neighbours {
		key := fmt.Sprintf("sessions/sess-10/files/%06d.bin", i)
		if err := b.Put(ctx, key, src); err != nil {
			t.Fatalf("Put(%q) = %v", key, err)
		}
	}

	// The premise. One unpaginated request must come back truncated, at the
	// page size the 1200-object conformance probe was sized against.
	page, err := raw.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("raw ListObjectsV2 = %v", err)
	}
	if !aws.ToBool(page.IsTruncated) {
		t.Fatalf("one raw ListObjectsV2 over %d objects was not truncated; "+
			"this store does not have the page boundary the suite's probe count assumes",
			mine+neighbours)
	}
	if got := len(page.Contents); got != 1000 {
		t.Errorf("one raw ListObjectsV2 page = %d keys, want 1000; "+
			"the conformance suite's 1200-object probe is sized against 1000", got)
	}

	got, err := b.List(ctx, "sessions/sess-1")
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	gotKeys := make([]string, 0, len(got))
	for _, o := range got {
		gotKeys = append(gotKeys, o.Key)
		if strings.HasPrefix(o.Key, "sessions/sess-10/") {
			t.Fatalf("List(%q) returned %q from the neighbouring session", "sessions/sess-1", o.Key)
		}
	}
	slices.Sort(gotKeys)
	if len(gotKeys) != mine {
		t.Fatalf("List(sessions/sess-1) = %d objects, want %d "+
			"(a paginator that stops after one page returns 1000)", len(gotKeys), mine)
	}
	if !slices.Equal(gotKeys, want) {
		t.Error("List(sessions/sess-1) returned the wrong set of keys across the page boundary")
	}

	// And the same rule through Hydrate, where getting it wrong writes another
	// session's files into this one's tree.
	dest := t.TempDir()
	if err := b.Hydrate(ctx, "sessions/sess-1", dest); err != nil {
		t.Fatalf("Hydrate = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dest, "files"))
	if err != nil {
		t.Fatalf("reading hydrated tree: %v", err)
	}
	if len(entries) != mine {
		t.Errorf("hydrated %d files, want %d", len(entries), mine)
	}
}

// TestLargeObjectRoundTrip pushes a multi-megabyte object through a real store.
//
// Put is deliberately single-part (see its doc comment: the transfer manager
// was rejected rather than shipped untested), so this is not a multipart test
// and must not be read as one. What it covers is everything single-part
// PutObject can still get wrong on a body too large to fit in one buffer, and
// which only a store that actually parses the request will notice: an
// unset or wrong ContentLength, a fall back to chunked transfer encoding, an
// aws-chunked framing stored as if it were content, or a body the SDK cannot
// rewind for a retry.
//
// The payload is deterministic pseudorandom rather than repeated bytes, so a
// truncation or an off-by-one in the middle cannot be hidden by the surrounding
// content being identical, and so nothing on the path can usefully compress it.
// It is compared by sha256 rather than by bytes.Equal so a failure prints two
// hashes instead of eight megabytes.
func TestLargeObjectRoundTrip(t *testing.T) {
	target := requireMinIO(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)
	b := newMinIOBackend(t, target, bucket)
	ctx := context.Background()

	// 8 MiB: comfortably past any single read buffer and past the SDK's
	// in-memory thresholds, while staying cheap enough on loopback that this
	// suite's wall clock is still dominated by the 1200-object probe.
	const size = 8 << 20
	payload := make([]byte, size)
	rng := mrand.New(mrand.NewPCG(0x6e657875, 0x73453453)) // fixed seed: reproducible failures
	for i := range payload {
		payload[i] = byte(rng.Uint32())
	}
	wantSum := sha256.Sum256(payload)

	local := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(local, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	const key = "files/big.bin"
	if err := b.Put(ctx, key, local); err != nil {
		t.Fatalf("Put of an %d-byte object = %v", size, err)
	}

	// What the store thinks it holds, asked of a client that did not write it.
	// A size mismatch here is the aws-chunked failure mode: the framing lands
	// in the object and the length is larger than the file.
	head, err := raw.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("HeadObject = %v", err)
	}
	if got := aws.ToInt64(head.ContentLength); got != size {
		t.Fatalf("stored object is %d bytes, want %d; the body was reframed on the wire", got, size)
	}

	// And what List reports, which is the number the engine's push path diffs
	// against the local tree.
	objs, err := b.List(ctx, "files")
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	if len(objs) != 1 || objs[0].Size != size {
		t.Errorf("List = %+v, want one object of %d bytes", objs, size)
	}

	dest := t.TempDir()
	if err := b.Hydrate(ctx, "", dest); err != nil {
		t.Fatalf("Hydrate = %v", err)
	}
	f, err := os.Open(filepath.Join(dest, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("opening hydrated object: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		t.Fatalf("reading hydrated object: %v", err)
	}
	if n != size {
		t.Fatalf("hydrated object is %d bytes, want %d", n, size)
	}
	if gotSum := h.Sum(nil); !bytes.Equal(gotSum, wantSum[:]) {
		t.Errorf("hydrated sha256 = %x, want %x", gotSum, wantSum)
	}
}

// TestKeyLayoutMirrorsTheLocalTreeAgainstMinIO is the same assertion as its
// counterpart in s3_test.go and a materially stronger one.
//
// Against the fake it proves the backend built the keys it meant to. Against
// MinIO it proves those keys are *signable and retrievable*: the space in
// "with spaces", the CJK segment and the leading dots all have to be escaped
// one specific way in the canonical URI SigV4 is computed over, and any other
// spelling is a 403 from a store that verifies signatures. The fake verifies
// none, so this is the class of bug it is structurally blind to.
//
// The physical keys are read back with the raw client, not with List, because
// List round-trips through the same joinKey/storeKey pair that produced them --
// a mapping that was wrong in both directions would agree with itself.
func TestKeyLayoutMirrorsTheLocalTreeAgainstMinIO(t *testing.T) {
	target := requireMinIO(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)

	const prefix = "prod/nexus"
	b := newMinIOBackend(t, target, bucket, func(c *objectstore.Config) { c.Prefix = prefix })
	ctx := context.Background()

	src := t.TempDir()
	want := make([]string, 0, len(awkwardKeys))
	for key, content := range awkwardKeys {
		local := filepath.Join(src, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := b.Put(ctx, key, local); err != nil {
			t.Fatalf("Put(%q) against MinIO = %v", key, err)
		}
		want = append(want, prefix+"/"+key)
	}
	slices.Sort(want)

	var got []string
	pager := s3.NewListObjectsV2Paginator(raw, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			t.Fatalf("raw list: %v", err)
		}
		for _, o := range page.Contents {
			got = append(got, aws.ToString(o.Key))
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("physical bucket keys =\n%v\nwant\n%v", got, want)
	}

	dest := t.TempDir()
	if err := b.Hydrate(ctx, "", dest); err != nil {
		t.Fatalf("Hydrate = %v", err)
	}
	for key, content := range awkwardKeys {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(key)))
		if err != nil {
			t.Errorf("hydrated %q: %v", key, err)
			continue
		}
		if string(got) != content {
			t.Errorf("hydrated %q = %q, want %q", key, got, content)
		}
	}
}

// TestMissingBucketIsAnError covers the remote-404 mapping.
//
// This is the deterministic half of "error mapping for missing keys". The
// other half -- a NoSuchKey from GetObject inside Hydrate's download -- is
// unreachable on purpose rather than untested: Hydrate lists and then fetches
// exactly what the list returned, and MinIO, like S3, is strongly consistent,
// so the only way to make that GetObject miss is to delete the object between
// the list and the fetch from another goroutine. A flaky test is worse than an
// honest gap, so what is pinned here instead is the same wrapping code reached
// through a 404 that IS deterministic: every method against a container that
// does not exist.
//
// The properties that matter to the engine: it is an error (a silent success
// would let a misconfigured bucket name look like a working push), it names the
// bucket (so the operator can see which one), and it is NOT an ErrInvalidKey,
// which the engine treats as a permanent local bug never worth retrying, unlike
// a remote failure.
func TestMissingBucketIsAnError(t *testing.T) {
	target := requireMinIO(t)
	// Never created, so MinIO answers NoSuchBucket rather than AccessDenied.
	const bucket = "nexus-e4s3-never-created"
	b := newMinIOBackend(t, target, bucket)
	ctx := context.Background()

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	check := func(op string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s against a missing bucket = nil, want an error", op)
			return
		}
		if !strings.Contains(err.Error(), bucket) {
			t.Errorf("%s error = %v, want it to name the bucket %q", op, err, bucket)
		}
		if errors.Is(err, objectstore.ErrInvalidKey) {
			t.Errorf("%s error = %v, want a remote failure, not ErrInvalidKey; "+
				"the engine gives up permanently on ErrInvalidKey", op, err)
		}
	}
	check("Put", b.Put(ctx, "files/a.txt", local))
	check("Delete", b.Delete(ctx, "files/a.txt"))
	_, listErr := b.List(ctx, "")
	check("List", listErr)
	check("Hydrate", b.Hydrate(ctx, "", t.TempDir()))
}

// TestDeniedAccessIsAnError covers the 403 half, and is one of the two tests in
// this file the fake could not host at all: it accepts any Authorization header
// by design, because verifying one would mean reimplementing SigV4 in the test.
// MinIO verifies, so a rejected credential is a real 403 off a real wire.
//
// This is an identity denial, not a policy denial. MinIO's root credential
// cannot be denied by a bucket policy, and reproducing an IAM-shaped
// AccessDenied would need the MinIO admin API and a second user -- which would
// still not be IAM. The legs that produce a policy denial in production (IRSA
// role scoping, an S3 bucket policy) are the ones doc.go and E4-S2 both record
// as reproducible by no emulator, and this test does not claim them. What it
// does claim is the part Nexus owns: a credential the store rejects surfaces as
// an error naming the object, and nothing is written.
func TestDeniedAccessIsAnError(t *testing.T) {
	target := requireMinIO(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)
	ctx := context.Background()

	wrong := target
	wrong.secretKey = "not-the-right-secret"
	b := newMinIOBackend(t, wrong, bucket)

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := b.Put(ctx, "files/a.txt", local)
	if err == nil {
		t.Fatal("Put signed with the wrong secret = nil, want a rejection; " +
			"the store is not verifying signatures, so this suite proves less than it claims")
	}
	if !strings.Contains(err.Error(), "files/a.txt") {
		t.Errorf("Put error = %v, want it to name the key", err)
	}
	if errors.Is(err, objectstore.ErrInvalidKey) {
		t.Errorf("Put error = %v, want a remote failure, not ErrInvalidKey", err)
	}
	if _, err := b.List(ctx, ""); err == nil {
		t.Error("List signed with the wrong secret = nil, want a rejection")
	}

	// Nothing may have been stored. Asked through a client whose credentials
	// the store does accept, so a false negative here cannot come from the
	// reader being rejected too.
	page, err := raw.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("raw list: %v", err)
	}
	if n := len(page.Contents); n != 0 {
		t.Errorf("bucket holds %d objects after a rejected Put, want none", n)
	}
}
