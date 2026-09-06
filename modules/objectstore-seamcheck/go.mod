// The first Go submodule in this repository. See docs/src/guides/go-modules.md
// for the layout, the go.work decision and the tagging rules; the short version
// lives in doc.go next door.
module github.com/frankbardon/nexus/modules/objectstore-seamcheck

go 1.26.0

// Pinned to the newest core release rather than left at a placeholder version,
// so `go get` of this module from outside the repo resolves to something real.
// Inside the repo the replace below is what actually takes effect, and the
// replace is ignored by anyone who depends on this module -- that asymmetry is
// why the require line has to name a version that exists.
require github.com/frankbardon/nexus v0.19.0

// Local development and CI both build the seam from the working tree, not from
// a published tag: the whole point of this module is to fail when a change in
// pkg/engine/objectstore breaks an out-of-module implementer, and a pinned
// version could not do that. Consumers of this module ignore this directive.
replace github.com/frankbardon/nexus => ../..
