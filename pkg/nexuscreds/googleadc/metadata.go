package googleadc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/compute/metadata"
)

// ErrNotOnGCE is returned by the metadata helpers when this process is not
// running on Google Compute Engine. It is a distinct sentinel rather than an
// empty result because the two mean different things to a resolution chain:
// "not on GCE" is a reason to try the next source, whereas a metadata server
// that answers with an error is a misconfiguration worth surfacing.
var ErrNotOnGCE = errors.New("googleadc: not running on GCE; there is no metadata server to ask")

// onGCE reports whether this process is on GCE. It is a variable, not a direct
// call, for one reason: metadata.OnGCE memoises its answer in a package-level
// sync.Once, so the first call in a process fixes it for the process's life —
// and when GCE_METADATA_HOST is unset that first call probes the real network.
// Tests pin it instead, which keeps them hermetic and order-independent.
var onGCE = metadata.OnGCE

// ProjectID returns the project this VM or pod belongs to, as reported by the
// GCE metadata server. It returns ErrNotOnGCE when there is no metadata server.
//
// This is the lowest-priority link in the project resolution chain and the one
// that makes project_id optional on GKE: a pod already knows its project, so
// making an operator repeat it in YAML is a chance to get it wrong.
func ProjectID(ctx context.Context) (string, error) {
	if !onGCE() {
		return "", ErrNotOnGCE
	}
	id, err := metadata.ProjectIDWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("googleadc: reading the project ID from the GCE metadata server: %w", err)
	}
	if id == "" {
		return "", fmt.Errorf("googleadc: the GCE metadata server returned an empty project ID")
	}
	return id, nil
}

// Zone returns this VM or pod's zone, e.g. "us-central1-c". It returns
// ErrNotOnGCE when there is no metadata server.
func Zone(ctx context.Context) (string, error) {
	if !onGCE() {
		return "", ErrNotOnGCE
	}
	zone, err := metadata.ZoneWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("googleadc: reading the zone from the GCE metadata server: %w", err)
	}
	if zone == "" {
		return "", fmt.Errorf("googleadc: the GCE metadata server returned an empty zone")
	}
	return zone, nil
}

// Region returns the region this VM or pod runs in, derived from its zone. It
// returns ErrNotOnGCE when there is no metadata server.
//
// Vertex AI is addressed by region, never by zone, which is why the derivation
// lives here rather than at each call site: a caller building a location
// resolution chain should ask for a region and get one.
func Region(ctx context.Context) (string, error) {
	zone, err := Zone(ctx)
	if err != nil {
		return "", err
	}
	region := RegionFromZone(zone)
	if region == "" {
		return "", fmt.Errorf("googleadc: the GCE metadata server returned zone %q, which is not of the form <region>-<letter>", zone)
	}
	return region, nil
}

// RegionFromZone strips a GCE zone's trailing "-<letter>" to give its region:
// "us-central1-c" becomes "us-central1". It returns "" for anything that is
// not shaped like a zone, so a caller can tell a derivation it should not
// trust from one it should.
//
// Exported because the derivation is the interesting half — a caller that
// already has a zone string from somewhere else should not re-implement it.
func RegionFromZone(zone string) string {
	i := strings.LastIndexByte(zone, '-')
	if i <= 0 || i != len(zone)-2 {
		return ""
	}
	c := zone[len(zone)-1]
	if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
		return ""
	}
	return zone[:i]
}
