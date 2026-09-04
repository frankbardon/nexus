package gcsstore_test

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGCS is an in-process, loopback-only Cloud Storage server: enough of the
// JSON API's object insert, delete and list, plus the media download endpoint,
// for this backend to be driven end to end.
//
// # Why a fake rather than a mocked SDK client
//
// The same reasoning as modules/objectstore-s3/fakes3_test.go, and it applies
// with more force here. Substituting an interface for *storage.Client would
// have been less code and would have tested nothing that matters: the parts of
// this backend that can realistically be wrong all live below that line.
// Whether a key with a space or a CJK segment survives the JSON API's two
// different escapings -- a query parameter on upload, a path segment on delete,
// a URL path on download. Whether the object list iterator is drained past its
// first page. Whether the credential source that was configured is the one that
// signs the request. Whether the CRC32C the client computes matches the object
// the server stored. A mock at the client boundary asserts that the right Go
// struct was built and throws away every one of those questions.
//
// So the real SDK client runs against a real HTTP server over the loopback
// interface. That keeps `make test` untagged, offline, secret-free and fast --
// no container, no daemon, no credentials -- while still exercising the whole
// stack. What it deliberately does NOT prove is fidelity: this fake implements
// what this backend uses, and agrees with GCS only as far as its author
// understood GCS. Closing that gap is E5-S2's job, running the same contract
// suite against fake-gcs-server behind a build tag. The two are complements,
// not duplicates: this one is always on and catches regressions, that one
// catches divergence between the fake and a real implementation.
//
// # What the fake checks rather than accepts
//
// It computes the CRC32C of every object it stores and reports it back, and it
// sends it again on download. The SDK verifies both, and refuses the object on
// a mismatch. That is not decoration: it means a bug in how this backend frames
// an upload body shows up as a checksum failure rather than as a silently
// corrupted session, which is the failure mode an object store exists to
// prevent.
//
// It does NOT verify credentials, for the reason the S3 fake does not verify
// signatures: it would mean reimplementing the auth server in the test. What
// the tests assert instead is the *identity* in the Authorization header, which
// is what distinguishes the credential sources this story has to exercise.
type fakeGCS struct {
	mu      sync.Mutex
	bucket  string
	objects map[string]fakeObject

	// pageSize is how many objects one list response carries. Small on purpose
	// -- see newFakeGCS.
	pageSize int

	// lastAuth is the Authorization header of the most recent request that was
	// not a token exchange, so a test can see which principal it was made as.
	lastAuth string

	// tokens counts token-exchange requests, which is how a test tells the
	// static-key path from the unauthenticated one without reading headers.
	tokens int

	server *httptest.Server
}

type fakeObject struct {
	data []byte
	mod  time.Time
}

// defaultFakePageSize is deliberately far below the real API's 1000.
//
// The contract suite's pagination case defaults to storing 1200 objects
// precisely because 1000 is the real page size. Replaying that against an
// in-process fake would mean 1200 round trips inside `make test`, which buys
// nothing here: what this fake can prove is that the iterator is drained at
// all, and a page size of 50 proves that with a fraction of the objects. The
// full-size probe belongs to the fake-gcs-server run, where the page size is
// genuinely 1000 and the cost is paid once in a tagged suite.
const defaultFakePageSize = 50

func newFakeGCS(t *testing.T, bucket string) *fakeGCS {
	t.Helper()
	f := &fakeGCS{
		bucket:   bucket,
		objects:  make(map[string]fakeObject),
		pageSize: defaultFakePageSize,
	}
	// A bare handler, not an http.ServeMux: ServeMux cleans request paths, and
	// this backend must be able to store a key like "files/...triple" without
	// the test harness silently rewriting it.
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// endpoint is what an operator would write for core.object_store.endpoint: a
// bare scheme://host, with no API path. normalizeEndpoint appends the rest, and
// pointing the tests at the bare form is what keeps that behaviour covered.
func (f *fakeGCS) endpoint() string { return f.server.URL }

// tokenURI is the OAuth2 token endpoint a test's service-account JSON points
// at, so the whole credential exchange stays on the loopback interface.
func (f *fakeGCS) tokenURI() string { return f.server.URL + "/token" }

func (f *fakeGCS) authHeader() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

func (f *fakeGCS) tokenRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens
}

// keys returns every stored object key, sorted. Used by tests that assert on
// the physical layout in the bucket rather than on what the backend reports.
func (f *fakeGCS) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *fakeGCS) put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = fakeObject{data: data, mod: time.Now().UTC()}
}

func (f *fakeGCS) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// The OAuth2 token endpoint a test's service-account JSON points at, so a
	// credentialed run never leaves the loopback interface. Counted rather than
	// recorded in lastAuth: the exchange itself is unauthenticated, and letting
	// it overwrite lastAuth would hide the identity of the request that follows.
	if path == "/token" {
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
		return
	}

	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.mu.Unlock()

	switch {
	// Upload: POST /upload/storage/v1/b/<bucket>/o?name=<key>&uploadType=multipart
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/upload/storage/v1/b/"):
		f.insert(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/upload/storage/v1/b/"), "/o"))

	// List: GET /storage/v1/b/<bucket>/o
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/storage/v1/b/") && strings.HasSuffix(path, "/o"):
		f.list(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/storage/v1/b/"), "/o"))

	// Delete: DELETE /storage/v1/b/<bucket>/o/<key>
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/storage/v1/b/"):
		bucket, key, ok := strings.Cut(strings.TrimPrefix(path, "/storage/v1/b/"), "/o/")
		if !ok {
			writeGCSError(w, http.StatusBadRequest, "unparseable object path "+path)
			return
		}
		f.deleteObject(w, bucket, key)

	// Media download: GET /<bucket>/<key>. The SDK reads object bodies from the
	// endpoint host's root, not from under the JSON API path, so this arm has
	// to exist separately -- and getting it wrong is exactly the kind of URL
	// construction bug a mocked client would hide.
	case r.Method == http.MethodGet:
		bucket, key, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		if !ok {
			writeGCSError(w, http.StatusNotFound, "unparseable media path "+path)
			return
		}
		f.get(w, bucket, key)

	default:
		writeGCSError(w, http.StatusMethodNotAllowed, r.Method+" "+path)
	}
}

// insert parses the multipart/related body the SDK sends for a single-request
// upload: a JSON metadata part naming the object, then the raw media part.
func (f *fakeGCS) insert(w http.ResponseWriter, r *http.Request, bucket string) {
	if bucket != f.bucket {
		writeGCSError(w, http.StatusNotFound, "no such bucket: "+bucket)
		return
	}
	key := r.URL.Query().Get("name")
	if key == "" {
		writeGCSError(w, http.StatusBadRequest, "no object name")
		return
	}

	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		writeGCSError(w, http.StatusBadRequest, "expected a multipart/related upload")
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var data []byte
	for i := 0; ; i++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeGCSError(w, http.StatusBadRequest, "malformed multipart body: "+err.Error())
			return
		}
		body, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			writeGCSError(w, http.StatusBadRequest, "unreadable part: "+err.Error())
			return
		}
		// Part 0 is the JSON metadata, part 1 is the media. Reading the media
		// part verbatim -- no trimming, no text decoding -- is what makes the
		// binary and zero-byte cases in the contract suite mean something.
		if i == 1 {
			data = body
		}
	}

	f.put(key, data)
	f.mu.Lock()
	res := f.resourceLocked(key)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, res)
}

func (f *fakeGCS) get(w http.ResponseWriter, bucket, escapedKey string) {
	if bucket != f.bucket {
		writeGCSError(w, http.StatusNotFound, "no such bucket: "+bucket)
		return
	}
	key, err := url.PathUnescape(escapedKey)
	if err != nil {
		writeGCSError(w, http.StatusBadRequest, "unescapable object name")
		return
	}
	f.mu.Lock()
	obj, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		writeGCSError(w, http.StatusNotFound, "no such object: "+key)
		return
	}
	// The SDK verifies this against what it received, so sending it is what
	// turns the fake into an integrity check rather than an echo.
	w.Header().Set("X-Goog-Hash", "crc32c="+crc32cOf(obj.data))
	w.Header().Set("X-Goog-Generation", "1")
	w.Header().Set("Last-Modified", obj.mod.Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj.data)
}

func (f *fakeGCS) deleteObject(w http.ResponseWriter, bucket, escapedKey string) {
	if bucket != f.bucket {
		writeGCSError(w, http.StatusNotFound, "no such bucket: "+bucket)
		return
	}
	key, err := url.PathUnescape(escapedKey)
	if err != nil {
		writeGCSError(w, http.StatusBadRequest, "unescapable object name")
		return
	}
	f.mu.Lock()
	_, existed := f.objects[key]
	delete(f.objects, key)
	f.mu.Unlock()
	if !existed {
		// GCS 404s a delete of a missing object, unlike S3. This is the whole
		// point of asserting it here: the backend has to translate it, and a
		// fake that quietly returned 204 would let a regression through.
		writeGCSError(w, http.StatusNotFound, "no such object: "+key)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeGCS) list(w http.ResponseWriter, r *http.Request, bucket string) {
	if bucket != f.bucket {
		writeGCSError(w, http.StatusNotFound, "no such bucket: "+bucket)
		return
	}
	q := r.URL.Query()
	prefix := q.Get("prefix")
	after := q.Get("pageToken")

	f.mu.Lock()
	all := make([]string, 0, len(f.objects))
	for k := range f.objects {
		// Plain string prefix matching, exactly as the API specifies it with no
		// delimiter set. The backend is required to be correct on top of this,
		// not to be rescued by a fake that is cleverer than the real thing.
		if strings.HasPrefix(k, prefix) {
			all = append(all, k)
		}
	}
	sort.Strings(all)

	start := 0
	if after != "" {
		start = sort.SearchStrings(all, after)
	}
	end := min(start+f.pageSize, len(all))

	items := make([]map[string]any, 0, end-start)
	for _, k := range all[start:end] {
		items = append(items, f.resourceLocked(k))
	}
	page := map[string]any{"kind": "storage#objects", "items": items}
	if end < len(all) {
		// The page token is just the next key. Opaque to the client, which is
		// all the API promises about it.
		page["nextPageToken"] = all[end]
	}
	f.mu.Unlock()

	writeJSON(w, http.StatusOK, page)
}

// resourceLocked renders one object as the JSON API's object resource. The
// caller must hold f.mu: list builds a whole page under one lock, and taking it
// again per object would deadlock.
func (f *fakeGCS) resourceLocked(key string) map[string]any {
	obj, ok := f.objects[key]
	if !ok {
		return nil
	}
	return map[string]any{
		"kind":       "storage#object",
		"bucket":     f.bucket,
		"name":       key,
		"generation": "1",
		// The API renders int64 fields as strings, and a fake that used a JSON
		// number would let a size-parsing bug through.
		"size":    strconv.Itoa(len(obj.data)),
		"crc32c":  crc32cOf(obj.data),
		"updated": obj.mod.Format(time.RFC3339Nano),
	}
}

// crc32cOf renders the Castagnoli CRC32 of b the way the API does: big-endian,
// base64. The SDK compares it against its own computation over what it sent and
// received.
func crc32cOf(b []byte) string {
	sum := crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], sum)
	return base64.StdEncoding.EncodeToString(raw[:])
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeGCSError renders the JSON API's error envelope. The SDK turns a 404 on
// an object handle into storage.ErrObjectNotExist by status code, so the shape
// matters less than the code -- but keeping the shape right is what lets a test
// read a failure without decoding it by hand.
func writeGCSError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": message,
		},
	})
}
