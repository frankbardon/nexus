// Package nexuscreds acquires the outbound credentials Nexus presents to the
// services it calls.
//
// It is the mirror image of pkg/nexusauth, and the split is stated here because
// "auth" on its own is ambiguous: a reader who finds one of the two packages
// will otherwise assume it covers both directions.
//
//   - nexusauth VERIFIES a credential someone else minted, turning an inbound
//     HTTP request into a Principal. It never issues anything.
//   - nexuscreds ACQUIRES a credential this process will present on an OUTBOUND
//     request. It never verifies anything.
//
// The two share no types and do not call each other. They are siblings by
// symmetry only.
//
// The single abstraction is Source: something that can hand back a token.
// Sources are selected by name through a database/sql-style registry — Register
// from package init, Open by name when a plugin initialises.
//
// # Why a registry for one entry
//
// Nexus ships exactly one Source: google-adc (pkg/nexuscreds/googleadc), which
// resolves Google Application Default Credentials so a keyless pod can reach
// Vertex AI. A registry with a single implementation reads as premature
// genericization, so the reason is written down here rather than left to be
// rediscovered.
//
// The registry exists for EMBEDDERS, not for Nexus. Any organisation large
// enough to run workload identity already has an opinion about where a token
// comes from: Vault, SPIFFE/SPIRE, a corporate token broker, a sidecar that
// drops a file on a shared volume. Without a seam, supporting any one of those
// means forking a provider plugin — the credential logic is welded to the
// provider that consumes it, so the fork has to be re-done for every provider
// and re-based on every release. With the seam it is a package in the
// embedder's own tree, a blank import, and a config key:
//
//	import _ "example.com/internal/nexus/vaultcreds"
//
// That is the same trade pkg/engine/objectstore makes, for the same reason, and
// this registry is modelled on it deliberately.
//
// # Provider neutrality
//
// This package is stdlib-only and knows nothing about any cloud. The Google
// dependency lives entirely in the googleadc subpackage, so a build that does
// not blank-import it pays nothing for it — neither in binary size nor in the
// root module's defended direct-dependency list. Keep it that way: a
// provider-specific import here would undo the separation the subpackage exists
// to create.
//
// # Facts are not credentials
//
// The sibling subpackage gcemeta answers where a process runs — on GCE or not,
// and if so its project, zone and region. It deliberately registers NOTHING and
// has no init function, because those facts are wanted by code that has no
// opinion about credentials: nexus.llm.gemini resolves a Vertex project and
// location from them whichever source signs its requests. Keeping them out of
// googleadc is what stops merely linking that plugin from putting "google-adc"
// into the registry of every build, which would make the default selection of a
// credential source an accident of the import graph rather than the embedder's
// choice.
package nexuscreds
