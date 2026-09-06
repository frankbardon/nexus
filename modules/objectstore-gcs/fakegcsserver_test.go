//go:build fakegcsserver

// The against-fake-gcs-server suite: the same backend and the same exported
// conformance suite as gcs_test.go, but pointed at a Cloud Storage
// implementation running in another process instead of the loopback fake in
// fakegcs_test.go.
//
// The build tag is the emulator's full name rather than "fakegcs", because
// "the fake" already means something in this package: fakegcs_test.go's
// in-process httptest server, which is untagged and runs on every `make test`.
// The two are complements. That one is always on and catches regressions in
// this backend; this one catches divergence between that fake and an
// implementation nobody here wrote.
//
// # Why this exists when the in-process fake already runs the same suite
//
// The fake agrees with the JSON API exactly as far as its author understood the
// JSON API, and it is the same author. Every wire-level thing it asserts -- that
// the multipart upload body was framed a particular way, that the media
// download lives at the endpoint host's root, that a delete of a missing object
// 404s -- is asserted against an implementation written from the same reading of
// the same documentation. A misreading is invisible to it by construction.
//
// fake-gcs-server is not: it is an independent implementation, used by other
// projects to stand in for GCS, and it was written from the API rather than
// from this backend's needs. What that buys, concretely:
//
//   - URL escaping through somebody else's router. The fake parses paths with
//     strings.Cut; fake-gcs-server uses gorilla/mux with SkipClean and
//     UseEncodedPath. A key with a space, a CJK segment or a "..." segment has
//     to survive both, and TestKeyLayoutMirrorsTheLocalTreeAgainstFakeGCSServer
//     is the same test as its fake counterpart and a materially stronger one.
//   - The resumable upload protocol. chunkSizeFor sends anything below 16 MiB
//     in a single multipart request, which is the only path the in-process fake
//     implements; at or above it the SDK switches to a resumable upload against
//     /upload/resumable/{uploadId}, a code path with no coverage at all until
//     here. TestLargeObjectRoundTrip drives both sides of that threshold.
//   - Durability outside this process. The fake's "fresh client" reads back
//     through the same Go map the first one wrote to, so a backend that never
//     sent anything would pass. Here the bytes have to have reached another
//     process.
//   - Volume. The conformance suite's pagination case runs at its full default
//     1200 objects here, rather than the 200 the untagged run pays.
//
// # What this suite deliberately does not claim
//
// Not IAM, and not the credential chain. Application Default Credentials, GKE
// Workload Identity, service-account impersonation and Workload Identity
// Federation -- the reason doc.go takes the Google SDK at all -- are reproduced
// by no emulator, fake-gcs-server included, and nothing here pretends
// otherwise. There is deliberately no counterpart to the S3 module's
// TestDeniedAccessIsAnError: fake-gcs-server does not authenticate at all, so a
// "rejected credential" test against it would assert nothing. Every backend in
// this file is built on the unauthenticated-emulator path resolveCredentials
// selects when an endpoint is set and nothing else is available; which
// credential source *would* be selected is covered against the in-process fake
// in gcs_test.go, where a loopback token endpoint makes the identity
// observable.
//
// Not a real page boundary either, and that one is measured rather than
// assumed: see TestFakeGCSServerDoesNotPaginateUnlessAsked.
package gcsstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	gcsstore "github.com/frankbardon/nexus/modules/objectstore-gcs"
)

// The knobs, env-supplied so the same test binary serves three callers:
// `make test-objectstore-fake-gcs` (which starts an emulator and sets both),
// CI (the same target), and a developer with their own fake-gcs-server already
// running who exports NEXUS_TEST_FAKE_GCS_ENDPOINT and nothing else.
const (
	envFakeGCSEndpoint = "NEXUS_TEST_FAKE_GCS_ENDPOINT"

	// envFakeGCSRequired turns the absent-emulator skip into a failure.
	//
	// This is the whole answer to "a suite that silently skips in CI is green
	// forever". The skip has to exist -- a contributor must be able to run
	// `go test -tags fakegcsserver ./...` with nothing running and get an
	// honest "not run" rather than a red they cannot act on -- but a skip is
	// indistinguishable from a pass in a CI summary, so the caller that
	// *provisioned* the emulator sets this and gets a hard failure if the suite
	// would have skipped anyway. scripts/with-fake-gcs.sh sets it
	// unconditionally, because by the time it runs the command it has already
	// waited for the health endpoint; a skip after that is a bug in this file,
	// not a missing dependency.
	envFakeGCSRequired = "NEXUS_TEST_FAKE_GCS_REQUIRED"
)

// defaultFakeGCSEndpoint is what a hand-started fake-gcs-server listens on.
// 4443 is its default port whichever scheme it was started with;
// scripts/with-fake-gcs.sh picks a free port instead and exports it, precisely
// so it cannot collide with one a developer already has.
const defaultFakeGCSEndpoint = "http://127.0.0.1:4443"

// emulatorProject is the project ID handed to bucket creation.
//
// GCS needs a project to create or list buckets, and nothing else; the backend
// itself never names one (see doc.go), which is why this constant lives in the
// test rather than in the config. fake-gcs-server ignores the value.
const emulatorProject = "nexus-emulator"

// fakeGCSTarget is a reachable fake-gcs-server.
type fakeGCSTarget struct {
	endpoint string
}

// jsonAPI returns the endpoint with the JSON API path the SDK expects.
// normalizeEndpoint does this for the backend under test; the raw client below
// is built by hand and has to do it itself.
func (t fakeGCSTarget) jsonAPI() string {
	return strings.TrimSuffix(t.endpoint, "/") + "/storage/v1/"
}

// requireFakeGCSServer resolves the target and proves something is listening,
// skipping the test when nothing is -- or failing it when the caller promised
// there would be.
//
// The probe is a bare HTTP request to the endpoint root rather than to
// fake-gcs-server's /_internal/healthcheck. Any Cloud Storage endpoint answers
// the root with *something*, so "the TCP connect and the HTTP exchange
// completed" is the reachability question, and asking it this way keeps the
// file usable against the Google testbench without pretending an emulator's
// private health path is standard. Status codes are ignored on purpose: a 404
// here means a server answered, which is all this needs to know.
func requireFakeGCSServer(t *testing.T) fakeGCSTarget {
	t.Helper()

	target := fakeGCSTarget{endpoint: envOr(envFakeGCSEndpoint, defaultFakeGCSEndpoint)}

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target.endpoint, nil)
	if err != nil {
		t.Fatalf("%s = %q is not a usable URL: %v", envFakeGCSEndpoint, target.endpoint, err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return target
	}

	msg := fmt.Sprintf("no fake-gcs-server at %s (%v); start one with "+
		"`make test-objectstore-fake-gcs`, or point %s at your own",
		target.endpoint, err, envFakeGCSEndpoint)
	if os.Getenv(envFakeGCSRequired) != "" {
		// The caller said it provisioned the emulator. A skip here would report
		// green while testing nothing, which is exactly the failure this suite
		// exists to make impossible.
		t.Fatalf("%s is set, so this suite must not skip: %s", envFakeGCSRequired, msg)
	}
	t.Skip(msg)
	return fakeGCSTarget{}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// TestFakeGCSServerIsReachable is the guard on the guard.
//
// It asserts nothing about the backend. It exists so that "was the emulator
// actually there?" is a named line in the test output rather than something a
// reader has to infer from the absence of skips, and so that the required/skip
// decision is exercised once by itself instead of only as a side effect of a
// bigger test.
func TestFakeGCSServerIsReachable(t *testing.T) {
	target := requireFakeGCSServer(t)
	t.Logf("fake-gcs-server at %s, required=%t", target.endpoint, os.Getenv(envFakeGCSRequired) != "")
}

// newRawClient builds a storage client that is NOT the backend under test.
//
// Used for the things the backend has no API for -- creating and emptying a
// bucket, reading the physical object names, asking what one page of a list
// looks like. It is built with option.WithoutAuthentication and an explicit
// endpoint rather than through gcsstore.New, so it is unaffected by whatever
// isolateGoogleEnv has done to the process for the backend's benefit: if the
// backend's credential resolution breaks, the assertions made through this
// client stay trustworthy.
func newRawClient(t *testing.T, target fakeGCSTarget) *storage.Client {
	t.Helper()
	c, err := storage.NewClient(context.Background(),
		option.WithoutAuthentication(),
		option.WithEndpoint(target.jsonAPI()))
	if err != nil {
		t.Fatalf("building the raw client against %s: %v", target.endpoint, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// newBucket creates a fresh bucket and registers its teardown.
//
// A bucket per case, not a prefix per case. The conformance suite calls its
// factory once per case and documents that per-case isolation is the
// implementer's to arrange; a unique *prefix* would arrange it, but it would
// also mean every case in the "no configured prefix" run was secretly running
// with one, and the whole point of running the suite twice is that the prefixed
// and unprefixed key paths are different code. A bucket is the only isolation
// that leaves Config.Prefix free to be empty.
func newBucket(t *testing.T, raw *storage.Client) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generating a bucket name: %v", err)
	}
	// GCS bucket names are 3-63 characters of lowercase alphanumerics, hyphens,
	// underscores and dots, starting and ending alphanumeric.
	bucket := "nexus-e5s2-" + hex.EncodeToString(b[:])

	ctx := context.Background()
	if err := raw.Bucket(bucket).Create(ctx, emulatorProject, nil); err != nil {
		t.Fatalf("creating bucket %q: %v", bucket, err)
	}
	t.Cleanup(func() { emptyAndRemoveBucket(t, raw, bucket) })
	return bucket
}

// emptyAndRemoveBucket deletes every object and then the bucket.
//
// Failures are reported, not swallowed. A developer running this against their
// own long-lived emulator would otherwise accumulate a bucket per case per run
// forever, and a leak here is also a signal in its own right -- the most likely
// reason a delete fails is that the store is not behaving the way the rest of
// this file assumes it does.
func emptyAndRemoveBucket(t *testing.T, raw *storage.Client, bucket string) {
	t.Helper()
	ctx := context.Background()

	// One at a time. The JSON API has a batch endpoint and fake-gcs-server
	// implements it, but the SDK does not expose batching, and hand-rolling a
	// multipart batch request in a teardown would be more code than the 1200
	// deletes it saves are worth on loopback.
	it := raw.Bucket(bucket).Objects(ctx, nil)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Errorf("emptying bucket %q: listing: %v", bucket, err)
			return
		}
		if err := raw.Bucket(bucket).Object(attrs.Name).Delete(ctx); err != nil &&
			!errors.Is(err, storage.ErrObjectNotExist) {
			t.Errorf("emptying bucket %q: deleting %q: %v", bucket, attrs.Name, err)
			return
		}
	}

	if err := raw.Bucket(bucket).Delete(ctx); err != nil {
		t.Errorf("removing bucket %q: %v", bucket, err)
	}
}

// newEmulatorBackend builds the backend under test against a bucket.
//
// isolateGoogleEnv (shared with the in-process fake's suite) clears every
// Google credential variable and points HOME at an empty directory, so a
// developer's `gcloud auth application-default login` cannot decide what this
// run authenticates as -- and, more importantly, cannot send a real credential
// to an emulator. STORAGE_EMULATOR_HOST is on that list too: the SDK honours it
// directly and it would silently redirect every request in this file.
func newEmulatorBackend(t *testing.T, target fakeGCSTarget, bucket string, mutate ...func(*objectstore.Config)) objectstore.Backend {
	t.Helper()
	isolateGoogleEnv(t)

	cfg := objectstore.Config{
		BackendName: gcsstore.BackendName,
		Bucket:      bucket,
		// The bare scheme://host form an operator writes, with no API path:
		// normalizeEndpoint appends the rest, and pointing the tests at the
		// bare form is what keeps that behaviour covered against a real server.
		Endpoint: target.endpoint,
		Logger:   quietLogger(),
	}
	for _, m := range mutate {
		m(&cfg)
	}

	b, err := gcsstore.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("gcsstore.New against fake-gcs-server: %v", err)
	}
	if c, ok := b.(io.Closer); ok {
		t.Cleanup(func() { _ = c.Close() })
	}
	return b
}

// ---------------------------------------------------------------------------
// The conformance suite, against a real server
// ---------------------------------------------------------------------------

// TestFakeGCSServerContractSuite is the acceptance criterion: the shared suite,
// at its full default probe count, against an implementation this repository
// did not write.
//
// No WithListProbeCount here, unlike the in-process run in gcs_test.go, which
// caps it at 200 against a 50-object page. The default is 1200 because the JSON
// API's default page is 1000, and paying that once in a tagged suite is the
// trade E4-S2 recorded. Read honestly, what it buys against *this* emulator is
// volume rather than a page boundary --
// TestFakeGCSServerDoesNotPaginateUnlessAsked measures why, and
// TestListCrossesAServerForcedPageBoundary covers the boundary itself.
//
// No Option at all, in particular no WithoutObjectAtPrefix, which the MinIO run
// needs. GCS has a genuinely flat key space and so does this emulator;
// TestFakeGCSServerHasAFlatKeySpace is the receipt for not passing it.
func TestFakeGCSServerContractSuite(t *testing.T) {
	target := requireFakeGCSServer(t)
	raw := newRawClient(t, target)
	objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
		return newEmulatorBackend(t, target, newBucket(t, raw))
	})
}

// TestFakeGCSServerContractSuiteUnderAConfiguredPrefix runs it again with a
// bucket prefix, for the reason gcs_test.go gives: the suite only ever speaks
// store-relative keys, so a prefix applied on Put but not on List, or stripped
// with a raw string trim, passes it cleanly. Against a real server this also
// covers the prefix's effect on the *server-side* Query.Prefix filter, which is
// where a trailing slash that is present locally but missing on the wire would
// show up.
func TestFakeGCSServerContractSuiteUnderAConfiguredPrefix(t *testing.T) {
	target := requireFakeGCSServer(t)
	raw := newRawClient(t, target)
	objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
		return newEmulatorBackend(t, target, newBucket(t, raw), func(c *objectstore.Config) {
			c.Prefix = "prod/nexus"
		})
	})
}

// TestFakeGCSServerHasAFlatKeySpace is the receipt for the option the two
// RunSuite calls above do NOT pass.
//
// The S3 module has to pass objectstoretest.WithoutObjectAtPrefix, because
// MinIO cannot hold an object at "sessions/sess-1" and another at
// "sessions/sess-1/files/a.txt" at the same time. That accommodation is easy to
// copy from one emulator suite to the other without checking, and copying it
// would silently drop a case that this backend genuinely passes. So the
// difference is measured, with the raw client, going nowhere near the backend
// under test.
//
// If this ever fails, fake-gcs-server has grown a hierarchical key space and
// the two suites above need WithoutObjectAtPrefix -- but a matching failure
// should be expected from the untagged in-process run first, and a divergence
// between the two is the more interesting finding.
func TestFakeGCSServerHasAFlatKeySpace(t *testing.T) {
	target := requireFakeGCSServer(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)
	ctx := context.Background()

	const child = "sessions/sess-1/files/a.txt"
	const atPrefix = "sessions/sess-1"

	put := func(key, body string) {
		t.Helper()
		w := raw.Bucket(bucket).Object(key).NewWriter(ctx)
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatalf("raw write %q: %v", key, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("raw close %q: %v", key, err)
		}
	}
	listAll := func() []string {
		t.Helper()
		var keys []string
		it := raw.Bucket(bucket).Objects(ctx, nil)
		for {
			attrs, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("raw list: %v", err)
			}
			keys = append(keys, attrs.Name)
		}
		slices.Sort(keys)
		return keys
	}

	put(child, "a")
	put(atPrefix, "an object at the prefix itself")

	got := listAll()
	if !slices.Equal(got, []string{atPrefix, child}) {
		t.Fatalf("bucket holds %v, want both %q and %q; this store does not have the flat "+
			"key space the two RunSuite calls above assume, and they now need "+
			"objectstoretest.WithoutObjectAtPrefix", got, atPrefix, child)
	}

	// And what that means through the backend, which is the half MinIO gets
	// wrong. objectstore.Backend excludes an object AT the prefix from
	// List(prefix) -- it has no path beneath a hydration destination, so there
	// is nowhere for it to go -- but the object UNDER the prefix must still be
	// there, and against MinIO it is not: writing the object at the prefix
	// makes the child unlistable.
	b := newEmulatorBackend(t, target, bucket)
	if keys := listKeys(t, b, "sessions/sess-1"); !slices.Equal(keys, []string{child}) {
		t.Errorf("List(sessions/sess-1) = %v, want exactly [%s]; an object at the prefix "+
			"must neither appear in nor hide what is under it", keys, child)
	}
	if keys := listKeys(t, b, "sessions"); !slices.Equal(keys, []string{atPrefix, child}) {
		t.Errorf("List(sessions) = %v, want %v", keys, []string{atPrefix, child})
	}
}

// listKeys is List, reduced to sorted keys.
func listKeys(t *testing.T, b objectstore.Backend, prefix string) []string {
	t.Helper()
	objs, err := b.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("List(%q) = %v", prefix, err)
	}
	keys := make([]string, 0, len(objs))
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	slices.Sort(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Pagination: what this emulator can and cannot prove
// ---------------------------------------------------------------------------

// TestFakeGCSServerDoesNotPaginateUnlessAsked is the honest footnote on the
// 1200-object conformance run above.
//
// Real GCS caps a list response at 1000 items and hands back a nextPageToken.
// fake-gcs-server has no cap at all: it pages only when the request carries
// maxResults, and the SDK's iterator does not send one unless asked. So the
// 1200-object case here exercises volume, JSON encoding and a real socket --
// but it comes back in ONE response, and reading it as proof that the iterator
// is drained would be wrong.
//
// Measured rather than asserted from the emulator's source, because that is the
// thing that could change under us: if fake-gcs-server ever grows a default
// page size, this test fails, and at that point the conformance run becomes a
// genuine boundary test and TestListCrossesAServerForcedPageBoundary below can
// be reconsidered.
func TestFakeGCSServerDoesNotPaginateUnlessAsked(t *testing.T) {
	target := requireFakeGCSServer(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)
	b := newEmulatorBackend(t, target, bucket)
	ctx := context.Background()

	// Past the 1000 a real GCS response would stop at.
	const count = 1010
	src := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for i := range count {
		key := fmt.Sprintf("page/%06d.bin", i)
		if err := b.Put(ctx, key, src); err != nil {
			t.Fatalf("Put(%q) = %v", key, err)
		}
	}

	// A raw JSON API list with no maxResults, read as JSON rather than through
	// the SDK, because the SDK's iterator hides exactly the field under test.
	listRaw := func(query string) (items int, nextPageToken string) {
		t.Helper()
		u := strings.TrimSuffix(target.endpoint, "/") + "/storage/v1/b/" + bucket + "/o"
		if query != "" {
			u += "?" + query
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			t.Fatalf("building the raw list request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("raw list: %v", err)
		}
		defer resp.Body.Close()
		var page struct {
			Items         []json.RawMessage `json:"items"`
			NextPageToken string            `json:"nextPageToken"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatalf("decoding the raw list response: %v", err)
		}
		return len(page.Items), page.NextPageToken
	}

	items, token := listRaw("")
	if token != "" {
		t.Fatalf("one unpaginated list over %d objects came back with nextPageToken %q; "+
			"this emulator now has a default page size, so the 1200-object conformance run "+
			"is a real boundary test and this test should say so", count, token)
	}
	if items != count {
		t.Fatalf("one unpaginated list returned %d of %d objects with no page token; "+
			"objects have gone missing, which is a different problem entirely", items, count)
	}

	// The other half: its pagination machinery is real, it simply has to be
	// asked. This is the premise TestListCrossesAServerForcedPageBoundary rests
	// on, and asserting it here keeps that test's proxy from being the only
	// thing standing between a broken emulator and a green run.
	items, token = listRaw("maxResults=400")
	if items != 400 || token == "" {
		t.Fatalf("maxResults=400 returned %d items and token %q, want 400 and a non-empty token; "+
			"this emulator cannot page at all and the forced-boundary test below proves nothing",
			items, token)
	}
}

// pageSizeProxy sits in front of the emulator and adds maxResults to every
// object-list request that does not already carry one.
//
// It exists because of the measurement above: without it, no list this backend
// issues against fake-gcs-server is ever paginated, and the paginator -- the
// single most consequential thing eachObject does, and the thing the conformance
// suite's 1200 objects were sized to catch -- would go untested against any
// implementation but the in-process fake.
//
// It rewrites a query parameter and nothing else. Every part of paging that
// could be wrong stays the server's: which objects land on which page, what the
// nextPageToken is, and whether following it enumerates each object exactly
// once. And it is a parameter the API defines and any client may send, so
// nothing here is a shape a real GCS conversation could not have.
//
// The rejected alternative was to give the backend a page size through
// objectstore.Config. That would be a production config key existing to make a
// test possible, on an interface that is deliberately narrow, and it would still
// not test the SDK iterator's own default.
func pageSizeProxy(t *testing.T, target fakeGCSTarget, pageSize int) (endpoint string, listRequests func() int) {
	t.Helper()
	upstream, err := url.Parse(target.endpoint)
	if err != nil {
		t.Fatalf("parsing %s: %v", target.endpoint, err)
	}

	var lists atomic.Int64
	return proxyTo(t, upstream, func(pr *httputil.ProxyRequest) {
		path := pr.Out.URL.Path
		if !strings.HasPrefix(path, "/storage/v1/b/") || !strings.HasSuffix(path, "/o") {
			return
		}
		lists.Add(1)
		q := pr.Out.URL.Query()
		if q.Get("maxResults") == "" {
			q.Set("maxResults", strconv.Itoa(pageSize))
			pr.Out.URL.RawQuery = q.Encode()
		}
	}), func() int { return int(lists.Load()) }
}

// proxyTo returns the URL of a loopback reverse proxy in front of upstream,
// with rewrite applied to every request on the way through.
//
// The Host header is forced to the upstream's own, so the emulator sees exactly
// the header a direct client would have sent. fake-gcs-server routes the
// media-download path (GET /<bucket>/<object>) by matching Host against its
// configured public host, and letting the proxy's Host through would take that
// route away -- turning every test that proxies into a download test as well,
// for no reason.
func proxyTo(t *testing.T, upstream *url.URL, rewrite func(*httputil.ProxyRequest)) string {
	t.Helper()
	proxy := httptest.NewServer(&httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			rewrite(pr)
		},
	})
	t.Cleanup(proxy.Close)
	return proxy.URL
}

// uploadTypeProxy records the uploadType of every object insert that passes
// through it, without changing anything.
//
// TestLargeObjectRoundTrip needs it because the two sizes it uploads are
// indistinguishable from the outside: both round-trip, both report the right
// length, and a chunkSizeFor that had regressed to "always single-request"
// would pass the whole test while leaving the resumable path -- the one with no
// other coverage anywhere in this repository -- dead. What arrives at the
// server is the only place the difference is visible.
//
// Only the negotiation is seen here. A resumable upload's chunk PUTs go to the
// absolute Location URL the server returns, which points at the server rather
// than at this proxy, so they bypass it -- which is fine: the uploadType on the
// opening request is the thing under assertion, and the bytes are checked by
// sha256 at the other end regardless of the route they took.
func uploadTypeProxy(t *testing.T, target fakeGCSTarget) (endpoint string, uploadTypes func() []string) {
	t.Helper()
	upstream, err := url.Parse(target.endpoint)
	if err != nil {
		t.Fatalf("parsing %s: %v", target.endpoint, err)
	}

	var mu sync.Mutex
	var seen []string
	proxied := proxyTo(t, upstream, func(pr *httputil.ProxyRequest) {
		if !strings.HasPrefix(pr.Out.URL.Path, "/upload/storage/v1/b/") {
			return
		}
		if kind := pr.Out.URL.Query().Get("uploadType"); kind != "" {
			mu.Lock()
			seen = append(seen, kind)
			mu.Unlock()
		}
	})
	return proxied, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// TestListCrossesAServerForcedPageBoundary is the paginator test the emulator
// cannot host on its own.
//
// It crosses a page boundary with the segment-aware prefix rule engaged, which
// is the combination that actually bites: "sessions/sess-1" and
// "sessions/sess-10" collide under the plain-string Query.Prefix the server
// matches on, the iterator has to be drained to see all of the first one, and a
// backend that post-filtered only the first page would return a plausible-
// looking round number. The counts are chosen so a first-page-only bug returns
// 400 rather than the correct 1005.
//
// Hydrate goes over the same boundary too, where getting it wrong writes another
// session's files into this one's tree.
func TestListCrossesAServerForcedPageBoundary(t *testing.T) {
	target := requireFakeGCSServer(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)

	const pageSize = 400
	proxied, listRequests := pageSizeProxy(t, target, pageSize)
	b := newEmulatorBackend(t, fakeGCSTarget{endpoint: proxied}, bucket)
	ctx := context.Background()

	// Just past two page boundaries, and a sibling whose key is a plain-string
	// prefix match for the one under test.
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

	before := listRequests()
	got, err := b.List(ctx, "sessions/sess-1")
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	if crossed := listRequests() - before; crossed < 2 {
		t.Fatalf("List over %d objects issued %d list requests at a page size of %d; "+
			"the boundary was not crossed, so this test proves nothing", mine+neighbours, crossed, pageSize)
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
			"(an iterator that stops after one page returns %d)", len(gotKeys), mine, pageSize)
	}
	if !slices.Equal(gotKeys, want) {
		t.Error("List(sessions/sess-1) returned the wrong set of keys across the page boundary")
	}

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

// ---------------------------------------------------------------------------
// The things only a real store can prove
// ---------------------------------------------------------------------------

// TestFlushDurabilityThroughAFreshClientAgainstFakeGCSServer is the assertion
// E1-S3 explicitly flagged as unprovable in process.
//
// TestFlushIsProvableThroughAFreshClient in gcs_test.go performs the same
// choreography against the in-process fake and proves less than it looks like
// it does: the "fresh" client reads back through the very Go map the first one
// wrote into, so a backend that buffered everything in the fake's memory -- or,
// for that matter, a fake that lied -- would satisfy it. Nothing about that run
// distinguishes "durable" from "still in this process".
//
// Here the two clients are separated by a process boundary. The first backend
// writes, Flush returns, and then a *second* backend -- its own storage.Client,
// its own connection pool -- reads the bytes back out. Nothing in the first
// backend's memory can satisfy the second one. That is what makes the
// synchronous-Put decision in doc.go checkable rather than merely asserted: if
// Put ever started queueing, this test is where it would be caught.
//
// The read-back goes through both of Backend's read paths. List proves the
// object is enumerable by a client that never saw the write, and Hydrate proves
// the bytes themselves survived, which List's metadata alone would not.
func TestFlushDurabilityThroughAFreshClientAgainstFakeGCSServer(t *testing.T) {
	target := requireFakeGCSServer(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)
	ctx := context.Background()

	// Two payloads, so the assertion is about content and not just presence,
	// and one of them is a delete so Flush's promise covers removals too -- a
	// queued delete is as durable a lie as a queued write.
	const key = "metadata/session.json"
	const want = `{"id":"sess-e5s2","turns":7}`

	first := newEmulatorBackend(t, target, bucket)
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
	second := newEmulatorBackend(t, target, bucket)

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

// TestLargeObjectRoundTrip drives both sides of chunkSizeFor's threshold
// through a real server.
//
// This is the one place the resumable upload protocol is exercised at all.
// Below defaultChunkSize the SDK sends a single multipart request, which is
// what the in-process fake implements and all it implements; at or above it,
// ChunkSize is non-zero and the SDK negotiates a resumable session and PUTs the
// body in chunks. Those are different code paths in the SDK, framed differently
// on the wire, and until here nothing in this repository had ever run the second
// one.
//
// The payload is deterministic pseudorandom rather than repeated bytes, so a
// truncation or an off-by-one in the middle cannot be hidden by the surrounding
// content being identical, and so nothing on the path can usefully compress it.
// It is compared by sha256 rather than by bytes.Equal so a failure prints two
// hashes instead of twenty megabytes.
func TestLargeObjectRoundTrip(t *testing.T) {
	target := requireFakeGCSServer(t)
	raw := newRawClient(t, target)

	// The threshold itself is defaultChunkSize, 16 MiB, and it is unexported --
	// deliberately: naming it here would pin this test to a constant rather than
	// to the behaviour on either side of it. These two sizes sit far enough
	// apart that a change to the threshold would have to be dramatic to make
	// both land on the same path, and TestFakeGCSServerIsReachable is not the
	// place to notice that.
	cases := []struct {
		name string
		size int
		// wantUploadType is the JSON API's own name for the path the SDK took,
		// read off the wire. Without it both cases pass against a chunkSizeFor
		// that has regressed to "always single-request", and the resumable path
		// would be dead code with a green test over it.
		wantUploadType string
	}{
		{"SingleRequestUpload", 8 << 20, "multipart"},
		{"ResumableUpload", 20 << 20, "resumable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bucket := newBucket(t, raw)
			proxied, uploadTypes := uploadTypeProxy(t, target)
			b := newEmulatorBackend(t, fakeGCSTarget{endpoint: proxied}, bucket)
			ctx := context.Background()

			payload := make([]byte, c.size)
			// Fixed seed: reproducible failures.
			rng := mrand.New(mrand.NewPCG(0x6e657875, 0x73453552))
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
				t.Fatalf("Put of a %d-byte object = %v", c.size, err)
			}
			if got := uploadTypes(); len(got) != 1 || got[0] != c.wantUploadType {
				t.Fatalf("a %d-byte Put negotiated uploadType %v, want exactly [%s]; "+
					"chunkSizeFor put this object on the other side of its threshold, so "+
					"this case is not covering the path it names", c.size, got, c.wantUploadType)
			}

			// What the store thinks it holds, asked of a client that did not
			// write it. A size mismatch here is the framing failure mode: chunk
			// headers land in the object and the length exceeds the file.
			attrs, err := raw.Bucket(bucket).Object(key).Attrs(ctx)
			if err != nil {
				t.Fatalf("Attrs = %v", err)
			}
			if attrs.Size != int64(c.size) {
				t.Fatalf("stored object is %d bytes, want %d; the body was reframed on the wire",
					attrs.Size, c.size)
			}

			// And what List reports, which is the number the engine's push path
			// diffs against the local tree.
			objs, err := b.List(ctx, "files")
			if err != nil {
				t.Fatalf("List = %v", err)
			}
			if len(objs) != 1 || objs[0].Size != int64(c.size) {
				t.Errorf("List = %+v, want one object of %d bytes", objs, c.size)
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
			if n != int64(c.size) {
				t.Fatalf("hydrated object is %d bytes, want %d", n, c.size)
			}
			if gotSum := h.Sum(nil); !bytes.Equal(gotSum, wantSum[:]) {
				t.Errorf("hydrated sha256 = %x, want %x", gotSum, wantSum)
			}
		})
	}
}

// TestKeyLayoutMirrorsTheLocalTreeAgainstFakeGCSServer is the same assertion as
// its counterpart in gcs_test.go and a materially stronger one.
//
// Against the in-process fake it proves the backend built the keys it meant to,
// through a handler that parses paths with strings.Cut. Against fake-gcs-server
// the same keys have to survive gorilla/mux with SkipClean and UseEncodedPath,
// and three different escapings on the way: a query parameter on upload, a path
// segment on delete, a URL path on download. The space in "with spaces", the
// CJK segment and the "..." segment are exactly where a router disagrees with a
// hand-written switch.
//
// The physical keys are read back with the raw client, not with List, because
// List round-trips through the same joinKey/storeKey pair that produced them --
// a mapping that was wrong in both directions would agree with itself.
func TestKeyLayoutMirrorsTheLocalTreeAgainstFakeGCSServer(t *testing.T) {
	target := requireFakeGCSServer(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)

	const prefix = "prod/nexus"
	b := newEmulatorBackend(t, target, bucket, func(c *objectstore.Config) { c.Prefix = prefix })
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
			t.Fatalf("Put(%q) against fake-gcs-server = %v", key, err)
		}
		want = append(want, prefix+"/"+key)
	}
	slices.Sort(want)

	var got []string
	it := raw.Bucket(bucket).Objects(ctx, nil)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("raw list: %v", err)
		}
		got = append(got, attrs.Name)
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
// This is the deterministic half of "error mapping for missing keys". The other
// half -- an ErrObjectNotExist from NewReader inside Hydrate's download -- is
// unreachable on purpose rather than untested: Hydrate lists and then fetches
// exactly what the list returned, and GCS is strongly consistent, so the only
// way to make that read miss is to delete the object between the list and the
// fetch from another goroutine. A flaky test is worse than an honest gap, so
// what is pinned here instead is the same wrapping code reached through a 404
// that IS deterministic.
//
// The properties that matter to the engine: it is an error (a silent success
// would let a misconfigured bucket name look like a working push), it names the
// bucket (so the operator can see which one), and it is NOT an ErrInvalidKey,
// which the engine treats as a permanent local bug never worth retrying, unlike
// a remote failure.
//
// Delete is deliberately absent from the list below, and
// TestDeleteAgainstAMissingBucketCannotBeDistinguished says why.
func TestMissingBucketIsAnError(t *testing.T) {
	target := requireFakeGCSServer(t)
	// Never created, so the server answers 404 rather than a permission error.
	const bucket = "nexus-e5s2-never-created"
	b := newEmulatorBackend(t, target, bucket)
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
	_, listErr := b.List(ctx, "")
	check("List", listErr)
	check("Hydrate", b.Hydrate(ctx, "", t.TempDir()))
}

// TestDeleteAgainstAMissingBucketCannotBeDistinguished pins a consequence of
// the one place GCS disagrees with S3 outright, and it is a real difference in
// behaviour between the two backends rather than a quirk of this emulator.
//
// objectstore.Backend specifies S3's semantics: deleting a key that was never
// there is a success, which is what lets the engine retry a delete without
// special-casing the second attempt. GCS instead 404s, and Delete absorbs that
// by swallowing storage.ErrObjectNotExist. A bucket that does not exist also
// 404s, and the SDK maps both to the same sentinel -- so a Delete against a
// misconfigured bucket name returns nil here where the S3 backend returns an
// error.
//
// Recorded rather than fixed. Distinguishing the two would mean a bucket-
// attributes probe on every delete, which is a round trip per object added to
// the hot path to improve an error message; and the misconfiguration it would
// catch is caught loudly by Put, List and Hydrate in the test above, all of
// which run long before any delete does.
func TestDeleteAgainstAMissingBucketCannotBeDistinguished(t *testing.T) {
	target := requireFakeGCSServer(t)
	const bucket = "nexus-e5s2-never-created"
	b := newEmulatorBackend(t, target, bucket)

	if err := b.Delete(context.Background(), "files/a.txt"); err != nil {
		t.Errorf("Delete against a missing bucket = %v, want nil; if this store has started "+
			"distinguishing a missing bucket from a missing object, Delete belongs back in "+
			"TestMissingBucketIsAnError", err)
	}
}

// TestDeleteOfAMissingObjectIsNotAnErrorAgainstARealServer is the S3/GCS
// divergence itself, against a 404 this repository did not generate.
//
// gcs_test.go asserts the same thing against the in-process fake, which returns
// that 404 because it was written to. This one is the check that the SDK maps a
// real server's real 404 onto storage.ErrObjectNotExist -- the sentinel Delete
// matches on -- and not onto some other error that would surface to the engine
// as a failed push of an object that was already gone.
func TestDeleteOfAMissingObjectIsNotAnErrorAgainstARealServer(t *testing.T) {
	target := requireFakeGCSServer(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)
	b := newEmulatorBackend(t, target, bucket)
	ctx := context.Background()

	if err := b.Delete(ctx, "files/never-existed.txt"); err != nil {
		t.Errorf("Delete of a missing object = %v, want nil", err)
	}

	// And once more after a real create/delete pair, which is the shape the
	// engine's retry actually produces: the first attempt succeeded, the
	// response was lost, the second attempt finds nothing.
	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(ctx, "files/a.txt", local); err != nil {
		t.Fatalf("Put = %v", err)
	}
	if err := b.Delete(ctx, "files/a.txt"); err != nil {
		t.Fatalf("first Delete = %v", err)
	}
	if err := b.Delete(ctx, "files/a.txt"); err != nil {
		t.Errorf("second Delete = %v, want nil; a retried delete must be a success", err)
	}
}
