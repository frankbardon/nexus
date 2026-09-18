package googleadc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/frankbardon/nexus/pkg/nexuscreds/gcemeta"
)

// The behaviour of the metadata helpers is tested in pkg/nexuscreds/gcemeta,
// where they now live. What is tested here is only that this package's
// re-exports still reach them, because a caller that predates the split should
// not notice it.

func TestErrNotOnGCE_IsTheSameSentinelAsGcemeta(t *testing.T) {
	// Identity, not merely an equal message: a caller comparing with errors.Is
	// against either package must match an error produced by the other, and the
	// only way that holds in both directions is if it is one value.
	if ErrNotOnGCE != gcemeta.ErrNotOnGCE {
		t.Fatalf("googleadc.ErrNotOnGCE = %v is not gcemeta.ErrNotOnGCE = %v", ErrNotOnGCE, gcemeta.ErrNotOnGCE)
	}
	// And the way a caller actually meets it: wrapped, coming out of gcemeta,
	// compared against the name this package exports.
	wrapped := fmt.Errorf("resolving the project: %w", gcemeta.ErrNotOnGCE)
	if !errors.Is(wrapped, ErrNotOnGCE) {
		t.Fatal("a wrapped gcemeta.ErrNotOnGCE does not match googleadc.ErrNotOnGCE")
	}
}

func TestProjectID_DelegatesToGcemeta(t *testing.T) {
	newFakeMetadata(t)

	got, err := ProjectID(context.Background())
	if err != nil {
		t.Fatalf("ProjectID: unexpected error: %v", err)
	}
	if got != testProjectID {
		t.Fatalf("ProjectID = %q, want %q", got, testProjectID)
	}
}

func TestZone_DelegatesToGcemeta(t *testing.T) {
	newFakeMetadata(t)

	got, err := Zone(context.Background())
	if err != nil {
		t.Fatalf("Zone: unexpected error: %v", err)
	}
	if got != testZone {
		t.Fatalf("Zone = %q, want %q", got, testZone)
	}
}

func TestRegion_DelegatesToGcemeta(t *testing.T) {
	newFakeMetadata(t)

	got, err := Region(context.Background())
	if err != nil {
		t.Fatalf("Region: unexpected error: %v", err)
	}
	if got != testRegion {
		t.Fatalf("Region = %q, want %q derived from zone %q", got, testRegion, testZone)
	}
}

func TestRegionFromZone_DelegatesToGcemeta(t *testing.T) {
	if got := RegionFromZone(testZone); got != testRegion {
		t.Fatalf("RegionFromZone(%q) = %q, want %q", testZone, got, testRegion)
	}
	if got := RegionFromZone("not-a-zone-at-all"); got != "" {
		t.Fatalf("RegionFromZone of a non-zone = %q, want the empty string", got)
	}
}
