package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/compute/metadata"

	"github.com/frankbardon/nexus/pkg/nexuscreds/gcemeta"
)

// The values the fake metadata server answers with.
//
// The project ID is a single constant on purpose: the metadata library
// memoises a successful project-ID read process-wide, so two fakes disagreeing
// about it would make these tests order-dependent. The zone is deliberately
// NOT in us-east5, so a region derived from it cannot be mistaken for
// defaultVertexRegion.
const (
	testMetadataProjectID = "nexus-anthropic-test-project"
	testMetadataZone      = "europe-west4-b"
	testMetadataRegion    = "europe-west4"
	testMetadataSAEmail   = "nexus-agent@nexus-anthropic-test-project.iam.gserviceaccount.com"
	testMetadataToken     = "ya29.fake-metadata-token"
)

// TestMain pins the one piece of library state that cannot be un-set once it
// has been observed, before any test in this package runs.
func TestMain(m *testing.M) {
	// A dead end on loopback that 404s every metadata path. Pointing
	// GCE_METADATA_HOST at it does two things. It makes metadata.OnGCE
	// short-circuit to true without probing anything — the library documents
	// the variable as the supported way to spoof the service — and it
	// guarantees that any lookup a test does not explicitly fake fails fast
	// and locally instead of dialling the real 169.254.169.254.
	deadEnd := httptest.NewServer(http.HandlerFunc(http.NotFound))
	os.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(deadEnd.URL, "http://"))

	// metadata.OnGCE memoises in a package-level sync.Once. Calling it here
	// fixes the answer while the variable above is set, so no test can poison
	// another through ordering and nothing in this binary ever reaches the
	// network.
	_ = metadata.OnGCE()

	code := m.Run()
	deadEnd.Close()
	os.Exit(code)
}

// pinMetadataProjectID makes the metadata rung of the project chain answer
// with a fixed result for the duration of the test. See the metadataProjectID
// variable in auth.go for why a seam is needed at all.
func pinMetadataProjectID(t *testing.T, id string, err error) {
	t.Helper()
	prev := metadataProjectID
	metadataProjectID = func(context.Context) (string, error) { return id, err }
	t.Cleanup(func() { metadataProjectID = prev })
}

// pinMetadataRegion is pinMetadataProjectID for the region chain.
func pinMetadataRegion(t *testing.T, region string, err error) {
	t.Helper()
	prev := metadataRegion
	metadataRegion = func(context.Context) (string, error) { return region, err }
	t.Cleanup(func() { metadataRegion = prev })
}

// pinNotOnGCE is the common case: a host with no metadata server at all, which
// cannot be reached by clearing an environment variable once OnGCE has been
// observed.
func pinNotOnGCE(t *testing.T) {
	t.Helper()
	pinMetadataProjectID(t, "", gcemeta.ErrNotOnGCE)
	pinMetadataRegion(t, "", gcemeta.ErrNotOnGCE)
}

// fakeMetadata is the httptest stand-in for the GCE metadata server. Beyond
// the project and zone the region chain reads, it serves the access-token
// endpoint a keyless pod mints from, so the whole Workload Identity boot — ADC
// resolution included — runs without a cloud account, container or emulator.
type fakeMetadata struct {
	mu          sync.Mutex
	tokenHits   int
	tokenStatus int // 0 => mint a token; otherwise refuse with this status
}

// newFakeMetadata starts a fake metadata server and points the process at it
// for the duration of the test. Nothing is pinned here on purpose: this is the
// one path that exercises gcemeta and the metadata library for real.
func newFakeMetadata(t *testing.T) *fakeMetadata {
	t.Helper()

	f := &fakeMetadata{}

	mux := http.NewServeMux()
	mux.HandleFunc("/computeMetadata/v1/project/project-id", func(w http.ResponseWriter, r *http.Request) {
		plainMetadata(w, testMetadataProjectID)
	})
	mux.HandleFunc("/computeMetadata/v1/instance/zone", func(w http.ResponseWriter, r *http.Request) {
		// The metadata server answers with a fully qualified zone path; the
		// library trims it to the bare zone.
		plainMetadata(w, "projects/123456789/zones/"+testMetadataZone)
	})
	mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/default/email", func(w http.ResponseWriter, r *http.Request) {
		plainMetadata(w, testMetadataSAEmail)
	})
	mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/default/token", f.serveToken)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))
	return f
}

func (f *fakeMetadata) serveToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokenHits++
	status := f.tokenStatus
	f.mu.Unlock()

	w.Header().Set("Metadata-Flavor", "Google")
	if status != 0 {
		http.Error(w, "the instance service account is not bound to a Google service account", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": testMetadataToken,
		"expires_in":   3600,
		"token_type":   "Bearer",
	})
}

// failToken makes the token endpoint refuse, which is what a pod whose
// Workload Identity binding is broken actually sees.
func (f *fakeMetadata) failToken(status int) {
	f.mu.Lock()
	f.tokenStatus = status
	f.mu.Unlock()
}

// hits reports how many times a token was asked for.
func (f *fakeMetadata) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenHits
}

// isolateADC points the Application Default Credentials chain away from
// whatever Google credentials the developer's machine happens to carry: ADC
// reads GOOGLE_APPLICATION_CREDENTIALS, then a well-known file under $HOME,
// and only then the metadata server. Without this a `make test` on a laptop
// with a live `gcloud auth application-default login` would resolve those and
// leave the process.
func isolateADC(t *testing.T) {
	t.Helper()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir())
}

func plainMetadata(w http.ResponseWriter, body string) {
	w.Header().Set("Metadata-Flavor", "Google")
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, body)
}
