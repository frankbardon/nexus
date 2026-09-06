package seamcheck_test

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
)

// TestSeamIsImplementableFromAnotherModule runs the exported conformance suite
// against the reference in-memory backend, from outside the root module.
//
// The assertions themselves duplicate what objectstoretest already runs in
// tree; the duplication is not the point and the runtime is a fraction of a
// second. What is being asserted here is the import: this file compiling and
// this suite running proves that a third party -- or the S3 and GCS modules
// next door -- can depend on objectstore and objectstoretest without pulling in
// anything unexported, and that the module wiring under modules/ resolves.
func TestSeamIsImplementableFromAnotherModule(t *testing.T) {
	objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
		return objectstoretest.NewMemory()
	})
}
