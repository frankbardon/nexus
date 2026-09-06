// Package seamcheck is not a backend and stores nothing.
//
// It exists to hold one property true: that objectstore.Backend can be
// implemented, and objectstoretest.RunSuite can be run, from a module that is
// not github.com/frankbardon/nexus. The S3 and GCS backends live in their own
// modules precisely so their cloud SDKs stay out of the root module's
// dependency list, and that arrangement only works if the seam and its
// conformance suite are importable across a module boundary -- exported types,
// no internal/ packages in the path, no dependency pointing the wrong way.
//
// That is a property nothing else checks. A root-module test of the same suite
// passes whether or not the boundary is crossable, and the first story to
// discover the boundary is broken would otherwise be a backend story, which is
// the wrong place to find out.
//
// It is also the canary for the multi-module build plumbing itself: this
// directory is invisible to every ./... pattern run from the repo root, so if
// the Makefile's submodule walk or CI ever stops covering modules/, the tests
// here stop running and stop reporting. Deliberately break something in this
// package to confirm `make build` and `make test` still go red -- if they do
// not, the plumbing has regressed, not this module.
//
// There is no non-test code here on purpose. Adding behaviour would make this a
// thing to maintain rather than a check to run.
package seamcheck
