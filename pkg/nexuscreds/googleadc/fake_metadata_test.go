package googleadc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/compute/metadata"
)

// The values every fake in this package answers with. The project ID is a
// single constant on purpose: cloud.google.com/go/compute/metadata memoises a
// successful project-ID read process-wide, so two fakes disagreeing about it
// would make the tests order-dependent.
const (
	testProjectID = "nexus-test-project"
	testZone      = "us-central1-c"
	testRegion    = "us-central1"
	testSAEmail   = "nexus-agent@nexus-test-project.iam.gserviceaccount.com"
)

// TestMain isolates the test binary from whatever Google credentials the host
// happens to have, and pins the one piece of library state that cannot be
// un-set once observed.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "googleadc-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, "googleadc tests: creating a temp home:", err)
		os.Exit(1)
	}

	// ADC's chain is: GOOGLE_APPLICATION_CREDENTIALS, then a well-known file
	// under $HOME, then the metadata server. A developer running `make test`
	// on a machine with real gcloud credentials would otherwise resolve them.
	os.Setenv("HOME", home)
	os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	// metadata.OnGCE memoises its answer in a package-level sync.Once: the
	// first call in the process fixes it forever, and with GCE_METADATA_HOST
	// unset that call probes the real network. Setting the variable makes it
	// short-circuit to true; making the call here fixes it before any test
	// runs, so no test can poison another through ordering and nothing in this
	// binary ever touches the network. The address is never dialled — every
	// test replaces it with its own httptest server.
	os.Setenv("GCE_METADATA_HOST", "169.254.169.254")
	_ = metadata.OnGCE()

	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// pinOnGCE fixes what the package's metadata helpers believe about their
// environment, and restores it afterwards. See the onGCE variable.
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

	mu          sync.Mutex
	tokenHits   int
	accessToken string
	expiresIn   int
	tokenStatus int    // 0 => mint a token; otherwise fail with this status
	tokenBody   string // body for a failing token response
	blockToken  chan struct{}
	zonePath    string
	emailStatus int
}

// newFakeMetadata starts a fake metadata server, points the process at it, and
// tears both down when the test ends.
func newFakeMetadata(t *testing.T) *fakeMetadata {
	t.Helper()
	f := &fakeMetadata{
		accessToken: "ya29.fake-metadata-token",
		expiresIn:   3600,
		zonePath:    "projects/123456789/zones/" + testZone,
	}

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
	mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/default/email", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		status := f.emailStatus
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, "no email", status)
			return
		}
		f.plain(w, testSAEmail)
	})
	mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/default/token", f.serveToken)

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

func (f *fakeMetadata) serveToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokenHits++
	token, expires, status, body, block := f.accessToken, f.expiresIn, f.tokenStatus, f.tokenBody, f.blockToken
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	if status != 0 {
		w.Header().Set("Metadata-Flavor", "Google")
		http.Error(w, body, status)
		return
	}
	w.Header().Set("Metadata-Flavor", "Google")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": token,
		"expires_in":   expires,
		"token_type":   "Bearer",
	})
}

// hits reports how many times the token endpoint was asked for a token. It is
// the whole point of several tests: caching is the library's job, and the only
// way to prove it happens is to count round trips.
func (f *fakeMetadata) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenHits
}

// setToken changes what the next mint returns.
func (f *fakeMetadata) setToken(token string, expiresIn int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessToken, f.expiresIn = token, expiresIn
}

// failToken makes the token endpoint answer with an HTTP failure.
func (f *fakeMetadata) failToken(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenStatus, f.tokenBody = status, body
}
