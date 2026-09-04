package s3store_test

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is an in-process, loopback-only S3 server: enough of GET, PUT, DELETE
// and ListObjectsV2 for this backend to be driven end to end.
//
// # Why a fake rather than a mocked SDK client
//
// Substituting an interface for *s3.Client would have been less code, and it
// would have tested nothing that matters. The parts of this backend that can
// realistically be wrong are all below that line: SigV4 signing, path-style URL
// construction, key escaping for spaces and non-ASCII segments, ListObjectsV2
// pagination, and the checksum/transfer-encoding settings that decide whether
// an S3-compatible store accepts the request at all. A mock at the client
// boundary asserts that the right Go struct was built and then throws away
// every one of those questions.
//
// So the real SDK client runs against a real HTTP server over the loopback
// interface. That keeps `make test` untagged, offline, secret-free and fast --
// no container, no daemon, no credentials -- while still exercising the whole
// stack. What it deliberately does NOT prove is fidelity: the fake implements
// what this backend uses, and agrees with S3 only as far as its author
// understood S3. That is E4-S3's job, running the same contract suite against
// MinIO behind a build tag. The two are complements, not duplicates: this one
// is always on and catches regressions, that one catches divergence between
// the fake and a real implementation.
//
// # Why not verify signatures
//
// The fake accepts any signature. Verifying one would mean reimplementing SigV4
// in the test, which would test the test. What the tests do assert is the
// *identity* in the Authorization header's Credential scope, which is what
// distinguishes the two credential sources this story has to exercise and is
// the only part of signing this backend actually chooses.
type fakeS3 struct {
	mu      sync.Mutex
	bucket  string
	objects map[string]fakeObject

	// pageSize is how many keys one ListObjectsV2 response carries. Small on
	// purpose -- see newFakeS3.
	pageSize int

	// lastAuth is the Authorization header of the most recent request, so a
	// test can see which principal signed it.
	lastAuth string

	server *httptest.Server
}

type fakeObject struct {
	data []byte
	mod  time.Time
}

// defaultFakePageSize is deliberately far below S3's 1000.
//
// The contract suite's pagination case defaults to storing 1200 objects
// precisely because 1000 is the real page size. Replaying that against an
// in-process fake would mean 1200 signed round trips inside `make test`, which
// buys nothing here: what this fake can prove is that the paginator is followed
// at all, and a page size of 50 proves that with a fraction of the objects. The
// full-size probe belongs to the MinIO run, where the page size is genuinely
// 1000 and the cost is paid once in a tagged suite.
const defaultFakePageSize = 50

func newFakeS3(t *testing.T, bucket string) *fakeS3 {
	t.Helper()
	f := &fakeS3{
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

func (f *fakeS3) endpoint() string { return f.server.URL }

func (f *fakeS3) authHeader() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

// keys returns every stored object key, sorted. Used by tests that assert on
// the physical layout in the bucket rather than on what the backend reports.
func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *fakeS3) put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = fakeObject{data: data, mod: time.Now().UTC()}
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.mu.Unlock()

	// Path-style addressing: /<bucket>[/<key>]. This backend forces path style
	// whenever an endpoint is configured, so a virtual-host request arriving
	// here is a bug worth failing loudly on rather than guessing at.
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != f.bucket {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "no such bucket: "+bucket)
		return
	}

	switch {
	case r.Method == http.MethodGet && key == "":
		f.list(w, r)
	case r.Method == http.MethodGet:
		f.get(w, key)
	case r.Method == http.MethodPut:
		f.putObject(w, r, key)
	case r.Method == http.MethodDelete:
		f.deleteObject(w, key)
	case r.Method == http.MethodHead:
		f.head(w, key)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (f *fakeS3) get(w http.ResponseWriter, key string) {
	f.mu.Lock()
	obj, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "no such key: "+key)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
	w.Header().Set("Last-Modified", obj.mod.Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj.data)
}

func (f *fakeS3) head(w http.ResponseWriter, key string) {
	f.mu.Lock()
	obj, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NotFound", "no such key: "+key)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
	w.Header().Set("Last-Modified", obj.mod.Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) putObject(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	// Reject aws-chunked bodies outright instead of storing the framing as if
	// it were content. This is the setting clientOptions turns off for
	// S3-compatible endpoints, and a regression there would otherwise show up
	// as a corrupted object rather than a failing test.
	if enc := r.Header.Get("Content-Encoding"); strings.Contains(enc, "aws-chunked") {
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest",
			"aws-chunked bodies are not supported by every S3-compatible store")
		return
	}
	f.put(key, body)
	w.Header().Set("ETag", fmt.Sprintf("%q", fmt.Sprintf("%x", len(body))))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) deleteObject(w http.ResponseWriter, key string) {
	f.mu.Lock()
	delete(f.objects, key)
	f.mu.Unlock()
	// S3 reports success whether or not the key was there, which is the
	// behaviour objectstore.Backend.Delete is specified against.
	w.WriteHeader(http.StatusNoContent)
}

// listResult mirrors the ListObjectsV2 response shape closely enough for the
// SDK's generated deserialiser. The xmlns matters: without it the SDK's
// unmarshaller still copes, but a real client library pointed at this fake
// would not, and the fake is more useful the closer it stays to the wire form.
type listResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	XMLNS                 string   `xml:"xmlns,attr"`
	Name                  string   `xml:"Name"`
	Prefix                string   `xml:"Prefix"`
	KeyCount              int      `xml:"KeyCount"`
	MaxKeys               int      `xml:"MaxKeys"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
	Contents              []listEntry
}

type listEntry struct {
	XMLName      xml.Name `xml:"Contents"`
	Key          string   `xml:"Key"`
	LastModified string   `xml:"LastModified"`
	Size         int64    `xml:"Size"`
	StorageClass string   `xml:"StorageClass"`
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("list-type") != "2" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "only ListObjectsV2 is implemented")
		return
	}
	prefix := q.Get("prefix")
	after := q.Get("continuation-token")

	f.mu.Lock()
	all := make([]string, 0, len(f.objects))
	for k := range f.objects {
		// Raw byte prefix matching, exactly as S3 specifies it. The backend is
		// required to be correct on top of this, not to be rescued by a fake
		// that is cleverer than the real thing.
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

	res := listResult{
		XMLNS:       "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:        f.bucket,
		Prefix:      prefix,
		MaxKeys:     f.pageSize,
		IsTruncated: end < len(all),
	}
	if res.IsTruncated {
		// The continuation token is just the next key. Opaque to the client,
		// which is all S3 promises about it.
		res.NextContinuationToken = all[end]
	}
	for _, k := range all[start:end] {
		obj := f.objects[k]
		res.Contents = append(res.Contents, listEntry{
			Key:          k,
			LastModified: obj.mod.Format("2006-01-02T15:04:05.000Z"),
			Size:         int64(len(obj.data)),
			StorageClass: "STANDARD",
		})
	}
	res.KeyCount = len(res.Contents)
	f.mu.Unlock()

	body, err := xml.Marshal(res)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	body, _ := xml.Marshal(s3Error{Code: code, Message: message})
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
