// Package gcemeta answers facts about the Google Compute Engine environment
// this process runs in: whether it is on GCE at all, and if so its project,
// zone and region.
//
// It says WHERE this process runs. It says nothing about HOW it signs — that
// is pkg/nexuscreds and its sources. The two are deliberately separate
// packages, and the separation is the reason this one exists.
//
// # Why it is not part of googleadc
//
// These helpers began life inside pkg/nexuscreds/googleadc, which was a
// reasonable home right up until something other than a credential source
// wanted them. nexus.llm.gemini does: a Vertex deployment resolves its project
// and location off the metadata server, and that resolution is orthogonal to
// which credential source signs the request.
//
// googleadc registers itself with nexuscreds from an init function, so a
// package that imported it for the metadata helpers alone dragged that
// registration into every binary containing it. The gemini plugin sits in
// pkg/engine/allplugins, which put "google-adc" in the registry of every build
// carrying the plugin, blank import or not — and since gemini's credentials
// key DEFAULTS to google-adc, an embedder shipping their own Vault or SPIFFE
// source got Google ADC silently selected by a config that merely omitted the
// key, instead of an error naming the sources their binary actually has.
//
// So: this package performs NO registration and has NO init function. Import
// it to learn where you are. Blank-import a credential source, deliberately, to
// decide how you sign.
//
// # No abstraction on purpose
//
// There is no interface and no registry here. These are concrete functions over
// one concrete service — the GCE metadata server at 169.254.169.254, spoofable
// through GCE_METADATA_HOST, which is what makes them testable without a cloud
// account. A second seam over a single implementation would be genericising for
// an imagined caller, which this repository avoids on purpose.
//
// # Dependency placement
//
// This package imports cloud.google.com/go/compute/metadata and nothing else
// from Google. It does NOT import pkg/nexuscreds, which keeps that package
// stdlib-only and keeps the import edge pointing one way: googleadc consumes
// gcemeta, never the reverse.
package gcemeta
