// Command stubvariant is the SECOND fake nexus instance used by the broker's
// integration test. It is byte-for-byte the same protocol implementation as
// testdata/stubinstance — both are thin mains over stubcore — and differs only
// in the compile-time identity it reports.
//
// It exists because a fake commandRunner can prove which path the broker
// RESOLVED but never that a second real executable boots, dials back and
// serves. Pointing a `binaries:` registry at this binary alongside stubinstance
// makes "the broker exec()d the entry the claim named" an assertion on
// behaviour rather than on a captured spawnSpec.
//
// The identity is linked in, not read from argv or the environment, precisely
// because a registry entry's args and env are themselves under test here: if a
// variant could be talked into reporting the other variant's name, the test
// would prove nothing.
//
// It lives under testdata/ so the normal `go build ./...` ignores it; the
// integration test builds it on demand and registers it as a spawn target.
package main

import "github.com/frankbardon/nexus/cmd/nexus-broker/testdata/stubcore"

func main() { stubcore.Run(stubcore.VariantAlt) }
