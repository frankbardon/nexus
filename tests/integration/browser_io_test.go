//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestBrowserIO_Boot validates that the io/browser plugin starts a local
// HTTP server (port 0 = OS-assigned), emits its session-start event, and
// shuts down cleanly. No browser is actually opened. This closes the
// zero-coverage gap for plugins/io/browser/.
func TestBrowserIO_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-browser-io.yaml",
		testharness.WithTimeout(15*time.Second))
	h.Run()

	h.AssertBooted("nexus.io.test", "nexus.io.browser")
	h.AssertEventEmitted("io.session.start")
}
