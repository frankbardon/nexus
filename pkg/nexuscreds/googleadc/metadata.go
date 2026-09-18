package googleadc

import (
	"context"

	"github.com/frankbardon/nexus/pkg/nexuscreds/gcemeta"
)

// The GCE metadata helpers moved to pkg/nexuscreds/gcemeta, which performs no
// registration. They are kept here, delegating, because they were exported from
// this package first and a caller that already imports it for the credential
// source should not have to add a second import to ask where it is running.
//
// What must NOT happen is the reverse: a caller that wants only the metadata
// facts should reach for gcemeta directly. This package registers "google-adc"
// with nexuscreds from init, so importing it for the facts alone puts a
// credential source into the binary that nobody asked for — which is exactly
// the regression the split exists to undo. nexus.llm.gemini was the caller that
// made that concrete; it now imports gcemeta.

// ErrNotOnGCE is returned by the metadata helpers when this process is not
// running on Google Compute Engine. It is the same sentinel value as
// gcemeta.ErrNotOnGCE, so errors.Is works whichever package a caller compares
// against.
var ErrNotOnGCE = gcemeta.ErrNotOnGCE

// ProjectID returns the project this VM or pod belongs to, as reported by the
// GCE metadata server. It returns ErrNotOnGCE when there is no metadata server.
func ProjectID(ctx context.Context) (string, error) { return gcemeta.ProjectID(ctx) }

// Zone returns this VM or pod's zone, e.g. "us-central1-c". It returns
// ErrNotOnGCE when there is no metadata server.
func Zone(ctx context.Context) (string, error) { return gcemeta.Zone(ctx) }

// Region returns the region this VM or pod runs in, derived from its zone. It
// returns ErrNotOnGCE when there is no metadata server.
func Region(ctx context.Context) (string, error) { return gcemeta.Region(ctx) }

// RegionFromZone strips a GCE zone's trailing "-<letter>" to give its region:
// "us-central1-c" becomes "us-central1". It returns "" for anything that is not
// shaped like a zone.
func RegionFromZone(zone string) string { return gcemeta.RegionFromZone(zone) }
