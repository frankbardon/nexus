package googleadc

import (
	"context"
	"errors"
	"testing"
)

func TestProjectID_ReadsTheMetadataServer(t *testing.T) {
	newFakeMetadata(t)

	got, err := ProjectID(context.Background())
	if err != nil {
		t.Fatalf("ProjectID: unexpected error: %v", err)
	}
	if got != testProjectID {
		t.Fatalf("ProjectID = %q, want %q", got, testProjectID)
	}
}

func TestProjectID_ReportsNotOnGCE(t *testing.T) {
	pinOnGCE(t, false)

	got, err := ProjectID(context.Background())
	if !errors.Is(err, ErrNotOnGCE) {
		t.Fatalf("ProjectID off GCE: err = %v, want ErrNotOnGCE", err)
	}
	if got != "" {
		t.Fatalf("ProjectID = %q alongside an error, want the empty string", got)
	}
}

func TestZone_ReadsTheMetadataServer(t *testing.T) {
	newFakeMetadata(t)

	got, err := Zone(context.Background())
	if err != nil {
		t.Fatalf("Zone: unexpected error: %v", err)
	}
	// The metadata server answers with the full "projects/N/zones/Z" path; the
	// library trims it, and callers here should see only the zone.
	if got != testZone {
		t.Fatalf("Zone = %q, want %q", got, testZone)
	}
}

func TestZone_ReportsNotOnGCE(t *testing.T) {
	pinOnGCE(t, false)

	if _, err := Zone(context.Background()); !errors.Is(err, ErrNotOnGCE) {
		t.Fatalf("Zone off GCE: err = %v, want ErrNotOnGCE", err)
	}
}

func TestRegion_DerivesTheRegionFromTheZone(t *testing.T) {
	newFakeMetadata(t)

	got, err := Region(context.Background())
	if err != nil {
		t.Fatalf("Region: unexpected error: %v", err)
	}
	if got != testRegion {
		t.Fatalf("Region = %q, want %q derived from zone %q", got, testRegion, testZone)
	}
}

func TestRegion_ReportsNotOnGCE(t *testing.T) {
	pinOnGCE(t, false)

	if _, err := Region(context.Background()); !errors.Is(err, ErrNotOnGCE) {
		t.Fatalf("Region off GCE: err = %v, want ErrNotOnGCE", err)
	}
}

func TestRegion_RejectsAZoneWithNoRegionSuffix(t *testing.T) {
	fake := newFakeMetadata(t)
	fake.mu.Lock()
	fake.zonePath = "projects/123456789/zones/somewhere"
	fake.mu.Unlock()

	got, err := Region(context.Background())
	if err == nil {
		t.Fatalf("Region = %q, want an error for a zone with no region suffix", got)
	}
	if errors.Is(err, ErrNotOnGCE) {
		t.Fatal("Region reported ErrNotOnGCE for a malformed zone; the two mean different things")
	}
}

func TestRegionFromZone_StripsTheTrailingZoneLetter(t *testing.T) {
	if got := RegionFromZone("us-central1-c"); got != "us-central1" {
		t.Errorf("RegionFromZone(us-central1-c) = %q, want us-central1", got)
	}
	if got := RegionFromZone("europe-west4-a"); got != "europe-west4" {
		t.Errorf("RegionFromZone(europe-west4-a) = %q, want europe-west4", got)
	}
	if got := RegionFromZone("northamerica-northeast1-b"); got != "northamerica-northeast1" {
		t.Errorf("RegionFromZone(northamerica-northeast1-b) = %q, want northamerica-northeast1", got)
	}
}

func TestRegionFromZone_RejectsAnythingNotShapedLikeAZone(t *testing.T) {
	for _, in := range []string{"", "us-central1", "us-central1-", "-c", "c", "us-central1-cd", "us_central1_c"} {
		if got := RegionFromZone(in); got != "" {
			t.Errorf("RegionFromZone(%q) = %q, want the empty string", in, got)
		}
	}
}
