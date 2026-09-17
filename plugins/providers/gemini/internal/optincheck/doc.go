// Package optincheck is a build-graph canary. It contains no production code
// and exists only so its test can observe something no test inside package
// gemini can: what the nexuscreds registry looks like in a binary that links
// the gemini plugin and nothing else.
//
// The gemini plugin's own test binary blank-imports pkg/nexuscreds/googleadc,
// because the Vertex tests open "google-adc" by name. That blank import makes
// the registry non-empty for every test in that binary, so the assertion this
// package makes — that merely linking the plugin registers NOTHING — is
// unobservable from there, in the in-package and the _test external package
// alike. A separate package with its own test binary is the only place the
// question can be asked.
//
// It is under internal/ because nothing should ever import it. Its whole value
// is the import graph it does not have.
package optincheck
