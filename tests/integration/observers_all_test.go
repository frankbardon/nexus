//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestObserversAll_Boot validates all three observer plugins (logger + otel
// + thinking) boot together without subscription conflicts. The OTel plugin
// is pointed at an unreachable port; it should log a connection error but
// not crash or block other observers.
func TestObserversAll_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-observers-all.yaml", testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.observe.logger",
		"nexus.observe.otel",
		"nexus.observe.thinking",
	)
	// Session must complete normally despite OTel collector being unreachable.
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")
}
