package gemini

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/compute/metadata"

	"github.com/frankbardon/nexus/pkg/nexuscreds/gcemeta"
)

// The values the fake metadata server answers with.
//
// The project ID is a single constant on purpose: the metadata library
// memoises a successful project-ID read process-wide, so two fakes disagreeing
// about it would make these tests order-dependent. The zone is deliberately
// NOT in us-central1, so a location derived from it cannot be mistaken for
// defaultVertexLocation.
const (
	testMetadataProjectID = "nexus-gemini-test-project"
	testMetadataZone      = "europe-west4-b"
	testMetadataRegion    = "europe-west4"
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
// variable for why a seam is needed at all.
func pinMetadataProjectID(t *testing.T, id string, err error) {
	t.Helper()
	prev := metadataProjectID
	metadataProjectID = func(context.Context) (string, error) { return id, err }
	t.Cleanup(func() { metadataProjectID = prev })
}

// pinMetadataRegion is pinMetadataProjectID for the location chain.
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

// newFakeMetadata starts an httptest stand-in for the GCE metadata server and
// points the process at it for the duration of the test. Nothing is pinned
// here on purpose: this is the one path that exercises gcemeta and the
// metadata library for real, which is what makes a pod-identity boot testable
// with no cloud account, container or emulator.
func newFakeMetadata(t *testing.T) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/computeMetadata/v1/project/project-id", func(w http.ResponseWriter, r *http.Request) {
		plainMetadata(w, testMetadataProjectID)
	})
	mux.HandleFunc("/computeMetadata/v1/instance/zone", func(w http.ResponseWriter, r *http.Request) {
		// The metadata server answers with a fully qualified zone path; the
		// library trims it to the bare zone.
		plainMetadata(w, "projects/123456789/zones/"+testMetadataZone)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))
}

func plainMetadata(w http.ResponseWriter, body string) {
	w.Header().Set("Metadata-Flavor", "Google")
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, body)
}
