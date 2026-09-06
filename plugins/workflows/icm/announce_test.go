package icm

import (
	"slices"
	"testing"
)

// Stage artifacts, sidecars and copied initial inputs land under the engine's
// session tree through raw os.* calls in the session subpackage, announced via
// the Announce hook run.go installs. Emissions() is the contract harness's
// only view of that, so declaring the two types is what keeps a later refactor
// from silently dropping them.
func TestPlugin_DeclaresSessionFileEmissions(t *testing.T) {
	declared := New().Emissions()
	for _, want := range []string{"session.file.created", "session.file.updated"} {
		if !slices.Contains(declared, want) {
			t.Errorf("Emissions() = %v, missing %q", declared, want)
		}
	}
}
