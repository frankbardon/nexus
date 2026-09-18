package optincheck

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	"github.com/frankbardon/nexus/pkg/nexuscreds"
)

// googleADCSourceName is spelled out rather than taken from googleadc.Name,
// because importing that package to name it would register it and make the
// assertion below meaningless. It must match the defaultCredentialSource the
// gemini and anthropic providers fall back to.
const googleADCSourceName = "google-adc"

// TestLinkingEveryPluginRegistersNoCredentialSource is the whole point of this
// package.
//
// Every binary built from this repository's plugin set links allplugins. If
// any plugin in it reached a credential source through a package that
// registers one from init, that source would be in the registry of every such
// binary — and because the Vertex credentials key defaults to "google-adc", an
// embedder who ships their own Vault or SPIFFE source would get Google ADC
// silently selected by a config that merely omitted the key, instead of an
// error naming the sources their binary actually carries.
//
// That is what happened once: the gemini plugin imported pkg/nexuscreds/googleadc
// for its GCE metadata helpers and inherited the registration. The helpers
// moved to pkg/nexuscreds/gcemeta, which registers nothing, and the anthropic
// provider was ported onto the same seam on the same terms. This test is what
// stops the import creeping back into either of them, or arriving with a
// plugin written later.
func TestLinkingEveryPluginRegistersNoCredentialSource(t *testing.T) {
	// Reference the plugin set so the import is real linkage rather than
	// something the compiler could elide. Registration runs no plugin Init and
	// touches no network.
	r := engine.NewPluginRegistry()
	allplugins.RegisterAll(r)

	if nexuscreds.Registered(googleADCSourceName) {
		t.Errorf("linking the plugin set registered the %q credential source; "+
			"it must come from a deliberate blank import in cmd/, not from a plugin's import graph",
			googleADCSourceName)
	}

	// The stronger statement, and the one that cannot pass by a misspelling:
	// linking the plugins puts NO source in the registry at all.
	if got := nexuscreds.Sources(); len(got) != 0 {
		t.Errorf("linking the plugin set registered credential sources %v, want none", got)
	}
}
