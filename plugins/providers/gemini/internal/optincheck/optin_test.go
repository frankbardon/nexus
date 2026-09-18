package optincheck

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/nexuscreds"
	"github.com/frankbardon/nexus/plugins/providers/gemini"
)

// googleADCSourceName is spelled out rather than taken from googleadc.Name,
// because importing that package to name it would register it and make the
// assertion below meaningless. It must match gemini's defaultCredentialSource.
const googleADCSourceName = "google-adc"

// TestLinkingTheGeminiPluginRegistersNoCredentialSource is the whole point of
// this package.
//
// The gemini plugin is in pkg/engine/allplugins, so every binary built from
// this repository's plugin set links it. If it reached a credential source
// through a package that registers one from init, that source would be in the
// registry of every such binary — and because gemini's credentials key defaults
// to "google-adc", an embedder who ships their own Vault or SPIFFE source would
// get Google ADC silently selected by a config that merely omitted the key,
// instead of an error naming the sources their binary actually carries.
//
// That is what happened once: the plugin imported pkg/nexuscreds/googleadc for
// its GCE metadata helpers and inherited the registration. The helpers moved to
// pkg/nexuscreds/gcemeta, which registers nothing. This test is what stops the
// import creeping back.
func TestLinkingTheGeminiPluginRegistersNoCredentialSource(t *testing.T) {
	// Reference the plugin so the import is real linkage rather than something
	// the compiler could elide. Constructing it runs no Init and touches no
	// network.
	if p := gemini.New(); p == nil {
		t.Fatal("gemini.New returned nil; this test is not linking the plugin it claims to")
	}

	if nexuscreds.Registered(googleADCSourceName) {
		t.Errorf("linking the gemini plugin registered the %q credential source; "+
			"it must come from a deliberate blank import in cmd/, not from the plugin's import graph",
			googleADCSourceName)
	}

	// The stronger statement, and the one that cannot pass by a misspelling:
	// linking the plugin puts NO source in the registry at all.
	if got := nexuscreds.Sources(); len(got) != 0 {
		t.Errorf("linking the gemini plugin registered credential sources %v, want none", got)
	}
}
