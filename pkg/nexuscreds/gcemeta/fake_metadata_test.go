package gcemeta

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/compute/metadata"
)

// The values the fake in this package answers with. The project ID is a single
// constant on purpose: cloud.google.com/go/compute/metadata memoises a
// successful project-ID read process-wide, so two fakes disagreeing about it
// would make the tests order-dependent.
const (
	testProjectID = "nexus-test-project"
	testZone      = "us-central1-c"
	testRegion    = "us-central1"
)

// TestMain pins the one piece of library state that cannot be un-set once it
// has been observed, before any test in this package runs.
func TestMain(m *testing.M) {
	// metadata.OnGCE memoises its answer in a package-level sync.Once: the
	// first call in the process fixes it forever, and with GCE_METADATA_HOST
	// unset that call probes the real network. Setting the variable makes it
	// short-circuit to true; making the call here fixes it before any test
	// runs, so no test can poison another through ordering and nothing in this
	// binary ever touches the network. The address is never dialled — every
	// test either replaces it with its own httptest server or pins onGCE.
	os.Setenv("GCE_METADATA_HOST", "169.254.169.254")
	_ = metadata.OnGCE()

	os.Exit(m.Run())
}

// pinOnGCE fixes what this package's helpers believe about their environment,
// and restores it afterwards. See the onGCE variable.
func pinOnGCE(t *testing.T, on bool) {
	t.Helper()
	prev := onGCE
	onGCE = func() bool { return on }
	t.Cleanup(func() { onGCE = prev })
}

// fakeMetadata is an httptest stand-in for the GCE metadata server, wired in
// through GCE_METADATA_HOST — the same lever the library documents for
// spoofing the service in a container. It is what makes the whole pod-identity
// path testable with no cloud account, container or emulator.
type fakeMetadata struct {
	srv *httptest.Server

	mu       sync.Mutex
	zonePath string
}

// newFakeMetadata starts a fake metadata server, points the process at it, and
// tears both down when the test ends.
func newFakeMetadata(t *testing.T) *fakeMetadata {
	t.Helper()
	f := &fakeMetadata{zonePath: "projects/123456789/zones/" + testZone}

	mux := http.NewServeMux()
	mux.HandleFunc("/computeMetadata/v1/project/project-id", func(w http.ResponseWriter, r *http.Request) {
		f.plain(w, testProjectID)
	})
	mux.HandleFunc("/computeMetadata/v1/instance/zone", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		zone := f.zonePath
		f.mu.Unlock()
		f.plain(w, zone)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(f.srv.URL, "http://"))
	pinOnGCE(t, true)
	return f
}

func (f *fakeMetadata) plain(w http.ResponseWriter, body string) {
	w.Header().Set("Metadata-Flavor", "Google")
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, body)
}

// setZonePath changes the fully qualified zone path the fake answers with.
func (f *fakeMetadata) setZonePath(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.zonePath = path
}
